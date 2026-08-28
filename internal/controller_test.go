//go:generate go run go.uber.org/mock/mockgen -typed -source controller.go -package internal -destination mock.go . getter

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestIncrementalDenormalization(t *testing.T) {
	// Looking up foreign editions should update relevant works to include
	// those editions, and authors should be updated to reflect the new works.
	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)
	getter := NewMockgetter(c)

	work := workResource{ForeignID: 1}

	englishEdition := bookResource{ForeignID: 100, Language: "en"}
	frenchEdition := bookResource{ForeignID: 200, Language: "fr"}
	work.Books = []bookResource{englishEdition}

	authorID := int64(1000)
	author := AuthorResource{ForeignID: authorID, Works: []workResource{work}}

	work.Authors = []AuthorResource{author}

	initialAuthorBytes, err := json.Marshal(author)
	require.NoError(t, err)
	initialWorkBytes, err := json.Marshal(work)
	require.NoError(t, err)
	frenchEditionBytes, err := json.Marshal(workResource{ForeignID: work.ForeignID, Books: []bookResource{frenchEdition}})
	require.NoError(t, err)
	englishEditionBytes, err := json.Marshal(workResource{ForeignID: work.ForeignID, Books: []bookResource{englishEdition}})
	require.NoError(t, err)

	cache := newMemoryCache()

	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)

	go ctrl.Run(t.Context())
	t.Cleanup(func() { ctrl.Shutdown(t.Context()) })

	// TODO: Generalize this into a test helper.
	getter.EXPECT().GetAuthor(gomock.Any(), author.ForeignID).DoAndReturn(func(ctx context.Context, authorID int64) ([]byte, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, AuthorKey(authorID))
		if ok {
			return cachedBytes, nil
		}
		return initialAuthorBytes, nil
	}).AnyTimes()

	getter.EXPECT().GetBook(gomock.Any(), englishEdition.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, bookID int64, saveEditions editionsCallback) ([]byte, int64, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, BookKey(bookID))
		if ok {
			return cachedBytes, 0, 0, nil
		}
		return englishEditionBytes, work.ForeignID, authorID, nil
	}).AnyTimes()

	getter.EXPECT().GetBook(gomock.Any(), frenchEdition.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, bookID int64, saveEditions editionsCallback) ([]byte, int64, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, BookKey(bookID))
		if ok {
			return cachedBytes, 0, 0, nil
		}
		return frenchEditionBytes, work.ForeignID, authorID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), work.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkBytes, authorID, nil
	}).AnyTimes()

	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(
		func(yield func(int64, error) bool) {
			if !yield(englishEdition.ForeignID, nil) {
				return
			}
			if !yield(frenchEdition.ForeignID, nil) {
				return
			}
		},
	).AnyTimes()

	// Getting the author will initially return it with only the "best" original-language edition.
	authorBytes, _, err := ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Len(t, author.Works, 1)
	assert.Equal(t, englishEdition.ForeignID, author.Works[0].Books[0].ForeignID)

	// Getting a foreign edition should add it to the work.
	_, _, err = ctrl.GetBook(ctx, frenchEdition.ForeignID)
	require.NoError(t, err)

	waitForDenorm(ctrl)

	workBytes, _, err := ctrl.GetWork(ctx, work.ForeignID)
	require.NoError(t, err)
	var w workResource
	require.NoError(t, json.Unmarshal(workBytes, &w))
	assert.Len(t, w.Books, 2)

	waitForDenorm(ctrl)

	// The work should have also been updated on the author.
	authorBytes, _, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works, 1)
	require.Len(t, author.Works[0].Books, 2)
	assert.Equal(t, englishEdition.ForeignID, author.Works[0].Books[0].ForeignID)
	assert.Equal(t, frenchEdition.ForeignID, author.Works[0].Books[1].ForeignID)

	// Force a cache miss to re-trigger denormalization.
	_ = ctrl.cache.Expire(ctx, BookKey(frenchEdition.ForeignID))
	_, _, _ = ctrl.GetBook(ctx, frenchEdition.ForeignID)

	_ = ctrl.refreshG.Wait()
	time.Sleep(100 * time.Millisecond) // Wait for the denormalization goroutine update things.

	workBytes, _, err = ctrl.GetWork(ctx, work.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(workBytes, &w))
	assert.Len(t, w.Books, 2)

	authorBytes, _, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works[0].Books, 2)

	// Force an author cache miss to re-trigger denormalization.
	_ = ctrl.cache.Expire(ctx, AuthorKey(author.ForeignID))
	_, _, _ = ctrl.GetAuthor(ctx, author.ForeignID)

	waitForDenorm(ctrl)

	authorBytes, _, err = ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(authorBytes, &author))
	assert.Len(t, author.Works[0].Books, 2)
}

func TestDenormalizeMissing(t *testing.T) {
	// Denormalizing relationships on objects that are missing should no-op.
	ctx := context.Background()

	authorID := int64(1)
	workID := int64(2)
	bookID := int64(3)

	cache := newMemoryCache()

	notFoundGetter := NewMockgetter(gomock.NewController(t))
	notFoundGetter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(nil, errNotFound).AnyTimes()
	notFoundGetter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(nil, 0, errNotFound).AnyTimes()

	ctrl, err := NewController(cache, notFoundGetter, nil, nil)
	require.NoError(t, err)

	err = ctrl.denormalizeEditions(ctx, workID, bookID)
	assert.ErrorIs(t, err, errNotFound)

	err = ctrl.denormalizeWorks(ctx, authorID, workID)
	assert.ErrorIs(t, err, errNotFound)
}

func TestShallowAuthorLookupDefersAndDeduplicatesCatalogueRefresh(t *testing.T) {
	t.Parallel()

	const (
		authorID = int64(100)
		workID   = int64(200)
	)

	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.refreshC = make(chan refreshAuthor, 2)

	authorBytes, err := json.Marshal(AuthorResource{ForeignID: authorID})
	require.NoError(t, err)
	workBytes, err := json.Marshal(workResource{
		ForeignID: workID,
		Authors:   []AuthorResource{{ForeignID: authorID}},
		Books:     []bookResource{{ForeignID: 300}},
	})
	require.NoError(t, err)

	getter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(authorBytes, nil)
	getter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(workBytes, authorID, nil)

	// Denormalization needs the author record, but must not recursively crawl
	// the author's complete catalogue.
	require.NoError(t, ctrl.denormalizeWorks(t.Context(), authorID, workID))
	assert.Len(t, ctrl.refreshC, 0)
	_, refreshNeeded := ctrl.cache.Get(t.Context(), authorRefreshNeededKey(authorID))
	assert.True(t, refreshNeeded)

	// An explicit request schedules exactly one full refresh, even when it is
	// repeated while that refresh is queued or running.
	got, _, err := ctrl.GetAuthor(t.Context(), authorID)
	require.NoError(t, err)
	var gotAuthor AuthorResource
	require.NoError(t, json.Unmarshal(got, &gotAuthor))
	assert.Equal(t, authorID, gotAuthor.ForeignID)
	assert.Len(t, ctrl.refreshC, 1)
	_, refreshNeeded = ctrl.cache.Get(t.Context(), authorRefreshNeededKey(authorID))
	assert.False(t, refreshNeeded)

	_, _, err = ctrl.GetAuthor(t.Context(), authorID)
	require.NoError(t, err)
	assert.Len(t, ctrl.refreshC, 1)
}

func TestExplicitAuthorLookupWinsShallowSingleflightRace(t *testing.T) {
	t.Parallel()

	const authorID = int64(100)
	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.refreshC = make(chan refreshAuthor, 2)

	authorBytes, err := json.Marshal(AuthorResource{ForeignID: authorID})
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	getter.EXPECT().GetAuthor(gomock.Any(), authorID).DoAndReturn(func(context.Context, int64) ([]byte, error) {
		close(started)
		<-release
		return authorBytes, nil
	})

	shallowDone := make(chan error, 1)
	go func() {
		_, _, err := ctrl.getAuthorForDenormalization(t.Context(), authorID)
		shallowDone <- err
	}()
	<-started

	explicitDone := make(chan error, 1)
	go func() {
		_, _, err := ctrl.GetAuthor(t.Context(), authorID)
		explicitDone <- err
	}()
	close(release)

	require.NoError(t, <-shallowDone)
	require.NoError(t, <-explicitDone)
	assert.Len(t, ctrl.refreshC, 1)
}

func TestShallowAuthorLookupJoiningExplicitSingleflightDoesNotDuplicateRefresh(t *testing.T) {
	t.Parallel()

	const authorID = int64(100)
	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.refreshC = make(chan refreshAuthor, 2)

	authorBytes, err := json.Marshal(AuthorResource{ForeignID: authorID})
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	getter.EXPECT().GetAuthor(gomock.Any(), authorID).DoAndReturn(func(context.Context, int64) ([]byte, error) {
		close(started)
		<-release
		return authorBytes, nil
	})

	explicitDone := make(chan error, 1)
	go func() {
		_, _, err := ctrl.GetAuthor(t.Context(), authorID)
		explicitDone <- err
	}()
	<-started

	shallowDone := make(chan error, 1)
	go func() {
		_, _, err := ctrl.getAuthorForDenormalization(t.Context(), authorID)
		shallowDone <- err
	}()
	close(release)

	require.NoError(t, <-explicitDone)
	require.NoError(t, <-shallowDone)
	assert.Len(t, ctrl.refreshC, 1)
}

func TestSubtitles(t *testing.T) {
	// Subtitles (i.e. FullTitle) are used in situations where multiple works
	// share the same primary title, or when the work belongs to a series..

	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)
	getter := NewMockgetter(c)

	workDupe1 := workResource{
		ForeignID: 1,
		Title:     "FOO",
		FullTitle: "Foo: First Work",
		Books: []bookResource{
			{ForeignID: 1, Title: "Foo", FullTitle: "Foo: First Edition"},
			{ForeignID: 2, Title: "Foo", FullTitle: ""},
		},
	}

	workDupe2 := workResource{
		ForeignID: 2,
		Title:     "Foo",
		FullTitle: "Foo: Second Work",
		Books: []bookResource{
			{ForeignID: 10, Title: "Foo", FullTitle: "Foo: Second Edition"},
			{ForeignID: 20, Title: "Foo", FullTitle: ""},
		},
	}

	workDupe3 := workResource{
		ForeignID:  3,
		Title:      "Foo",
		FullTitle:  "Foo: Third Work",
		ShortTitle: "Foo",
		Books: []bookResource{
			{ForeignID: 30, Title: "Foo", FullTitle: "Foo: Third Edition"},
			{ForeignID: 40, Title: "Foo", FullTitle: ""},
		},
	}

	workDupe4 := workResource{
		ForeignID:  4,
		Title:      "Foo",
		FullTitle:  "Foo: Fourth Work",
		ShortTitle: "Foo",
		Books: []bookResource{
			{ForeignID: 50, Title: "Foo", FullTitle: "Foo: Fourth Edition"},
			{ForeignID: 60, Title: "Foo", FullTitle: ""},
		},
	}

	workUnique := workResource{
		ForeignID: 5,
		Title:     "Bar",
		FullTitle: "Bar: Not Foo",
		Books: []bookResource{
			{ForeignID: 70, Title: "Bar", FullTitle: "Bar: Not Foo"},
			{ForeignID: 80, Title: "Bar", FullTitle: ""},
		},
	}

	workSeries := workResource{
		ForeignID:  6,
		Title:      "Baz",
		FullTitle:  "Baz: The Baz Series #3",
		ShortTitle: "Baz",
		Books: []bookResource{
			{
				ForeignID:  90,
				Title:      "Baz",
				FullTitle:  "Baz: The Baz Series #3",
				ShortTitle: "Baz",
			},
		},
		Series: []SeriesResource{{ForeignID: 1234}},
	}

	author := AuthorResource{ForeignID: 1000, Works: []workResource{
		workDupe1,
		workDupe2,
		workUnique,
		workSeries,
	}}

	workDupe1.Authors = []AuthorResource{author}
	workDupe2.Authors = []AuthorResource{author}
	workDupe3.Authors = []AuthorResource{author}
	workDupe4.Authors = []AuthorResource{author}
	workUnique.Authors = []AuthorResource{author}
	workSeries.Authors = []AuthorResource{author}

	initialAuthorBytes, err := json.Marshal(author)
	require.NoError(t, err)
	initialWorkDupe1Bytes, err := json.Marshal(workDupe1)
	require.NoError(t, err)
	initialWorkDupe2Bytes, err := json.Marshal(workDupe2)
	require.NoError(t, err)
	initialWorkDupe3Bytes, err := json.Marshal(workDupe3)
	require.NoError(t, err)
	initialWorkDupe4Bytes, err := json.Marshal(workDupe4)
	require.NoError(t, err)
	initialWorkUniqueBytes, err := json.Marshal(workUnique)
	require.NoError(t, err)
	initialWorkSeriesBytes, err := json.Marshal(workSeries)
	require.NoError(t, err)

	cache := newMemoryCache()

	ctrl, err := NewController(cache, getter, nil, nil)
	go ctrl.Run(t.Context())
	t.Cleanup(func() { ctrl.Shutdown(context.Background()) })
	require.NoError(t, err)

	getter.EXPECT().GetAuthor(gomock.Any(), author.ForeignID).DoAndReturn(func(ctx context.Context, authorID int64) ([]byte, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, AuthorKey(authorID))
		if ok {
			return cachedBytes, nil
		}
		return initialAuthorBytes, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe1.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe1Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe2.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe2Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe3.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe3Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workDupe4.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkDupe4Bytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workUnique.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkUniqueBytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetWork(gomock.Any(), workSeries.ForeignID, gomock.Any()).DoAndReturn(func(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
		cachedBytes, ok := ctrl.cache.Get(ctx, WorkKey(workID))
		if ok {
			return cachedBytes, 0, nil
		}
		return initialWorkSeriesBytes, author.ForeignID, nil
	}).AnyTimes()

	getter.EXPECT().GetSeries(gomock.Any(), int64(1234)).Return(&SeriesResource{
		ForeignID: 1234,
		LinkItems: []seriesWorkLinkResource{},
	}, nil)

	getter.EXPECT().GetAuthorBooks(gomock.Any(), author.ForeignID).Return(iter.Seq2[int64, error](func(func(int64, error) bool) {}))

	err = ctrl.denormalizeWorks(ctx, author.ForeignID, workDupe1.ForeignID, workDupe2.ForeignID, workUnique.ForeignID)
	require.NoError(t, err)

	// Add these after the others have already had subtitles applied. We should
	// still apply a subtitle to this new work, instead of using its short
	// title.
	err = ctrl.denormalizeWorks(ctx, author.ForeignID, workDupe3.ForeignID)
	require.NoError(t, err)
	err = ctrl.denormalizeWorks(ctx, author.ForeignID, workDupe4.ForeignID)
	require.NoError(t, err)

	authorBytes, _, err := ctrl.GetAuthor(ctx, author.ForeignID)
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Equal(t, "Foo: First Work", author.Works[0].Title)
	assert.Equal(t, "Foo: Second Work", author.Works[1].Title)
	assert.Equal(t, "Foo: Third Work", author.Works[2].Title)
	assert.Equal(t, "Foo: Fourth Work", author.Works[3].Title)
	assert.Equal(t, "Bar", author.Works[4].Title)

	assert.Equal(t, "Foo: First Edition", author.Works[0].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[0].Books[1].Title)

	assert.Equal(t, "Foo: Second Edition", author.Works[1].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[1].Books[1].Title)

	assert.Equal(t, "Foo: Third Edition", author.Works[2].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[2].Books[1].Title)

	assert.Equal(t, "Foo: Fourth Edition", author.Works[3].Books[0].Title)
	assert.Equal(t, "Foo", author.Works[3].Books[1].Title)

	assert.Equal(t, "Bar", author.Works[4].Books[0].Title)
	assert.Equal(t, "Bar", author.Works[4].Books[1].Title)

	assert.Equal(t, "Baz: The Baz Series #3", author.Works[5].Books[0].Title)
}

func TestMergedEditions(t *testing.T) {
	// GetBook(X) and GetBook(Y) can both return an edition with ID X if the
	// editions were merged. That shouldn't manifest as a work containing two
	// copies of the same edition, because the client requires uniqueness.
	ctx := t.Context()
	c := gomock.NewController(t)
	getter := NewMockgetter(c)
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)

	bookID := int64(1)
	mergedID := int64(2)
	workID := int64(10)
	authorID := int64(100)

	bookBytes, err := json.Marshal(workResource{
		ForeignID: workID,
		Books: []bookResource{{
			ForeignID: bookID,
		}},
	})
	require.NoError(t, err)

	// Treat editions 1 and 2 as merged.
	getter.EXPECT().GetBook(gomock.Any(), bookID, nil).Return(bookBytes, workID, authorID, nil)
	getter.EXPECT().GetBook(gomock.Any(), mergedID, nil).Return(bookBytes, workID, authorID, nil)

	// Treat 1 as the work's best book.
	getter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(bookBytes, authorID, nil)

	err = ctrl.denormalizeEditions(ctx, workID, bookID, mergedID)
	require.NoError(t, err)

	// The work shouldn't have a duplicated edition.
	workBytes, _, err := ctrl.GetWork(ctx, workID)
	require.NoError(t, err)

	var work workResource
	require.NoError(t, json.Unmarshal(workBytes, &work))

	assert.Len(t, work.Books, 1)
}

func TestMergedWorks(t *testing.T) {
	// Same principle as TestMergedEditions.

	ctx := t.Context()
	getter := NewMockgetter(gomock.NewController(t))
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)
	// This test exercises synchronous denormalization only. Keep any explicit
	// author refresh queued so asynchronous catalogue work cannot outlive the
	// mock expectations for this test.
	ctrl.refreshC = make(chan refreshAuthor, 1)

	workID := int64(1)
	mergedID := int64(2)
	authorID := int64(100)

	workBytes, err := json.Marshal(workResource{
		ForeignID: workID,
		Books:     []bookResource{{ForeignID: 1000}},
	})
	require.NoError(t, err)

	authorBytes, err := json.Marshal(AuthorResource{
		ForeignID: authorID,
	})
	require.NoError(t, err)

	// Treat works 1 and 2 as merged.
	getter.EXPECT().GetWork(gomock.Any(), workID, nil).Return(workBytes, authorID, nil)
	getter.EXPECT().GetWork(gomock.Any(), mergedID, nil).Return(workBytes, authorID, nil)

	getter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(authorBytes, nil)
	err = ctrl.denormalizeWorks(ctx, authorID, workID, mergedID)
	require.NoError(t, err)

	// The author shouldn't have a duplicated work.
	authorBytes, _, err = ctrl.GetAuthor(ctx, authorID)
	require.NoError(t, err)

	var author AuthorResource
	require.NoError(t, json.Unmarshal(authorBytes, &author))

	assert.Len(t, author.Works, 1)
}

func TestRefreshAuthorDoesNotMultiplyTransportRetries(t *testing.T) {
	t.Parallel()

	const (
		authorID = int64(100)
		bookID   = int64(200)
	)

	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.denormC = make(chan edge, 10)

	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(iter.Seq2[int64, error](func(yield func(int64, error) bool) {
		yield(bookID, nil)
	}))
	// The GraphQL batcher/transport owns bounded physical retries. The
	// controller must make exactly one logical call and defer the whole author
	// attempt after that owner reports failure.
	getter.EXPECT().GetBook(gomock.Any(), bookID, gomock.Any()).Return(nil, int64(0), int64(0), statusErr(http.StatusServiceUnavailable))

	complete, cause := ctrl.refreshAuthorAttempt(t.Context(), authorID)
	assert.False(t, complete)
	assert.ErrorIs(t, cause, statusErr(http.StatusServiceUnavailable))
}

func TestRefreshAuthorStopsImmediatelyOnRateLimit(t *testing.T) {
	t.Parallel()

	const (
		authorID   = int64(100)
		firstBook  = int64(200)
		secondBook = int64(201)
	)

	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.denormC = make(chan edge, 1)

	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(iter.Seq2[int64, error](func(yield func(int64, error) bool) {
		if !yield(firstBook, nil) {
			return
		}
		yield(secondBook, nil)
	}))
	// There is intentionally no expectation for a retry or for secondBook.
	getter.EXPECT().GetBook(gomock.Any(), firstBook, gomock.Any()).Return(nil, int64(0), int64(0), statusErr(http.StatusTooManyRequests))

	complete, cause := ctrl.refreshAuthorAttempt(t.Context(), authorID)
	assert.False(t, complete)
	assert.ErrorIs(t, cause, statusErr(http.StatusTooManyRequests))
	assert.Empty(t, ctrl.denormC)
}

func TestRefreshAuthorPropagatesRateLimitDeadlineAfterEarlierFailure(t *testing.T) {
	t.Parallel()

	const authorID = int64(100)
	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.denormC = make(chan edge, 1)

	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(iter.Seq2[int64, error](func(yield func(int64, error) bool) {
		if yield(200, nil) {
			yield(201, nil)
		}
	}))
	getter.EXPECT().GetBook(gomock.Any(), int64(200), gomock.Any()).Return(nil, int64(0), int64(0), statusErr(http.StatusServiceUnavailable))
	deadline := time.Now().Add(time.Hour)
	getter.EXPECT().GetBook(gomock.Any(), int64(201), gomock.Any()).Return(nil, int64(0), int64(0), &RateLimitError{Until: deadline})

	complete, cause := ctrl.refreshAuthorAttempt(t.Context(), authorID)
	assert.False(t, complete)
	var rateLimit *RateLimitError
	require.ErrorAs(t, cause, &rateLimit)
	assert.Equal(t, deadline, rateLimit.Until)
}

func TestRefreshAuthorKeepsFailedEnumerationPending(t *testing.T) {
	t.Parallel()

	const authorID = int64(100)

	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.denormC = make(chan edge, 1)

	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(iter.Seq2[int64, error](func(yield func(int64, error) bool) {
		yield(0, statusErr(http.StatusTooManyRequests))
	}))

	assert.False(t, ctrl.refreshAuthorOnce(t.Context(), authorID))
	assert.Empty(t, ctrl.denormC)
}

func TestAuthorRefreshRateLimitPlanPreservesBudgetAndDeadline(t *testing.T) {
	t.Parallel()

	ctrl, err := NewController(newMemoryCache(), NewMockgetter(gomock.NewController(t)), nil, nil)
	require.NoError(t, err)
	ctrl.authorRefreshRetryDelay = time.Second

	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	cause := &RateLimitError{Until: now.Add(45 * time.Minute), now: func() time.Time { return now }}
	nextAttempt, delay := ctrl.authorRefreshRetryPlan(refreshAuthor{attempt: 3}, cause)
	assert.Equal(t, 3, nextAttempt, "a provider cooldown must not consume author retry budget")
	assert.Equal(t, 45*time.Minute, delay)
}

func TestAuthorRefreshRateLimitRetryDoesNotWakeBeforeCooldown(t *testing.T) {
	ctrl, err := NewController(newMemoryCache(), NewMockgetter(gomock.NewController(t)), nil, nil)
	require.NoError(t, err)
	ctrl.authorRefreshRetryDelay = time.Millisecond
	ctrl.refreshC = make(chan refreshAuthor, 1)

	const cooldown = 50 * time.Millisecond
	start := time.Now()
	cause := &RateLimitError{Until: start.Add(cooldown)}
	ctrl.scheduleAuthorRefreshRetry(t.Context(), refreshAuthor{id: 100, attempt: 2}, cause)

	select {
	case retry := <-ctrl.refreshC:
		assert.Equal(t, 2, retry.attempt)
		assert.GreaterOrEqual(t, time.Since(start), cooldown-5*time.Millisecond)
	case <-time.After(time.Second):
		t.Fatal("rate-limited author refresh was not rescheduled")
	}
}

func TestAuthorRefreshBackoffIsExponentialAndCapped(t *testing.T) {
	t.Parallel()

	ctrl, err := NewController(newMemoryCache(), NewMockgetter(gomock.NewController(t)), nil, nil)
	require.NoError(t, err)
	ctrl.authorRefreshRetryDelay = time.Minute
	ctrl.authorRefreshRetryMax = 8 * time.Minute

	assert.Equal(t, time.Minute, ctrl.authorRefreshBackoff(0))
	assert.Equal(t, 2*time.Minute, ctrl.authorRefreshBackoff(1))
	assert.Equal(t, 4*time.Minute, ctrl.authorRefreshBackoff(2))
	assert.Equal(t, 8*time.Minute, ctrl.authorRefreshBackoff(3))
	assert.Equal(t, 8*time.Minute, ctrl.authorRefreshBackoff(100))
}

func TestAuthorRefreshPausesAfterBoundedAttempts(t *testing.T) {
	const authorID = int64(100)
	cache := newMemoryCache()
	ctrl, err := NewController(cache, NewMockgetter(gomock.NewController(t)), nil, nil)
	require.NoError(t, err)
	ctrl.authorRefreshMaxAttempts = 1
	require.True(t, ctrl.claimAuthorRefresh(authorID))
	ctrl.metrics.refreshWaitingAdd(1)

	ctrl.scheduleAuthorRefreshRetry(t.Context(), refreshAuthor{id: authorID}, errors.New("incomplete catalogue"))

	_, markerExists := cache.Get(t.Context(), authorRefreshNeededKey(authorID))
	assert.True(t, markerExists)
	assert.True(t, ctrl.claimAuthorRefresh(authorID), "paused refresh must be manually resumable")
	assert.Equal(t, float64(0), ctrl.metrics.refreshWaitingGet())
}

type recordingPersister struct {
	mu      sync.Mutex
	pending map[int64][]byte
}

func (p *recordingPersister) Persist(_ context.Context, authorID int64, state []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[int64][]byte)
	}
	p.pending[authorID] = append([]byte(nil), state...)
	return nil
}

func (p *recordingPersister) Persisted(context.Context) ([]int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int64, 0, len(p.pending))
	for id := range p.pending {
		ids = append(ids, id)
	}
	return ids, nil
}

func (p *recordingPersister) Delete(_ context.Context, authorID int64) error {
	p.mu.Lock()
	delete(p.pending, authorID)
	p.mu.Unlock()
	return nil
}

func (p *recordingPersister) has(authorID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.pending[authorID]
	return ok
}

func TestShutdownDrainsRefreshCompletionBeforeRunStops(t *testing.T) {
	const authorID = int64(100)
	started := make(chan struct{})
	release := make(chan struct{})

	getter := NewMockgetter(gomock.NewController(t))
	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(iter.Seq2[int64, error](func(yield func(int64, error) bool) {
		close(started)
		<-release
	}))

	persist := &recordingPersister{}
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, persist, nil)
	require.NoError(t, err)
	go ctrl.Run(context.Background())
	select {
	case <-ctrl.runReady:
	case <-time.After(time.Second):
		t.Fatal("controller did not start")
	}

	cache.Set(t.Context(), authorRefreshNeededKey(authorID), []byte{1}, time.Hour)
	ctrl.queueAuthorRefreshIfNeeded(t.Context(), authorID, []byte(`{"id":100}`))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("author refresh did not start")
	}
	require.True(t, persist.has(authorID))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		ctrl.Shutdown(shutdownCtx)
		close(shutdownDone)
	}()

	// Shutdown must leave Run consuming denormalization edges while the active
	// refresh finishes and submits refreshDone.
	close(release)
	select {
	case <-shutdownDone:
	case <-shutdownCtx.Done():
		t.Fatal("shutdown left an author refresh blocked")
	}

	assert.False(t, persist.has(authorID), "completed refresh must not remain persisted across deploy")
	assert.True(t, ctrl.claimAuthorRefresh(authorID), "refresh claim must be released after persistence cleanup")
}

func TestShutdownDrainsWorkSpawnedWhileProcessingAuthorEdge(t *testing.T) {
	const (
		authorID = int64(100)
		workID   = int64(200)
		bookID   = int64(300)
	)
	authorHeld := make(chan struct{})
	releaseAuthor := make(chan struct{})
	workStarted := make(chan struct{})
	releaseWork := make(chan struct{})
	bookProcessed := make(chan struct{})

	getter := NewMockgetter(gomock.NewController(t))
	cache := newMemoryCache()
	var ctrl *Controller

	authorBytes, err := json.Marshal(AuthorResource{ForeignID: authorID})
	require.NoError(t, err)
	cache.Set(t.Context(), AuthorKey(authorID), authorBytes, time.Hour)
	work := workResource{ForeignID: workID, Books: []bookResource{{ForeignID: bookID}}}
	workBytes, err := json.Marshal(work)
	require.NoError(t, err)

	firstWork := getter.EXPECT().GetWork(gomock.Any(), workID, nil).DoAndReturn(func(context.Context, int64, editionsCallback) ([]byte, int64, error) {
		close(authorHeld)
		<-releaseAuthor
		ctrl.workG.Go(func() error {
			close(workStarted)
			<-releaseWork
			_ = ctrl.enqueueDenorm(context.Background(), edge{kind: workEdge, parentID: workID, childIDs: newSet(bookID)})
			return nil
		})
		return workBytes, authorID, nil
	})
	secondWork := getter.EXPECT().GetWork(gomock.Any(), workID, nil).After(firstWork.Call).Return(workBytes, authorID, nil)
	getter.EXPECT().GetBook(gomock.Any(), bookID, nil).After(secondWork).DoAndReturn(func(context.Context, int64, editionsCallback) ([]byte, int64, int64, error) {
		close(bookProcessed)
		return workBytes, workID, authorID, nil
	})

	ctrl, err = NewController(cache, getter, nil, nil)
	require.NoError(t, err)
	go ctrl.Run(context.Background())
	select {
	case <-ctrl.runReady:
	case <-time.After(time.Second):
		t.Fatal("controller did not start")
	}

	ctrl.denormC <- edge{kind: authorEdge, parentID: authorID, childIDs: newSet(workID)}
	select {
	case <-authorHeld:
	case <-time.After(time.Second):
		t.Fatal("author edge was not being processed")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		ctrl.Shutdown(shutdownCtx)
		close(shutdownDone)
	}()
	close(releaseAuthor)
	select {
	case <-workStarted:
	case <-time.After(time.Second):
		t.Fatal("author edge did not spawn bounded work")
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before spawned work completed")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseWork)
	select {
	case <-shutdownDone:
	case <-shutdownCtx.Done():
		t.Fatal("shutdown did not finish")
	}
	select {
	case <-bookProcessed:
	default:
		t.Fatal("work edge submitted during shutdown was not drained")
	}
}

func TestShutdownWaitsForAdmittedDenormProducer(t *testing.T) {
	ctrl, err := NewController(newMemoryCache(), NewMockgetter(gomock.NewController(t)), nil, nil)
	require.NoError(t, err)
	go ctrl.Run(context.Background())
	<-ctrl.runReady

	started := make(chan struct{})
	release := make(chan struct{})
	ctrl.submitDenormTask(func() {
		close(started)
		<-release
		_ = ctrl.enqueueDenorm(context.Background(), edge{kind: authorEdge, parentID: 4699102})
	})
	<-started

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		ctrl.Shutdown(shutdownCtx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("shutdown returned before admitted producer finished")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-shutdownCtx.Done():
		t.Fatal("shutdown did not drain admitted producer")
	}
}

func TestRecommendationsWaitsForHydration(t *testing.T) {
	const workID = int64(100)
	started := make(chan struct{})
	release := make(chan struct{})
	getter := NewMockgetter(gomock.NewController(t))
	getter.EXPECT().Recommendations(gomock.Any(), int64(1)).Return(RecommentationsResource{WorkIDs: []int64{workID}}, nil)
	workBytes, err := json.Marshal(workResource{ForeignID: workID})
	require.NoError(t, err)
	getter.EXPECT().GetWork(gomock.Any(), workID, gomock.Any()).DoAndReturn(func(context.Context, int64, editionsCallback) ([]byte, int64, error) {
		close(started)
		<-release
		return workBytes, int64(0), nil
	})
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	ctrl.denormC = make(chan edge, 1)

	type result struct {
		recs RecommentationsResource
		err  error
	}
	done := make(chan result, 1)
	go func() {
		recs, recommendationErr := ctrl.Recommendations(t.Context(), 1)
		done <- result{recs: recs, err: recommendationErr}
	}()
	<-started
	select {
	case <-done:
		t.Fatal("recommendations returned before hydration completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, []int64{workID}, got.recs.WorkIDs)
}

func TestFuzz(t *testing.T) {
	fuzzed := fuzz(_authorTTL, 2)
	assert.Less(t, fuzzed, _authorTTL*2)
	assert.Greater(t, fuzzed, _authorTTL)
}

func waitForDenorm(ctrl *Controller) {
	for ctrl.metrics.refreshWaitingGet() != 0 {
		time.Sleep(100 * time.Millisecond)
	}
	for ctrl.metrics.denormWaitingGet() != 0 {
		time.Sleep(100 * time.Millisecond)
	}

	if os.Getenv("CI") != "" {
		time.Sleep(1 * time.Second)
	} else {
		time.Sleep(100 * time.Millisecond)
	}
}
