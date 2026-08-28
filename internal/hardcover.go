package internal

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/isbn"
	"github.com/blampe/rreading-glasses/hardcover"
)

// HCGetter implements a Getter using the Hardcover API as its source. It
// attempts to minimize upstream HEAD requests (to resolve book/work IDs) by
// relying on HC's raw external data.
type HCGetter struct {
	cache             cache[[]byte]
	gql               graphql.Client
	searchResultLimit int
}

var _ getter = (*HCGetter)(nil)

func (*HCGetter) workCacheSchemaVersion() int { return workCacheSchemaVersion }

// NewHardcoverGetter returns a new Getter backed by Hardcover.
func NewHardcoverGetter(cache cache[[]byte], gql graphql.Client) (*HCGetter, error) {
	return NewConfiguredHardcoverGetter(cache, gql, 5)
}

// NewConfiguredHardcoverGetter creates a Hardcover getter with a bounded
// search hydration fan-out. A normal search result can contain 15 works and
// hydrating every one is disproportionately expensive under provider quotas.
func NewConfiguredHardcoverGetter(cache cache[[]byte], gql graphql.Client, searchResultLimit int) (*HCGetter, error) {
	if searchResultLimit <= 0 || searchResultLimit > 15 {
		return nil, fmt.Errorf("search result limit must be between 1 and 15")
	}
	return &HCGetter{cache: cache, gql: gql, searchResultLimit: searchResultLimit}, nil
}

// Search hits the GraphQL endpoint to fetch relevant work IDs and then fetches
// those in order to return the necessary edition and author IDs to the client.
func (g *HCGetter) Search(ctx context.Context, query string) ([]SearchResource, error) {
	ctx = WithInteractivePriority(ctx)
	workIDs := []int64{}

	// Try a lookup by ASIN/ISBN if the query looks like one
	if _asin.Match([]byte(query)) || isbn.Validate(query) {
		resp, err := hardcover.GetWorkByASINISBN(ctx, g.gql, query)
		if err != nil {
			return nil, fmt.Errorf("looking up: %w", err)
		}
		for _, e := range resp.Editions {
			workIDs = append(workIDs, e.Book_id)
		}
	} else {
		// Otherwise do a normal search.
		resp, err := hardcover.Search(ctx, g.gql, query)
		if err != nil {
			return nil, fmt.Errorf("searching: %w", err)
		}
		workIDs = resp.Search.Ids
	}

	workIDs = slices.Compact(workIDs)
	if len(workIDs) > g.searchResultLimit {
		workIDs = workIDs[:g.searchResultLimit]
	}

	// Hydrate in provider relevance order and stop immediately on a quota
	// signal. Returning a partial HTTP 200 after a 429 would cause Bookshelf to
	// cache incomplete search results for a day.
	results := make([]SearchResource, 0, len(workIDs))
	var firstErr error
	for _, workID := range workIDs {
		bytes, _, err := g.GetWork(ctx, workID, nil)
		if err != nil {
			if errors.Is(err, statusErr(http.StatusTooManyRequests)) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if firstErr == nil {
				firstErr = err
			}
			Log(ctx).Warn("unable to hydrate search result", "workID", workID, "err", err)
			continue
		}

		var workRsc workResource
		if err := json.Unmarshal(bytes, &workRsc); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(workRsc.Authors) == 0 {
			Log(ctx).Warn("work is missing an author", "workID", workID)
			continue
		}
		results = append(results, SearchResource{
			BookID: workRsc.BestBookID,
			WorkID: workRsc.ForeignID,
			Author: SearchResourceAuthor{ID: workRsc.Authors[0].ForeignID},
		})
	}
	if len(results) == 0 && firstErr != nil {
		return nil, fmt.Errorf("hydrating search results: %w", firstErr)
	}
	return results, nil
}

// GetWork returns the canonical edition for a work.
func (g *HCGetter) GetWork(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
	if workID == 0 {
		return nil, 0, errors.Join(errBadRequest, errors.New("work ID missing"))
	}

	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(workID))
	if ok && !hasCurrentWorkCacheSchema(workBytes, g.workCacheSchemaVersion()) {
		Log(ctx).Info("refreshing legacy Hardcover work payload", "workID", workID)
		if err := g.cache.Expire(ctx, WorkKey(workID)); err != nil {
			Log(ctx).Warn("unable to expire legacy Hardcover work payload", "workID", workID, "err", err)
		}
		ok = false
	}
	if ok && ttl > 0 {
		return workBytes, 0, nil
	}

	Log(ctx).Debug("getting work", "workID", workID)

	resp, err := hardcover.GetWork(ctx, g.gql, workID)
	if err != nil {
		return nil, 0, fmt.Errorf("getting work: %w", err)
	}

	if resp.Books_by_pk.Id == 0 {
		return nil, 0, errors.Join(errNotFound, fmt.Errorf("invalid work info"))
	}

	if resp.Books_by_pk.Canonical_id != 0 {
		return g.GetWork(ctx, resp.Books_by_pk.Canonical_id, saveEditions)
	}

	author, err := bestAuthor(hardcover.AsContributions(resp.Books_by_pk.Contributions))
	if err != nil {
		return nil, 0, err
	}
	authorID := author.Id
	providerDefaults := validatedHardcoverDefaults(resp.Books_by_pk.DefaultEditions, authorID)
	requiredEditionIDs := editionIDSet(providerDefaults.allIDs())
	hydrated := selectHardcoverEditions(ctx, resp.Books_by_pk.Editions, resp.Books_by_pk.WorkInfo, requiredEditionIDs)
	hydratedEditionIDs := hardcoverWorkEditionIDs(hydrated)

	rejectedEditionIDs := make(map[int64]struct{})
	queriedEditionIDs := make(map[int64]struct{})
	var base workResource
	baseFound := false
	for _, editionID := range hardcoverEditionCandidates(resp.Books_by_pk.DefaultEditions, authorID) {
		queriedEditionIDs[editionID] = struct{}{}
		candidate, valid, candidateErr := g.validatedHydratedEdition(ctx, editionID, resp.Books_by_pk.Id, authorID)
		if candidateErr != nil {
			return nil, 0, fmt.Errorf("getting default edition %d: %w", editionID, candidateErr)
		}
		if !valid {
			rejectedEditionIDs[editionID] = struct{}{}
			continue
		}
		base = candidate
		baseFound = true
		if _, retained := hydratedEditionIDs[editionID]; !retained {
			hydrated = append(hydrated, candidate)
			hydratedEditionIDs[editionID] = struct{}{}
		}
		break
	}
	if !baseFound {
		return nil, 0, errors.Join(errNotFound, fmt.Errorf("work has no valid default edition"))
	}

	// The full Work query normally contains every provider default. If an
	// otherwise validated default is absent from that edition list, hydrate it
	// explicitly once. Provider quota/cancellation errors abort the whole
	// snapshot so callers never cache a partial set of defaults.
	for _, editionID := range providerDefaults.allIDs() {
		if editionID == 0 {
			continue
		}
		if _, rejected := rejectedEditionIDs[editionID]; rejected {
			continue
		}
		if _, retained := hydratedEditionIDs[editionID]; retained {
			continue
		}
		if _, queried := queriedEditionIDs[editionID]; queried {
			continue
		}

		queriedEditionIDs[editionID] = struct{}{}
		candidate, valid, candidateErr := g.validatedHydratedEdition(ctx, editionID, resp.Books_by_pk.Id, authorID)
		if candidateErr != nil {
			return nil, 0, fmt.Errorf("hydrating provider default edition %d: %w", editionID, candidateErr)
		}
		if !valid {
			rejectedEditionIDs[editionID] = struct{}{}
			continue
		}
		hydrated = append(hydrated, candidate)
		hydratedEditionIDs[editionID] = struct{}{}
	}

	sanitizedDefaults := providerDefaults.without(rejectedEditionIDs)
	hydrated = filterAndOverlayHardcoverEditions(hydrated, resp.Books_by_pk.Id, sanitizedDefaults, rejectedEditionIDs)
	final := mergeHydratedHardcoverWork(base, hydrated, resp.Books_by_pk.Id, rejectedEditionIDs)
	overlayHardcoverProviderDefaults(&final, sanitizedDefaults)
	providerEditionIDs := hardcoverProviderEditionMembership(resp.Books_by_pk.Editions, hydrated, resp.Books_by_pk.Id)
	overlayHardcoverProviderEditionMembership(&final, providerEditionIDs)
	for index := range hydrated {
		overlayHardcoverProviderEditionMembership(&hydrated[index], providerEditionIDs)
	}

	if saveEditions != nil && len(hydrated) != 0 {
		saveEditions(hydrated...)
	}

	workBytes, err = json.Marshal(final)
	if err != nil {
		return nil, 0, fmt.Errorf("marshaling hydrated work %d: %w", resp.Books_by_pk.Id, err)
	}
	return workBytes, authorID, nil
}

func (g *HCGetter) validatedHydratedEdition(ctx context.Context, editionID, expectedWorkID, expectedAuthorID int64) (workResource, bool, error) {
	workBytes, actualWorkID, actualAuthorID, err := g.GetBook(ctx, editionID, nil)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return workResource{}, false, nil
		}
		return workResource{}, false, err
	}
	if actualWorkID != expectedWorkID {
		Log(ctx).Warn("default edition belongs to another work",
			"workID", expectedWorkID,
			"editionID", editionID,
			"actualWorkID", actualWorkID)
		return workResource{}, false, nil
	}
	if actualAuthorID != expectedAuthorID {
		Log(ctx).Warn("default edition belongs to another author",
			"workID", expectedWorkID,
			"editionID", editionID,
			"expectedAuthorID", expectedAuthorID,
			"actualAuthorID", actualAuthorID)
		return workResource{}, false, nil
	}

	var work workResource
	if err := json.Unmarshal(workBytes, &work); err != nil {
		return workResource{}, false, fmt.Errorf("unmarshaling edition %d: %w", editionID, err)
	}
	book, ok := hardcoverWorkEdition(work, editionID)
	if !ok {
		Log(ctx).Warn("default edition cache payload does not contain its edition",
			"workID", expectedWorkID,
			"editionID", editionID)
		return workResource{}, false, nil
	}
	work.Books = []bookResource{book}
	return work, true, nil
}

type hardcoverEditionRetentionKey struct {
	title      string
	language   string
	mediaClass string
	special    bool
}

// selectHardcoverEditions keeps a bounded, deterministic set of useful
// editions. Hardcover orders the input by provider score, so the first entry
// for a retention class wins. Ordinary and special editions are deliberately
// separate classes; otherwise a high-scoring illustrated/revised edition can
// hide the ordinary edition with the same title and language.
func selectHardcoverEditions(ctx context.Context, editions []hardcover.GetWorkBooks_by_pkBooksEditions, work hardcover.WorkInfo, requiredEditionIDs map[int64]struct{}) []workResource {
	selected := make([]workResource, 0, len(editions))
	seen := make(map[hardcoverEditionRetentionKey]struct{})
	seenEditionIDs := make(map[int64]struct{})

	for _, edition := range editions {
		mapped, err := mapHardcoverToWorkResource(ctx, edition.EditionInfo, work)
		if err != nil || len(mapped.Books) != 1 {
			continue
		}
		book := mapped.Books[0]
		key := hardcoverEditionRetentionKey{
			title:      strings.ToUpper(book.FullTitle),
			language:   strings.ToLower(book.Language),
			mediaClass: hardcoverMediaClass(book),
			special:    isSpecialEdition(book),
		}
		_, required := requiredEditionIDs[book.ForeignID]
		if _, exists := seen[key]; exists && !required {
			continue
		}
		if _, exists := seenEditionIDs[book.ForeignID]; exists {
			continue
		}
		seen[key] = struct{}{}
		seenEditionIDs[book.ForeignID] = struct{}{}
		selected = append(selected, mapped)
	}

	return selected
}

func hardcoverMediaClass(book bookResource) string {
	mediaType := book.MediaType
	if mediaType == mediaTypeUnknown {
		mediaType = classifyMediaType(book.Format)
	}
	if mediaType == mediaTypeAudiobook {
		return "audio"
	}
	if mediaType == mediaTypeEbook || book.IsEbook {
		return "ebook"
	}
	return "physical"
}

func editionIDSet(ids []int64) map[int64]struct{} {
	set := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id != 0 {
			set[id] = struct{}{}
		}
	}
	return set
}

func hardcoverWorkEditionIDs(works []workResource) map[int64]struct{} {
	ids := make(map[int64]struct{}, len(works))
	for _, work := range works {
		for _, book := range work.Books {
			if book.ForeignID != 0 {
				ids[book.ForeignID] = struct{}{}
			}
		}
	}
	return ids
}

func hardcoverProviderEditionMembership(editions []hardcover.GetWorkBooks_by_pkBooksEditions, hydrated []workResource, workID int64) []int64 {
	ids := make(map[int64]struct{}, len(editions)+len(hydrated))
	for _, edition := range editions {
		if edition.Id != 0 && edition.Book_id == workID {
			ids[edition.Id] = struct{}{}
		}
	}
	for _, work := range hydrated {
		if work.ForeignID != workID {
			continue
		}
		for _, book := range work.Books {
			if book.ForeignID != 0 {
				ids[book.ForeignID] = struct{}{}
			}
		}
	}
	membership := slices.Collect(maps.Keys(ids))
	slices.Sort(membership)
	return membership
}

func hardcoverWorkEdition(work workResource, editionID int64) (bookResource, bool) {
	for _, book := range work.Books {
		if book.ForeignID == editionID {
			return book, true
		}
	}
	return bookResource{}, false
}

func filterAndOverlayHardcoverEditions(works []workResource, expectedWorkID int64, defaults hardcoverDefaultEditionIDs, rejected map[int64]struct{}) []workResource {
	filtered := make([]workResource, 0, len(works))
	seen := make(map[int64]struct{}, len(works))
	for _, work := range works {
		if work.ForeignID != expectedWorkID || len(work.Books) != 1 {
			continue
		}
		bookID := work.Books[0].ForeignID
		if bookID == 0 {
			continue
		}
		if _, invalid := rejected[bookID]; invalid {
			continue
		}
		if _, duplicate := seen[bookID]; duplicate {
			continue
		}
		seen[bookID] = struct{}{}
		overlayHardcoverProviderDefaults(&work, defaults)
		filtered = append(filtered, work)
	}
	return filtered
}

func mergeHydratedHardcoverWork(base workResource, hydrated []workResource, expectedWorkID int64, rejected map[int64]struct{}) workResource {
	final := base
	if len(hydrated) != 0 {
		// Works mapped from the fresh GetWork response appear first, so their
		// top-level metadata replaces a potentially stale BookKey payload.
		final = hydrated[0]
	}

	books := make(map[int64]bookResource, len(base.Books)+len(hydrated))
	add := func(work workResource) {
		if work.ForeignID != expectedWorkID {
			return
		}
		for _, book := range work.Books {
			if book.ForeignID == 0 {
				continue
			}
			if _, invalid := rejected[book.ForeignID]; invalid {
				continue
			}
			books[book.ForeignID] = book
		}
	}
	add(base)
	for _, work := range hydrated {
		add(work)
	}

	final.Books = slices.Collect(maps.Values(books))
	slices.SortFunc(final.Books, func(a, b bookResource) int {
		return cmp.Compare(a.ForeignID, b.ForeignID)
	})
	return final
}

func overlayHardcoverProviderDefaults(work *workResource, defaults hardcoverDefaultEditionIDs) {
	set := func(target *workResource) {
		target.DefaultCoverEditionID = defaults.cover
		target.DefaultEbookEditionID = defaults.ebook
		target.DefaultAudioEditionID = defaults.audio
		target.DefaultPhysicalEditionID = defaults.physical
		target.BestBookID = defaults.legacyBestID()
		if target.BestBookID == 0 {
			stabilizeLegacyBestBookID(target)
		}
	}

	set(work)
	for authorIndex := range work.Authors {
		for workIndex := range work.Authors[authorIndex].Works {
			if work.Authors[authorIndex].Works[workIndex].ForeignID == work.ForeignID {
				set(&work.Authors[authorIndex].Works[workIndex])
			}
		}
	}
}

func overlayHardcoverProviderEditionMembership(work *workResource, editionIDs []int64) {
	work.ProviderEditionIDs = slices.Clone(editionIDs)
	for authorIndex := range work.Authors {
		for workIndex := range work.Authors[authorIndex].Works {
			if work.Authors[authorIndex].Works[workIndex].ForeignID == work.ForeignID {
				work.Authors[authorIndex].Works[workIndex].ProviderEditionIDs = slices.Clone(editionIDs)
			}
		}
	}
}

// GetBook looks up a GR book (edition) in Hardcover's mappings.
func (g *HCGetter) GetBook(ctx context.Context, editionID int64, _ editionsCallback) ([]byte, int64, int64, error) {
	if editionID == 0 {
		return nil, 0, 0, errors.Join(errBadRequest, errors.New("edition missing ID"))
	}

	workBytes, ttl, ok := g.cache.GetWithTTL(ctx, BookKey(editionID))
	if ok && !hasCurrentWorkCacheSchema(workBytes, g.workCacheSchemaVersion()) {
		Log(ctx).Info("refreshing legacy Hardcover edition payload", "editionID", editionID)
		if err := g.cache.Expire(ctx, BookKey(editionID)); err != nil {
			Log(ctx).Warn("unable to expire unusable Hardcover edition payload", "editionID", editionID, "err", err)
		}
		ok = false
	}
	if ok && ttl > 0 {
		var cached workResource
		if err := json.Unmarshal(workBytes, &cached); err == nil &&
			cached.ForeignID != 0 &&
			len(cached.Authors) > 0 &&
			cached.Authors[0].ForeignID != 0 {
			if _, containsEdition := hardcoverWorkEdition(cached, editionID); containsEdition {
				return workBytes, cached.ForeignID, cached.Authors[0].ForeignID, nil
			}
		}
		Log(ctx).Warn("discarding cached edition with incomplete or mismatched identity", "editionID", editionID)
		if err := g.cache.Expire(ctx, BookKey(editionID)); err != nil {
			Log(ctx).Warn("unable to expire unusable Hardcover edition payload", "editionID", editionID, "err", err)
		}
	}

	Log(ctx).Debug("getting edition", "editionID", editionID)

	resp, err := hardcover.GetEdition(ctx, g.gql, editionID)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("getting book: %w", err)
	}
	if resp.Editions_by_pk.Id != editionID {
		return nil, 0, 0, errors.Join(errNotFound, fmt.Errorf(
			"edition identity mismatch: requested=%d returned=%d",
			editionID,
			resp.Editions_by_pk.Id,
		))
	}
	work := resp.Editions_by_pk.Book.WorkInfo

	if work.Id == 0 {
		return nil, 0, 0, errors.Join(errNotFound, fmt.Errorf("edition without work info"))
	}
	if resp.Editions_by_pk.Book_id == 0 || resp.Editions_by_pk.Book_id != work.Id {
		return nil, 0, 0, errors.Join(errNotFound, fmt.Errorf(
			"edition %d has inconsistent work identity: book_id=%d relationship=%d",
			editionID,
			resp.Editions_by_pk.Book_id,
			work.Id,
		))
	}

	workRsc, err := mapHardcoverToWorkResource(ctx, resp.Editions_by_pk.EditionInfo, work)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("mapping for book: %w", err)
	}
	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work: %w", err)
	}

	if len(workRsc.Authors) == 0 {
		Log(ctx).Warn("missing author", "editionID", editionID)
		return nil, 0, 0, errors.Join(errNotFound, errors.New("missing author"))
	}

	return out, workRsc.ForeignID, workRsc.Authors[0].ForeignID, nil
}

func mapHardcoverToWorkResource(ctx context.Context, edition hardcover.EditionInfo, work hardcover.WorkInfo) (workResource, error) {
	if edition.Id == 0 || work.Id == 0 {
		return workResource{}, errors.Join(errBadRequest, errors.New("missing ID"))
	}
	if edition.Book_id == 0 || edition.Book_id != work.Id {
		return workResource{}, errors.Join(errBadRequest, fmt.Errorf(
			"edition %d has inconsistent work identity: book_id=%d work=%d",
			edition.Id,
			edition.Book_id,
			work.Id,
		))
	}

	tags := []struct {
		Tag string `json:"tag"`
	}{}
	genres := []string{}

	_ = json.Unmarshal(work.Cached_tags, &tags)
	for _, t := range tags {
		genres = append(genres, t.Tag)
	}
	if len(genres) == 0 {
		genres = []string{"none"}
	}

	series := []SeriesResource{}
	for _, s := range work.Book_series {
		series = append(series, SeriesResource{
			Title:       s.Series.Name,
			ForeignID:   s.Series.Id,
			Description: s.Series.Description,

			LinkItems: []seriesWorkLinkResource{{
				PositionInSeries: fmt.Sprint(s.Position),
				SeriesPosition:   int(s.Position), // TODO: What's the difference b/t placement?
				ForeignWorkID:    work.Id,
				Primary:          false, // TODO: What is this?
			}},
		})
	}

	editionDescription := work.Description // edition.Description is no longer populated.
	if editionDescription == "" {
		editionDescription = "N/A" // Must be set?
	}

	editionTitle := edition.Title
	editionFullTitle := editionTitle
	editionSubtitle := edition.Subtitle

	if editionSubtitle != "" {
		editionTitle = strings.ReplaceAll(editionTitle, ": "+editionSubtitle, "")
		editionFullTitle = editionTitle + ": " + editionSubtitle
	}

	mediaType := classifyMediaType(edition.Edition_format)
	bookRsc := bookResource{
		ForeignID:   edition.Id,
		Asin:        edition.Asin,
		Description: editionDescription,
		Isbn13:      edition.Isbn_13,
		Title:       editionTitle,

		FullTitle:          editionFullTitle,
		ShortTitle:         editionTitle,
		Language:           edition.Language.Code3,
		Format:             edition.Edition_format,
		EditionInformation: edition.Edition_information, // TODO: Is this used anywhere?
		Publisher:          edition.Publisher.Name,      // TODO: Ignore books without publishers?
		ImageURL:           strings.ReplaceAll(string(work.Cached_image), `"`, ``),
		IsEbook:            mediaType == mediaTypeEbook,
		MediaType:          mediaType,
		NumPages:           edition.Pages,
		RatingCount:        work.Ratings_count,
		RatingSum:          int64(float64(work.Ratings_count) * work.Rating),
		AverageRating:      work.Rating,
		URL:                "https://hardcover.app/books/" + work.Slug,
		ReleaseDate:        hcReleaseDate(edition.Release_date),
		ReleaseDateRaw:     edition.Release_date,

		// TODO: Grab release date from book if absent

		// TODO: Omitting release date is a way to essentially force R to hide
		// the book from the frontend while allowing the user to still add it
		// via search. Better UX depending on what you're after.
	}

	author, err := bestAuthor(hardcover.AsContributions(work.Contributions))
	if err != nil {
		return workResource{}, err
	}

	authorDescription := "N/A" // Must be set?
	if author.Bio != "" {
		authorDescription = author.Bio
	}

	authorRsc := AuthorResource{
		CacheSchemaVersion: workCacheSchemaVersion,
		Name:               author.Name,
		ForeignID:          author.Id,
		URL:                "https://hardcover.app/authors/" + author.Slug,
		ImageURL:           strings.ReplaceAll(string(author.Cached_image), `"`, ``),
		Description:        authorDescription,
		Series:             series, // TODO:: Doesn't fully work yet #17.
	}

	workTitle := work.Title
	workFullTitle := workTitle
	workSubtitle := work.Subtitle

	if workSubtitle != "" {
		workTitle = strings.ReplaceAll(workTitle, ": "+workSubtitle, "")
		workFullTitle = workTitle + ": " + workSubtitle
	}

	defaults := validatedHardcoverDefaults(work.DefaultEditions, author.Id)
	bestBookID := defaults.legacyBestID()
	if bestBookID == 0 {
		// GetBook remains usable for malformed legacy data which has no provider
		// defaults. Denormalization preserves this choice unless a validated
		// provider default later becomes available.
		bestBookID = edition.Id
	}

	workRsc := workResource{
		CacheSchemaVersion: workCacheSchemaVersion,
		Title:              workTitle,
		FullTitle:          workFullTitle,
		ShortTitle:         workTitle,
		ForeignID:          work.Id,
		BestBookID:         bestBookID,
		URL:                "https://hardcover.app/books/" + work.Slug,
		ReleaseDate:        hcReleaseDate(work.Release_date),
		ReleaseDateRaw:     work.Release_date,
		Series:             series,
		Genres:             genres,
		RelatedWorks:       []int{},

		RatingCount:   work.Ratings_count,
		RatingSum:     int64(float64(work.Ratings_count) * work.Rating),
		AverageRating: work.Rating,

		DefaultCoverEditionID:    defaults.cover,
		DefaultEbookEditionID:    defaults.ebook,
		DefaultAudioEditionID:    defaults.audio,
		DefaultPhysicalEditionID: defaults.physical,
	}

	bookRsc.Contributors = []contributorResource{{ForeignID: author.Id, Role: "Author"}}
	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []AuthorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc} // TODO: Add best book here as well?

	return workRsc, nil
}

// GetAuthorBooks returns all GR book (edition) IDs.
func (g *HCGetter) GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq2[int64, error] {
	return func(yield func(int64, error) bool) {
		limit, offset := int64(100), int64(0)
		for {
			editions, err := hardcover.GetAuthorEditions(ctx, g.gql, authorID, limit, offset)
			if err != nil {
				Log(ctx).Warn("problem getting author editions", "err", err, "authorID", authorID)
				yield(0, err)
				return
			}

			if len(editions.Authors_by_pk.Contributions) == 0 {
				break // All done.
			}

			for _, c := range editions.Authors_by_pk.Contributions {
				author, err := bestAuthor(hardcover.AsContributions(c.Book.Contributions))
				if err != nil {
					continue
				}
				if author.Id != authorID {
					continue // Ignore anything that doesn't have this as the primary author.
				}

				editionID := bestHardcoverEdition(c.Book.DefaultEditions, authorID)
				if editionID == 0 {
					continue // Shouldn't happen.
				}
				if !yield(editionID, nil) {
					return
				}
			}

			offset += limit
		}
	}
}

// Recommendations returns trending work IDs from the past week.
func (g *HCGetter) Recommendations(ctx context.Context, page int64) (RecommentationsResource, error) {
	now := time.Now()
	lastWeek := now.Add(-7 * 24 * time.Hour)
	if page < 1 {
		return RecommentationsResource{}, fmt.Errorf("page must be gte 1")
	}

	recommended, err := hardcover.GetRecommended(ctx, g.gql, lastWeek.String(), now.String(), 100, 100*(page-1))
	if err != nil {
		return RecommentationsResource{}, fmt.Errorf("getting recommended: %w", err)
	}

	return RecommentationsResource{WorkIDs: recommended.Books_trending.WorkIDs}, nil
}

type hardcoverDefaultEditionIDs struct {
	cover    int64
	ebook    int64
	audio    int64
	physical int64
	fallback int64
}

func (ids hardcoverDefaultEditionIDs) legacyBestID() int64 {
	for _, id := range []int64{ids.cover, ids.ebook, ids.audio, ids.physical, ids.fallback} {
		if id != 0 {
			return id
		}
	}
	return 0
}

func (ids hardcoverDefaultEditionIDs) allIDs() []int64 {
	return []int64{ids.cover, ids.ebook, ids.audio, ids.physical, ids.fallback}
}

func (ids hardcoverDefaultEditionIDs) without(rejected map[int64]struct{}) hardcoverDefaultEditionIDs {
	keepUnlessRejected := func(id int64) int64 {
		if _, invalid := rejected[id]; invalid {
			return 0
		}
		return id
	}
	ids.cover = keepUnlessRejected(ids.cover)
	ids.ebook = keepUnlessRejected(ids.ebook)
	ids.audio = keepUnlessRejected(ids.audio)
	ids.physical = keepUnlessRejected(ids.physical)
	ids.fallback = keepUnlessRejected(ids.fallback)
	return ids
}

func validatedHardcoverDefaults(defaults hardcover.DefaultEditions, expectedAuthorID int64) hardcoverDefaultEditionIDs {
	cover := defaults.Default_cover_edition
	ebook := defaults.Default_ebook_edition
	audio := defaults.Default_audio_edition
	physical := defaults.Default_physical_edition

	ids := hardcoverDefaultEditionIDs{
		cover: validatedHardcoverEdition(
			defaults.Id,
			cover.Id,
			cover.Book_id,
			expectedAuthorID,
			hardcover.AsContributions(cover.Contributions),
		),
		ebook: validatedHardcoverEdition(
			defaults.Id,
			ebook.Id,
			ebook.Book_id,
			expectedAuthorID,
			hardcover.AsContributions(ebook.Contributions),
		),
		audio: validatedHardcoverEdition(
			defaults.Id,
			audio.Id,
			audio.Book_id,
			expectedAuthorID,
			hardcover.AsContributions(audio.Contributions),
		),
		physical: validatedHardcoverEdition(
			defaults.Id,
			physical.Id,
			physical.Book_id,
			expectedAuthorID,
			hardcover.AsContributions(physical.Contributions),
		),
	}

	if len(defaults.Fallback) == 1 {
		fallback := defaults.Fallback[0]
		ids.fallback = validatedHardcoverEdition(
			defaults.Id,
			fallback.Id,
			fallback.Book_id,
			expectedAuthorID,
			hardcover.AsContributions(fallback.Contributions),
		)
	}

	return ids
}

func validatedHardcoverEdition(workID, editionID, editionWorkID, expectedAuthorID int64, contributions []hardcover.Contributions) int64 {
	if workID == 0 || editionID == 0 || editionWorkID != workID {
		return 0
	}
	if expectedAuthorID != 0 && len(contributions) != 0 {
		// Edition contribution edges are incomplete on otherwise valid
		// Hardcover records. The work identity is already authoritative here;
		// reject only when the edition positively names a different primary
		// author, not when its contribution list is empty or inconclusive.
		matchedExpectedAuthor := false
		identifiedPrimaryAuthor := false
		for _, contribution := range contributions {
			if !isPrimaryHardcoverAuthorContribution(contribution) || contribution.Author.Id == 0 {
				continue
			}
			identifiedPrimaryAuthor = true
			if contribution.Author.Id == expectedAuthorID {
				matchedExpectedAuthor = true
				break
			}
		}
		if identifiedPrimaryAuthor && !matchedExpectedAuthor {
			return 0
		}
	}
	return editionID
}

func hardcoverEditionCandidates(defaults hardcover.DefaultEditions, expectedAuthorID int64) []int64 {
	ids := validatedHardcoverDefaults(defaults, expectedAuthorID)
	candidates := make([]int64, 0, 5)
	seen := make(map[int64]struct{}, 5)
	for _, id := range []int64{ids.cover, ids.ebook, ids.audio, ids.physical, ids.fallback} {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}
	return candidates
}

func bestHardcoverEdition(defaults hardcover.DefaultEditions, expectedAuthorID int64) int64 {
	author, err := bestAuthor(hardcover.AsContributions(defaults.Contributions))
	if err != nil {
		Log(context.TODO()).Warn("no author", "workID", defaults.Id)
		return 0
	}
	if expectedAuthorID != 0 && expectedAuthorID != author.Id {
		Log(context.TODO()).Warn("author mismatch", "expected", expectedAuthorID, "got", author.Id, "workID", defaults.Id)
		return 0
	}

	if id := validatedHardcoverDefaults(defaults, author.Id).legacyBestID(); id != 0 {
		return id
	}

	Log(context.TODO()).Warn("no valid editions", "workID", defaults.Id)
	return 0
}

func bestAuthor(contributions []hardcover.Contributions) (hardcover.ContributionsAuthorAuthors, error) {
	if len(contributions) == 0 {
		return hardcover.ContributionsAuthorAuthors{}, errors.Join(errNotFound, fmt.Errorf("no contributions"))
	}
	for _, c := range contributions {
		if isPrimaryHardcoverAuthorContribution(c) {
			// "Primary" authors seem to almost never have this set.
			return c.Author, nil
		}
	}
	return hardcover.ContributionsAuthorAuthors{}, errors.Join(errNotFound, fmt.Errorf("no valid contribution"))
}

func isPrimaryHardcoverAuthorContribution(contribution hardcover.Contributions) bool {
	// Hardcover's role field is unstructured. Keep this deliberately narrow so
	// translators, narrators, illustrators, and editors cannot displace a work's
	// already validated primary author.
	switch strings.ToLower(contribution.Contribution) {
	case "", "author", "author/narrator":
		return true
	default:
		return false
	}
}

// GetAuthor looks up an author on Hardcover.
func (g *HCGetter) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	Log(ctx).Debug("getting author", "authorID", authorID)

	if authorID == 0 {
		return nil, errors.Join(errBadRequest, errors.New("author ID missing"))
	}

	resp, err := hardcover.GetAuthorEditions(ctx, g.gql, authorID, 20, 0)
	if err != nil {
		return nil, fmt.Errorf("getting author editions: %w", err)
	}

	if resp.Authors_by_pk.Id == 0 {
		return nil, errors.Join(errNotFound, fmt.Errorf("invalid author editions"))
	}

	author, err := bestAuthor(hardcover.AsContributions(resp.Authors_by_pk.Contributions))
	if err != nil {
		return nil, err
	}
	if author.Id != authorID {
		Log(ctx).Warn("author mismatch, possibly merged?", "expected", authorID, "got", author.Id)
		return nil, errors.Join(errNotFound, fmt.Errorf("author mismatch"))
	}

	for _, cc := range resp.Authors_by_pk.Contributions {
		editionID := bestHardcoverEdition(cc.Book.DefaultEditions, authorID)
		if editionID == 0 {
			continue
		}
		workBytes, _, _, err := g.GetBook(ctx, editionID, nil)
		if err != nil {
			Log(ctx).Warn("problem getting initial book for author", "err", err, "editionID", editionID, "authorID", authorID)
			return nil, fmt.Errorf("initial edition: %w", err)
		}

		var w workResource
		err = json.Unmarshal(workBytes, &w)
		if err != nil {
			Log(ctx).Warn("problem unmarshaling work for author", "err", err, "bookID", editionID)
			_ = g.cache.Expire(ctx, BookKey(editionID))
			return nil, fmt.Errorf("unmarshaling: %w", err)
		}

		author := w.Authors[0]
		author.Works = []workResource{w}

		return json.Marshal(author)
	}

	Log(ctx).Warn("no valid works found", "authorID", authorID)
	return nil, errors.Join(errNotFound, fmt.Errorf("no valid works found"))
}

// GetSeries isn't implemented yet.
func (g *HCGetter) GetSeries(ctx context.Context, seriesID int64) (*SeriesResource, error) {
	seriesRsc := &SeriesResource{
		LinkItems: []seriesWorkLinkResource{},
	}

	limit, offset := int64(1000), int64(0)

	var lastPosition float32

	// Max out at 3k for the series.
	for offset < 3*limit {
		series, err := hardcover.GetSeries(ctx, g.gql, seriesID, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("getting series %d: %w", seriesID, err)
		}

		seriesRsc.Title = series.Series_by_pk.Name
		seriesRsc.Description = series.Series_by_pk.Description
		seriesRsc.ForeignID = series.Series_by_pk.Id

		if len(series.Series_by_pk.Book_series) == 0 {
			break
		}

		for _, bs := range series.Series_by_pk.Book_series {
			if lastPosition > 0 && bs.Position == lastPosition {
				// Skip less popular duplicates.
				continue
			}
			seriesRsc.LinkItems = append(seriesRsc.LinkItems, seriesWorkLinkResource{
				ForeignWorkID:    bs.Book_id,
				PositionInSeries: bs.Details,
				SeriesPosition:   int(bs.Position),
				Primary:          bs.Featured,
			})
			lastPosition = bs.Position
		}

		if len(seriesRsc.LinkItems) >= int(series.Series_by_pk.Books_count) {
			break
		}

		offset += limit
	}

	return seriesRsc, nil
}

func hcReleaseDate(d string) string {
	if strings.HasSuffix(d, "BC") {
		return "0001-01-01"
	}
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		return ""
	}
	if t.Year() > 9999 {
		return ""
	}
	return d
}
