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
	audio := testSplitWork(2436030, 32711278, "Audio", "eng", 23)

	works, aliases := mergeDuplicateFormatWorks([]workResource{audio, physical})

	require.Len(t, works, 1)
	assert.Equal(t, int64(551936), works[0].ForeignID)
	assert.Equal(t, int64(30510379), works[0].BestBookID)
	assert.Len(t, works[0].Books, 2)
	assert.Equal(t, int64(551936), works[0].Series[0].LinkItems[0].ForeignWorkID)
	assert.Equal(t, int64(551936), aliases[2436030])
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
