//go:generate go run go.uber.org/mock/mockgen -typed -source hardcover_test.go -package hardcover -destination ../hardcover/mock.go . gql
package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/blampe/rreading-glasses/hardcover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

//nolint:unused // Used for mock generation.
type gql interface {
	graphql.Client // Used for mock generation.
}

//nolint:unused
type transport interface {
	http.RoundTripper
}

func TestBestHardcoverEditionUsesAudioDefault(t *testing.T) {
	const (
		authorID = int64(123)
		audioID  = int64(456)
	)

	author := hardcover.ContributionsAuthorAuthors{
		AuthorInfo: hardcover.AuthorInfo{Id: authorID},
	}
	defaults := hardcover.DefaultEditions{
		Id: 789,
		Contributions: []hardcover.DefaultEditionsContributions{{
			Contributions: hardcover.Contributions{Author: author},
		}},
		Default_audio_edition: hardcover.DefaultEditionsDefault_audio_editionEditions{
			Id:      audioID,
			Book_id: 789,
			Contributions: []hardcover.DefaultEditionsDefault_audio_editionEditionsContributions{{
				Contributions: hardcover.Contributions{Author: author},
			}},
		},
	}

	assert.Equal(t, audioID, bestHardcoverEdition(defaults, authorID))
}

func TestHardcoverDefaultsPreserveDistinctProviderChoices(t *testing.T) {
	const (
		workID     = int64(100)
		authorID   = int64(200)
		coverID    = int64(301)
		ebookID    = int64(302)
		audioID    = int64(303)
		physicalID = int64(304)
	)

	defaults := testHardcoverDefaults(workID, authorID, coverID, ebookID, audioID, physicalID, 0)
	ids := validatedHardcoverDefaults(defaults, authorID)
	assert.Equal(t, hardcoverDefaultEditionIDs{
		cover:    coverID,
		ebook:    ebookID,
		audio:    audioID,
		physical: physicalID,
	}, ids)
	assert.Equal(t, coverID, ids.legacyBestID())

	work, err := mapHardcoverToWorkResource(t.Context(), hardcover.EditionInfo{
		Id:      ebookID,
		Book_id: workID,
		Title:   "Provider Defaults",
		Language: hardcover.EditionInfoLanguageLanguages{
			Code3: "eng",
		},
	}, testHardcoverWork(workID, authorID, defaults))
	require.NoError(t, err)
	assert.Equal(t, coverID, work.BestBookID)
	assert.Equal(t, workCacheSchemaVersion, work.CacheSchemaVersion)
	assert.Equal(t, coverID, work.DefaultCoverEditionID)
	assert.Equal(t, ebookID, work.DefaultEbookEditionID)
	assert.Equal(t, audioID, work.DefaultAudioEditionID)
	assert.Empty(t, work.ProviderEditionIDs, "a one-edition lookup must not claim complete provider membership")
	assert.Equal(t, physicalID, work.DefaultPhysicalEditionID)

	payload, err := json.Marshal(work)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{
		"BestBookId": %d,
		"DefaultCoverEditionId": %d,
		"DefaultEbookEditionId": %d,
		"DefaultAudioEditionId": %d,
		"DefaultPhysicalEditionId": %d
	}`, coverID, coverID, ebookID, audioID, physicalID), selectDefaultEditionJSON(t, payload))
}

func TestHardcoverDefaultsRejectWrongWorkAndAuthor(t *testing.T) {
	const (
		workID   = int64(100)
		authorID = int64(200)
	)

	defaults := testHardcoverDefaults(workID, authorID, 301, 302, 303, 304, 305)
	defaults.Default_cover_edition.Book_id = 999
	defaults.Default_ebook_edition.Contributions[0].Author.Id = 999

	ids := validatedHardcoverDefaults(defaults, authorID)
	assert.Zero(t, ids.cover)
	assert.Zero(t, ids.ebook)
	assert.Equal(t, int64(303), ids.audio)
	assert.Equal(t, int64(304), ids.physical)
	assert.Equal(t, int64(305), ids.fallback)
	assert.Equal(t, int64(303), ids.legacyBestID())
}

func TestValidatedHardcoverEditionAcceptsExpectedAuthorRegardlessContributionOrder(t *testing.T) {
	const (
		workID        = int64(100)
		editionID     = int64(301)
		expectedID    = int64(200)
		otherAuthorID = int64(201)
	)
	authorContribution := func(authorID int64) hardcover.Contributions {
		return hardcover.Contributions{
			Contribution: "author",
			Author: hardcover.ContributionsAuthorAuthors{
				AuthorInfo: hardcover.AuthorInfo{Id: authorID},
			},
		}
	}
	expected := authorContribution(expectedID)
	other := authorContribution(otherAuthorID)

	for name, contributions := range map[string][]hardcover.Contributions{
		"expected first": {expected, other},
		"expected last":  {other, expected},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, editionID, validatedHardcoverEdition(
				workID, editionID, workID, expectedID, contributions,
			))
		})
	}

	assert.Zero(t, validatedHardcoverEdition(
		workID, editionID, workID, expectedID, []hardcover.Contributions{other},
	))
	assert.Equal(t, editionID, validatedHardcoverEdition(
		workID, editionID, workID, expectedID, []hardcover.Contributions{{
			Contribution: "narrator",
			Author: hardcover.ContributionsAuthorAuthors{
				AuthorInfo: hardcover.AuthorInfo{Id: otherAuthorID},
			},
		}},
	))
}

func TestHardcoverGetWorkSkipsResolvedCrossWorkDefault(t *testing.T) {
	const (
		workID   = int64(100)
		authorID = int64(200)
		coverID  = int64(301)
		ebookID  = int64(302)
	)

	defaults := testHardcoverDefaults(workID, authorID, coverID, ebookID, 0, 0, 0)
	editionCalls := 0
	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			switch req.OpName {
			case "GetWork":
				data := resp.Data.(*hardcover.GetWorkResponse)
				data.Books_by_pk.WorkInfo = testHardcoverWork(workID, authorID, defaults)
				return nil
			case "GetEdition":
				editionCalls++
				editionID := coverID
				actualWorkID := int64(999)
				actualDefaults := testHardcoverDefaults(actualWorkID, authorID, coverID, 0, 0, 0, 0)
				if editionCalls == 2 {
					editionID = ebookID
					actualWorkID = workID
					actualDefaults = defaults
				}

				data := resp.Data.(*hardcover.GetEditionResponse)
				data.Editions_by_pk = hardcover.GetEditionEditions_by_pkEditions{
					EditionInfo: hardcover.EditionInfo{
						Id:      editionID,
						Book_id: actualWorkID,
						Title:   "Cross-work default",
						Language: hardcover.EditionInfoLanguageLanguages{
							Code3: "eng",
						},
					},
					Book: hardcover.GetEditionEditions_by_pkEditionsBookBooks{
						WorkInfo: testHardcoverWork(actualWorkID, authorID, actualDefaults),
					},
				}
				return nil
			default:
				return fmt.Errorf("unexpected operation %s", req.OpName)
			}
		},
	).Times(3)

	getter, err := NewHardcoverGetter(newMemoryCache(), gql)
	require.NoError(t, err)
	workBytes, gotAuthorID, err := getter.GetWork(t.Context(), workID, nil)
	require.NoError(t, err)
	assert.Equal(t, authorID, gotAuthorID)
	assert.Equal(t, 2, editionCalls)

	var work workResource
	require.NoError(t, json.Unmarshal(workBytes, &work))
	assert.Equal(t, workID, work.ForeignID)
	assert.Zero(t, work.DefaultCoverEditionID)
	assert.Equal(t, ebookID, work.DefaultEbookEditionID)
	assert.Equal(t, ebookID, work.BestBookID)
}

func TestHardcoverGetWorkDoesNotPublishPartialDefaultsAfterTransientFailure(t *testing.T) {
	const (
		workID   = int64(100)
		authorID = int64(200)
	)
	transientErr := errors.New("temporary upstream failure")
	defaults := testHardcoverDefaults(workID, authorID, 301, 302, 0, 0, 0)
	editionCalls := 0
	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			switch req.OpName {
			case "GetWork":
				resp.Data.(*hardcover.GetWorkResponse).Books_by_pk.WorkInfo = testHardcoverWork(workID, authorID, defaults)
				return nil
			case "GetEdition":
				editionCalls++
				return transientErr
			default:
				return fmt.Errorf("unexpected operation %s", req.OpName)
			}
		},
	).Times(2)

	getter, err := NewHardcoverGetter(newMemoryCache(), gql)
	require.NoError(t, err)
	_, _, err = getter.GetWork(t.Context(), workID, nil)
	require.ErrorIs(t, err, transientErr)
	assert.Equal(t, 1, editionCalls, "a transient failure must not fall through to a partial provider snapshot")
}

func TestHardcoverGetWorkOverlaysFreshDefaultsOnCachedDefaultEdition(t *testing.T) {
	const (
		workID      = int64(100)
		authorID    = int64(200)
		coverID     = int64(301)
		staleBookID = int64(999)
	)

	cache := newMemoryCache()
	stale := workResource{
		CacheSchemaVersion:    workCacheSchemaVersion,
		ForeignID:             workID,
		BestBookID:            staleBookID,
		DefaultCoverEditionID: staleBookID,
		DefaultEbookEditionID: staleBookID,
		Authors:               []AuthorResource{{ForeignID: authorID}},
		Books:                 []bookResource{{ForeignID: coverID, Title: "Cached cover"}},
	}
	staleBytes, err := json.Marshal(stale)
	require.NoError(t, err)
	cache.Set(t.Context(), BookKey(coverID), staleBytes, time.Hour)

	defaults := testHardcoverDefaults(workID, authorID, coverID, 0, 0, 0, 0)
	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			require.Equal(t, "GetWork", req.OpName)
			resp.Data.(*hardcover.GetWorkResponse).Books_by_pk.WorkInfo = testHardcoverWork(workID, authorID, defaults)
			return nil
		},
	)

	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)
	workBytes, _, err := getter.GetWork(t.Context(), workID, nil)
	require.NoError(t, err)

	var work workResource
	require.NoError(t, json.Unmarshal(workBytes, &work))
	assert.Equal(t, coverID, work.BestBookID)
	assert.Equal(t, coverID, work.DefaultCoverEditionID)
	assert.Zero(t, work.DefaultEbookEditionID)
	assert.Zero(t, work.DefaultAudioEditionID)
	assert.Zero(t, work.DefaultPhysicalEditionID)
}

func TestHardcoverGetWorkAcceptsSameWorkDefaultsWithMissingEditionContributions(t *testing.T) {
	const (
		workID     = int64(100)
		authorID   = int64(200)
		coverID    = int64(301)
		fallbackID = int64(302)
	)

	cache := newMemoryCache()
	for _, editionID := range []int64{coverID, fallbackID} {
		payload, err := json.Marshal(workResource{
			CacheSchemaVersion: workCacheSchemaVersion,
			ForeignID:          workID,
			Authors:            []AuthorResource{{ForeignID: authorID}},
			Books:              []bookResource{{ForeignID: editionID, Title: "Incomplete contribution edges"}},
		})
		require.NoError(t, err)
		cache.Set(t.Context(), BookKey(editionID), payload, time.Hour)
	}

	defaults := testHardcoverDefaults(workID, authorID, coverID, 0, 0, 0, fallbackID)
	defaults.Default_cover_edition.Contributions = nil
	defaults.Fallback[0].Contributions = nil
	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			require.Equal(t, "GetWork", req.OpName)
			resp.Data.(*hardcover.GetWorkResponse).Books_by_pk.WorkInfo = testHardcoverWork(workID, authorID, defaults)
			return nil
		},
	)

	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)
	workBytes, _, err := getter.GetWork(t.Context(), workID, nil)
	require.NoError(t, err)
	var work workResource
	require.NoError(t, json.Unmarshal(workBytes, &work))
	assert.Equal(t, coverID, work.BestBookID)
	assert.Equal(t, coverID, work.DefaultCoverEditionID)
	assert.Contains(t, hardcoverWorkEditionIDs([]workResource{work}), fallbackID)
}

func TestHardcoverGetWorkRetainsAndPublishesEveryProviderDefault(t *testing.T) {
	const (
		workID     = int64(100)
		authorID   = int64(200)
		coverID    = int64(301)
		ebookID    = int64(302)
		audioID    = int64(303)
		otherEbook = int64(401)
	)

	cache := newMemoryCache()
	coverBytes, err := json.Marshal(workResource{
		CacheSchemaVersion: workCacheSchemaVersion,
		ForeignID:          workID,
		Authors:            []AuthorResource{{ForeignID: authorID}},
		Books:              []bookResource{{ForeignID: coverID, Title: "Cover"}},
	})
	require.NoError(t, err)
	cache.Set(t.Context(), BookKey(coverID), coverBytes, time.Hour)

	defaults := testHardcoverDefaults(workID, authorID, coverID, ebookID, audioID, 0, 0)
	edition := func(id int64, format string) hardcover.GetWorkBooks_by_pkBooksEditions {
		return hardcover.GetWorkBooks_by_pkBooksEditions{EditionInfo: hardcover.EditionInfo{
			Id:             id,
			Book_id:        workID,
			Title:          "Provider Defaults",
			Edition_format: format,
			Language: hardcover.EditionInfoLanguageLanguages{
				Code3: "eng",
			},
		}}
	}

	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			require.Equal(t, "GetWork", req.OpName)
			data := resp.Data.(*hardcover.GetWorkResponse)
			data.Books_by_pk.WorkInfo = testHardcoverWork(workID, authorID, defaults)
			data.Books_by_pk.Editions = []hardcover.GetWorkBooks_by_pkBooksEditions{
				edition(otherEbook, "ebook"), // Same class/title/language as the provider default.
				edition(ebookID, "ebook"),
				edition(coverID, "Hardcover"),
				edition(audioID, "Audiobook"),
			}
			return nil
		},
	)

	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)
	var saved []workResource
	workBytes, _, err := getter.GetWork(t.Context(), workID, func(editions ...workResource) {
		saved = append(saved, editions...)
	})
	require.NoError(t, err)

	var work workResource
	require.NoError(t, json.Unmarshal(workBytes, &work))
	assert.Equal(t, coverID, work.DefaultCoverEditionID)
	assert.Equal(t, ebookID, work.DefaultEbookEditionID)
	assert.Equal(t, audioID, work.DefaultAudioEditionID)
	assert.Equal(t, []int64{coverID, ebookID, audioID, otherEbook}, work.ProviderEditionIDs)
	assert.Subset(t, hardcoverWorkEditionIDs([]workResource{work}), map[int64]struct{}{
		coverID: {}, ebookID: {}, audioID: {},
	})
	assert.Subset(t, hardcoverWorkEditionIDs(saved), map[int64]struct{}{
		coverID: {}, ebookID: {}, audioID: {},
	})
}

func TestHardcoverGetWorkFailsFastWhenMissingDefaultHydrationIsRateLimited(t *testing.T) {
	const (
		workID   = int64(100)
		authorID = int64(200)
		coverID  = int64(301)
		audioID  = int64(303)
	)

	cache := newMemoryCache()
	coverBytes, err := json.Marshal(workResource{
		CacheSchemaVersion: workCacheSchemaVersion,
		ForeignID:          workID,
		Authors:            []AuthorResource{{ForeignID: authorID}},
		Books:              []bookResource{{ForeignID: coverID, Title: "Cover"}},
	})
	require.NoError(t, err)
	cache.Set(t.Context(), BookKey(coverID), coverBytes, time.Hour)

	defaults := testHardcoverDefaults(workID, authorID, coverID, 0, audioID, 0, 0)
	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			switch req.OpName {
			case "GetWork":
				data := resp.Data.(*hardcover.GetWorkResponse)
				data.Books_by_pk.WorkInfo = testHardcoverWork(workID, authorID, defaults)
				data.Books_by_pk.Editions = []hardcover.GetWorkBooks_by_pkBooksEditions{{
					EditionInfo: hardcover.EditionInfo{
						Id:             coverID,
						Book_id:        workID,
						Title:          "Cover",
						Edition_format: "Hardcover",
						Language: hardcover.EditionInfoLanguageLanguages{
							Code3: "eng",
						},
					},
				}}
				return nil
			case "GetEdition":
				return statusErr(http.StatusTooManyRequests)
			default:
				return fmt.Errorf("unexpected operation %s", req.OpName)
			}
		},
	).Times(2)

	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)
	callbackCalled := false
	_, _, err = getter.GetWork(t.Context(), workID, func(...workResource) { callbackCalled = true })
	require.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
	assert.False(t, callbackCalled, "a partial provider-default snapshot must never be published")
}

func TestHardcoverGetBookRejectsInconsistentWorkIdentity(t *testing.T) {
	const (
		editionID = int64(301)
		workID    = int64(100)
		authorID  = int64(200)
	)

	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			require.Equal(t, "GetEdition", req.OpName)
			data := resp.Data.(*hardcover.GetEditionResponse)
			data.Editions_by_pk = hardcover.GetEditionEditions_by_pkEditions{
				EditionInfo: hardcover.EditionInfo{Id: editionID, Book_id: 999},
				Book: hardcover.GetEditionEditions_by_pkEditionsBookBooks{
					WorkInfo: testHardcoverWork(
						workID,
						authorID,
						testHardcoverDefaults(workID, authorID, editionID, 0, 0, 0, 0),
					),
				},
			}
			return nil
		},
	)

	getter, err := NewHardcoverGetter(newMemoryCache(), gql)
	require.NoError(t, err)
	_, _, _, err = getter.GetBook(t.Context(), editionID, nil)
	require.ErrorIs(t, err, errNotFound)
}

func TestSelectHardcoverEditionsKeepsOrdinaryAndRevisedRepresentatives(t *testing.T) {
	const (
		workID   = int64(100)
		authorID = int64(200)
	)
	defaults := testHardcoverDefaults(workID, authorID, 301, 0, 0, 0, 0)
	work := testHardcoverWork(workID, authorID, defaults)
	edition := func(id int64, information string) hardcover.GetWorkBooks_by_pkBooksEditions {
		return hardcover.GetWorkBooks_by_pkBooksEditions{EditionInfo: hardcover.EditionInfo{
			Id:                  id,
			Book_id:             workID,
			Title:               "Same title",
			Edition_format:      "Hardcover",
			Edition_information: information,
			Language: hardcover.EditionInfoLanguageLanguages{
				Code3: "eng",
			},
		}}
	}

	selected := selectHardcoverEditions(t.Context(), []hardcover.GetWorkBooks_by_pkBooksEditions{
		edition(401, "Revised edition"), // Higher provider score/order.
		edition(402, ""),
		edition(403, ""), // Same ordinary class: bounded away.
	}, work, nil)

	require.Len(t, selected, 2)
	assert.Equal(t, int64(401), selected[0].Books[0].ForeignID)
	assert.Equal(t, int64(402), selected[1].Books[0].ForeignID)
}

func TestHardcoverMediaTypeContractUsesCaseInsensitiveFormatClassification(t *testing.T) {
	const (
		workID    = int64(100)
		authorID  = int64(200)
		editionID = int64(301)
	)
	defaults := testHardcoverDefaults(workID, authorID, editionID, 0, 0, 0, 0)
	work := testHardcoverWork(workID, authorID, defaults)

	tests := []struct {
		format    string
		mediaType int
	}{
		{format: "ebook", mediaType: mediaTypeEbook},
		{format: "E-Book", mediaType: mediaTypeEbook},
		{format: "Kindle Edition", mediaType: mediaTypeEbook},
		{format: "Digital Edition", mediaType: mediaTypeEbook},
		{format: "EPUB", mediaType: mediaTypeEbook},
		{format: "MOBI", mediaType: mediaTypeEbook},
		{format: "PDF", mediaType: mediaTypeEbook},
		{format: "Audiobook", mediaType: mediaTypeAudiobook},
		{format: "Audible Audio", mediaType: mediaTypeAudiobook},
		{format: "Audio CD", mediaType: mediaTypeAudiobook},
		{format: "MP3 CD", mediaType: mediaTypeAudiobook},
		{format: "Cassette", mediaType: mediaTypeAudiobook},
		{format: "Playaway", mediaType: mediaTypeAudiobook},
		{format: "Hardcover", mediaType: mediaTypeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			mapped, err := mapHardcoverToWorkResource(t.Context(), hardcover.EditionInfo{
				Id:             editionID,
				Book_id:        workID,
				Title:          "Media type",
				Edition_format: tt.format,
				Language: hardcover.EditionInfoLanguageLanguages{
					Code3: "eng",
				},
			}, work)
			require.NoError(t, err)
			require.Len(t, mapped.Books, 1)
			assert.Equal(t, tt.mediaType, mapped.Books[0].MediaType)
			assert.Equal(t, tt.mediaType == mediaTypeEbook, mapped.Books[0].IsEbook)

			payload, err := json.Marshal(mapped.Books[0])
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(payload, &wire))
			assert.Equal(t, float64(tt.mediaType), wire["MediaType"])
		})
	}
}

func testHardcoverWork(workID, authorID int64, defaults hardcover.DefaultEditions) hardcover.WorkInfo {
	defaults.Id = workID
	return hardcover.WorkInfo{
		Id:              workID,
		Title:           "Provider Defaults",
		Description:     "Test work",
		Slug:            "provider-defaults",
		DefaultEditions: defaults,
	}
}

func testHardcoverDefaults(workID, authorID, coverID, ebookID, audioID, physicalID, fallbackID int64) hardcover.DefaultEditions {
	author := hardcover.ContributionsAuthorAuthors{AuthorInfo: hardcover.AuthorInfo{Id: authorID}}
	contribution := hardcover.Contributions{Author: author}
	defaults := hardcover.DefaultEditions{
		Id: workID,
		Contributions: []hardcover.DefaultEditionsContributions{{
			Contributions: contribution,
		}},
	}
	if coverID != 0 {
		defaults.Default_cover_edition = hardcover.DefaultEditionsDefault_cover_editionEditions{
			Id:      coverID,
			Book_id: workID,
			Contributions: []hardcover.DefaultEditionsDefault_cover_editionEditionsContributions{{
				Contributions: contribution,
			}},
		}
	}
	if ebookID != 0 {
		defaults.Default_ebook_edition = hardcover.DefaultEditionsDefault_ebook_editionEditions{
			Id:      ebookID,
			Book_id: workID,
			Contributions: []hardcover.DefaultEditionsDefault_ebook_editionEditionsContributions{{
				Contributions: contribution,
			}},
		}
	}
	if audioID != 0 {
		defaults.Default_audio_edition = hardcover.DefaultEditionsDefault_audio_editionEditions{
			Id:      audioID,
			Book_id: workID,
			Contributions: []hardcover.DefaultEditionsDefault_audio_editionEditionsContributions{{
				Contributions: contribution,
			}},
		}
	}
	if physicalID != 0 {
		defaults.Default_physical_edition = hardcover.DefaultEditionsDefault_physical_editionEditions{
			Id:      physicalID,
			Book_id: workID,
			Contributions: []hardcover.DefaultEditionsDefault_physical_editionEditionsContributions{{
				Contributions: contribution,
			}},
		}
	}
	if fallbackID != 0 {
		defaults.Fallback = []hardcover.DefaultEditionsFallbackEditions{{
			Id:      fallbackID,
			Book_id: workID,
			Contributions: []hardcover.DefaultEditionsFallbackEditionsContributions{{
				Contributions: contribution,
			}},
		}}
	}
	return defaults
}

func selectDefaultEditionJSON(t *testing.T, payload []byte) string {
	t.Helper()
	var resource map[string]any
	require.NoError(t, json.Unmarshal(payload, &resource))
	selected := map[string]any{}
	for _, field := range []string{
		"BestBookId",
		"DefaultCoverEditionId",
		"DefaultEbookEditionId",
		"DefaultAudioEditionId",
		"DefaultPhysicalEditionId",
	} {
		selected[field] = resource[field]
	}
	out, err := json.Marshal(selected)
	require.NoError(t, err)
	return string(out)
}

func TestHardcoverSearchLimitsHydrationAndPreservesOrder(t *testing.T) {
	cache := newMemoryCache()
	ctx := t.Context()
	ids := []int64{10, 20, 30, 40, 50, 60}
	for _, id := range ids {
		bytes, err := json.Marshal(workResource{
			CacheSchemaVersion: workCacheSchemaVersion,
			ForeignID:          id,
			BestBookID:         id + 1000,
			Authors:            []AuthorResource{{ForeignID: id + 2000}},
		})
		require.NoError(t, err)
		cache.Set(ctx, WorkKey(id), bytes, time.Hour)
	}

	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			require.Equal(t, "Search", req.OpName)
			data := resp.Data.(*hardcover.SearchResponse)
			data.Search.Ids = ids
			return nil
		},
	)
	getter, err := NewConfiguredHardcoverGetter(cache, gql, 5)
	require.NoError(t, err)

	results, err := getter.Search(ctx, "ordered results")
	require.NoError(t, err)
	require.Len(t, results, 5)
	for index, result := range results {
		assert.Equal(t, ids[index], result.WorkID)
	}
}

func TestHardcoverSearchPropagatesHydrationRateLimit(t *testing.T) {
	gql := hardcover.NewMockgql(gomock.NewController(t))
	gql.EXPECT().MakeRequest(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, req *graphql.Request, resp *graphql.Response) error {
			switch req.OpName {
			case "Search":
				data := resp.Data.(*hardcover.SearchResponse)
				data.Search.Ids = []int64{10, 20}
				return nil
			case "GetWork":
				return &RateLimitError{Until: time.Now().Add(time.Hour)}
			default:
				return fmt.Errorf("unexpected operation %s", req.OpName)
			}
		},
	).Times(2)
	getter, err := NewConfiguredHardcoverGetter(newMemoryCache(), gql, 5)
	require.NoError(t, err)

	results, err := getter.Search(t.Context(), "limited")
	require.Error(t, err)
	assert.Nil(t, results)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
}

func TestGetBookDataIntegrity(t *testing.T) {
	// The client is particularly sensitive to null values.
	// For a given work resource, it MUST
	// - have non-null top-level books
	// - non-null ratingcount, averagerating
	// - have a contributor with a foreign id

	t.Parallel()

	ctx := context.Background()
	c := gomock.NewController(t)

	gql := hardcover.NewMockgql(c)
	gql.EXPECT().MakeRequest(gomock.Any(),
		gomock.AssignableToTypeOf(&graphql.Request{}),
		gomock.AssignableToTypeOf(&graphql.Response{})).DoAndReturn(
		func(ctx context.Context, req *graphql.Request, res *graphql.Response) error {
			if req.OpName == "GetWork" {
				gwr, ok := res.Data.(*hardcover.GetWorkResponse)
				if !ok {
					panic(gwr)
				}
				gwr.Books_by_pk.Editions = []hardcover.GetWorkBooks_by_pkBooksEditions{{
					EditionInfo: hardcover.EditionInfo{
						Id:             30405274,
						Title:          "Out of My Mind",
						Asin:           "",
						Isbn_13:        "9781416971702",
						Edition_format: "Hardcover",
						Pages:          295,
						Audio_seconds:  0,
						Language: hardcover.EditionInfoLanguageLanguages{
							Code3: "eng",
						},
						Publisher: hardcover.EditionInfoPublisherPublishers{
							Name: "Atheneum",
						},
						Release_date: "2010-01-01",
						Book_id:      141397,
					},
				}}
				gwr.Books_by_pk.WorkInfo = hardcover.WorkInfo{
					Id:           141397,
					Title:        "Out of My Mind",
					Description:  "foo",
					Release_date: "2010-01-01",
					Cached_tags: json.RawMessage(`[
							{
							  "tag": "Fiction",
							  "tagSlug": "fiction",
							  "category": "Genre",
							  "categorySlug": "genre",
							  "spoilerRatio": 0,
							  "count": 29758
							},
							{
							  "tag": "Young Adult",
							  "tagSlug": "young-adult",
							  "category": "Genre",
							  "categorySlug": "genre",
							  "spoilerRatio": 0,
							  "count": 22645
							},
							{
							  "tag": "Juvenile Fiction",
							  "tagSlug": "juvenile-fiction",
							  "category": "Genre",
							  "categorySlug": "genre",
							  "spoilerRatio": 0,
							  "count": 3661
							},
							{
							  "tag": "Juvenile Nonfiction",
							  "tagSlug": "juvenile-nonfiction-6a8774e3-9173-46e1-87d7-ea5fa5eb20e8",
							  "category": "Genre",
							  "categorySlug": "genre",
							  "spoilerRatio": 0,
							  "count": 1561
							},
							{
							  "tag": "Family",
							  "tagSlug": "family",
							  "category": "Genre",
							  "categorySlug": "genre",
							  "spoilerRatio": 0,
							  "count": 847
							}
						  ]`),
					Cached_image: json.RawMessage("https://assets.hardcover.app/edition/30405274/d41534ce6075b53289d1c4d57a6dac34b974ce91.jpeg"),
					DefaultEditions: hardcover.DefaultEditions{
						Id: 141397,
						Contributions: []hardcover.DefaultEditionsContributions{
							{
								Contributions: hardcover.Contributions{
									Author: hardcover.ContributionsAuthorAuthors{
										AuthorInfo: hardcover.AuthorInfo{
											Id:           51942,
											Name:         "Sharon M. Draper",
											Slug:         "sharon-m-draper",
											Cached_image: json.RawMessage("https://assets.hardcover.app/books/97020/10748148-L.jpg"),
										},
									},
								},
							},
						},
						Default_cover_edition: hardcover.DefaultEditionsDefault_cover_editionEditions{
							Id:      30405274,
							Book_id: 141397,
							Contributions: []hardcover.DefaultEditionsDefault_cover_editionEditionsContributions{
								{
									Contributions: hardcover.Contributions{
										Author: hardcover.ContributionsAuthorAuthors{
											AuthorInfo: hardcover.AuthorInfo{
												Id: 51942,
											},
										},
									},
								},
							},
						},
					},
					Slug: "out-of-my-mind",
					Book_series: []hardcover.WorkInfoBook_series{
						{
							Position: 1,
							Series: hardcover.WorkInfoBook_seriesSeries{
								Id:   141397,
								Name: "Out of My Mind",
							},
						},
					},
					Rating:        4.111111111111111,
					Ratings_count: 63,
				}

				return nil

			}

			if req.OpName == "GetEdition" {
				ge, ok := res.Data.(*hardcover.GetEditionResponse)
				if !ok {
					panic(ge)
				}
				ge.Editions_by_pk = hardcover.GetEditionEditions_by_pkEditions{
					EditionInfo: hardcover.EditionInfo{
						Id:      30405274,
						Book_id: 141397,
					},
					Book: hardcover.GetEditionEditions_by_pkEditionsBookBooks{
						WorkInfo: hardcover.WorkInfo{
							Id: 141397,
							DefaultEditions: hardcover.DefaultEditions{
								Id: 141397,
								Contributions: []hardcover.DefaultEditionsContributions{
									{
										Contributions: hardcover.Contributions{
											Author: hardcover.ContributionsAuthorAuthors{
												AuthorInfo: hardcover.AuthorInfo{
													Id: 51942,
												},
											},
										},
									},
								},
								Default_cover_edition: hardcover.DefaultEditionsDefault_cover_editionEditions{
									Id:      30405274,
									Book_id: 141397,
									Contributions: []hardcover.DefaultEditionsDefault_cover_editionEditionsContributions{
										{
											Contributions: hardcover.Contributions{
												Author: hardcover.ContributionsAuthorAuthors{
													AuthorInfo: hardcover.AuthorInfo{
														Id: 51942,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}

				return nil
			}
			if req.OpName == "GetAuthorEditions" {
				gaw, ok := res.Data.(*hardcover.GetAuthorEditionsResponse)
				if !ok {
					panic(gaw)
				}
				gaw.Authors_by_pk = hardcover.GetAuthorEditionsAuthors_by_pkAuthors{
					AuthorInfo: hardcover.AuthorInfo{
						Id:   51942,
						Slug: "sharon-m-draper",
					},
					Contributions: []hardcover.GetAuthorEditionsAuthors_by_pkAuthorsContributions{
						{
							Contributions: hardcover.Contributions{
								Author: hardcover.ContributionsAuthorAuthors{
									AuthorInfo: hardcover.AuthorInfo{
										Id: 51942,
									},
								},
								Contribution: "",
							},
							Book: hardcover.GetAuthorEditionsAuthors_by_pkAuthorsContributionsBookBooks{
								Id: 141397,
								DefaultEditions: hardcover.DefaultEditions{
									Id: 141397,
									Contributions: []hardcover.DefaultEditionsContributions{
										{
											Contributions: hardcover.Contributions{
												Author: hardcover.ContributionsAuthorAuthors{
													AuthorInfo: hardcover.AuthorInfo{
														Id: 51942,
													},
												},
											},
										},
									},
									Default_cover_edition: hardcover.DefaultEditionsDefault_cover_editionEditions{
										Id:      30405274,
										Book_id: 141397,
										Contributions: []hardcover.DefaultEditionsDefault_cover_editionEditionsContributions{
											{
												Contributions: hardcover.Contributions{
													Author: hardcover.ContributionsAuthorAuthors{
														AuthorInfo: hardcover.AuthorInfo{
															Id: 51942,
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}

				return nil
			}

			return fmt.Errorf("unrecognized op %q", req.OpName)
		}).AnyTimes()

	cache := newMemoryCache()
	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)

	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)

	go ctrl.Run(t.Context()) // Denormalize data in the background.
	t.Cleanup(func() { ctrl.Shutdown(t.Context()) })

	t.Run("GetBook", func(t *testing.T) {
		bookBytes, ttl, err := ctrl.GetBook(ctx, 30405274)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		require.NoError(t, json.Unmarshal(bookBytes, &work))

		assert.Equal(t, int64(141397), work.ForeignID)
		require.Len(t, work.Authors, 1)
		require.Len(t, work.Authors[0].Works, 1)
		assert.Equal(t, int64(51942), work.Authors[0].ForeignID)

		require.Len(t, work.Books, 1)
		assert.Equal(t, int64(30405274), work.Books[0].ForeignID)
	})

	waitForDenorm(ctrl)

	t.Run("GetAuthor", func(t *testing.T) {
		authorBytes, ttl, err := ctrl.GetAuthor(ctx, 51942)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		// author -> .Works.Authors.Works must not be null, but books can be

		var author AuthorResource
		require.NoError(t, json.Unmarshal(authorBytes, &author))

		assert.Equal(t, int64(51942), author.ForeignID)
		require.Len(t, author.Works, 1)
		require.Len(t, author.Works[0].Authors, 1)
		require.Len(t, author.Works[0].Books, 1)
	})

	t.Run("GetWork", func(t *testing.T) {
		workBytes, ttl, err := ctrl.GetWork(ctx, 141397)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		require.NoError(t, json.Unmarshal(workBytes, &work))

		require.Len(t, work.Authors, 1)
		assert.Equal(t, int64(51942), work.Authors[0].ForeignID)
		require.Len(t, work.Authors[0].Works, 1)

		require.Len(t, work.Books, 1)
		assert.Equal(t, int64(30405274), work.Books[0].ForeignID)
	})
}

func TestHardcoverIntegration(t *testing.T) {
	if os.Getenv("RUN_HARDCOVER_INTEGRATION") != "1" {
		t.Skip("set RUN_HARDCOVER_INTEGRATION=1 to run live Hardcover tests")
	}

	key := os.Getenv("HARDCOVER_API_KEY")
	if key == "" {
		t.Skip("missing HARDCOVER_API_KEY env var")
		return
	}

	cache := newMemoryCache()

	hcTransport := ScopedTransport{
		Host: "api.hardcover.app",
		RoundTripper: &HeaderTransport{
			Key:          "Authorization",
			Value:        "Bearer " + key,
			RoundTripper: http.DefaultTransport,
		},
	}

	hcClient := &http.Client{Transport: hcTransport}

	gql, err := NewBatchedGraphQLClient("https://api.hardcover.app/v1/graphql", hcClient, 2*time.Second, 1, nil)
	require.NoError(t, err)

	getter, err := NewHardcoverGetter(cache, gql)
	require.NoError(t, err)

	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)
	go ctrl.Run(t.Context())
	t.Cleanup(func() { ctrl.Shutdown(context.Background()) })

	t.Run("GetAuthor", func(t *testing.T) {
		t.Parallel()
		authorBytes, ttl, err := ctrl.GetAuthor(t.Context(), 91460)
		require.NoError(t, err)
		assert.NotZero(t, ttl)

		var author AuthorResource
		err = json.Unmarshal(authorBytes, &author)
		assert.NoError(t, err)

		assert.Equal(t, int64(91460), author.ForeignID)
		assert.Equal(t, "https://hardcover.app/authors/cormac-mccarthy", author.URL)
		assert.NotEmpty(t, author.Works)
	})

	t.Run("GetBook", func(t *testing.T) {
		t.Parallel()
		bookBytes, ttl, err := ctrl.GetBook(t.Context(), 642392)
		assert.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		err = json.Unmarshal(bookBytes, &work)
		assert.NoError(t, err)

		assert.Equal(t, int64(642392), work.Books[0].ForeignID)
		assert.Equal(t, int64(36087), work.ForeignID)
		assert.Equal(t, int64(91460), work.Authors[0].ForeignID)
		assert.NotEqual(t, "", work.ReleaseDate)
		assert.NotEqual(t, "", work.Books[0].ReleaseDate)
	})

	t.Run("GetWork", func(t *testing.T) {
		t.Parallel()
		workBytes, ttl, err := ctrl.GetWork(t.Context(), 36087)
		assert.NoError(t, err)
		assert.NotZero(t, ttl)

		var work workResource
		err = json.Unmarshal(workBytes, &work)
		assert.NoError(t, err)

		assert.NotEqual(t, "", work.ReleaseDate)
		assert.Equal(t, int64(36087), work.ForeignID)
		assert.Equal(t, int64(91460), work.Authors[0].ForeignID)
	})

	t.Run("GetAuthorBooks", func(t *testing.T) {
		t.Parallel()
		iter := getter.GetAuthorBooks(t.Context(), 91460)
		gotBook := false
		for workID := range iter {
			if workID == 30713111 {
				gotBook = true
			}
		}
		assert.True(t, gotBook)
	})

	t.Run("GetOldBook", func(t *testing.T) {
		t.Parallel()

		// bagavadgita
		editionBytes, _, err := ctrl.GetBook(t.Context(), 32049008)
		require.NoError(t, err)

		var edition workResource
		require.NoError(t, json.Unmarshal(editionBytes, &edition))

		assert.Equal(t, "0001-01-01", edition.ReleaseDate)
		assert.Equal(t, "0001-01-01", edition.Books[0].ReleaseDate)
	})

	t.Run("Pending", func(t *testing.T) {
		t.Skip("TODO: no longer pending, need to find a new ID")
		_, _, err := ctrl.GetWork(t.Context(), 885684)
		assert.ErrorContains(t, err, "pending")
	})

	t.Run("Duplicate", func(t *testing.T) {
		workBytes, _, err := ctrl.GetWork(t.Context(), 2272705)
		require.NoError(t, err)
		var work workResource
		err = json.Unmarshal(workBytes, &work)
		assert.NoError(t, err)
		assert.Equal(t, int64(42), work.ForeignID)
	})

	t.Run("Search (query)", func(t *testing.T) {
		t.Parallel()
		results, err := ctrl.Search(t.Context(), "the crossing")
		require.NoError(t, err)

		expected := SearchResource{
			BookID: 30713122,
			WorkID: 369140,
			Author: SearchResourceAuthor{
				ID: 91460,
			},
		}
		assert.Contains(t, results, expected)
	})

	t.Run("Search (isbn)", func(t *testing.T) {
		t.Parallel()
		results, err := getter.Search(t.Context(), "9780307762467")
		require.NoError(t, err)

		expected := SearchResource{
			BookID: 30713122,
			WorkID: 369140,
			Author: SearchResourceAuthor{
				ID: 91460,
			},
		}
		assert.Contains(t, results, expected)
	})

	t.Run("Search (asin)", func(t *testing.T) {
		t.Parallel()
		results, err := getter.Search(t.Context(), "B0192CTMYG")
		require.NoError(t, err)

		expected := SearchResource{
			BookID: 14969655,
			WorkID: 328491,
			Author: SearchResourceAuthor{
				ID: 80626,
			},
		}
		assert.Contains(t, results, expected)
	})

	t.Run("Series (unnumbered)", func(t *testing.T) {
		t.Parallel()
		series, err := getter.GetSeries(t.Context(), 8781)
		require.NoError(t, err)

		assert.Greater(t, len(series.LinkItems), 1000)
		assert.Equal(t, "Warhammer 40,000", series.Title)
	})

	t.Run("Series (numbered)", func(t *testing.T) {
		t.Parallel()
		series, err := getter.GetSeries(t.Context(), 40337)
		require.NoError(t, err)

		assert.Equal(t, len(series.LinkItems), 16)
	})

	t.Run("Recommended", func(t *testing.T) {
		t.Parallel()
		recommended, err := getter.Recommendations(t.Context(), 1)
		require.NoError(t, err)
		assert.NotEmpty(t, recommended.WorkIDs)
	})
}

func TestBestAuthor(t *testing.T) {
	tests := []struct {
		name    string
		given   []hardcover.Contributions
		want    int64
		wantErr error
	}{
		{
			name: "ignore non-authors",
			given: []hardcover.Contributions{
				{
					Contribution: "Illustration",
					Author: hardcover.ContributionsAuthorAuthors{
						AuthorInfo: hardcover.AuthorInfo{
							Id: 1,
						},
					},
				},
				{
					Contribution: "",
					Author: hardcover.ContributionsAuthorAuthors{
						AuthorInfo: hardcover.AuthorInfo{
							Id: 2,
						},
					},
				},
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := bestAuthor(tt.given)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			assert.Equal(t, tt.want, actual.Id)
		})
	}
}

func TestHCReleaseDate(t *testing.T) {
	tests := []struct {
		given string
		want  string
	}{
		{
			given: "400-01-01 BC",
			want:  "0001-01-01",
		},
		{
			given: "2005-10-15",
			want:  "2005-10-15",
		},
		{
			given: "42020-08-04",
			want:  "", // Ignore
		},
		{
			given: "abcd",
			want:  "", // Ignore
		},
	}
	for _, tt := range tests {
		t.Run(tt.given, func(t *testing.T) {
			got := hcReleaseDate(tt.given)
			assert.Equal(t, tt.want, got)
		})
	}
}
