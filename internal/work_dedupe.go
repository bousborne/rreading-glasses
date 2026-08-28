package internal

import (
	"cmp"
	"maps"
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
	mergeProviderDefaultEditions(&canonical, duplicate)
	membership := make(map[int64]struct{}, len(canonical.ProviderEditionIDs)+len(duplicate.ProviderEditionIDs))
	for _, editionID := range canonical.ProviderEditionIDs {
		if editionID != 0 {
			membership[editionID] = struct{}{}
		}
	}
	for _, editionID := range duplicate.ProviderEditionIDs {
		if editionID != 0 {
			membership[editionID] = struct{}{}
		}
	}
	canonical.ProviderEditionIDs = slices.Collect(maps.Keys(membership))
	slices.Sort(canonical.ProviderEditionIDs)
	stabilizeLegacyBestBookID(&canonical)

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

// reconcileRefreshedWork publishes a fresh provider snapshot while retaining
// only stale editions whose IDs the caller has independently validated as
// current members of the same work. Provider membership and metadata,
// including explicitly cleared defaults, are otherwise authoritative. This
// prevents removed or reassigned editions from becoming immortal cache data.
func reconcileRefreshedWork(fresh, stale workResource, validatedStaleBookIDs map[int64]struct{}) workResource {
	if fresh.ForeignID == 0 || stale.ForeignID == 0 || fresh.ForeignID != stale.ForeignID {
		stabilizeLegacyBestBookID(&fresh)
		return fresh
	}

	books := make(map[int64]bookResource, len(stale.Books)+len(fresh.Books))
	for _, book := range stale.Books {
		if _, validated := validatedStaleBookIDs[book.ForeignID]; validated && book.ForeignID != 0 {
			books[book.ForeignID] = book
		}
	}
	for _, book := range fresh.Books {
		if book.ForeignID != 0 {
			books[book.ForeignID] = book
		}
	}
	fresh.Books = slices.Collect(maps.Values(books))
	slices.SortFunc(fresh.Books, func(a, b bookResource) int {
		return cmp.Compare(a.ForeignID, b.ForeignID)
	})

	// Do not restore the stale legacy selection. If the provider removed all
	// defaults, stabilizeLegacyBestBookID deterministically chooses from the
	// reconciled edition set instead of pinning an obsolete cached preference.
	stabilizeLegacyBestBookID(&fresh)
	return fresh
}

func mergeProviderDefaultEditions(target *workResource, source workResource) {
	if target.DefaultCoverEditionID == 0 {
		target.DefaultCoverEditionID = source.DefaultCoverEditionID
	}
	if target.DefaultEbookEditionID == 0 {
		target.DefaultEbookEditionID = source.DefaultEbookEditionID
	}
	if target.DefaultAudioEditionID == 0 {
		target.DefaultAudioEditionID = source.DefaultAudioEditionID
	}
	if target.DefaultPhysicalEditionID == 0 {
		target.DefaultPhysicalEditionID = source.DefaultPhysicalEditionID
	}
}

func providerLegacyBestBookID(work workResource) int64 {
	for _, id := range []int64{
		work.DefaultCoverEditionID,
		work.DefaultEbookEditionID,
		work.DefaultAudioEditionID,
		work.DefaultPhysicalEditionID,
	} {
		if id != 0 {
			return id
		}
	}
	return 0
}

// stabilizeLegacyBestBookID keeps BestBookId as a compatibility projection of
// Hardcover's provider defaults. Edition enrichment must not change that
// projection. Providers without explicit defaults retain an existing valid
// choice and only use local display ranking as a final fallback.
func stabilizeLegacyBestBookID(work *workResource) {
	if providerBest := providerLegacyBestBookID(*work); providerBest != 0 {
		work.BestBookID = providerBest
		return
	}

	if work.BestBookID != 0 {
		for _, book := range work.Books {
			if book.ForeignID == work.BestBookID {
				return
			}
		}
	}

	if preferred := preferredDisplayEdition(work.Books); preferred != nil {
		work.BestBookID = preferred.ForeignID
	}
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
	return book.MediaType == mediaTypeAudiobook || classifyMediaType(book.Format) == mediaTypeAudiobook
}

const (
	mediaTypeUnknown   = 0
	mediaTypeEbook     = 1
	mediaTypeAudiobook = 2
)

// classifyMediaType is the single compatibility classifier for provider
// format strings. Cached legacy resources without MediaType continue to work,
// while new resources emit the explicit numeric contract Bookshelf consumes.
func classifyMediaType(format string) int {
	format = strings.ToLower(strings.TrimSpace(format))
	for _, marker := range []string{
		"audio",
		"audible",
		"cassette",
		"mp3 cd",
		"playaway",
	} {
		if strings.Contains(format, marker) {
			return mediaTypeAudiobook
		}
	}
	for _, marker := range []string{
		"ebook",
		"e-book",
		"kindle",
		"digital",
		"epub",
		"mobi",
		"pdf",
	} {
		if strings.Contains(format, marker) {
			return mediaTypeEbook
		}
	}
	return mediaTypeUnknown
}

func isSpecialEdition(book bookResource) bool {
	description := strings.ToLower(strings.Join([]string{
		book.Title,
		book.EditionInformation,
		book.Format,
		book.Publisher,
	}, " "))

	for _, term := range []string{
		"anniversary edition",
		"box set",
		"boxed set",
		"collector's edition",
		"collection",
		"deluxe edition",
		"illustrated edition",
		"large print",
		"library binding",
		"omnibus",
		"revised edition",
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
