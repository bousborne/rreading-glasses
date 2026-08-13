package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

	batchSize      int            // batchSize is the max number of queries per batch.
	queue          []batchedQuery // queue contains spillover in cases where we've accumulated more queries than our batch size allows.
	every          time.Duration  // every controls how often requests are flushed.
	maxRetries     int            // maxRetries bounds retries for an upstream 429 response.
	retryBase      time.Duration  // retryBase controls exponential backoff after an upstream 429 response.
	requestTimeout time.Duration  // requestTimeout bounds a batch, including rate-limit backoff.
	metrics        *gqlMetrics    // metrics tracks batches and queries sent.

	wrapped graphql.Client
}

// NewBatchedGraphQLClient creates a batching GraphQL client. Queries are
// accumulated and executed regularly accurding to the given rate.
func NewBatchedGraphQLClient(url string, client *http.Client, every time.Duration, batchSize int, reg *prometheus.Registry) (graphql.Client, error) {
	wrapped := graphql.NewClient(url, client)

	c := &batchedgqlclient{
		batchSize:      batchSize,
		wrapped:        wrapped,
		queue:          []batchedQuery{},
		metrics:        newGQLMetrics(reg),
		every:          every,
		maxRetries:     4,
		retryBase:      5 * time.Second,
		requestTimeout: 2 * time.Minute,
	}

	go func() {
		ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("batch-flush-%d", time.Now().Unix()))
		for {
			time.Sleep(c.every)
			c.flush(ctx)
		}
	}()

	// Log gql stats every minute.
	go func() {
		ctx := context.Background()
		for {
			time.Sleep(1 * time.Minute)
			batchesWaiting := c.metrics.batchesWaitingGet()
			batchesSent := c.metrics.batchesSentGet()
			queriesSent := c.metrics.queriesSentGet()

			Log(ctx).Debug("query stats",
				"batchesWaiting", batchesWaiting,
				"batchesSent", batchesSent,
				"queriesSent", queriesSent,
				"averageBatchSize", (float32(queriesSent) / float32(batchesSent)),
			)
		}
	}()

	return c, nil
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
	c.metrics.batchesWaitingSet(len(c.queue))

	if len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}

	batch := c.queue[0]
	c.queue = c.queue[1:]
	c.mu.Unlock()

	c.fire(ctx, batch)
}

// fire executes a single batch as one GraphQL request. Rate-limited requests
// are retried with bounded exponential backoff before the error is returned to
// callers.
func (c *batchedgqlclient) fire(ctx context.Context, batch batchedQuery) {
	c.metrics.batchesSentInc()
	c.metrics.queriesSentAdd(int64(len(batch.subscribers)))

	query, vars, err := batch.qb.build()
	if err != nil {
		Log(ctx).Error("unable to build query", "err", err)
		for _, sub := range batch.subscribers {
			sub.respC <- err
		}
		return
	}

	req := &graphql.Request{
		Query:     query,
		Variables: vars,
		OpName:    batch.qb.op.Name.Value,
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
		requestErr = c.wrapped.MakeRequest(ctx, req, resp)
		if requestErr != nil {
			requestErr = gqlStatusErr(requestErr)
		}

		if requestErr == nil || len(resp.Errors) > 0 ||
			!errors.Is(requestErr, statusErr(http.StatusTooManyRequests)) ||
			attempt >= c.maxRetries {
			break
		}

		delay := c.retryBase * time.Duration(1<<attempt)
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}

		Log(ctx).Warn("Hardcover rate limited batched query; retrying",
			"attempt", attempt+1,
			"maxRetries", c.maxRetries,
			"delay", delay,
			"count", len(batch.subscribers))

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			requestErr = errors.Join(requestErr, ctx.Err())
			break retryLoop
		case <-timer.C:
		}
	}

	// Extract any field-level errors, and return them to their subscribers.
	// We can ignore the top-level error in this case, because it is just the
	// wrapped version of our response errors.
	if resp != nil && len(resp.Errors) > 0 {
		for _, e := range resp.Errors {
			sub, ok := batch.subscribers[e.Path.String()]
			if !ok {
				continue
			}
			sub.respC <- gqlStatusErr(e)
			delete(batch.subscribers, e.Path.String())
		}
	} else if requestErr != nil {
		Log(ctx).Warn("batched query error", "count", len(batch.subscribers), "err", requestErr)
		for _, sub := range batch.subscribers {
			sub.respC <- requestErr
		}
		return
	}

	for id, sub := range batch.subscribers {
		// TODO: missing response.
		byt, err := json.Marshal(map[string]any{
			sub.field: data[id],
		})
		if err != nil {
			sub.respC <- err
			continue
		}

		sub.respC <- sonic.ConfigStd.Unmarshal(byt, &sub.resp.Data)
	}
}

// MakeRequest implements graphql.Client.
func (c *batchedgqlclient) MakeRequest(
	ctx context.Context,
	req *graphql.Request,
	resp *graphql.Response,
) error {
	err := <-c.enqueue(ctx, req, resp).respC
	return err
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

	// Take the youngest batch if it isn't full yet, otherwise start a new batch.
	if len(c.queue) == 0 || len(c.queue[len(c.queue)-1].subscribers) >= c.batchSize {
		c.queue = append(c.queue, batchedQuery{
			qb:          newQueryBuilder(),
			subscribers: map[string]*subscription{},
		})
	}
	batch := c.queue[len(c.queue)-1]

	respC := make(chan error, 1)

	sub := &subscription{
		ctx:   ctx,
		resp:  resp,
		respC: respC,
	}

	var vars map[string]any
	out, _ := json.Marshal(req.Variables)
	_ = sonic.ConfigStd.Unmarshal(out, &vars)

	id, field, err := batch.qb.add(req.Query, vars)
	if err != nil {
		respC <- err
	}

	batch.subscribers[id] = &subscription{
		ctx:   ctx,
		resp:  resp,
		respC: respC,
		field: field,
	}

	return sub
}

// subscription holds information about a caller who is waiting for a query to
// be resolved as part of a batch.
type subscription struct {
	ctx   context.Context
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
	qb          *queryBuilder
	subscribers map[string]*subscription
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
