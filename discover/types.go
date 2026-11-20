// Package discover supplies editorial discovery lists without triggering resource searches.
package discover

import "time"

type Item struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	OriginalTitle  string   `json:"original_title"`
	Year           string   `json:"year"`
	ReleaseDate    string   `json:"release_date,omitempty"`
	PosterURL      string   `json:"poster_url"`
	Overview       string   `json:"overview"`
	Genres         []string `json:"genres"`
	Tags           []string `json:"tags,omitempty"`
	DoubanRating   *float64 `json:"douban_rating"`
	DoubanVotes    *int     `json:"douban_votes"`
	DoubanURL      string   `json:"douban_url"`
	TMDBID         int      `json:"tmdb_id,omitempty"`
	MetadataSource string   `json:"metadata_source"`
	Reason         string   `json:"reason"`
	MediaType      string   `json:"media_type"`
	DetailsCached  bool     `json:"details_cached"`
}

type Response struct {
	Items          []Item             `json:"items"`
	UpdatedAt      time.Time          `json:"updated_at"`
	Stale          bool               `json:"stale"`
	TMDBConfigured bool               `json:"tmdb_configured"`
	Warning        string             `json:"warning,omitempty"`
	Total          int                `json:"total"`
	CatalogTotal   int                `json:"catalog_total"`
	Page           int                `json:"page"`
	PageSize       int                `json:"page_size"`
	HasMore        bool               `json:"has_more"`
	Genres         []string           `json:"genres"`
	Years          []string           `json:"years"`
	Tags           []string           `json:"tags,omitempty"`
	Indexing       bool               `json:"indexing"`
	CatalogSync    *CatalogSyncStatus `json:"catalog_sync,omitempty"`
}

// CatalogSyncStatus distinguishes the complete ID index from downloaded
// metadata. Public errors are fixed codes and never include provider URLs.
type CatalogSyncStatus struct {
	Enabled   bool      `json:"enabled"`
	Stage     string    `json:"stage"`
	Indexed   int       `json:"indexed"`
	Hydrated  int       `json:"hydrated"`
	Total     int       `json:"total,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type cacheEntry struct {
	Items     []Item    `json:"items"`
	UpdatedAt time.Time `json:"updated_at"`
}
