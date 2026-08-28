package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blampe/rreading-glasses/gr"
	"github.com/blampe/rreading-glasses/hardcover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

func TestQueryBuilderMultipleQueries(t *testing.T) {
	t.Run("hardcover", func(t *testing.T) {
		qb := newQueryBuilder()

		query1 := hardcover.GetWork_Operation
		vars1 := map[string]any{"grBookIDs": []string{"1"}}

		query2 := hardcover.GetAuthorEditions_Operation
		vars2 := map[string]any{
			"id":     1,
			"limit":  2,
			"offset": 3,
		}

		id1, _, err := qb.add(query1, vars1)
		require.NoError(t, err)

		id2, _, err := qb.add(query2, vars2)
		require.NoError(t, err)

		query, vars, err := qb.build()
		require.NoError(t, err)

		expected := fmt.Sprintf(`query GetWork($%s_bookID: Int!, $%s_id: Int!, $%s_limit: Int!, $%s_offset: Int!) {
  %s: books_by_pk(id: $%s_bookID) {
    ...WorkInfo
    editions(order_by: {score: desc_nulls_last}) {
      ...EditionInfo
    }
  }
  %s: authors_by_pk(id: $%s_id) {
    ...AuthorInfo
    contributions(limit: $%s_limit, offset: $%s_offset, order_by: {book: {ratings_count: desc}}, where: {contributable_type: {_eq: "Book"}, book: {book_status_id: {_eq: "1"}}}) {
      ...Contributions
      book {
        id
        title
        ratings_count
        ...DefaultEditions
      }
    }
  }
}
fragment AuthorInfo on authors {
  id
  name
  slug
  bio
  cached_image(path: "url")
}
fragment Contributions on contributions {
  contribution
  author {
    ...AuthorInfo
  }
}
fragment DefaultEditions on books {
  id
  contributions {
    ...Contributions
  }
  default_audio_edition {
    id
    book_id
    contributions {
      ...Contributions
    }
  }
  default_physical_edition {
    id
    book_id
    contributions {
      ...Contributions
    }
  }
  default_cover_edition {
    id
    book_id
    contributions {
      ...Contributions
    }
  }
  default_ebook_edition {
    id
    book_id
    contributions {
      ...Contributions
    }
  }
  fallback: editions(order_by: [{score: desc_nulls_last}, {users_read_count: desc}, {id: asc}], limit: 1) {
    id
    book_id
    contributions {
      ...Contributions
    }
  }
}
fragment EditionInfo on editions {
  id
  title
  subtitle
  asin
  isbn_13
  edition_format
  pages
  audio_seconds
  language {
    code3
  }
  publisher {
    name
  }
  release_date
  audio_seconds
  physical_format
  physical_information
  edition_information
  users_read_count
  book_id
  score
}
fragment WorkInfo on books {
  id
  title
  subtitle
  description
  release_date
  cached_tags(path: "$.Genre")
  cached_image(path: "url")
  slug
  state
  canonical_id
  book_series {
    position
    series {
      id
      name
      description
    }
  }
  rating
  ratings_count
  ...DefaultEditions
}`, id1, id2, id2, id2, id1, id1, id2, id2, id2, id2)

		assert.Equal(t, expected, query, query)

		assert.Len(t, vars, 4)
		assert.Contains(t, vars, id1+"_bookID", id2+"_id", id2+"_limit", id2+"_offset")
	})

	t.Run("gr", func(t *testing.T) {
		qb := newQueryBuilder()

		query1 := gr.GetBook_Operation
		vars1 := map[string]any{"legacyId": []string{"1"}}

		query2 := gr.GetAuthorWorks_Operation
		vars2 := map[string]any{
			"pagination":                 map[string]string{},
			"getWorksByContributorInput": map[string]string{},
		}

		id1, _, err := qb.add(query1, vars1)
		require.NoError(t, err)

		id2, _, err := qb.add(query2, vars2)
		require.NoError(t, err)

		query, vars, err := qb.build()
		require.NoError(t, err)

		expected := fmt.Sprintf(`query GetBook($%s_legacyId: Int!, $%s_getWorksByContributorInput: GetWorksByContributorInput!, $%s_pagination: PaginationInput!) {
  %s: getBookByLegacyId(legacyId: $%s_legacyId) {
    ...BookInfo
    work {
      id
      legacyId
      details {
        webUrl
        publicationTime
      }
      bestBook {
        legacyId
        title
        titlePrimary
        primaryContributorEdge {
          role
          node {
            legacyId
          }
        }
      }
      editions {
        edges {
          node {
            ...BookInfo
          }
        }
      }
    }
  }
  %s: getWorksByContributor(getWorksByContributorInput: $%s_getWorksByContributorInput, pagination: $%s_pagination) {
    edges {
      node {
        id
        bestBook {
          legacyId
          primaryContributorEdge {
            role
            node {
              legacyId
            }
          }
          secondaryContributorEdges {
            role
          }
        }
      }
    }
    pageInfo {
      hasNextPage
      nextPageToken
    }
  }
}
fragment BookInfo on Book {
  id
  legacyId
  description(stripped: true)
  bookGenres {
    genre {
      name
    }
  }
  bookSeries {
    series {
      id
      title
      webUrl
    }
    seriesPlacement
  }
  details {
    asin
    isbn13
    format
    numPages
    language {
      name
    }
    officialUrl
    publisher
    publicationTime
  }
  imageUrl
  primaryContributorEdge {
    node {
      id
      name
      legacyId
      webUrl
      profileImageUrl
      description
    }
  }
  stats {
    averageRating
    ratingsCount
    ratingsSum
  }
  title
  titlePrimary
  webUrl
}`, id1, id2, id2, id1, id1, id2, id2, id2)

		assert.Equal(t, expected, query)

		assert.Len(t, vars, 3)
		assert.Contains(t, vars, id1+"_legacyId", id2+"_getWorksByContributorInput", id2+"_pagination")
	})
}

func TestBatching(t *testing.T) {
	if os.Getenv("RUN_HARDCOVER_INTEGRATION") != "1" {
		t.Skip("set RUN_HARDCOVER_INTEGRATION=1 to run live Hardcover tests")
	}
	apiKey := os.Getenv("HARDCOVER_API_KEY")
	if apiKey == "" {
		t.Skip("missing HARDCOVER_API_KEY")
		return
	}
	transport := &HeaderTransport{
		Key:          "Authorization",
		Value:        "Bearer " + apiKey,
		RoundTripper: http.DefaultTransport,
	}

	client := &http.Client{Transport: transport}

	url := "https://api.hardcover.app/v1/graphql"

	gql, err := NewBatchedGraphQLClient(url, client, 2*time.Second, 1, nil)
	require.NoError(t, err)

	start := time.Now()

	wg := sync.WaitGroup{}
	wg.Go(func() {
		_, err := hardcover.GetWork(context.Background(), gql, 156028352)
		if err != nil {
			panic(err)
		}
	})

	wg.Go(func() {
		_, err := hardcover.GetWork(context.Background(), gql, 164005178)
		if err != nil {
			panic(err)
		}
	})

	wg.Go(func() {
		_, err := hardcover.GetWork(context.Background(), gql, 340640138)
		if err != nil {
			panic(err)
		}
	})

	wg.Go(func() {
		_, err := hardcover.GetWork(context.Background(), gql, -1) // Missing.
		if err != nil {
			panic(err)
		}
	})

	wg.Wait()

	assert.Less(t, time.Since(start), 4*time.Second)
}

func TestBatchingOverflow(t *testing.T) {
	calls := atomic.Int32{}

	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			body := `{"data": {}, "errors": []}`
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	gql, err := NewBatchedGraphQLClient("https://foo.com", client, 50*time.Millisecond, 1, nil)
	require.NoError(t, err)

	wg := sync.WaitGroup{}

	// var resp1, resp2 *gr.GetBookResponse
	var err1, err2 error

	// Spawn more queries than our batch allows. They should get executed in
	// separate batches.
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = gr.GetBook(t.Context(), gql, 1)
	}()
	go func() {
		defer wg.Done()
		_, err2 = gr.GetBook(t.Context(), gql, 2)
	}()
	wg.Wait()

	assert.NoError(t, err1)
	assert.NoError(t, err2)

	assert.Equal(t, int32(2), calls.Load())
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

// TestBatchingRespectsBatchSize verifies that when more queries arrive than a
// single batch allows, the client splits them across multiple upstream
// requests — each containing at most batchSize top-level fields. This is the
// regression test for the Hardcover top_level_limit_exceeded 403s described in
// https://github.com/blampe/rreading-glasses/issues/574: Hardcover caps
// top-level queries per request at 5, so a batch of 12 must be split rather
// than rejected wholesale.
func TestBatchingRespectsBatchSize(t *testing.T) {
	const batchSize = 5
	const numQueries = 12

	var calls atomic.Int32
	var bodiesMu sync.Mutex
	var bodies []string

	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			bs, _ := io.ReadAll(r.Body)
			bodiesMu.Lock()
			bodies = append(bodies, string(bs))
			bodiesMu.Unlock()
			body := `{"data": {}, "errors": []}`
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	gql, err := NewBatchedGraphQLClient("https://foo.com", client, 50*time.Millisecond, batchSize, nil)
	require.NoError(t, err)

	wg := sync.WaitGroup{}
	errs := make([]error, numQueries)
	for i := range numQueries {
		wg.Add(1)

		go func() {
			defer wg.Done()
			_, errs[i] = gr.GetBook(t.Context(), gql, int64(i+1))
		}()
	}
	wg.Wait()

	for i, e := range errs {
		assert.NoError(t, e, "query %d", i)
	}

	// 12 queries at batch size 5 must be split across ceil(12/5) = 3 requests.
	assert.Equal(t, int32(3), calls.Load(), "expected ceil(12/5)=3 upstream requests; got bodies=%v", bodies)

	// Sanity: no single request should carry more than batchSize top-level
	// selection fields. We count the aliased fields in each body by counting
	// occurrences of the alias prefix gr.GetBook uses ("getBookByLegacyId").
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	for i, b := range bodies {
		count := strings.Count(b, "getBookByLegacyId")
		assert.LessOrEqual(t, count, batchSize, "request %d had %d top-level fields, want <= %d", i, count, batchSize)
	}
}

func TestBatchingAllowsOnlyOneUpstreamRequestInFlight(t *testing.T) {
	const numQueries = 3

	started := make(chan struct{}, numQueries)
	release := make(chan struct{}, numQueries)
	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			started <- struct{}{}
			<-release
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data": {}}`)),
			}, nil
		}),
	}

	gql, err := NewBatchedGraphQLClient("https://foo.com", client, 5*time.Millisecond, 1, nil)
	require.NoError(t, err)

	defer close(release)
	wg := sync.WaitGroup{}
	wg.Add(numQueries)
	for i := range numQueries {
		go func(id int64) {
			defer wg.Done()
			_, _ = gr.GetBook(t.Context(), gql, id)
		}(int64(i + 1))
	}

	for i := range numQueries {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("request %d did not start", i+1)
		}

		select {
		case <-started:
			t.Fatalf("request %d started while request %d was still in flight", i+2, i+1)
		case <-time.After(20 * time.Millisecond):
		}

		release <- struct{}{}
	}

	wg.Wait()
}

func TestBatchingSpacesEveryPhysicalRetry(t *testing.T) {
	var mu sync.Mutex
	attempts := make([]time.Time, 0, 3)
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		attempts = append(attempts, time.Now())
		attempt := len(attempts)
		mu.Unlock()
		if attempt < 3 {
			return nil, &net.DNSError{Err: "temporary test failure", IsTemporary: true}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data": {}}`)),
		}, nil
	})}

	config := DefaultHardcoverBatcherConfig()
	config.BatchInterval = 40 * time.Millisecond
	config.NetworkRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	config.RequestTimeout = time.Second
	gql, err := NewConfiguredBatchedGraphQLClient(t.Context(), "https://foo.com", client, config, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gql.Close() })

	_, err = gr.GetBook(t.Context(), gql, 1)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, attempts, 3)
	for i := 1; i < len(attempts); i++ {
		assert.GreaterOrEqual(t, attempts[i].Sub(attempts[i-1]), 35*time.Millisecond,
			"physical attempt %d started before BATCH_INTERVAL elapsed", i+1)
	}
}

func TestBatchingRateLimitOpensCircuitWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	gate := NewRateLimitGate(time.Hour)
	client := &http.Client{Transport: RateLimitTransport{
		Gate: gate,
		RoundTripper: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
				Header:     http.Header{"Retry-After": []string{"3600"}},
				Body:       io.NopCloser(strings.NewReader(`{"data": null}`)),
			}, nil
		}),
	}}

	config := DefaultHardcoverBatcherConfig()
	config.BatchInterval = time.Millisecond
	config.NetworkRetryDelays = nil
	gql, err := NewConfiguredBatchedGraphQLClient(t.Context(), "https://foo.com", client, config, gate, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gql.Close() })

	_, err = gr.GetBook(t.Context(), gql, 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
	assert.Equal(t, int32(1), calls.Load())

	_, err = gr.GetBook(t.Context(), gql, 2)
	require.Error(t, err)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
	assert.Equal(t, int32(1), calls.Load(), "open circuit must reject locally")
}

func TestBatchingCanceledBeforeFlushMakesNoUpstreamRequest(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data": {}}`))}, nil
	})}
	config := DefaultHardcoverBatcherConfig()
	config.BatchInterval = 10 * time.Millisecond
	gql, err := NewConfiguredBatchedGraphQLClient(t.Context(), "https://foo.com", client, config, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gql.Close() })
	batched := gql.(*batchedgqlclient)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = gr.GetBook(ctx, gql, 1)
	assert.ErrorIs(t, err, context.Canceled)
	batched.flush(t.Context())
	assert.Equal(t, int32(0), calls.Load())
}

func TestBatchingRejectsQueueOverflowImmediately(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data": {}}`))}, nil
	})}
	config := DefaultHardcoverBatcherConfig()
	config.BatchInterval = time.Hour
	config.MaxPendingQueries = 1
	gql, err := NewConfiguredBatchedGraphQLClient(t.Context(), "https://foo.com", client, config, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gql.Close() })
	batched := gql.(*batchedgqlclient)

	firstCtx, cancelFirst := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		_, requestErr := gr.GetBook(firstCtx, gql, 1)
		firstDone <- requestErr
	}()
	require.Eventually(t, func() bool {
		batched.mu.Lock()
		defer batched.mu.Unlock()
		return batched.pendingQueries == 1
	}, time.Second, time.Millisecond)

	started := time.Now()
	_, err = gr.GetBook(t.Context(), gql, 2)
	assert.ErrorIs(t, err, statusErr(http.StatusServiceUnavailable))
	assert.Less(t, time.Since(started), 100*time.Millisecond)
	cancelFirst()
	assert.ErrorIs(t, <-firstDone, context.Canceled)
}

func TestHardcoverBatcherConfigRejectsOversizedBatches(t *testing.T) {
	config := DefaultHardcoverBatcherConfig()
	config.BatchSize = 6
	_, err := NewConfiguredBatchedGraphQLClient(t.Context(), "https://foo.com", http.DefaultClient, config, nil, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "5-field limit")
}

func TestHardcoverBatcherConfigCapsPhysicalAttempts(t *testing.T) {
	config := DefaultHardcoverBatcherConfig()
	config.NetworkRetryDelays = []time.Duration{time.Second, time.Second, time.Second}
	_, err := NewConfiguredBatchedGraphQLClient(t.Context(), "https://foo.com", http.DefaultClient, config, nil, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "at most three physical attempts")
}

func TestGQLStatusCode(t *testing.T) {
	var err error = &gqlerror.Error{Message: "womp"}
	assert.ErrorIs(t, err, gqlStatusErr(err))

	err = &gqlerror.Error{Message: "Request failed with status code 403"}
	err403 := statusErr(403)
	assert.ErrorAs(t, gqlStatusErr(err), &err403)

	err = errors.New(`returned error 429: {"data":null}`)
	err429 := statusErr(http.StatusTooManyRequests)
	assert.ErrorAs(t, gqlStatusErr(err), &err429)
}
