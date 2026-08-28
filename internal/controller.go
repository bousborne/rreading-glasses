package internal

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blampe/isbn"
	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/option"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// Use lower values while we're beta testing.
var (
	_authorTTL = 7 * 24 * time.Hour // 7 days.
	// _authorTTL  = 30 * 24 * time.Hour     // 1 month.

	_workTTL = 14 * 24 * time.Hour // 2 weeks.
	// _workTTL    = 30 * 24 * time.Hour     // 1 month.

	_editionTTL = 28 * 24 * time.Hour // 1 month.
	// _editionTTL = 6 * 30 * 24 * time.Hour // 6 months.

	_seriesTTL = 14 * 24 * time.Hour // 2 weeks

	// Format-split work relationships are provider repair metadata rather than
	// ordinary response cache entries. Keep them durable across many normal
	// work-cache lifetimes so either source ID can be refreshed as one atomic
	// canonical work after a deployment or cache expiry.
	_formatSplitMergeTTL = 100 * 365 * 24 * time.Hour

	// _missing is a sentinel value we cache for 404 responses.
	_missing = []byte{0}

	// _missingTTL is how long we'll wait before retrying a 404.
	_missingTTL = 7 * 24 * time.Hour
)

// unknownAuthor author corresponds to the "unknown" or "anonymous" authors
// which always 404. The valid "unknown" author ID seems to be 4699102 instead.
func unknownAuthor(authorID int64) bool {
	return authorID == 22294257 || authorID == 5158478 || authorID == 5481957 || authorID == 4699102 ||
		authorID == 14144674 || // SuperSummary, 10k works
		authorID == 5153555 || // Wikipedia, 120k
		authorID == 4340042 // Books LLC, 31k
}

// Controller facilitates operations on our cache by scheduling background work
// and handling cache invalidation.
//
// Most operations take place inside a singleflight group to prevent redundant
// work.
//
// The request path is limited to Get methods which at worst perform only O(1)
// lookups. More expensive work, like denormalization, is handled in the
// background. The original metadata server likely does this work in the
// request path, hence why larger authors don't work -- it can't complete
// O(work * editions) within the lifespan of the request.
//
// Another significant difference is that we cache data eagerly, when it is
// requested. We don't require a full database dump, so we're able to grab new
// works as soon as they're available.
type Controller struct {
	cache     cache[[]byte]
	getter    getter             // Core GetBook/GetAuthor/GetWork implementation.
	persister persister          // persister tracks state across reboots.
	group     singleflight.Group // Coalesce lookups for the same key.

	// denormC erializes denormalization updates. This should only be used when
	// all resources have already been fetched.
	denormC chan edge

	// refreshG limits how many authors/works we sync in the background. Use
	// this to fetch things in the background in a bounded way.
	refreshG errgroup.Group
	// refreshC collects author refreshes.
	refreshC chan refreshAuthor
	// authorRefreshRetryDelay controls how long an incomplete catalogue refresh
	// waits before it is placed back on the worker queue.
	authorRefreshRetryDelay time.Duration
	// authorRefreshRetryMax caps exponential retry backoff. A failed refresh is
	// kept persisted, but it must not continuously consume the upstream quota.
	authorRefreshRetryMax time.Duration
	// authorRefreshMaxAttempts pauses an unhealthy author after a bounded number
	// of complete-catalog attempts. Its durable marker remains available for an
	// explicit request or a later process restart.
	authorRefreshMaxAttempts int
	// authorRecoveryInterval spaces persisted refreshes recovered at startup.
	// Recovery is intentionally paced so a restart cannot create a request
	// storm against the metadata provider.
	authorRecoveryInterval time.Duration
	// authorRefreshes tracks queued, running, and delayed author refreshes. It
	// prevents duplicate work from explicit author requests, denormalization
	// races, and startup recovery.
	authorRefreshMu sync.Mutex
	authorRefreshes map[int64]struct{}

	// Run and Shutdown use separate scheduler and worker lifetimes. Shutdown
	// first stops admitting refresh work, then waits for active workers and a
	// FIFO denormalization barrier before stopping Run. This prevents completed
	// refreshes from being stranded in persistence during a normal deployment.
	lifecycleMu         sync.Mutex
	runReady            chan struct{}
	runDone             chan struct{}
	refreshDispatchDone chan struct{}
	runCancel           context.CancelFunc
	schedulerCancel     context.CancelFunc
	started             bool
	shutdownOnce        sync.Once

	// denormProducerG tracks request/callback tasks which can enqueue edges.
	// Admission is closed before Shutdown waits, preventing WaitGroup Add/Wait
	// races; a nested submission after closure runs inline in its already
	// tracked parent so no denormalization work is lost.
	denormProducerMu sync.Mutex
	denormProducerG  sync.WaitGroup
	denormAccepting  bool

	// workG collects work refreshes.
	workG errgroup.Group

	metrics *controllerMetrics
}

// getter allows alternative implementations of the core logic to be injected.
// Don't write to the cache if you use it.
type getter interface {
	// GetWork gets the work with the given ID. A work is an abstract
	// collection of editions. The saveEditions callback can be invoked if the
	// work can be loaded with editions in one request, to reduce load
	// upstream. When authorID is returned the work will be denormalized to the
	// author.
	//
	// A serialized workResource is returned.
	GetWork(ctx context.Context, workID int64, saveEditions editionsCallback) (_ []byte, authorID int64, _ error)

	// GetBook gets an individual edition of a work. The saveEditions
	// callback can be invoked if the edition can be loaded with other editions
	// in one request, to reduce load upstream.
	//
	// Confusingly, a serialized workResource is also returned here. It should
	// be a valid work and it should include one book (the one being loaded).
	GetBook(ctx context.Context, bookID int64, saveEditions editionsCallback) (_ []byte, workID int64, authorID int64, _ error) // Returns a serialized Work??

	// GetAuthor gets an author's details.
	//
	// A serialized AuthorResource is returned.
	GetAuthor(ctx context.Context, authorID int64) ([]byte, error)

	GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq2[int64, error] // Returns book/edition IDs, not works.

	// GetSeries returns a list of works contained in a series. The works may
	// not all be by the same author.
	GetSeries(ctx context.Context, seriesID int64) (*SeriesResource, error)

	// Search performs a natural language query against the upstream (or other
	// search index).
	//
	// A serialied searchResource is returned.
	Search(ctx context.Context, query string) ([]SearchResource, error)

	// Recommendations returns a list of work IDs which are trending or popular.
	// Eventually we may consider implementing OAuth in order to return
	// custom-tailored recommendations.
	Recommendations(ctx context.Context, page int64) (RecommentationsResource, error)
}

// workCacheSchemaGetter identifies providers which emit the current serialized
// work schema. Test/third-party getters without this optional interface retain
// legacy cache behavior; the built-in GR and Hardcover getters both implement
// it and therefore invalidate pre-upgrade WorkKey and BookKey rows.
type workCacheSchemaGetter interface {
	workCacheSchemaVersion() int
}

func currentWorkCachePayload(g getter, payload []byte) bool {
	versioned, ok := g.(workCacheSchemaGetter)
	if !ok {
		return true
	}
	return hasCurrentWorkCacheSchema(payload, versioned.workCacheSchemaVersion())
}

func currentAuthorCachePayload(g getter, payload []byte) bool {
	// Authors and their nested works advance under the same resource schema.
	return currentWorkCachePayload(g, payload)
}

// NewUpstream creates a new http.Client with middleware appropriate for use
// with an upstream.
func NewUpstream(host string, proxy string) (*http.Client, error) {
	upstream := &http.Client{
		Transport: throttledTransport{
			ticker: time.NewTicker(time.Second / 3),
			RoundTripper: ScopedTransport{
				Host:         host,
				RoundTripper: errorProxyTransport{http.DefaultTransport},
			},
		},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			// Don't follow redirects on HEAD requests. We use this to sniff
			// work->book mappings without loading everything.
			if req.Method == http.MethodHead {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	if proxy != "" {
		url, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		// TODO: This doesn't work.
		upstream.Transport.(*http.Transport).Proxy = http.ProxyURL(url)
	}

	return upstream, nil
}

type ControllerConfig struct {
	AuthorRefreshConcurrency int
	AuthorRefreshRetryBase   time.Duration
	AuthorRefreshRetryMax    time.Duration
	AuthorRefreshMaxAttempts int
	AuthorRecoveryInterval   time.Duration
}

func DefaultControllerConfig() ControllerConfig {
	return ControllerConfig{
		AuthorRefreshConcurrency: 1,
		AuthorRefreshRetryBase:   time.Hour,
		AuthorRefreshRetryMax:    24 * time.Hour,
		AuthorRefreshMaxAttempts: 6,
		AuthorRecoveryInterval:   5 * time.Minute,
	}
}

// NewController creates a controller with conservative refresh defaults.
func NewController(cache cache[[]byte], getter getter, persister persister, reg *prometheus.Registry) (*Controller, error) {
	return NewConfiguredController(cache, getter, persister, DefaultControllerConfig(), reg)
}

// NewConfiguredController creates a controller. Author catalogue refreshes
// use a dedicated bounded pool, while edition/work maintenance remains
// independent.
func NewConfiguredController(cache cache[[]byte], getter getter, persister persister, config ControllerConfig, reg *prometheus.Registry) (*Controller, error) {
	if config.AuthorRefreshConcurrency <= 0 {
		return nil, fmt.Errorf("author refresh concurrency must be positive")
	}
	if config.AuthorRefreshRetryBase <= 0 || config.AuthorRefreshRetryMax < config.AuthorRefreshRetryBase {
		return nil, fmt.Errorf("author refresh retry range is invalid")
	}
	if config.AuthorRefreshMaxAttempts <= 0 || config.AuthorRecoveryInterval <= 0 {
		return nil, fmt.Errorf("author refresh attempts and recovery interval must be positive")
	}
	metrics := newControllerMetrics(reg)
	c := &Controller{
		cache:     cache,
		getter:    getter,
		persister: &nopersist{},
		metrics:   metrics,

		denormC:  make(chan edge),
		refreshC: make(chan refreshAuthor),

		authorRefreshRetryDelay:  config.AuthorRefreshRetryBase,
		authorRefreshRetryMax:    config.AuthorRefreshRetryMax,
		authorRefreshMaxAttempts: config.AuthorRefreshMaxAttempts,
		authorRecoveryInterval:   config.AuthorRecoveryInterval,
		authorRefreshes:          make(map[int64]struct{}),
		runReady:                 make(chan struct{}),
		runDone:                  make(chan struct{}),
		refreshDispatchDone:      make(chan struct{}),
		denormAccepting:          true,
	}
	if persister != nil {
		c.persister = persister
	}

	// Author refreshes expand into many book/work lookups. Keep them strictly
	// serialized; the GraphQL batching layer still handles individual request
	// throughput, while this limit prevents catalogue crawls from multiplying
	// that load.
	c.refreshG.SetLimit(config.AuthorRefreshConcurrency)
	c.workG.SetLimit(25) // Sure why not.

	return c, nil
}

// GetBook loads a book (edition) or returns a cached value if one exists.
// TODO: This should only return a book!
func (c *Controller) GetBook(ctx context.Context, bookID int64) ([]byte, time.Duration, error) {
	p, err, _ := c.group.Do(BookKey(bookID), func() (any, error) {
		return c.getBook(ctx, bookID)
	})
	pair := p.(ttlpair)
	return pair.bytes, pair.ttl, err
}

// Search queries the metadata provider.
func (c *Controller) Search(ctx context.Context, query string) ([]SearchResource, error) {
	if _asin.Match([]byte(query)) {
		// Try an ASIN lookup and fall back to regular search if that doesn't work.
		if results := c.searchASIN(ctx, query); len(results) > 0 {
			return results, nil
		}
	}
	if isbn, err := isbn.Parse(query); err == nil && isbn != nil {
		if results := c.searchISBN(ctx, *isbn); len(results) > 0 {
			return results, nil
		}
	}
	results, err := c.getter.Search(ctx, query)
	if err != nil {
		return nil, err
	}

	seenWorks := map[int64]struct{}{}
	seenBooks := map[int64]struct{}{}
	deduped := []SearchResource{}
	for _, r := range results {
		if _, seen := seenBooks[r.BookID]; seen {
			continue
		}
		if _, seen := seenWorks[r.WorkID]; seen {
			continue
		}
		seenBooks[r.BookID] = struct{}{}
		seenWorks[r.WorkID] = struct{}{}
		deduped = append(deduped, r)
	}
	return deduped, nil
}

// Recommendations returns recommended work IDs.
func (c *Controller) Recommendations(ctx context.Context, page int64) (RecommentationsResource, error) {
	recs, err := c.getter.Recommendations(ctx, page)
	if err != nil {
		return recs, err
	}

	// Try to fetch everything and return only the stuff that won't 404.
	mu := sync.Mutex{}
	wg := sync.WaitGroup{}
	workIDs := []int64{}

	for _, workID := range recs.WorkIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := c.GetWork(ctx, workID)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			workIDs = append(workIDs, workID)
		}()
	}
	wg.Wait()
	recs.WorkIDs = workIDs
	return recs, nil
}

func (c *Controller) searchASIN(ctx context.Context, asin string) []SearchResource {
	editionID, err := c.GetASIN(ctx, asin)
	if err != nil {
		return nil
	}

	workBytes, _, err := c.GetBook(ctx, editionID)
	if err != nil {
		return nil
	}

	var workRsc workResource
	err = json.Unmarshal(workBytes, &workRsc)
	if err != nil {
		return nil
	}

	return []SearchResource{{
		BookID: workRsc.Books[0].ForeignID,
		WorkID: workRsc.ForeignID,
		Author: SearchResourceAuthor{
			ID: workRsc.Authors[0].ForeignID,
		},
	}}
}

func (c *Controller) searchISBN(ctx context.Context, isbn isbn.ISBN) []SearchResource {
	editionID, err := c.GetISBN(ctx, isbn)
	if err != nil {
		return nil
	}

	workBytes, _, err := c.GetBook(ctx, editionID)
	if err != nil {
		return nil
	}

	var workRsc workResource
	err = json.Unmarshal(workBytes, &workRsc)
	if err != nil {
		return nil
	}

	return []SearchResource{{
		BookID: workRsc.Books[0].ForeignID,
		WorkID: workRsc.ForeignID,
		Author: SearchResourceAuthor{
			ID: workRsc.Authors[0].ForeignID,
		},
	}}
}

// GetWork loads a work or returns a cached value if one exists.
func (c *Controller) GetWork(ctx context.Context, workID int64) ([]byte, time.Duration, error) {
	flightKey := WorkKey(workID)
	if merge, ok := c.loadFormatSplitMerge(ctx, workID); ok {
		flightKey = formatSplitMergeKey(merge.CanonicalWorkID)
	}
	p, err, _ := c.group.Do(flightKey, func() (any, error) {
		return c.getWork(ctx, workID)
	})
	pair := p.(ttlpair)
	return pair.bytes, pair.ttl, err
}

// GetAuthor loads an author or returns a cached value if one exists.
func (c *Controller) GetAuthor(ctx context.Context, authorID int64) ([]byte, time.Duration, error) {
	// The "unknown author" ID is never loadable, so we can short-circuit.
	if unknownAuthor(authorID) {
		return nil, _missingTTL, errNotFound
	}
	p, err, _ := c.group.Do(AuthorKey(authorID), func() (any, error) {
		return c.getAuthor(ctx, authorID)
	})
	pair := p.(ttlpair)
	if err == nil {
		// A shallow lookup may have won the singleflight race. Scheduling after
		// the shared call guarantees that an explicit author request still starts
		// exactly one full catalogue refresh.
		c.queueAuthorRefreshIfNeeded(ctx, authorID, pair.bytes)
	}
	return pair.bytes, pair.ttl, err
}

// getAuthorForDenormalization loads enough author data to maintain cache
// relationships without starting a full catalogue crawl. A subsequent
// explicit GetAuthor call observes the refresh-needed marker and schedules the
// normal background refresh.
func (c *Controller) getAuthorForDenormalization(ctx context.Context, authorID int64) ([]byte, time.Duration, error) {
	if unknownAuthor(authorID) {
		return nil, _missingTTL, errNotFound
	}
	p, err, _ := c.group.Do(AuthorKey(authorID), func() (any, error) {
		return c.getAuthor(ctx, authorID)
	})
	pair := p.(ttlpair)
	return pair.bytes, pair.ttl, err
}

// GetSeries returns a cached series if one exists.
func (c *Controller) GetSeries(ctx context.Context, seriesID int64) ([]byte, error) {
	out, err, _ := c.group.Do(seriesKey(seriesID), func() (any, error) {
		return c.getSeries(ctx, seriesID)
	})
	return out.([]byte), err
}

// GetASIN returns the best known edition ID for the given ASIN, or a not found
// error if there is none.
func (c *Controller) GetASIN(ctx context.Context, asin string) (int64, error) {
	out, err, _ := c.group.Do(asin, func() (any, error) {
		return c.getASIN(ctx, asin)
	})
	return out.(int64), err
}

func (c *Controller) getASIN(ctx context.Context, asin string) (int64, error) {
	bytes, ok := c.cache.Get(ctx, asinKey(asin))
	if !ok {
		return 0, errNotFound
	}

	var asinRsc lookupResource
	err := json.Unmarshal(bytes, &asinRsc)
	if err != nil {
		return 0, fmt.Errorf("unmarshaling for asin: %w", err)
	}

	return asinRsc.EditionID, nil
}

func (c *Controller) setASIN(ctx context.Context, asin string, editionID int64) error {
	bytes, err := json.Marshal(lookupResource{EditionID: editionID})
	if err != nil {
		return fmt.Errorf("marshaling for asin: %w", err)
	}
	c.cache.Set(ctx, asinKey(asin), bytes, 24*time.Hour*365)
	return nil
}

// GetISBN returns the best known edition ID for the given ISBN13, or a not found
// error if there is none.
func (c *Controller) GetISBN(ctx context.Context, isbn isbn.ISBN) (int64, error) {
	out, err, _ := c.group.Do(isbn.Canonical(), func() (any, error) {
		return c.getISBN(ctx, isbn)
	})
	return out.(int64), err
}

func (c *Controller) getISBN(ctx context.Context, isbn isbn.ISBN) (int64, error) {
	bytes, ok := c.cache.Get(ctx, isbnKey(isbn))
	if !ok {
		return 0, errNotFound
	}

	var asinRsc lookupResource
	err := json.Unmarshal(bytes, &asinRsc)
	if err != nil {
		return 0, fmt.Errorf("unmarshaling for asin: %w", err)
	}

	return asinRsc.EditionID, nil
}

func (c *Controller) setISBN(ctx context.Context, isbn isbn.ISBN, editionID int64) error {
	bytes, err := json.Marshal(lookupResource{EditionID: editionID})
	if err != nil {
		return fmt.Errorf("marshaling for asin: %w", err)
	}
	c.cache.Set(ctx, isbnKey(isbn), bytes, 24*time.Hour*365)
	return nil
}

func (c *Controller) getBook(ctx context.Context, bookID int64) (ttlpair, error) {
	workBytes, ttl, ok := c.cache.GetWithTTL(ctx, BookKey(bookID))
	if ok && !slices.Equal(workBytes, _missing) && !currentWorkCachePayload(c.getter, workBytes) {
		Log(ctx).Info("refreshing legacy cached edition payload", "bookID", bookID)
		if err := c.cache.Expire(ctx, BookKey(bookID)); err != nil {
			Log(ctx).Warn("unable to expire legacy cached edition payload", "bookID", bookID, "err", err)
		}
		workBytes, ttl, ok = nil, 0, false
	}
	if ok && ttl > 0 {
		if slices.Equal(workBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: workBytes, ttl: ttl}, nil
	}

	// Cache miss.
	workBytes, workID, authorID, err := c.getter.GetBook(ctx, bookID, c.saveEditions)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, BookKey(bookID), _missing, _missingTTL)
		return ttlpair{}, err
	}
	if err != nil {
		Log(ctx).Warn("problem getting book", "err", err, "bookID", bookID)
		return ttlpair{}, err
	}

	ttl = fuzz(_editionTTL, 2.0)
	c.cache.Set(ctx, BookKey(bookID), workBytes, ttl)

	if workID > 0 {
		// Ensure the edition/book is included with the work, but don't block the response.
		c.submitDenormTask(func() {
			// Decouple our context from the request.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if _, _, err := c.GetWork(ctx, workID); err != nil { // Ensure fetched.
				Log(ctx).Warn("skipping work denorm due to error", "bookID", bookID, "workID", workID, "err", err)
				return
			}
			if _, _, err := c.getAuthorForDenormalization(ctx, authorID); err != nil { // Ensure fetched without crawling.
				if errors.Is(err, errNotFound) {
					// Something's not right -- we know this author must exist
					// because the work belongs to it, but we have a 404
					// cached. Trigger a fresh fetch and delete any in-progress
					// refresh. This should hopefully be enough to get back to
					// a good state.
					Log(ctx).Warn("force refreshing author due to unexpected 404", "bookID", bookID, "authorID", authorID)
					_ = c.cache.Expire(ctx, AuthorKey(authorID))
					_, _, err = c.getAuthorForDenormalization(ctx, authorID)
					if err != nil {
						Log(ctx).Warn("skipping author denorm after forced refresh failed", "bookID", bookID, "authorID", authorID, "err", err)
						return
					}
				} else {
					Log(ctx).Warn("skipping author denorm due to error", "bookID", bookID, "authorID", authorID, "err", err)
					return
				}
			}
			_ = c.enqueueDenorm(ctx, edge{kind: workEdge, parentID: workID, childIDs: newSet(bookID)})
		})
	}

	return ttlpair{bytes: workBytes, ttl: ttl}, nil
}

func (c *Controller) getWork(ctx context.Context, workID int64) (ttlpair, error) {
	cachedBytes, ttl, ok := c.cache.GetWithTTL(ctx, WorkKey(workID))
	var legacyReconcileBytes []byte
	if ok && !slices.Equal(cachedBytes, _missing) && !currentWorkCachePayload(c.getter, cachedBytes) {
		Log(ctx).Info("refreshing legacy cached work payload", "workID", workID)
		// This payload is not safe to return or use as an upstream identity
		// shortcut, but its enriched edition metadata can still be retained after
		// the fresh provider membership independently validates each edition ID.
		legacyReconcileBytes = bytes.Clone(cachedBytes)
		if err := c.cache.Expire(ctx, WorkKey(workID)); err != nil {
			Log(ctx).Warn("unable to expire legacy cached work payload", "workID", workID, "err", err)
		}
		cachedBytes, ttl, ok = nil, 0, false
	}
	if ok && ttl > 0 {
		if slices.Equal(cachedBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: cachedBytes, ttl: ttl}, nil
	}
	if merge, merged := c.loadFormatSplitMerge(ctx, workID); merged {
		return c.refreshFormatSplitMerge(ctx, workID, merge, cachedBytes)
	}

	// Cache miss.
	workBytes, authorID, err := c.getter.GetWork(ctx, workID, c.saveEditions)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, WorkKey(workID), _missing, _missingTTL)
		return ttlpair{}, err
	}
	if err != nil {
		Log(ctx).Warn("problem getting work", "err", err, "workID", workID)
		return ttlpair{}, err
	}

	var fresh workResource
	if unmarshalErr := json.Unmarshal(workBytes, &fresh); unmarshalErr != nil {
		return ttlpair{}, fmt.Errorf("unmarshaling refreshed work %d: %w", workID, unmarshalErr)
	}
	reconcileBytes := cachedBytes
	if len(reconcileBytes) == 0 {
		reconcileBytes = legacyReconcileBytes
	}
	if len(reconcileBytes) > 0 {
		var stale workResource
		if unmarshalErr := json.Unmarshal(reconcileBytes, &stale); unmarshalErr == nil {
			fresh = reconcileRefreshedWork(fresh, stale, editionIDSet(fresh.ProviderEditionIDs))
		}
	}
	workBytes, err = json.Marshal(fresh)
	if err != nil {
		return ttlpair{}, fmt.Errorf("marshaling refreshed work %d: %w", workID, err)
	}

	ttl = fuzz(_workTTL, 1.5)
	c.cache.Set(ctx, WorkKey(workID), workBytes, ttl)

	// Submit relationship maintenance to the bounded pool before returning.
	// Group.Go returns immediately while capacity is available and applies
	// deliberate backpressure at the configured limit; there is no untracked
	// dispatcher goroutine that can race Shutdown's workG.Wait.
	c.workG.Go(func() error {
		ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("refresh-work-%d", workID))

		defer func() {
			if r := recover(); r != nil {
				Log(ctx).Error("panic", "details", r)
			}
		}()

		// Ensure we keep whatever editions we already had cached.
		var cached workResource
		_ = json.Unmarshal(cachedBytes, &cached)

		cachedBookIDs := []int64{}
		for _, b := range cached.Books {
			if _, _, err := c.GetBook(ctx, b.ForeignID); err == nil { // Ensure fetched.
				cachedBookIDs = append(cachedBookIDs, b.ForeignID)
			}
		}

		if authorID > 0 {
			_, _, _ = c.getAuthorForDenormalization(ctx, authorID) // Ensure fetched without crawling.
		}

		_ = c.enqueueDenorm(ctx, edge{kind: workEdge, parentID: workID, childIDs: newSet(cachedBookIDs...)})

		if authorID > 0 {
			// Ensure the work belongs to its author.
			_ = c.enqueueDenorm(ctx, edge{kind: authorEdge, parentID: authorID, childIDs: newSet(workID)})
		}
		return nil
	})

	// The provider refresh and stale-edition reconciliation above are complete
	// and have already been published atomically. Returning the expired payload
	// here would let the first caller persist provider defaults which are older
	// than the value we just wrote to the cache.
	return ttlpair{bytes: workBytes, ttl: ttl}, err
}

type formatSplitMerge struct {
	CanonicalWorkID int64   `json:"canonicalWorkId"`
	SourceWorkIDs   []int64 `json:"sourceWorkIds"`
}

func formatSplitMergeKey(workID int64) string {
	return fmt.Sprintf("fm%d", workID)
}

func formatSplitSourceKey(workID int64) string {
	return fmt.Sprintf("fs%d", workID)
}

func normalizeFormatSplitMerge(merge formatSplitMerge) (formatSplitMerge, bool) {
	if merge.CanonicalWorkID == 0 {
		return formatSplitMerge{}, false
	}

	seen := make(map[int64]struct{}, len(merge.SourceWorkIDs)+1)
	sources := make([]int64, 0, len(merge.SourceWorkIDs)+1)
	add := func(id int64) {
		if id == 0 {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		sources = append(sources, id)
	}
	add(merge.CanonicalWorkID)
	for _, id := range merge.SourceWorkIDs {
		add(id)
	}
	if len(sources) < 2 {
		return formatSplitMerge{}, false
	}
	aliases := sources[1:]
	slices.Sort(aliases)
	merge.SourceWorkIDs = sources
	return merge, true
}

func (c *Controller) loadFormatSplitMerge(ctx context.Context, workID int64) (formatSplitMerge, bool) {
	data, _, ok := c.cache.GetWithTTL(ctx, formatSplitMergeKey(workID))
	if !ok || len(data) == 0 {
		return formatSplitMerge{}, false
	}
	var merge formatSplitMerge
	if err := json.Unmarshal(data, &merge); err != nil {
		Log(ctx).Warn("discarding malformed format-split merge record", "workID", workID, "err", err)
		return formatSplitMerge{}, false
	}
	merge, valid := normalizeFormatSplitMerge(merge)
	if !valid || !slices.Contains(merge.SourceWorkIDs, workID) {
		Log(ctx).Warn("discarding invalid format-split merge record", "workID", workID)
		return formatSplitMerge{}, false
	}
	return merge, true
}

func (c *Controller) persistFormatSplitMerge(ctx context.Context, merge formatSplitMerge, sources map[int64]workResource) {
	merge, ok := normalizeFormatSplitMerge(merge)
	if !ok {
		return
	}
	data, err := json.Marshal(merge)
	if err != nil {
		Log(ctx).Warn("unable to marshal format-split merge record", "err", err)
		return
	}
	for _, sourceID := range merge.SourceWorkIDs {
		source, exists := sources[sourceID]
		if !exists || source.ForeignID != sourceID {
			Log(ctx).Warn("refusing incomplete format-split merge record", "sourceWorkID", sourceID)
			return
		}
	}

	// Write source snapshots first and publish the relationship last. A reader
	// can therefore never observe a new merge descriptor without every source
	// required to rebuild it.
	for _, sourceID := range merge.SourceWorkIDs {
		sourceBytes, marshalErr := json.Marshal(sources[sourceID])
		if marshalErr != nil {
			Log(ctx).Warn("unable to marshal format-split source", "sourceWorkID", sourceID, "err", marshalErr)
			return
		}
		c.cache.Set(ctx, formatSplitSourceKey(sourceID), sourceBytes, _formatSplitMergeTTL)
	}
	for _, sourceID := range merge.SourceWorkIDs {
		c.cache.Set(ctx, formatSplitMergeKey(sourceID), data, _formatSplitMergeTTL)
	}
}

func (c *Controller) refreshFormatSplitMerge(ctx context.Context, requestedWorkID int64, merge formatSplitMerge, expiredBytes []byte) (ttlpair, error) {
	merge, ok := normalizeFormatSplitMerge(merge)
	if !ok {
		return ttlpair{}, errors.Join(errNotFound, errors.New("invalid format-split merge"))
	}

	// HCGetter shares this cache. Expire every published merged WorkKey before
	// fetching so a still-live sibling key cannot masquerade as a raw provider
	// source and silently preserve stale defaults.
	for _, sourceID := range merge.SourceWorkIDs {
		if err := c.cache.Expire(ctx, WorkKey(sourceID)); err != nil {
			return ttlpair{}, fmt.Errorf("expiring merged source work %d: %w", sourceID, err)
		}
	}

	sources := make(map[int64]workResource, len(merge.SourceWorkIDs))
	authorID := int64(0)
	for _, sourceID := range merge.SourceWorkIDs {
		freshBytes, freshAuthorID, err := c.getter.GetWork(ctx, sourceID, c.saveEditions)
		if err != nil {
			return ttlpair{}, fmt.Errorf("refreshing format-split source work %d: %w", sourceID, err)
		}
		var fresh workResource
		if err := json.Unmarshal(freshBytes, &fresh); err != nil {
			return ttlpair{}, fmt.Errorf("unmarshaling format-split source work %d: %w", sourceID, err)
		}
		if fresh.ForeignID != sourceID {
			if fresh.ForeignID == 0 {
				return ttlpair{}, fmt.Errorf("format-split source identity missing: requested=%d", sourceID)
			}
			// Hardcover has canonicalized one of the formerly split works. The
			// returned provider canonical is now authoritative; retaining the old
			// relationship would make every future expiry retry an impossible raw
			// source ID forever.
			return c.resolveFormatSplitRedirect(ctx, merge, fresh, freshAuthorID, expiredBytes)
		}
		if authorID == 0 {
			authorID = freshAuthorID
		} else if freshAuthorID != 0 && freshAuthorID != authorID {
			return ttlpair{}, fmt.Errorf("format-split sources have different authors: expected=%d got=%d", authorID, freshAuthorID)
		}

		if staleBytes, _, found := c.cache.GetWithTTL(ctx, formatSplitSourceKey(sourceID)); found {
			var stale workResource
			if err := json.Unmarshal(staleBytes, &stale); err == nil {
				fresh = reconcileRefreshedWork(fresh, stale, editionIDSet(fresh.ProviderEditionIDs))
			}
		}
		sources[sourceID] = fresh
	}
	if !formatSplitSourcesStillMerge(merge, sources) {
		return c.resolveFormatSplitDivergence(ctx, requestedWorkID, merge, sources, authorID)
	}

	merged := sources[merge.CanonicalWorkID]
	for _, sourceID := range merge.SourceWorkIDs[1:] {
		merged = combineWorks(merged, sources[sourceID])
	}
	if merged.ForeignID != merge.CanonicalWorkID {
		return ttlpair{}, fmt.Errorf("format-split canonical identity mismatch: expected=%d got=%d", merge.CanonicalWorkID, merged.ForeignID)
	}
	if len(expiredBytes) > 0 {
		var staleMerged workResource
		if err := json.Unmarshal(expiredBytes, &staleMerged); err == nil {
			merged = reconcileRefreshedWork(merged, staleMerged, editionIDSet(merged.ProviderEditionIDs))
		}
	}

	mergedBytes, err := json.Marshal(merged)
	if err != nil {
		return ttlpair{}, fmt.Errorf("marshaling format-split work %d: %w", merge.CanonicalWorkID, err)
	}
	ttl := fuzz(_workTTL, 1.5)
	// Publish only after every source refreshed and the canonical payload was
	// fully rebuilt. Both source IDs receive the exact same canonical bytes.
	for _, sourceID := range merge.SourceWorkIDs {
		c.cache.Set(ctx, WorkKey(sourceID), mergedBytes, ttl)
	}
	c.persistFormatSplitMerge(ctx, merge, sources)

	c.scheduleFormatSplitAuthor(authorID, merge.CanonicalWorkID)
	return ttlpair{bytes: mergedBytes, ttl: ttl}, nil
}

func formatSplitSourcesStillMerge(merge formatSplitMerge, sources map[int64]workResource) bool {
	works := make([]workResource, 0, len(merge.SourceWorkIDs))
	for _, sourceID := range merge.SourceWorkIDs {
		source, ok := sources[sourceID]
		if !ok {
			return false
		}
		works = append(works, source)
	}
	merged, aliases := mergeDuplicateFormatWorks(works)
	if len(merged) != 1 || merged[0].ForeignID != merge.CanonicalWorkID || len(aliases) != len(merge.SourceWorkIDs)-1 {
		return false
	}
	for _, sourceID := range merge.SourceWorkIDs {
		if sourceID == merge.CanonicalWorkID {
			continue
		}
		if aliases[sourceID] != merge.CanonicalWorkID {
			return false
		}
	}
	return true
}

func (c *Controller) retireFormatSplitMerge(ctx context.Context, merge formatSplitMerge) error {
	var retireErr error
	for _, sourceID := range merge.SourceWorkIDs {
		retireErr = errors.Join(retireErr, c.cache.Delete(ctx, formatSplitMergeKey(sourceID)))
		retireErr = errors.Join(retireErr, c.cache.Delete(ctx, formatSplitSourceKey(sourceID)))
	}
	return retireErr
}

func (c *Controller) resolveFormatSplitDivergence(ctx context.Context, requestedWorkID int64, merge formatSplitMerge, sources map[int64]workResource, authorID int64) (ttlpair, error) {
	serialized := make(map[int64][]byte, len(sources))
	for sourceID, source := range sources {
		data, err := json.Marshal(source)
		if err != nil {
			return ttlpair{}, fmt.Errorf("marshaling corrected format-split source %d: %w", sourceID, err)
		}
		serialized[sourceID] = data
	}
	if err := c.retireFormatSplitMerge(ctx, merge); err != nil {
		return ttlpair{}, fmt.Errorf("retiring diverged format-split relationship: %w", err)
	}

	ttl := fuzz(_workTTL, 1.5)
	for sourceID, data := range serialized {
		c.cache.Set(ctx, WorkKey(sourceID), data, ttl)
	}
	requested, ok := serialized[requestedWorkID]
	if !ok {
		return ttlpair{}, fmt.Errorf("corrected format-split relationship omitted requested work %d", requestedWorkID)
	}
	if authorID > 0 {
		// The author's cached work list may still contain the old combined
		// record. Mark it for a complete explicit refresh rather than trying to
		// repair a topology change with a single incremental edge.
		c.cache.Set(ctx, authorRefreshNeededKey(authorID), []byte{1}, 365*24*time.Hour)
	}
	return ttlpair{bytes: requested, ttl: ttl}, nil
}

func (c *Controller) resolveFormatSplitRedirect(ctx context.Context, merge formatSplitMerge, canonical workResource, authorID int64, expiredBytes []byte) (ttlpair, error) {
	if len(expiredBytes) > 0 {
		var staleMerged workResource
		if err := json.Unmarshal(expiredBytes, &staleMerged); err == nil && staleMerged.ForeignID == canonical.ForeignID {
			canonical = reconcileRefreshedWork(canonical, staleMerged, editionIDSet(canonical.ProviderEditionIDs))
		}
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return ttlpair{}, fmt.Errorf("marshaling redirected format-split work %d: %w", canonical.ForeignID, err)
	}

	// Delete the relationship before publishing the redirect. If durable state
	// cannot be retired, fail closed so a later request can retry the cleanup
	// instead of leaving a permanent descriptor/identity mismatch loop.
	if err := c.retireFormatSplitMerge(ctx, merge); err != nil {
		return ttlpair{}, fmt.Errorf("retiring canonicalized format-split relationship: %w", err)
	}

	ttl := fuzz(_workTTL, 1.5)
	for _, sourceID := range merge.SourceWorkIDs {
		c.cache.Set(ctx, WorkKey(sourceID), canonicalBytes, ttl)
	}
	c.cache.Set(ctx, WorkKey(canonical.ForeignID), canonicalBytes, ttl)
	c.scheduleFormatSplitAuthor(authorID, canonical.ForeignID)
	return ttlpair{bytes: canonicalBytes, ttl: ttl}, nil
}

func (c *Controller) scheduleFormatSplitAuthor(authorID, workID int64) {
	if authorID == 0 {
		return
	}
	c.workG.Go(func() error {
		background := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("refresh-format-split-%d", workID))
		_, _, _ = c.getAuthorForDenormalization(background, authorID)
		_ = c.enqueueDenorm(background, edge{kind: authorEdge, parentID: authorID, childIDs: newSet(workID)})
		return nil
	})
}

func (c *Controller) getSeries(ctx context.Context, seriesID int64) ([]byte, error) {
	seriesBytes, ttl, ok := c.cache.GetWithTTL(ctx, seriesKey(seriesID))
	if ok && ttl > 0 {
		if slices.Equal(seriesBytes, _missing) {
			return nil, errNotFound
		}
		return seriesBytes, nil
	}

	Log(ctx).Debug("getting series", "seriesID", seriesID)

	series, err := c.getter.GetSeries(ctx, seriesID)
	if err != nil {
		Log(ctx).Warn("problem getting series", "seriesID", seriesID, "err", err)
		return nil, err
	}

	out, err := json.Marshal(series)
	if err != nil {
		return nil, err
	}

	c.cache.Set(ctx, seriesKey(seriesID), out, _seriesTTL)

	return out, nil
}

func (c *Controller) saveEditions(grBooks ...workResource) {
	c.submitDenormTask(func() {
		ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("save-editions-%d", time.Now().Unix()))

		var grWorkID int64
		grBookIDs := []int64{}

		for _, w := range grBooks {
			if len(w.Books) != 1 {
				// We expect a single book wrapped in a work -- side effect of R's odd data model.
				Log(ctx).Warn("malformed edition", "grWorkID", w.ForeignID)
				continue
			}
			if grWorkID == 0 {
				grWorkID = w.ForeignID
			}
			if w.ForeignID != grWorkID {
				// Editions should all belong to the same work.
				Log(ctx).Warn("work-edition mismatch", "expected", grWorkID, "got", w.ForeignID)
				continue
			}
			if len(w.Authors) == 0 {
				Log(ctx).Warn("missing author", "workID", w.ForeignID)
				continue
			}
			authorID := w.Authors[0].ForeignID

			if len(w.Books) == 0 {
				Log(ctx).Warn("missing books", "workID", w.ForeignID)
				continue
			}
			book := w.Books[0]

			if book.Asin != "" && _asin.Match([]byte(book.Asin)) {
				Log(ctx).Debug("found asin", "editionID", book.ForeignID, "asin", book.Asin)
				if err := c.setASIN(ctx, book.Asin, book.ForeignID); err != nil {
					Log(ctx).Warn("problem persisting asin", "editionID", book.ForeignID, "asin", book.Asin)
				}
			}
			if isbn, err := isbn.Parse(book.Isbn13); err == nil && isbn != nil {
				if err := c.setISBN(ctx, *isbn, book.ForeignID); err != nil {
					Log(ctx).Warn("problem persisting isbn", "editionID", book.ForeignID, "isbn", book.Isbn13)
				}
			}

			if len(book.Contributors) == 0 {
				Log(ctx).Warn("missing contributors", "workID", w.ForeignID, "editionID", book.ForeignID)
				continue
			}
			if book.Contributors[0].ForeignID != authorID {
				continue // Skip editions not attributed to this author.
			}

			out, err := json.Marshal(w)
			if err != nil {
				continue
			}
			c.cache.Set(ctx, BookKey(book.ForeignID), out, fuzz(_editionTTL, 2.0))
			grBookIDs = append(grBookIDs, book.ForeignID)
		}

		if grWorkID == 0 || len(grBookIDs) == 0 {
			return // Shouldn't happen.
		}

		_ = c.enqueueDenorm(ctx, edge{kind: workEdge, parentID: grWorkID, childIDs: newSet(grBookIDs...)})
	})
}

// getAuthor returns an AuthorResource with up to 20 works populated on first
// load. Additional works are populated asynchronously. The previous state is
// returned while a refresh is ongoing.
func (c *Controller) getAuthor(ctx context.Context, authorID int64) (ttlpair, error) {
	// We prefer a refresh key, if one exists, because it contains the author's
	// state prior to refreshing.
	preRefreshBytes, ok := c.cache.Get(ctx, refreshAuthorKey(authorID))
	if ok {
		if slices.Equal(preRefreshBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		if currentAuthorCachePayload(c.getter, preRefreshBytes) {
			return ttlpair{bytes: preRefreshBytes, ttl: time.Hour}, nil
		}
		Log(ctx).Info("discarding legacy pre-refresh author payload", "authorID", authorID)
		if err := c.cache.Delete(ctx, refreshAuthorKey(authorID)); err != nil {
			Log(ctx).Warn("unable to delete legacy pre-refresh author payload", "authorID", authorID, "err", err)
		}
	}

	// If we're not refreshing then return the cached value as long as it's
	// still valid.
	cachedBytes, ttl, ok := c.cache.GetWithTTL(ctx, AuthorKey(authorID))
	if ok && !slices.Equal(cachedBytes, _missing) && !currentAuthorCachePayload(c.getter, cachedBytes) {
		Log(ctx).Info("refreshing legacy cached author payload", "authorID", authorID)
		if err := c.cache.Expire(ctx, AuthorKey(authorID)); err != nil {
			Log(ctx).Warn("unable to expire legacy cached author payload", "authorID", authorID, "err", err)
		}
		cachedBytes, ttl, ok = nil, 0, false
	}
	if ok && ttl > 0 {
		if slices.Equal(cachedBytes, _missing) {
			return ttlpair{}, errNotFound
		}
		return ttlpair{bytes: cachedBytes, ttl: ttl}, nil
	}

	// Cache miss. Fetch new data.
	authorBytes, err := c.getter.GetAuthor(ctx, authorID)
	if errors.Is(err, errNotFound) {
		c.cache.Set(ctx, AuthorKey(authorID), _missing, _missingTTL)
		return ttlpair{}, err
	}
	if err != nil {
		Log(ctx).Warn("problem getting author", "err", err, "authorID", authorID)
		return ttlpair{}, err
	}
	if !currentAuthorCachePayload(c.getter, authorBytes) {
		return ttlpair{}, fmt.Errorf("provider returned author %d with stale cache schema", authorID)
	}

	ttl = fuzz(_authorTTL, 1.5)
	// Publish the durable refresh-needed marker before making a shallow author
	// record visible. An explicit request racing this load can therefore never
	// mistake the shallow record for a completed catalogue refresh.
	c.cache.Set(ctx, authorRefreshNeededKey(authorID), []byte{1}, 365*24*time.Hour)
	c.cache.Set(ctx, AuthorKey(authorID), authorBytes, ttl)

	// From here we'll prefer to use the last-known state. If this is the first
	// time we've loaded the author we won't have previous state, so use
	// whatever we just fetched.
	if len(cachedBytes) == 0 {
		cachedBytes = authorBytes
	}

	// Return the last cached value to give the refresh time to complete.
	return ttlpair{bytes: cachedBytes, ttl: ttl}, nil
}

type refreshAuthor struct {
	id      int64
	state   []byte
	isRetry bool
	attempt int
}

func authorRefreshNeededKey(authorID int64) string {
	return fmt.Sprintf("arn%d", authorID)
}

func (c *Controller) claimAuthorRefresh(authorID int64) bool {
	c.authorRefreshMu.Lock()
	defer c.authorRefreshMu.Unlock()
	if _, ok := c.authorRefreshes[authorID]; ok {
		return false
	}
	c.authorRefreshes[authorID] = struct{}{}
	return true
}

func (c *Controller) releaseAuthorRefresh(authorID int64) {
	c.authorRefreshMu.Lock()
	delete(c.authorRefreshes, authorID)
	c.authorRefreshMu.Unlock()
}

func (c *Controller) enqueueAuthorRefresh(ctx context.Context, refresh refreshAuthor) bool {
	select {
	case <-ctx.Done():
		return false
	case c.refreshC <- refresh:
		return true
	}
}

func (c *Controller) queueAuthorRefreshIfNeeded(ctx context.Context, authorID int64, state []byte) {
	if _, needed := c.cache.Get(ctx, authorRefreshNeededKey(authorID)); !needed {
		return
	}
	if !c.claimAuthorRefresh(authorID) {
		return
	}

	// Persist before enqueueing so a shutdown at any point after this can be
	// recovered safely. Leave the refresh-needed marker intact on failure.
	if err := c.persister.Persist(ctx, authorID, state); err != nil {
		c.releaseAuthorRefresh(authorID)
		Log(ctx).Warn("problem persisting refresh", "authorID", authorID, "err", err)
		return
	}
	// Hold the scheduler lock through admission and marker removal. This makes
	// acceptance atomic with respect to refresh completion releasing the claim:
	// the durable marker is cleared only after the job has been accepted, and a
	// second explicit request cannot slip through if the first job is very fast.
	c.authorRefreshMu.Lock()
	select {
	case <-ctx.Done():
		delete(c.authorRefreshes, authorID)
		c.authorRefreshMu.Unlock()
		return
	case c.refreshC <- refreshAuthor{id: authorID, state: state}:
	}
	if err := c.cache.Delete(ctx, authorRefreshNeededKey(authorID)); err != nil {
		Log(ctx).Warn("problem deleting author refresh-needed marker", "authorID", authorID, "err", err)
	}
	c.authorRefreshMu.Unlock()
}

func (c *Controller) authorRefreshBackoff(attempt int) time.Duration {
	delay := c.authorRefreshRetryDelay
	if delay <= 0 {
		return 0
	}
	if c.authorRefreshRetryMax <= 0 {
		return delay
	}
	for i := 0; i < attempt && delay < c.authorRefreshRetryMax; i++ {
		if delay > c.authorRefreshRetryMax/2 {
			return c.authorRefreshRetryMax
		}
		delay *= 2
	}
	if delay > c.authorRefreshRetryMax {
		return c.authorRefreshRetryMax
	}
	return delay
}

func (c *Controller) refreshAuthor(workerCtx, schedulerCtx context.Context, refresh refreshAuthor) {
	authorID := refresh.id
	workerCtx = context.WithValue(workerCtx, middleware.RequestIDKey, fmt.Sprintf("refresh-author-%d", authorID))
	schedulerCtx = context.WithValue(schedulerCtx, middleware.RequestIDKey, fmt.Sprintf("retry-author-%d", authorID))

	defer func() {
		if r := recover(); r != nil {
			panicErr := fmt.Errorf("panic refreshing author %d: %v", authorID, r)
			Log(workerCtx).Error("panic", "details", r)
			c.scheduleAuthorRefreshRetry(schedulerCtx, refresh, panicErr)
		}
	}()

	complete, cause := c.refreshAuthorAttempt(workerCtx, authorID)
	if !complete {
		c.scheduleAuthorRefreshRetry(schedulerCtx, refresh, cause)
		return
	}

	_ = c.enqueueDenorm(workerCtx, edge{kind: refreshDone, parentID: authorID})
}

func (c *Controller) authorRefreshRetryPlan(refresh refreshAuthor, cause error) (int, time.Duration) {
	nextAttempt := refresh.attempt + 1
	delay := c.authorRefreshBackoff(refresh.attempt)

	// A provider cooldown is admission control, not a failed catalogue attempt.
	// Preserve the attempt budget and honor the absolute Retry-After deadline.
	var rateLimit *RateLimitError
	if errors.As(cause, &rateLimit) {
		nextAttempt = refresh.attempt
		delay = max(delay, rateLimit.RetryAfter())
	} else if rateLimitedRefreshError(cause) {
		// A bare typed 429 has no deadline. Keep the attempt budget and use the
		// controller's conservative retry floor rather than retrying immediately.
		nextAttempt = refresh.attempt
	}

	return nextAttempt, delay
}

func (c *Controller) scheduleAuthorRefreshRetry(ctx context.Context, refresh refreshAuthor, cause error) {
	nextAttempt, delay := c.authorRefreshRetryPlan(refresh, cause)
	if c.authorRefreshMaxAttempts > 0 && nextAttempt >= c.authorRefreshMaxAttempts {
		Log(ctx).Error("author refresh paused after repeated incomplete attempts",
			"authorID", refresh.id,
			"attempts", nextAttempt,
			"err", cause)
		c.cache.Set(ctx, authorRefreshNeededKey(refresh.id), []byte{1}, 365*24*time.Hour)
		c.releaseAuthorRefresh(refresh.id)
		c.metrics.refreshWaitingAdd(-1)
		return
	}
	Log(ctx).Warn("author refresh incomplete; keeping it pending for retry",
		"authorID", refresh.id,
		"attempt", nextAttempt,
		"retryIn", delay.String(),
		"err", cause)
	if ctx.Err() != nil {
		return
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		select {
		case <-ctx.Done():
		case c.refreshC <- refreshAuthor{id: refresh.id, state: refresh.state, isRetry: true, attempt: nextAttempt}:
		}
	}()
}

// refreshAuthorOnce fetches one complete snapshot of an author's catalogue.
// It returns false when an upstream failure made the snapshot incomplete. A
// partial snapshot is still denormalized so successful work is not discarded,
// but the persisted refresh marker is retained until a later attempt succeeds.
func (c *Controller) refreshAuthorOnce(ctx context.Context, authorID int64) bool {
	complete, _ := c.refreshAuthorAttempt(ctx, authorID)
	return complete
}

// refreshAuthorAttempt returns the first error which made the snapshot
// incomplete. The scheduler uses typed RateLimitError deadlines to avoid both
// consuming retry budget and waking before Hardcover's cooldown expires.
func (c *Controller) refreshAuthorAttempt(ctx context.Context, authorID int64) (bool, error) {
	Log(ctx).Info("fetching all works for author", "authorID", authorID)

	n := 0
	start := time.Now()
	workIDSToDenormalize := []int64{}
	complete := true
	var incompleteCause error

	for bookID, listErr := range c.getter.GetAuthorBooks(ctx, authorID) {
		if listErr != nil {
			Log(ctx).Warn("problem enumerating books for author", "authorID", authorID, "err", listErr)
			complete = false
			incompleteCause = listErr
			break
		}
		if n > 1000 {
			Log(ctx).Warn("found too many editions", "authorID", authorID)
			break // Some authors (e.g. Wikipedia) have an obscene number of works. Give up.
		}

		bookBytes, _, err := c.GetBook(ctx, bookID)
		var w workResource
		if err == nil {
			if unmarshalErr := json.Unmarshal(bookBytes, &w); unmarshalErr != nil {
				_ = c.cache.Expire(ctx, BookKey(bookID))
				err = fmt.Errorf("unmarshaling book %d: %w", bookID, unmarshalErr)
			}
		}
		if err != nil {
			Log(ctx).Warn("problem getting book for author", "authorID", authorID, "bookID", bookID, "err", err)
			if refreshAttemptIncomplete(ctx, err) {
				complete = false
				if incompleteCause == nil || rateLimitedRefreshError(err) {
					incompleteCause = err
				}
			}
			if rateLimitedRefreshError(err) {
				break
			}
			continue
		}

		if len(w.Authors) > 0 && w.Authors[0].ForeignID != authorID {
			Log(ctx).Debug("skipping edition due to author mismatch", "authorID", authorID, "got", w.Authors[0].ForeignID)
			continue
		}

		workID := w.ForeignID
		_, _, err = c.GetWork(ctx, workID)
		if err == nil { // Ensure fetched before denormalizing.
			workIDSToDenormalize = append(workIDSToDenormalize, workID)
		} else {
			Log(ctx).Warn("problem getting work for author", "authorID", authorID, "bookID", bookID, "workID", workID, "err", err)
			if refreshAttemptIncomplete(ctx, err) {
				complete = false
				if incompleteCause == nil || rateLimitedRefreshError(err) {
					incompleteCause = err
				}
			}
			if rateLimitedRefreshError(err) {
				break
			}
		}
		n++
	}

	slices.Sort(workIDSToDenormalize)
	workIDSToDenormalize = slices.Compact(workIDSToDenormalize)

	if len(workIDSToDenormalize) > 0 {
		_ = c.enqueueDenorm(ctx, edge{kind: authorEdge, parentID: authorID, childIDs: newSet(workIDSToDenormalize...)})
	}
	Log(ctx).Info("finished author refresh attempt", "authorID", authorID, "count", len(workIDSToDenormalize), "complete", complete, "duration", time.Since(start).String())
	return complete, incompleteCause
}

func rateLimitedRefreshError(err error) bool {
	return errors.Is(err, statusErr(http.StatusTooManyRequests))
}

func refreshAttemptIncomplete(_ context.Context, err error) bool {
	if err == nil || errors.Is(err, errNotFound) {
		return false
	}
	// Non-transient failures are not retried immediately, but they still mean
	// this catalogue snapshot was incomplete. The outer bounded scheduler will
	// retain the durable marker and eventually pause instead of publishing a
	// partial author as complete.
	return true
}

func (c *Controller) enqueueDenorm(ctx context.Context, edge edge) bool {
	select {
	case <-ctx.Done():
		return false
	case c.denormC <- edge:
		return true
	}
}

func (c *Controller) submitDenormTask(task func()) {
	c.denormProducerMu.Lock()
	if !c.denormAccepting {
		c.denormProducerMu.Unlock()
		// Shutdown has stopped new detached tasks. Execute nested work inline so
		// the caller that Shutdown is already waiting on owns its completion.
		task()
		return
	}
	c.denormProducerG.Add(1)
	c.denormProducerMu.Unlock()

	go func() {
		defer c.denormProducerG.Done()
		task()
	}()
}

func (c *Controller) stopAndWaitDenormProducers(ctx context.Context) bool {
	c.denormProducerMu.Lock()
	c.denormAccepting = false
	c.denormProducerMu.Unlock()

	done := make(chan struct{})
	go func() {
		c.denormProducerG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// drainDenormalization uses two consecutive FIFO barriers. The second catches
// any synchronous child edge produced while Run was processing an edge ahead
// of the first barrier (workEdge can produce authorEdge).
func (c *Controller) drainDenormalization(ctx context.Context) bool {
	for range 2 {
		barrier := edge{kind: drainEdge, done: make(chan struct{})}
		select {
		case c.denormC <- barrier:
		case <-ctx.Done():
			return false
		}
		select {
		case <-barrier.done:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// Run is responsible for denormalizing data and handling our worker pools.
// Cancellation of the caller's context does not tear down consumers
// immediately. The owner must call Shutdown, which performs an ordered drain.
func (c *Controller) Run(parent context.Context) {
	c.lifecycleMu.Lock()
	if c.started {
		done := c.runDone
		c.lifecycleMu.Unlock()
		<-done
		return
	}
	c.started = true
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(parent))
	schedulerCtx, schedulerCancel := context.WithCancel(runCtx)
	c.runCancel = runCancel
	c.schedulerCancel = schedulerCancel
	close(c.runReady)
	c.lifecycleMu.Unlock()
	defer close(c.runDone)

	// Log controller stats every minute.
	go func() {
		ctx := context.WithValue(runCtx, middleware.RequestIDKey, "stats")
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				Log(ctx).Debug("controller stats",
					"refreshWaiting", c.metrics.refreshWaitingGet(),
					"denormWaiting", c.metrics.denormWaitingGet(),
					"etagMatches", c.metrics.etagMatchesGet(),
					"etagRatio", c.metrics.etagRatioGet(),
				)
			}
		}
	}()

	// Retry any author refreshes that were in-flight when we last shut down.
	go func() {
		ctx := context.WithValue(schedulerCtx, middleware.RequestIDKey, "recovery")
		authorIDs, err := c.persister.Persisted(ctx)
		if err != nil {
			Log(ctx).Error("problem retrying in-flight refreshes", "err", err)
		}
		for i, authorID := range authorIDs {
			if !c.claimAuthorRefresh(authorID) {
				continue
			}
			Log(ctx).Debug("resuming author refresh", "authorID", authorID)
			if !c.enqueueAuthorRefresh(ctx, refreshAuthor{id: authorID}) {
				c.releaseAuthorRefresh(authorID)
				return
			}

			if i == len(authorIDs)-1 {
				continue
			}
			timer := time.NewTimer(c.authorRecoveryInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()

	// Hand author refreshes to the bounded worker pool.
	refreshes := accumulateWithContext(schedulerCtx, c.refreshC, &slicebuffer[refreshAuthor]{})
	go func() {
		defer close(c.refreshDispatchDone)
		workerCtx := context.WithValue(runCtx, middleware.RequestIDKey, "refresh")
		retryCtx := context.WithValue(schedulerCtx, middleware.RequestIDKey, "refresh-retry")
		for r := range refreshes {
			if !r.isRetry {
				c.metrics.refreshWaitingAdd(1)
			}
			c.refreshG.Go(func() error {
				c.refreshAuthor(workerCtx, retryCtx, r)
				return nil
			})
		}
	}()

	denormBuf := &edgebuf{}
	denorms := accumulateWithContext(runCtx, c.denormC, denormBuf)
	for edge := range denorms {
		ctx, cancel := context.WithTimeout(runCtx, 1*time.Minute)
		ctx = context.WithValue(ctx, middleware.RequestIDKey, fmt.Sprintf("denorm-%d-%d", edge.kind, edge.parentID))

		switch edge.kind {
		case authorEdge:
			if unknownAuthor(edge.parentID) {
				break
			}
			if err := c.denormalizeWorks(ctx, edge.parentID, slices.Collect(maps.Keys(edge.childIDs))...); err != nil {
				Log(ctx).Warn("problem ensuring work", "err", err, "authorID", edge.parentID, "workIDs", edge.childIDs)
			}
		case workEdge:
			if err := c.denormalizeEditions(ctx, edge.parentID, slices.Collect(maps.Keys(edge.childIDs))...); err != nil {
				Log(ctx).Warn("problem ensuring edition", "err", err, "workID", edge.parentID, "bookIDs", edge.childIDs)
			}
		case refreshDone:
			c.metrics.refreshWaitingAdd(-1)
			if err := c.persister.Delete(ctx, edge.parentID); err != nil {
				Log(ctx).Warn("problem un-persisting refresh", "err", err)
			} else {
				if err := c.cache.Delete(ctx, authorRefreshNeededKey(edge.parentID)); err != nil {
					Log(ctx).Warn("problem deleting stale author refresh-needed marker", "authorID", edge.parentID, "err", err)
				}
				c.releaseAuthorRefresh(edge.parentID)
			}
		case drainEdge:
			close(edge.done)
		}
		cancel()
		c.metrics.denormWaitingSet(denormBuf.len())
	}
}

// Shutdown stops refresh admission, waits for active refresh and work pools,
// drains all submitted denormalization edges through a FIFO barrier, and then
// stops Run. A canceled shutdown context forces Run to stop while leaving any
// incomplete refresh persisted for recovery on the next start.
func (c *Controller) Shutdown(ctx context.Context) {
	select {
	case <-c.runReady:
	case <-ctx.Done():
		return
	}

	c.shutdownOnce.Do(func() {
		c.lifecycleMu.Lock()
		schedulerCancel := c.schedulerCancel
		runCancel := c.runCancel
		c.lifecycleMu.Unlock()

		// Phase one: stop recovery and delayed retries, then wait until the
		// dispatcher cannot add more jobs to refreshG.
		schedulerCancel()
		select {
		case <-c.refreshDispatchDone:
		case <-ctx.Done():
			runCancel()
			return
		}

		refreshDone := make(chan struct{})
		go func() {
			_ = c.refreshG.Wait()
			close(refreshDone)
		}()
		select {
		case <-refreshDone:
		case <-ctx.Done():
			runCancel()
			return
		}

		// No HTTP handlers remain when the production owner calls Shutdown and
		// refreshG is now idle. Close detached denormalization admission and wait
		// for every already-admitted producer (getBook/saveEditions) to finish.
		if !c.stopAndWaitDenormProducers(ctx) {
			runCancel()
			return
		}

		// The first barrier processes refresh/producer edges. Processing an
		// author edge may itself admit bounded workG maintenance, so this barrier
		// must precede workG.Wait.
		if !c.drainDenormalization(ctx) {
			runCancel()
			return
		}

		// Wait for work spawned by those drained edges.
		workDone := make(chan struct{})
		go func() {
			_ = c.workG.Wait()
			close(workDone)
		}()
		select {
		case <-workDone:
		case <-ctx.Done():
			runCancel()
			return
		}

		// The second barrier drains workG-produced edges and their synchronous
		// child edges, including refreshDone persistence cleanup, before Run is
		// canceled.
		if !c.drainDenormalization(ctx) {
			runCancel()
			return
		}

		runCancel()
		select {
		case <-c.runDone:
		case <-ctx.Done():
		}
	})
}

// denormalizeEditions ensures that the given editions exists on the work. It
// deserializes the target work once. (TODO: No-op if it includes an edition
// with the same language and title).
//
// This is what allows us to support translated editions. We intentionally
// don't add every edition available, because then the user has potentially
// hundreds (or thousands!) of editions to crawl through in order to find one
// in the language they need.
//
// Instead, we (a) only add editions that users actually search for and use,
// (b) only add editions that are meaningful enough to appear in auto_complete,
// and (c) keep the total number of editions small enough for users to more
// easily select from.
func (c *Controller) denormalizeEditions(ctx context.Context, workID int64, bookIDs ...int64) error {
	if len(bookIDs) == 0 {
		return nil
	}
	if merge, merged := c.loadFormatSplitMerge(ctx, workID); merged {
		return c.denormalizeFormatSplitEditions(ctx, merge, bookIDs...)
	}

	workBytes, _, err := c.getter.GetWork(ctx, workID, nil)
	if err != nil {
		Log(ctx).Debug("problem getting work", "err", err)
		return err
	}

	var work workResource
	err = sonic.ConfigStd.Unmarshal(workBytes, &work)
	if err != nil {
		Log(ctx).Debug("problem unmarshaling work", "err", err, "workID", workID)
		_ = c.cache.Expire(ctx, WorkKey(workID))
		return err
	}
	baseline := work
	if cachedBytes, _, ok := c.cache.GetWithTTL(ctx, WorkKey(work.ForeignID)); ok && !slices.Equal(cachedBytes, _missing) {
		var cached workResource
		if err := sonic.ConfigStd.Unmarshal(cachedBytes, &cached); err == nil {
			baseline = cached
			work = reconcileRefreshedWork(work, cached, editionIDSet(work.ProviderEditionIDs))
		}
	}

	old := newETagWriter()
	if err := sonic.ConfigStd.NewEncoder(old).Encode(baseline); err != nil {
		return fmt.Errorf("hashing cached work %d: %w", workID, err)
	}

	Log(ctx).Debug("ensuring work-edition edges", "workID", workID, "bookIDs", bookIDs)
	providerMembership := editionIDSet(work.ProviderEditionIDs)

	for _, bookID := range bookIDs {
		if len(providerMembership) != 0 {
			if _, current := providerMembership[bookID]; !current {
				Log(ctx).Warn("refusing edition omitted from authoritative provider membership",
					"workID", workID,
					"bookID", bookID)
				continue
			}
		}
		workBytes, _, _, err = c.getter.GetBook(ctx, bookID, nil)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			Log(ctx).Warn("unable to denormalize edition", "err", err, "workID", workID, "bookID", bookID)
			continue
		}
		var w workResource
		err = sonic.ConfigStd.Unmarshal(workBytes, &w)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling book", "err", err, "bookID", bookID)
			_ = c.cache.Expire(ctx, BookKey(bookID))
			continue
		}
		if len(w.Books) != 1 {
			Log(ctx).Warn("unexpected number of books", "bookID", bookID, "count", len(w.Books))
			continue
		}
		if w.ForeignID != work.ForeignID {
			Log(ctx).Warn("refusing cross-work edition denormalization",
				"requestedWorkID", workID,
				"targetWorkID", work.ForeignID,
				"editionWorkID", w.ForeignID,
				"bookID", bookID)
			continue
		}

		// GetBook can return a merged book/edition with an ID not matching
		// bookID, and that's the ID we need to probe for.
		bookID = w.Books[0].ForeignID

		idx, found := slices.BinarySearchFunc(work.Books, bookID, func(b bookResource, id int64) int {
			return cmp.Compare(b.ForeignID, id)
		})

		if found {
			work.Books[idx] = w.Books[0] // Replace.
		} else {
			work.Books = slices.Insert(work.Books, idx, w.Books[0]) // Insert.
		}
	}

	stabilizeLegacyBestBookID(&work)

	buf := _buffers.Get()
	defer buf.Free()
	neww := newETagWriter()
	w := io.MultiWriter(buf, neww)
	err = sonic.ConfigStd.NewEncoder(w).Encode(work)
	if err != nil {
		return err
	}

	if neww.ETag() == old.ETag() {
		// The work didn't change, so we're done.
		c.metrics.etagMatchesInc()
		return nil
	}
	c.metrics.etagMismatchesInc()

	// We can't persist the shared buffer in the cache so clone it.
	out := bytes.Clone(buf.Bytes())

	c.cache.Set(ctx, WorkKey(workID), out, fuzz(_workTTL, 1.5))

	// We modified the work, so the author also needs to be updated. Remove the
	// relationship so it doesn't no-op during the denormalization.
	for _, author := range work.Authors {
		if !c.enqueueDenorm(ctx, edge{kind: authorEdge, parentID: author.ForeignID, childIDs: newSet(workID)}) {
			return ctx.Err()
		}
	}

	return nil
}

// denormalizeFormatSplitEditions updates the raw provider source identified by
// each edition before rebuilding the canonical format-split work. WorkKey for
// an alias deliberately contains the canonical payload, so the ordinary
// denormalization identity check cannot distinguish a legitimate alias
// edition from an unrelated cross-work edition.
func (c *Controller) denormalizeFormatSplitEditions(ctx context.Context, merge formatSplitMerge, bookIDs ...int64) error {
	merge, ok := normalizeFormatSplitMerge(merge)
	if !ok {
		return errors.Join(errNotFound, errors.New("invalid format-split merge"))
	}

	sources := make(map[int64]workResource, len(merge.SourceWorkIDs))
	for _, sourceID := range merge.SourceWorkIDs {
		sourceBytes, _, found := c.cache.GetWithTTL(ctx, formatSplitSourceKey(sourceID))
		if !found || len(sourceBytes) == 0 {
			return fmt.Errorf("format-split source snapshot %d is missing", sourceID)
		}
		var source workResource
		if err := sonic.ConfigStd.Unmarshal(sourceBytes, &source); err != nil {
			return fmt.Errorf("unmarshaling format-split source %d: %w", sourceID, err)
		}
		if source.ForeignID != sourceID {
			return fmt.Errorf("format-split source identity mismatch: expected=%d got=%d", sourceID, source.ForeignID)
		}
		sources[sourceID] = source
	}

	changed := false
	for _, requestedBookID := range bookIDs {
		bookBytes, relatedWorkID, _, err := c.getter.GetBook(ctx, requestedBookID, nil)
		if err != nil {
			Log(ctx).Warn("unable to denormalize format-split edition",
				"err", err,
				"canonicalWorkID", merge.CanonicalWorkID,
				"bookID", requestedBookID)
			continue
		}
		var editionWork workResource
		if err := sonic.ConfigStd.Unmarshal(bookBytes, &editionWork); err != nil {
			Log(ctx).Warn("problem unmarshaling format-split edition", "err", err, "bookID", requestedBookID)
			_ = c.cache.Expire(ctx, BookKey(requestedBookID))
			continue
		}
		if len(editionWork.Books) != 1 {
			Log(ctx).Warn("unexpected number of books in format-split edition",
				"bookID", requestedBookID,
				"count", len(editionWork.Books))
			continue
		}
		if relatedWorkID != 0 && relatedWorkID != editionWork.ForeignID {
			Log(ctx).Warn("refusing format-split edition with inconsistent provider relationship",
				"bookID", requestedBookID,
				"relationshipWorkID", relatedWorkID,
				"payloadWorkID", editionWork.ForeignID)
			continue
		}

		source, belongsToSource := sources[editionWork.ForeignID]
		if !belongsToSource {
			Log(ctx).Warn("refusing edition outside format-split source relationship",
				"canonicalWorkID", merge.CanonicalWorkID,
				"editionWorkID", editionWork.ForeignID,
				"bookID", requestedBookID)
			continue
		}

		book := editionWork.Books[0]
		if book.ForeignID == 0 {
			Log(ctx).Warn("refusing format-split edition without provider identity", "bookID", requestedBookID)
			continue
		}
		providerMembership := editionIDSet(source.ProviderEditionIDs)
		if len(providerMembership) != 0 {
			if _, current := providerMembership[requestedBookID]; !current {
				Log(ctx).Warn("refusing format-split edition omitted from authoritative source membership",
					"sourceWorkID", source.ForeignID,
					"bookID", requestedBookID)
				continue
			}
			if _, current := providerMembership[book.ForeignID]; !current {
				Log(ctx).Warn("refusing resolved format-split edition omitted from authoritative source membership",
					"sourceWorkID", source.ForeignID,
					"requestedBookID", requestedBookID,
					"resolvedBookID", book.ForeignID)
				continue
			}
		}
		idx, found := slices.BinarySearchFunc(source.Books, book.ForeignID, func(candidate bookResource, id int64) int {
			return cmp.Compare(candidate.ForeignID, id)
		})
		if found {
			source.Books[idx] = book
		} else {
			source.Books = slices.Insert(source.Books, idx, book)
		}

		stabilizeLegacyBestBookID(&source)
		sources[source.ForeignID] = source
		changed = true
	}
	if !changed {
		return nil
	}
	if !formatSplitSourcesStillMerge(merge, sources) {
		return errors.New("format-split source topology changed while denormalizing edition")
	}

	merged := sources[merge.CanonicalWorkID]
	for _, sourceID := range merge.SourceWorkIDs[1:] {
		merged = combineWorks(merged, sources[sourceID])
	}
	if merged.ForeignID != merge.CanonicalWorkID {
		return fmt.Errorf("format-split canonical identity mismatch: expected=%d got=%d", merge.CanonicalWorkID, merged.ForeignID)
	}
	mergedBytes, err := sonic.ConfigStd.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshaling denormalized format-split work %d: %w", merge.CanonicalWorkID, err)
	}

	// Persist source provenance first, then publish the same complete canonical
	// value under every public source key. A later expiry can therefore rebuild
	// the merge without losing an edition discovered through an alias.
	c.persistFormatSplitMerge(ctx, merge, sources)
	ttl := fuzz(_workTTL, 1.5)
	for _, sourceID := range merge.SourceWorkIDs {
		c.cache.Set(ctx, WorkKey(sourceID), mergedBytes, ttl)
	}

	for _, author := range merged.Authors {
		if !c.enqueueDenorm(ctx, edge{kind: authorEdge, parentID: author.ForeignID, childIDs: newSet(merge.CanonicalWorkID)}) {
			return ctx.Err()
		}
	}
	return nil
}

// denormalizeWorks ensures that the given works exist on the author. This is a
// no-op if our cached work already includes the work's ID. This is meant to be
// invoked in the background, and it's what allows us to support large authors.
func (c *Controller) denormalizeWorks(ctx context.Context, authorID int64, workIDs ...int64) error {
	if len(workIDs) == 0 {
		return nil
	}

	authorBytes, _, err := c.getAuthorForDenormalization(ctx, authorID)
	if err != nil {
		Log(ctx).Debug("problem loading author for denormalizeWorks", "err", err)
		return err
	}

	old := newETagWriter()
	r := io.TeeReader(bytes.NewReader(authorBytes), old)

	var author AuthorResource
	err = sonic.ConfigStd.NewDecoder(r).Decode(&author)
	if err != nil {
		Log(ctx).Debug("problem unmarshaling author", "err", err, "authorID", authorID)
		_ = c.cache.Expire(ctx, AuthorKey(authorID))
		return err
	}

	Log(ctx).Debug("ensuring author-work edges", "authorID", authorID, "workIDs", workIDs)

	for _, workID := range workIDs {
		workBytes, _, err := c.getter.GetWork(ctx, workID, nil)
		if err != nil {
			// Maybe the cache wasn't able to refresh because it was deleted? Move on.
			Log(ctx).Warn("unable to denormalize work", "err", err, "authorID", authorID, "workID", workID)
			continue
		}
		var work workResource
		err = sonic.ConfigStd.Unmarshal(workBytes, &work)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling work", "err", err, "workID", workID)
			_ = c.cache.Expire(ctx, WorkKey(workID))
			continue
		}
		workID = work.ForeignID // GetWork can return a merged work with a different ID.

		idx, found := slices.BinarySearchFunc(author.Works, workID, func(w workResource, id int64) int {
			return cmp.Compare(w.ForeignID, id)
		})

		if len(work.Books) == 0 {
			Log(ctx).Warn("work had no editions", "workID", workID)
			continue
		}

		if found {
			author.Works[idx] = work // Replace.
		} else {
			author.Works = slices.Insert(author.Works, idx, work) // Insert.
		}
	}

	sourceWorks := make(map[int64]workResource, len(author.Works))
	for _, source := range author.Works {
		sourceWorks[source.ForeignID] = source
	}

	var aliases map[int64]int64
	author.Works, aliases = mergeDuplicateFormatWorks(author.Works)
	aliasesByCanonical := make(map[int64][]int64)
	for aliasID, canonicalID := range aliases {
		aliasesByCanonical[canonicalID] = append(aliasesByCanonical[canonicalID], aliasID)
	}
	for canonicalID, aliasIDs := range aliasesByCanonical {
		idx, found := slices.BinarySearchFunc(author.Works, canonicalID, func(w workResource, id int64) int {
			return cmp.Compare(w.ForeignID, id)
		})
		if !found {
			continue
		}

		mergedBytes, marshalErr := sonic.ConfigStd.Marshal(author.Works[idx])
		if marshalErr != nil {
			continue
		}
		merge := formatSplitMerge{
			CanonicalWorkID: canonicalID,
			SourceWorkIDs:   append([]int64{canonicalID}, aliasIDs...),
		}
		c.persistFormatSplitMerge(ctx, merge, sourceWorks)
		c.cache.Set(ctx, WorkKey(canonicalID), mergedBytes, fuzz(_workTTL, 1.5))
		for _, aliasID := range aliasIDs {
			c.cache.Set(ctx, WorkKey(aliasID), mergedBytes, fuzz(_workTTL, 1.5))
		}
		Log(ctx).Info("merged high-confidence format-split works", "canonicalWorkID", canonicalID, "aliasWorkIDs", aliasIDs)
	}

	author.Series = []SeriesResource{}

	wg := sync.WaitGroup{}
	mu := sync.Mutex{}

	// Keep track of any duplicated titles so we can disambiguate them with subtitles.
	titles := map[string]int{}

	ratingSum := int64(0)
	ratingCount := int64(0)
	for _, w := range author.Works {
		if w.ShortTitle != "" {
			titles[strings.ToUpper(w.ShortTitle)]++
		} else {
			titles[strings.ToUpper(w.Title)]++
		}
		// HC stores rating on the work while GR is on the edition.
		ratingCount += w.RatingCount
		ratingSum += w.RatingSum
		if w.RatingCount == 0 {
			for _, b := range w.Books {
				ratingCount += b.RatingCount
				ratingSum += b.RatingSum
			}
		}
		for _, s := range w.Series {
			// Fetch the complete series since we might not derive it correctly from works alone.
			wg.Go(func() {
				s, err := c.GetSeries(ctx, s.ForeignID)
				if err != nil {
					return
				}

				var ss SeriesResource
				err = json.Unmarshal(s, &ss)
				if err != nil {
					return
				}

				mu.Lock()
				defer mu.Unlock()

				idx, found := slices.BinarySearchFunc(author.Series, ss.ForeignID, func(s SeriesResource, id int64) int {
					return cmp.Compare(s.ForeignID, id)
				})

				if !found {
					author.Series = slices.Insert(author.Series, idx, ss)
				}
			})
		}
	}

	// Disambiguate works which share the same title by including subtitles.
	for idx := range author.Works {
		shortTitle := author.Works[idx].Title
		if author.Works[idx].ShortTitle != "" {
			shortTitle = author.Works[idx].ShortTitle
		}
		// If this is part of a series, always include the subtitle.
		inSeries := len(author.Works[idx].Series) > 0
		if !inSeries && titles[strings.ToUpper(shortTitle)] <= 1 {
			// If the short title is already unique there's nothing to do.
			continue
		}
		if author.Works[idx].FullTitle == "" {
			continue
		}
		author.Works[idx].Title = author.Works[idx].FullTitle
		for bidx := range author.Works[idx].Books {
			if author.Works[idx].Books[bidx].FullTitle == "" {
				continue
			}
			author.Works[idx].Books[bidx].Title = author.Works[idx].Books[bidx].FullTitle
		}
	}
	if ratingCount != 0 {
		author.RatingCount = ratingCount
		author.AverageRating = float32(ratingSum) / float32(ratingCount)
	}

	wg.Wait()

	buf := _buffers.Get()
	defer buf.Free()
	neww := newETagWriter()
	w := io.MultiWriter(buf, neww)
	err = sonic.ConfigStd.NewEncoder(w).Encode(author)
	if err != nil {
		return err
	}

	if neww.ETag() == old.ETag() {
		// The author didn't change, so we're done.
		c.metrics.etagMatchesInc()
		return nil
	}
	c.metrics.etagMismatchesInc()

	// We can't persist the shared buffer in the cache so clone it.
	out := bytes.Clone(buf.Bytes())

	c.cache.Set(ctx, AuthorKey(authorID), out, fuzz(_authorTTL, 1.5))

	return nil
}

// editionsCallback can be used by a Getter to trigger async loading of
// additional editions.
type editionsCallback func(...workResource)

// fuzz scales the given duration into the range (d, d * f).
func fuzz(d time.Duration, f float64) time.Duration {
	if f < 1.0 {
		f += 1.0
	}
	factor := 1.0 + rand.Float64()*(f-1.0)
	return time.Duration(float64(d) * factor)
}

type ttlpair struct {
	bytes []byte
	ttl   time.Duration
}

// Configure sonic's memory pooling.
func init() {
	option.LimitBufferSize = 100 * 1024 * 1024    // 100MB max buffer.
	option.DefaultDecoderBufferSize = 1024 * 1024 // 1MB
	option.DefaultEncoderBufferSize = 1024 * 1024 // 1MB
}
