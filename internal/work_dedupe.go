package internal

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// mergeDuplicateFormatWorks conservatively combines the common Hardcover
// error where an audiobook edition was created as a second work. It only
// merges exact English titles from the same year and the same series slot,
// and only when one work is audio-only and the other has no audio editions.
// Ambiguous standalones and two ordinary works are intentionally left alone.
func mergeDuplicateFormatWorks(works []workResource) ([]workResource, map[int64]int64) {
	merged := make([]workResource, 0, len(works))
	aliases := map[int64]int64{}
	used := make([]bool, len(works))

	for i := range works {
		if used[i] {
			continue
		}

		canonical := works[i]
		used[i] = true

		for j := i + 1; j < len(works); j++ {
			if used[j] || !sameFormatSplitWork(canonical, works[j]) {
				continue
			}

			candidate := works[j]
			if audioOnly(canonical) && !audioOnly(candidate) {
				aliases[canonical.ForeignID] = candidate.ForeignID
				canonical = combineWorks(candidate, canonical)
			} else {
				aliases[candidate.ForeignID] = canonical.ForeignID
				canonical = combineWorks(canonical, candidate)
			}

			used[j] = true
		}

		merged = append(merged, canonical)
	}

	slices.SortFunc(merged, func(a, b workResource) int {
		return cmp.Compare(a.ForeignID, b.ForeignID)
	})

	return merged, aliases
}

func sameFormatSplitWork(a, b workResource) bool {
	if normalizedWorkTitle(a) == "" || normalizedWorkTitle(a) != normalizedWorkTitle(b) {
		return false
	}

	if releaseYear(a) == 0 || releaseYear(a) != releaseYear(b) {
		return false
	}

	if !hasEnglishEdition(a) || !hasEnglishEdition(b) || !sharesSeriesSlot(a, b) {
		return false
	}

	return (audioOnly(a) && nonAudioOnly(b)) || (audioOnly(b) && nonAudioOnly(a))
}

func combineWorks(canonical, duplicate workResource) workResource {
	seenBooks := map[int64]bool{}
	for _, book := range canonical.Books {
		seenBooks[book.ForeignID] = true
	}
	for _, book := range duplicate.Books {
		if !seenBooks[book.ForeignID] {
			canonical.Books = append(canonical.Books, book)
			seenBooks[book.ForeignID] = true
		}
	}

	slices.SortFunc(canonical.Books, func(a, b bookResource) int {
		return cmp.Compare(a.ForeignID, b.ForeignID)
	})
	if preferred := preferredDisplayEdition(canonical.Books); preferred != nil {
		canonical.BestBookID = preferred.ForeignID
	}

	for _, series := range duplicate.Series {
		found := false
		for idx := range canonical.Series {
			if canonical.Series[idx].ForeignID == series.ForeignID {
				found = true
				break
			}
		}
		if !found {
			canonical.Series = append(canonical.Series, series)
		}
	}

	for sidx := range canonical.Series {
		for lidx := range canonical.Series[sidx].LinkItems {
			if canonical.Series[sidx].LinkItems[lidx].ForeignWorkID == duplicate.ForeignID {
				canonical.Series[sidx].LinkItems[lidx].ForeignWorkID = canonical.ForeignID
			}
		}
	}

	if duplicate.RatingCount > canonical.RatingCount {
		canonical.RatingCount = duplicate.RatingCount
		canonical.RatingSum = duplicate.RatingSum
		canonical.AverageRating = duplicate.AverageRating
	}

	return canonical
}

func preferredDisplayEdition(books []bookResource) *bookResource {
	if len(books) == 0 {
		return nil
	}

	best := &books[0]
	for idx := 1; idx < len(books); idx++ {
		if displayEditionScore(books[idx]) > displayEditionScore(*best) {
			best = &books[idx]
		}
	}
	return best
}

func displayEditionScore(book bookResource) int {
	score := 0
	if strings.EqualFold(book.Language, "eng") {
		score += 100
	}
	if !isAudioEdition(book) {
		score += 50
	}
	if !isSpecialEdition(book) {
		score += 20
	}
	if book.Isbn13 != "" || book.Asin != "" {
		score += 5
	}
	return score
}

func normalizedWorkTitle(work workResource) string {
	title := work.ShortTitle
	if title == "" {
		title = work.Title
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, title)
}

func releaseYear(work workResource) int {
	date := work.ReleaseDateRaw
	if date == "" {
		date = work.ReleaseDate
	}
	if len(date) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(date[:4])
	return year
}

func hasEnglishEdition(work workResource) bool {
	for _, book := range work.Books {
		if strings.EqualFold(book.Language, "eng") {
			return true
		}
	}
	return false
}

func audioOnly(work workResource) bool {
	if len(work.Books) == 0 {
		return false
	}
	for _, book := range work.Books {
		if !isAudioEdition(book) {
			return false
		}
	}
	return true
}

func nonAudioOnly(work workResource) bool {
	if len(work.Books) == 0 {
		return false
	}
	for _, book := range work.Books {
		if isAudioEdition(book) {
			return false
		}
	}
	return true
}

func isAudioEdition(book bookResource) bool {
	format := strings.ToLower(book.Format)
	return strings.Contains(format, "audio") ||
		strings.Contains(format, "audible") ||
		strings.Contains(format, "cassette") ||
		strings.Contains(format, "mp3 cd")
}

func isSpecialEdition(book bookResource) bool {
	description := strings.ToLower(strings.Join([]string{
		book.Title,
		book.EditionInformation,
		book.Format,
		book.Publisher,
	}, " "))

	for _, term := range []string{
		"box set",
		"boxed set",
		"collection",
		"large print",
		"library binding",
		"omnibus",
		"school & library",
		"turtleback",
	} {
		if strings.Contains(description, term) {
			return true
		}
	}
	return false
}

func sharesSeriesSlot(a, b workResource) bool {
	type slot struct {
		seriesID int64
		position string
	}

	slots := map[slot]bool{}
	for _, series := range a.Series {
		for _, link := range series.LinkItems {
			position := strings.TrimSpace(link.PositionInSeries)
			if position == "" {
				position = strconv.Itoa(link.SeriesPosition)
			}
			if position != "" && position != "0" {
				slots[slot{seriesID: series.ForeignID, position: position}] = true
			}
		}
	}

	for _, series := range b.Series {
		for _, link := range series.LinkItems {
			position := strings.TrimSpace(link.PositionInSeries)
			if position == "" {
				position = strconv.Itoa(link.SeriesPosition)
			}
			if slots[slot{seriesID: series.ForeignID, position: position}] {
				return true
			}
		}
	}

	return false
}
