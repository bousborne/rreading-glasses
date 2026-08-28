package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/bytedance/sonic"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/printer"
	"github.com/graphql-go/graphql/language/source"
	"github.com/graphql-go/graphql/language/visitor"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/rand"
)

// batchedgqlclient accumulates queries and executes them in batch in order to
// make better use of RPS limits.
type batchedgqlclient struct {
	mu sync.Mutex
	// attemptMu serializes physical transport calls and lastPhysicalAttempt
	// anchors the minimum start-to-start interval across both batches and
	// in-batch network retries.
	attemptMu           sync.Mutex
	lastPhysicalAttempt time.Time

	batchSize          int
	searchBatchSize    int
	maxPendingQueries  int
	pendingQueries     int
	queue              []batchedQuery
	priorityQueue      []batchedQuery
	priorityStreak     int
	every              time.Duration
	requestTimeout     time.Duration
	networkRetryDelays []time.Duration
	gate               *RateLimitGate
	metrics            *gqlMetrics

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once

	wrapped graphql.Client
}

// BatcherConfig controls GraphQL admission, pacing, and retry behavior. HTTP
// errors are never retried here. NetworkRetryDelays applies only to typed
// transport failures such as timeouts, connection resets, and premature EOF.
type BatcherConfig struct {
	BatchInterval      time.Duration
	BatchSize          int
	SearchBatchSize    int
	MaxPendingQueries  int
	RequestTimeout     time.Duration
	RateLimitFallback  time.Duration
	NetworkRetryDelays []time.Duration
	MaximumBatchSize   int
}

// BatchingGraphQLClient is a GraphQL client with an explicit lifecycle.
type BatchingGraphQLClient interface {
	graphql.Client
	Close() error
}

func DefaultHardcoverBatcherConfig() BatcherConfig {
	return BatcherConfig{
		BatchInterval:      2 * time.Second,
		BatchSize:          1,
		SearchBatchSize:    1,
		MaxPendingQueries:  100,
		RequestTimeout:     30 * time.Second,
		RateLimitFallback:  15 * time.Minute,
		NetworkRetryDelays: []time.Duration{time.Second, 5 * time.Second},
		MaximumBatchSize:   5,
	}
}

func (c BatcherConfig) validate() error {
	if c.BatchInterval <= 0 {
		return fmt.Errorf("batch interval must be positive")
	}
	if c.BatchSize <= 0 || c.SearchBatchSize <= 0 {
		return fmt.Errorf("batch sizes must be positive")
	}
	if c.MaximumBatchSize > 0 && (c.BatchSize > c.MaximumBatchSize || c.SearchBatchSize > c.MaximumBatchSize) {
		return fmt.Errorf("batch sizes must not exceed Hardcover's %d-field limit", c.MaximumBatchSize)
	}
	if c.MaxPendingQueries <= 0 {
		return fmt.Errorf("maximum pending queries must be positive")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request timeout must be positive")
	}
	if len(c.NetworkRetryDelays) > 2 {
		return fmt.Errorf("network retry delays permit at most three physical attempts")
	}
	for _, delay := range c.NetworkRetryDelays {
		if delay < 0 {
			return fmt.Errorf("network retry delays must not be negative")
		}
	}
	return nil
}

type requestPriorityKey struct{}

// WithInteractivePriority marks a request and all of its follow-up hydration
// calls as user-facing. Interactive batches get bounded priority: after three
// consecutive interactive batches, one background batch is allowed through.
func WithInteractivePriority(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestPriorityKey{}, true)
}

func isInteractive(ctx context.Context) bool {
	interactive, _ := ctx.Value(requestPriorityKey{}).(bool)
	return interactive
}

var (
	errBatcherClosed = errors.New("GraphQL batcher is closed")
	errQueueFull     = &BackoffError{Code: http.StatusServiceUnavailable, Delay: 30 * time.Second, Message: "GraphQL queue is full"}
)

// NewBatchedGraphQLClient creates a batching GraphQL client. Queries are
// accumulated and executed regularly accurding to the given rate.
func NewBatchedGraphQLClient(url string, client *http.Client, every time.Duration, batchSize int, reg *prometheus.Registry) (graphql.Client, error) {
	config := BatcherConfig{
		BatchInterval:      every,
		BatchSize:          batchSize,
		SearchBatchSize:    1,
		MaxPendingQueries:  100,
		RequestTimeout:     30 * time.Second,
		RateLimitFallback:  15 * time.Minute,
		NetworkRetryDelays: []time.Duration{time.Second, 5 * time.Second},
	}
	return NewConfiguredBatchedGraphQLClient(context.Background(), url, client, config, nil, reg)
}

// NewConfiguredBatchedGraphQLClient creates a cancelable, bounded batching
// client. Passing a shared gate lets the HTTP transport and GraphQL response
// parser enforce one provider-wide cooldown.
func NewConfiguredBatchedGraphQLClient(
	ctx context.Context,
	url string,
	client *http.Client,
	config BatcherConfig,
	gate *RateLimitGate,
	reg *prometheus.Registry,
) (BatchingGraphQLClient, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if gate == nil {
		gate = NewRateLimitGate(config.RateLimitFallback)
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &batchedgqlclient{
		batchSize:          config.BatchSize,
		searchBatchSize:    config.SearchBatchSize,
		maxPendingQueries:  config.MaxPendingQueries,
		every:              config.BatchInterval,
		requestTimeout:     config.RequestTimeout,
		networkRetryDelays: slices.Clone(config.NetworkRetryDelays),
		gate:               gate,
		metrics:            newGQLMetrics(reg),
		wrapped:            graphql.NewClient(url, client),
		ctx:                ctx,
		cancel:             cancel,
	}

	c.wg.Add(2)
	go c.flushLoop()
	go c.statsLoop()
	return c, nil
}

func (c *batchedgqlclient) flushLoop() {
	defer c.wg.Done()
	ctx := context.WithValue(c.ctx, middleware.RequestIDKey, fmt.Sprintf("batch-flush-%d", time.Now().Unix()))
	timer := time.NewTimer(c.interval())
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			c.flush(ctx)
			timer.Reset(c.interval())
		}
	}
}

func (c *batchedgqlclient) statsLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			batchesSent := c.metrics.batchesSentGet()
			average := float32(0)
			if batchesSent > 0 {
				average = float32(c.metrics.queriesSentGet()) / float32(batchesSent)
			}
			Log(c.ctx).Debug("query stats",
				"batchesWaiting", c.metrics.batchesWaitingGet(),
				"batchesSent", batchesSent,
				"queriesSent", c.metrics.queriesSentGet(),
				"averageBatchSize", average,
			)
		}
	}
}

func (c *batchedgqlclient) interval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.every
}

// flush pops the oldest batchedQuery off the queue and executes it.
// Individualized errors are returned to listeners if possible, so one query
// can fail without the entire batch failing. The whole batch can still fail in
// other cases, e.g. 4XX response codes.
//
// Only one batch is sent per tick, and fire runs synchronously, so there can be
// at most one upstream GraphQL request in flight. This deliberately trades
// burst throughput for predictable pressure on Hardcover's rate limiter.
func (c *batchedgqlclient) flush(ctx context.Context) {
	c.mu.Lock()
	if len(c.queue) == 0 && len(c.priorityQueue) == 0 {
		c.metrics.batchesWaitingSet(0)
		c.mu.Unlock()
		return
	}

	var batch batchedQuery
	if len(c.priorityQueue) > 0 && (len(c.queue) == 0 || c.priorityStreak < 3) {
		batch = c.priorityQueue[0]
		c.priorityQueue = c.priorityQueue[1:]
		c.priorityStreak++
	} else {
		batch = c.queue[0]
		c.queue = c.queue[1:]
		c.priorityStreak = 0
	}
	c.pendingQueries -= len(batch.subscribers)
	c.metrics.batchesWaitingSet(len(c.queue) + len(c.priorityQueue))
	c.metrics.queriesWaitingSet(c.pendingQueries)
	c.mu.Unlock()

	c.fire(ctx, batch)
}

// fire executes a single batch as one GraphQL request. Canceled subscribers
// are removed before the query is built, so an entirely canceled batch makes
// no physical request.
func (c *batchedgqlclient) fire(ctx context.Context, batch batchedQuery) {
	qb := newQueryBuilder()
	subscribers := map[string]*subscription{}
	for _, sub := range batch.subscribers {
		if sub.ctx.Err() != nil {
			c.metrics.canceledInc()
			continue
		}
		var vars map[string]any
		out, err := json.Marshal(sub.req.Variables)
		if err == nil {
			err = sonic.ConfigStd.Unmarshal(out, &vars)
		}
		if err != nil {
			deliver(sub, err)
			continue
		}
		id, field, err := qb.add(sub.req.Query, vars)
		if err != nil {
			deliver(sub, err)
			continue
		}
		sub.field = field
		subscribers[id] = sub
	}
	if len(subscribers) == 0 {
		return
	}

	if err := c.gate.Check(); err != nil {
		for _, sub := range subscribers {
			deliver(sub, err)
		}
		c.failQueued(err)
		return
	}

	query, vars, err := qb.build()
	if err != nil {
		Log(ctx).Error("unable to build query", "err", err)
		for _, sub := range subscribers {
			deliver(sub, err)
		}
		return
	}
	c.metrics.batchesSentInc()
	c.metrics.queriesSentAdd(int64(len(subscribers)))

	req := &graphql.Request{
		Query:     query,
		Variables: vars,
		OpName:    qb.op.Name.Value,
	}

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	var data map[string]any
	var resp *graphql.Response
	var requestErr error
retryLoop:
	for attempt := 0; ; attempt++ {
		data = map[string]any{}
		resp = &graphql.Response{Data: &data}
		requestErr = c.makePhysicalRequest(ctx, req, resp)
		if requestErr != nil {
			requestErr = gqlStatusErr(requestErr)
		}
		if requestErr == nil || len(resp.Errors) > 0 || attempt >= len(c.networkRetryDelays) || !retryableNetworkError(requestErr) {
			break
		}
		delay := c.networkRetryDelays[attempt]
		Log(ctx).Warn("transient network error; retrying batched query",
			"attempt", attempt+1,
			"delay", delay,
			"count", len(subscribers),
			"err", requestErr)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			requestErr = errors.Join(requestErr, ctx.Err())
			break retryLoop
		case <-timer.C:
		}
	}

	if errors.Is(requestErr, statusErr(http.StatusTooManyRequests)) {
		var rateLimit *RateLimitError
		if !errors.As(requestErr, &rateLimit) {
			requestErr = c.gate.Open("", requestErr)
		}
		c.metrics.upstreamRateLimitsInc()
		c.metrics.cooldownSecondsSet(retryAfterDuration(requestErr))
		c.failQueued(requestErr)
	}
	if requestErr == nil {
		c.metrics.cooldownSecondsSet(0)
	}
	if requestErr == nil && resp != nil {
		for _, responseErr := range resp.Errors {
			mapped := gqlStatusErr(responseErr)
			if errors.Is(mapped, statusErr(http.StatusTooManyRequests)) {
				requestErr = c.gate.Open("", mapped)
				c.metrics.upstreamRateLimitsInc()
				c.metrics.cooldownSecondsSet(retryAfterDuration(requestErr))
				c.failQueued(requestErr)
				break
			}
		}
	}

	// Extract any field-level errors, and return them to their subscribers.
	// We can ignore the top-level error in this case, because it is just the
	// wrapped version of our response errors.
	if requestErr == nil && resp != nil && len(resp.Errors) > 0 {
		for _, e := range resp.Errors {
			sub, ok := subscribers[e.Path.String()]
			if !ok {
				continue
			}
			deliver(sub, gqlStatusErr(e))
			delete(subscribers, e.Path.String())
		}
	} else if requestErr != nil {
		Log(ctx).Warn("batched query error", "count", len(subscribers), "err", requestErr)
		for _, sub := range subscribers {
			deliver(sub, requestErr)
		}
		return
	}

	for id, sub := range subscribers {
		// TODO: missing response.
		byt, err := json.Marshal(map[string]any{
			sub.field: data[id],
		})
		if err != nil {
			deliver(sub, err)
			continue
		}

		deliver(sub, sonic.ConfigStd.Unmarshal(byt, &sub.resp.Data))
	}
}

// makePhysicalRequest is the sole path to the underlying GraphQL transport.
// The configured batch interval is a minimum between physical attempt start
// times, including retries after typed network failures.
func (c *batchedgqlclient) makePhysicalRequest(ctx context.Context, req *graphql.Request, resp *graphql.Response) error {
	c.attemptMu.Lock()
	defer c.attemptMu.Unlock()

	if !c.lastPhysicalAttempt.IsZero() {
		wait := c.interval() - time.Since(c.lastPhysicalAttempt)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}

	c.lastPhysicalAttempt = time.Now()
	c.metrics.physicalAttemptsInc()
	return c.wrapped.MakeRequest(ctx, req, resp)
}

// MakeRequest implements graphql.Client.
func (c *batchedgqlclient) MakeRequest(
	ctx context.Context,
	req *graphql.Request,
	resp *graphql.Response,
) error {
	if err := c.gate.Check(); err != nil {
		c.metrics.localCooldownRejectsInc()
		c.metrics.cooldownSecondsSet(retryAfterDuration(err))
		return err
	}
	sub := c.enqueue(ctx, req, resp)
	select {
	case err := <-sub.respC:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errBatcherClosed
	}
}

// enqueue adds a query to the batch and returns a subscription whose result
// channel resolves when the batch is executed.
func (c *batchedgqlclient) enqueue(
	ctx context.Context,
	req *graphql.Request,
	resp *graphql.Response,
) *subscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	respC := make(chan error, 1)
	sub := &subscription{ctx: ctx, req: req, resp: resp, respC: respC}

	if err := ctx.Err(); err != nil {
		deliver(sub, err)
		return sub
	}
	if err := c.ctx.Err(); err != nil {
		deliver(sub, errBatcherClosed)
		return sub
	}
	c.pruneCanceledLocked()
	if c.pendingQueries >= c.maxPendingQueries {
		c.metrics.queueFullRejectsInc()
		deliver(sub, errQueueFull)
		return sub
	}

	// Take the youngest batch if it isn't full yet, otherwise start a new batch.
	queue := &c.queue
	batchSize := c.batchSize
	if isInteractive(ctx) {
		queue = &c.priorityQueue
		batchSize = c.searchBatchSize
	}
	if len(*queue) == 0 || len((*queue)[len(*queue)-1].subscribers) >= batchSize {
		*queue = append(*queue, batchedQuery{})
	}
	last := len(*queue) - 1
	(*queue)[last].subscribers = append((*queue)[last].subscribers, sub)
	c.pendingQueries++
	c.metrics.batchesWaitingSet(len(c.queue) + len(c.priorityQueue))
	c.metrics.queriesWaitingSet(c.pendingQueries)
	return sub
}

func (c *batchedgqlclient) pruneCanceledLocked() {
	prune := func(queue []batchedQuery) []batchedQuery {
		keptBatches := queue[:0]
		for _, batch := range queue {
			keptSubscribers := batch.subscribers[:0]
			for _, sub := range batch.subscribers {
				if sub.ctx.Err() != nil {
					c.pendingQueries--
					c.metrics.canceledInc()
					continue
				}
				keptSubscribers = append(keptSubscribers, sub)
			}
			if len(keptSubscribers) > 0 {
				batch.subscribers = keptSubscribers
				keptBatches = append(keptBatches, batch)
			}
		}
		return keptBatches
	}
	c.priorityQueue = prune(c.priorityQueue)
	c.queue = prune(c.queue)
	c.metrics.batchesWaitingSet(len(c.queue) + len(c.priorityQueue))
	c.metrics.queriesWaitingSet(c.pendingQueries)
}

// Close stops background goroutines and unblocks all queued callers.
func (c *batchedgqlclient) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.failQueued(errBatcherClosed)
		c.wg.Wait()
	})
	return nil
}

func (c *batchedgqlclient) failQueued(err error) {
	c.mu.Lock()
	queued := append(slices.Clone(c.priorityQueue), c.queue...)
	c.priorityQueue = nil
	c.queue = nil
	c.pendingQueries = 0
	c.metrics.batchesWaitingSet(0)
	c.metrics.queriesWaitingSet(0)
	c.mu.Unlock()
	for _, batch := range queued {
		for _, sub := range batch.subscribers {
			deliver(sub, err)
		}
	}
}

func deliver(sub *subscription, err error) {
	select {
	case sub.respC <- err:
	default:
	}
}

func retryableNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var status statusErr
	if errors.As(err, &status) {
		return false
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func retryAfterDuration(err error) time.Duration {
	var retryable interface{ RetryAfter() time.Duration }
	if errors.As(err, &retryable) {
		return retryable.RetryAfter()
	}
	return 0
}

// subscription holds information about a caller who is waiting for a query to
// be resolved as part of a batch.
type subscription struct {
	ctx   context.Context
	req   *graphql.Request
	resp  *graphql.Response
	respC chan error
	field string
}

// gqlStatusErr translates errors into meaningful status codes. The client
// normally returns error responses with a 200 OK status code and a populated
// "Errors" field containing stringed errors. We want to instead surface e.g.
// 404 errors directly.
//
// The error is returned unchanged if it doesn't include a status code.
func gqlStatusErr(err error) error {
	errStr := err.Error()
	for _, marker := range []string{"Request failed with status code", "returned error"} {
		idx := strings.Index(errStr, marker)
		if idx == -1 {
			continue
		}

		code, parseErr := pathToID(errStr[idx:])
		if parseErr == nil {
			return errors.Join(err, statusErr(code))
		}
	}

	return err
}

// queryBuilder accumulates queries into one query with multiple fields so they
// can all be executed as part of one request.
type queryBuilder struct {
	op        *ast.OperationDefinition
	fragments map[string]struct{}
	vars      map[string]any
}

type batchedQuery struct {
	subscribers []*subscription
}

// _fragments holds string representations of fragment nodes since they are static.
var _fragments = map[string]string{}

// newQueryBuilder initializes a new QueryBuilder with an empty Document.
func newQueryBuilder() *queryBuilder {
	return &queryBuilder{
		vars:      make(map[string]any),
		fragments: map[string]struct{}{},
	}
}

var runes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

// randRunes returns a short random string of length n.
func randRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = runes[rand.Intn(len(runes))]
	}
	return string(b)
}

// add extends the current query with a new field. The field's alias and name
// are returned so they can be recovered later.
func (qb *queryBuilder) add(query string, vars map[string]any) (id string, field string, err error) {
	src := source.NewSource(&source.Source{
		Body: []byte(query),
	})

	parsedDoc, err := parser.Parse(parser.ParseParams{Source: src})
	if err != nil {
		return "", "", fmt.Errorf("failed to parse query: %w", err)
	}

	id = randRunes(8)

	varRename := make(map[string]string)

	// TODO: Only handle one def
	for _, def := range parsedDoc.Definitions {
		// Include fragments, if there are any, and cache their strings because
		// they don't change.
		if fragDef, ok := def.(*ast.FragmentDefinition); ok {
			name := fragDef.Name.Value
			if _, seen := qb.fragments[name]; !seen {
				if _, cached := _fragments[name]; !cached {
					_fragments[name] = printer.Print(fragDef).(string)
				}
				qb.fragments[name] = struct{}{}
			}
		}

		opDef, ok := def.(*ast.OperationDefinition)
		if !ok {
			continue
		}

		if qb.op == nil {
			qb.op = opDef
		}

		// Visit the AST to rename vars and alias fields
		opts := visitor.VisitInParallel(&visitor.VisitorOptions{
			Enter: func(p visitor.VisitFuncParams) (string, any) {
				switch node := p.Node.(type) {
				case *ast.VariableDefinition:
					oldName := node.Variable.Name.Value
					newName := id + "_" + oldName
					varRename[oldName] = newName
					node.Variable.Name.Value = newName
					qb.vars[newName] = vars[oldName]
				case *ast.Variable:
					if newName, ok := varRename[node.Name.Value]; ok {
						node.Name.Value = newName
					}
				case *ast.Field:
					if len(p.Ancestors) == 3 {
						field = node.Name.Value
						node.Alias = &ast.Name{Value: id, Kind: "Name"}
					}
				}
				return visitor.ActionNoChange, nil
			},
		})
		visitor.Visit(opDef, opts, nil)

		if qb.op == opDef {
			continue
		}

		qb.op.SelectionSet.Selections = append(qb.op.SelectionSet.Selections, opDef.SelectionSet.Selections...)
		qb.op.VariableDefinitions = append(qb.op.VariableDefinitions, opDef.VariableDefinitions...)
	}

	return id, field, nil
}

// Build returns the merged query string and variables map.
func (qb *queryBuilder) build() (string, map[string]any, error) {
	builder := strings.Builder{}

	builder.WriteString(printer.Print(qb.op).(string))

	for _, fragName := range slices.Sorted(maps.Keys(qb.fragments)) {
		builder.WriteString("\n")
		builder.WriteString(_fragments[fragName])
	}

	return builder.String(), qb.vars, nil
}
