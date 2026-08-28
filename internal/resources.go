package internal

import "encoding/json"

// workCacheSchemaVersion is embedded in serialized work resources. It lets a
// deployment distinguish pre-upgrade WorkKey/BookKey rows from payloads which
// carry the media/default/membership semantics expected by this build.
const workCacheSchemaVersion = 1

func hasCurrentWorkCacheSchema(payload []byte, expected int) bool {
	// Preserve the existing negative-cache sentinel.
	if len(payload) == 1 && payload[0] == 0 {
		return true
	}
	var marker struct {
		CacheSchemaVersion int `json:"CacheSchemaVersion"`
	}
	return json.Unmarshal(payload, &marker) == nil && marker.CacheSchemaVersion == expected
}

// TODO: These could be generated from the OpenAPI spec.
// https://github.com/Readarr/Readarr/blob/develop/src/Readarr.Api.V1/openapi.json

type bulkBookResource struct {
	Works   []workResource   `json:"Works"`
	Series  []SeriesResource `json:"Series"`
	Authors []AuthorResource `json:"Authors"`
}

type workResource struct {
	CacheSchemaVersion int `json:"CacheSchemaVersion,omitempty"`

	ForeignID      int64    `json:"ForeignId"`
	Title          string   `json:"Title"`      // This is what's ultimately displayed in the app.
	FullTitle      string   `json:"FullTitle"`  // The title + subtitle.
	ShortTitle     string   `json:"ShortTitle"` // Just the title.
	URL            string   `json:"Url"`
	ReleaseDate    string   `json:"ReleaseDate,omitempty"`
	ReleaseDateRaw string   `json:"ReleaseDateRaw,omitempty"` // New for forks to parse themselves.
	Genres         []string `json:"Genres"`
	RelatedWorks   []int    `json:"RelatedWorks"` // ForeignId

	Books   []bookResource   `json:"Books"`
	Series  []SeriesResource `json:"Series"`
	Authors []AuthorResource `json:"Authors"`
	// ProviderEditionIDs is the authoritative current edition membership from
	// providers which can return a complete work-edition relationship. Older
	// clients safely ignore this additive field.
	ProviderEditionIDs []int64 `json:"ProviderEditionIds,omitempty"`

	// New fields
	KCA                      string `json:"KCA"`
	BestBookID               int64  `json:"BestBookId"`
	DefaultCoverEditionID    int64  `json:"DefaultCoverEditionId,omitempty"`
	DefaultEbookEditionID    int64  `json:"DefaultEbookEditionId,omitempty"`
	DefaultAudioEditionID    int64  `json:"DefaultAudioEditionId,omitempty"`
	DefaultPhysicalEditionID int64  `json:"DefaultPhysicalEditionId,omitempty"`

	RatingCount   int64   `json:"RatingCount"`
	AverageRating float64 `json:"AverageRating"`
	RatingSum     int64   `json:"RatingSum"`
}

// AuthorResource collects every edition of every work by an author.
type AuthorResource struct {
	CacheSchemaVersion int `json:"CacheSchemaVersion,omitempty"`

	ForeignID     int64   `json:"ForeignId"`
	Name          string  `json:"Name"`
	Description   string  `json:"Description"`
	ImageURL      string  `json:"ImageUrl"`
	URL           string  `json:"Url"`
	RatingCount   int64   `json:"RatingCount"`
	AverageRating float32 `json:"AverageRating"`

	// Relations.
	Works  []workResource   `json:"Works"`
	Series []SeriesResource `json:"Series"`

	// New fields.
	KCA string `json:"KCA"`
}

type bookResource struct {
	ForeignID          int64   `json:"ForeignId"`
	Asin               string  `json:"Asin"`
	Description        string  `json:"Description"`
	Isbn13             string  `json:"Isbn13,omitempty"`
	Title              string  `json:"Title"`      // This is what's ultimately displayed in the app.
	FullTitle          string  `json:"FullTitle"`  // The title + subtitle.
	ShortTitle         string  `json:"ShortTitle"` // Just the title.
	Language           string  `json:"Language"`
	Format             string  `json:"Format"`
	EditionInformation string  `json:"EditionInformation"`
	Publisher          string  `json:"Publisher"`
	ImageURL           string  `json:"ImageUrl"`
	IsEbook            bool    `json:"IsEbook"`
	MediaType          int     `json:"MediaType"` // 0 unknown/physical, 1 ebook, 2 audiobook.
	NumPages           int64   `json:"NumPages"`
	RatingCount        int64   `json:"RatingCount"`
	AverageRating      float64 `json:"AverageRating"`
	URL                string  `json:"Url"`
	ReleaseDate        string  `json:"ReleaseDate,omitempty"`
	ReleaseDateRaw     string  `json:"ReleaseDateRaw,omitempty"` // New for forks to parse themselves.

	Contributors []contributorResource `json:"Contributors"`

	// New fields
	KCA       string `json:"KCA"`
	RatingSum int64  `json:"RatingSum"`
}

// SeriesResource is a collection of works by one or more authors.
type SeriesResource struct {
	ForeignID   int64  `json:"ForeignId"`
	Title       string `json:"Title"`
	Description string `json:"Description"`

	LinkItems []seriesWorkLinkResource `json:"LinkItems"`

	// New fields
	KCA string `json:"KCA"`
}

type seriesWorkLinkResource struct {
	ForeignWorkID    int64  `json:"ForeignWorkId"`
	PositionInSeries string `json:"PositionInSeries"`
	SeriesPosition   int    `json:"SeriesPosition"`
	Primary          bool   `json:"Primary"`
}

type contributorResource struct {
	ForeignID int64  `json:"ForeignId"`
	Role      string `json:"Role"`
}

// SearchResource represents a single search result.
type SearchResource struct {
	BookID int64                `json:"bookId"`
	WorkID int64                `json:"workId"`
	Author SearchResourceAuthor `json:"author"`
}

// SearchResourceAuthor is a nested field on SearchResource.
type SearchResourceAuthor struct {
	ID int64 `json:"id"`
}

// RecommentationsResource contains recommended work IDs.
type RecommentationsResource struct {
	WorkIDs []int64 `json:"workIds"`
}

// lookupResource is a new resource which maps ASINs and ISBNs to their
// corresponding editions.
type lookupResource struct {
	EditionID int64 `json:"editionId"`
}
