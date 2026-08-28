package internal

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeDuplicateFormatWorks(t *testing.T) {
	physical := testSplitWork(551936, 30510379, "Hardcover", "eng", 23)
	physical.Books[0].Publisher = "Turtleback Books"
	physical.DefaultCoverEditionID = 30510379
	physical.DefaultEbookEditionID = 30510380
	physical.ProviderEditionIDs = []int64{30510379, 30510380}
	audio := testSplitWork(2436030, 32711278, "Audio", "eng", 23)
	audio.DefaultAudioEditionID = 32711278
	audio.ProviderEditionIDs = []int64{32711278}

	works, aliases := mergeDuplicateFormatWorks([]workResource{audio, physical})

	require.Len(t, works, 1)
	assert.Equal(t, int64(551936), works[0].ForeignID)
	assert.Equal(t, int64(30510379), works[0].BestBookID)
	assert.Equal(t, int64(30510379), works[0].DefaultCoverEditionID)
	assert.Equal(t, int64(30510380), works[0].DefaultEbookEditionID)
	assert.Equal(t, int64(32711278), works[0].DefaultAudioEditionID)
	assert.Equal(t, []int64{30510379, 30510380, 32711278}, works[0].ProviderEditionIDs)
	assert.Len(t, works[0].Books, 2)
	assert.Equal(t, int64(551936), works[0].Series[0].LinkItems[0].ForeignWorkID)
	assert.Equal(t, int64(551936), aliases[2436030])
}

func TestStabilizeLegacyBestBookIDPreservesProviderDefaultDuringEnrichment(t *testing.T) {
	work := workResource{
		BestBookID:            50,
		DefaultCoverEditionID: 50,
		Books: []bookResource{
			{ForeignID: 50, Title: "Revised edition", Language: "deu", Format: "Hardcover"},
			{ForeignID: 10, Title: "Original edition", Language: "eng", Format: "ebook", IsEbook: true},
		},
	}

	stabilizeLegacyBestBookID(&work)
	assert.Equal(t, int64(50), work.BestBookID)
}

func TestReconcileRefreshedWorkPreservesEnrichedEditionsAtomically(t *testing.T) {
	stale := workResource{
		ForeignID:                100,
		BestBookID:               10,
		DefaultCoverEditionID:    10,
		DefaultEbookEditionID:    20,
		DefaultAudioEditionID:    30,
		DefaultPhysicalEditionID: 40,
		Books: []bookResource{
			{ForeignID: 10, Title: "Cover"},
			{ForeignID: 20, Title: "Ebook"},
			{ForeignID: 30, Title: "Audiobook"},
		},
	}
	fresh := workResource{
		ForeignID:                100,
		BestBookID:               11,
		DefaultCoverEditionID:    11,
		DefaultAudioEditionID:    31,
		DefaultPhysicalEditionID: 41,
		Books: []bookResource{
			{ForeignID: 11, Title: "New provider cover"},
		},
	}

	got := reconcileRefreshedWork(fresh, stale, map[int64]struct{}{
		10: {}, 20: {}, 30: {},
	})
	assert.Equal(t, int64(11), got.BestBookID)
	assert.Equal(t, int64(11), got.DefaultCoverEditionID)
	assert.Zero(t, got.DefaultEbookEditionID, "a provider-cleared default must not be resurrected from stale cache data")
	assert.Equal(t, int64(31), got.DefaultAudioEditionID)
	assert.Equal(t, int64(41), got.DefaultPhysicalEditionID)
	require.Len(t, got.Books, 4)
	assert.Equal(t, []int64{10, 11, 20, 30}, []int64{
		got.Books[0].ForeignID,
		got.Books[1].ForeignID,
		got.Books[2].ForeignID,
		got.Books[3].ForeignID,
	})
}

func TestReconcileRefreshedWorkNeverMixesDifferentWorkIdentities(t *testing.T) {
	fresh := workResource{
		ForeignID:             100,
		BestBookID:            10,
		DefaultCoverEditionID: 10,
		Books:                 []bookResource{{ForeignID: 10}},
	}
	stale := workResource{
		ForeignID:             999,
		BestBookID:            20,
		DefaultEbookEditionID: 20,
		Books:                 []bookResource{{ForeignID: 20}},
	}

	got := reconcileRefreshedWork(fresh, stale, nil)
	assert.Equal(t, fresh, got)
}

func TestReconcileRefreshedWorkDoesNotRestoreStaleLegacySelection(t *testing.T) {
	stale := workResource{
		ForeignID:  100,
		BestBookID: 20,
		Books: []bookResource{{
			ForeignID:          20,
			Title:              "Revised edition",
			Language:           "deu",
			Format:             "Hardcover",
			EditionInformation: "Revised edition",
		}},
	}
	fresh := workResource{
		ForeignID: 100,
		Books: []bookResource{{
			ForeignID: 10,
			Title:     "Ordinary edition",
			Language:  "eng",
			Format:    "ebook",
			IsEbook:   true,
			MediaType: mediaTypeEbook,
		}},
	}

	got := reconcileRefreshedWork(fresh, stale, nil)
	assert.Equal(t, int64(10), got.BestBookID)
	assert.Zero(t, got.DefaultCoverEditionID)
}

func TestReconcileRefreshedWorkDropsStaleEditionWithoutValidatedProvenance(t *testing.T) {
	fresh := workResource{
		ForeignID:             100,
		BestBookID:            10,
		DefaultCoverEditionID: 10,
		Books:                 []bookResource{{ForeignID: 10, Title: "Current provider edition"}},
	}
	stale := workResource{
		ForeignID:  100,
		BestBookID: 99,
		Books: []bookResource{
			{ForeignID: 10, Title: "Old current edition"},
			{ForeignID: 99, Title: "Removed or reassigned edition"},
		},
	}

	got := reconcileRefreshedWork(fresh, stale, nil)
	assert.Equal(t, int64(10), got.BestBookID)
	require.Len(t, got.Books, 1)
	assert.Equal(t, int64(10), got.Books[0].ForeignID)
}

func TestMergeDuplicateFormatWorksRequiresMatchingSeriesPosition(t *testing.T) {
	physical := testSplitWork(1, 10, "Hardcover", "eng", 23)
	audio := testSplitWork(2, 20, "Audio", "eng", 24)

	works, aliases := mergeDuplicateFormatWorks([]workResource{physical, audio})

	assert.Len(t, works, 2)
	assert.Empty(t, aliases)
}

func TestMergeDuplicateFormatWorksDoesNotGuessForStandaloneBooks(t *testing.T) {
	physical := testSplitWork(1, 10, "Hardcover", "eng", 0)
	physical.Series = nil
	audio := testSplitWork(2, 20, "Audio", "eng", 0)
	audio.Series = nil

	works, aliases := mergeDuplicateFormatWorks([]workResource{physical, audio})

	assert.Len(t, works, 2)
	assert.Empty(t, aliases)
}

func TestMergeDuplicateFormatWorksRequiresEnglishMetadata(t *testing.T) {
	physical := testSplitWork(1, 10, "Hardcover", "deu", 23)
	audio := testSplitWork(2, 20, "Audio", "eng", 23)

	works, aliases := mergeDuplicateFormatWorks([]workResource{physical, audio})

	assert.Len(t, works, 2)
	assert.Empty(t, aliases)
}

func testSplitWork(workID, editionID int64, format, language string, position int) workResource {
	return workResource{
		ForeignID:      workID,
		Title:          "High Time for Heroes",
		ShortTitle:     "High Time for Heroes",
		ReleaseDate:    "2014-01-01",
		ReleaseDateRaw: "2014-01-01",
		BestBookID:     editionID,
		Books: []bookResource{{
			ForeignID: editionID,
			Title:     "High Time for Heroes",
			Language:  language,
			Format:    format,
			Isbn13:    "9780307980519",
		}},
		Series: []SeriesResource{{
			ForeignID: 12155,
			Title:     "Magic Tree House Merlin Missions",
			LinkItems: []seriesWorkLinkResource{{
				ForeignWorkID:    workID,
				PositionInSeries: strconv.Itoa(position),
				SeriesPosition:   position,
			}},
		}},
	}
}
