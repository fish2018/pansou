package discover

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"pansou/util"
)

const catalogMaxItems = 10000
const catalogMaxBytes = 16 << 20
const catalogHardMaxItems = 20000
const catalogHardMaxBytes = 32 << 20

func boundedCatalogLimit(value, fallback, maximum int) int {
	if value <= 0 || value > maximum {
		return fallback
	}
	return value
}

// Source cursors and membership travel with the bounded snapshot so restarts
// resume the next page rather than downloading every source again.
type catalogCursor struct {
	NextStart   int       `json:"next_start"`
	Done        bool      `json:"done"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	IDs         []string  `json:"ids,omitempty"`
	PreviousIDs []string  `json:"previous_ids,omitempty"`
	Total       int       `json:"total,omitempty"`
}

type catalogSnapshot struct {
	Version     int                      `json:"version"`
	Items       []Item                   `json:"items"`
	Sources     map[string]catalogCursor `json:"sources"`
	NextSource  int                      `json:"next_source"`
	UpdatedAt   time.Time                `json:"updated_at"`
	Limited     bool                     `json:"limited,omitempty"`
	LimitReason string                   `json:"limit_reason,omitempty"`
	ItemLimit   int                      `json:"item_limit,omitempty"`
	ByteLimit   int                      `json:"byte_limit,omitempty"`
}

type ListOptions struct {
	Page     int
	PageSize int
	Genre    string
	Query    string
	Tag      string
	Year     string
	YearFrom string
	YearTo   string
}

func (o ListOptions) Valid() bool {
	if o.Page < 1 || o.Page > 1000000 || o.PageSize < 1 || o.PageSize > 60 || utf8.RuneCountInString(o.Genre) > 40 || utf8.RuneCountInString(o.Query) > 128 || utf8.RuneCountInString(o.Tag) > 60 {
		return false
	}
	now := time.Now().UTC()
	if o.Year != "" && o.Year != "unknown" && !validCatalogYear(o.Year, now) {
		return false
	}
	if o.YearFrom != "" || o.YearTo != "" {
		return o.Year == "" && validCatalogYear(o.YearFrom, now) && validCatalogYear(o.YearTo, now) && o.YearFrom <= o.YearTo
	}
	return true
}

func validCatalogYear(value string, now time.Time) bool {
	if len(value) != 4 {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	year, err := strconv.Atoi(value)
	return err == nil && year >= 1800 && year <= now.Year()
}

// Query filters the entire current candidate pool before slicing a page. Genre
// choices describe the full recommendation pool, or the full category library
// for library requests, including entries outside the visible page.
func (s *Service) Query(ctx context.Context, category, collection string, options ListOptions) (Response, error) {
	if !options.Valid() || !ValidSelection(category, collection) {
		return Response{}, errors.New("invalid discovery page or filters")
	}
	fullCatalogUnavailable := false
	if collection == "library" && s.cfg.TMDBCatalogSyncEnabled {
		response, err := s.QueryFullCatalog(ctx, category, options)
		if err == nil {
			return response, nil
		}
		// Keep the existing catalog usable if the disk index is temporarily
		// unavailable; its status and warning make the fallback explicit.
		fullCatalogUnavailable = true
	}
	response, err := s.Get(ctx, category, collection)
	if err != nil {
		return Response{}, err
	}
	if collection != "library" {
		genres := make(map[string]bool)
		for _, item := range response.Items {
			for _, genre := range item.Genres {
				if genre != "" {
					genres[genre] = true
				}
			}
		}
		response.Genres = sortedFacet(genres)
	}
	filtered := make([]Item, 0, len(response.Items))
	query := normalizeTitle(options.Query)
	now := time.Now().UTC()
	for _, item := range response.Items {
		if options.Year == "unknown" && validCatalogYear(item.Year, now) {
			continue
		}
		if options.Year != "" && options.Year != "unknown" && item.Year != options.Year {
			continue
		}
		if options.YearFrom != "" && (!validCatalogYear(item.Year, now) || item.Year < options.YearFrom || item.Year > options.YearTo) {
			continue
		}
		if options.Genre != "" && !contains(item.Genres, options.Genre) {
			continue
		}
		if options.Tag != "" && !contains(item.Tags, options.Tag) {
			continue
		}
		if query != "" && !strings.Contains(normalizeTitle(item.Title), query) && !strings.Contains(normalizeTitle(item.OriginalTitle), query) {
			continue
		}
		filtered = append(filtered, item)
	}
	response.Total = len(filtered)
	lastPage := (response.Total + options.PageSize - 1) / options.PageSize
	if lastPage < 1 {
		lastPage = 1
	}
	if options.Page > lastPage {
		options.Page = lastPage
	}
	response.Page = options.Page
	response.PageSize = options.PageSize
	start := (options.Page - 1) * options.PageSize
	end := start + options.PageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	response.Items = filtered[start:end]
	response.HasMore = end < len(filtered)
	response.CatalogSync = s.FullCatalogStatus()
	if fullCatalogUnavailable {
		response.Warning = "完整片库暂时不可用，当前显示原有已收录作品。"
	}
	return response, nil
}

func (s *Service) describeCatalog(category, collection string, response *Response) {
	facets := response.Items
	if snapshot, ok := s.peekCatalog(category); ok {
		if collection != "library" {
			facets = s.response(cacheEntry{Items: snapshot.Items}, "library", false).Items
		}
		response.CatalogTotal = len(facets)
		response.Indexing = catalogIndexing(snapshot, catalogSources(category), time.Now())
		if snapshot.Limited {
			response.Warning = "当前片库已达到收录容量，仍可通过搜索查找其他作品"
		}
	} else {
		response.CatalogTotal = len(facets)
		response.Indexing = true
	}
	genreSet, tagSet, yearSet := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	now := time.Now().UTC()
	for _, item := range facets {
		if validCatalogYear(item.Year, now) {
			yearSet[item.Year] = true
		}
		for _, genre := range item.Genres {
			if genre != "" {
				genreSet[genre] = true
			}
		}
		for _, tag := range item.Tags {
			if tag != "" {
				tagSet[tag] = true
			}
		}
	}
	response.Genres = sortedFacet(genreSet)
	response.Tags = sortedFacet(tagSet)
	response.Years = sortedFacet(yearSet)
	sort.Sort(sort.Reverse(sort.StringSlice(response.Years)))
	response.Total = len(response.Items)
	response.Page = 1
	response.PageSize = len(response.Items)
}

func sortedFacet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Service) catalogPath(category string) string {
	return filepath.Join(s.cfg.CacheDir, "catalog_"+category+".json")
}

func (s *Service) peekCatalog(category string) (catalogSnapshot, bool) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	snapshot, ok := s.catalogs[category]
	return snapshot, ok
}

func (s *Service) loadCatalog(category string) (catalogSnapshot, bool) {
	if snapshot, ok := s.peekCatalog(category); ok {
		return snapshot, true
	}
	if s.cfg.CacheDir == "" {
		return catalogSnapshot{}, false
	}
	file, err := os.Open(s.catalogPath(category))
	if err != nil {
		return catalogSnapshot{}, false
	}
	defer file.Close()
	data, err := util.ReadAllLimited(file, catalogHardMaxBytes)
	var snapshot catalogSnapshot
	if err != nil || json.Unmarshal(data, &snapshot) != nil || snapshot.Version != 1 || snapshot.Items == nil || len(snapshot.Items) > catalogHardMaxItems {
		return catalogSnapshot{}, false
	}
	if snapshot.Sources == nil {
		snapshot.Sources = make(map[string]catalogCursor)
	}
	snapshot = resumeCatalog(snapshot, s.cfg.CatalogMaxItems, s.cfg.CatalogMaxBytes)
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if current, ok := s.catalogs[category]; ok {
		return current, true
	}
	s.catalogs[category] = snapshot
	return snapshot, true
}

func (s *Service) saveCatalog(category string, snapshot catalogSnapshot) {
	s.catalogMu.Lock()
	s.catalogs[category] = snapshot
	s.catalogMu.Unlock()
	if s.cfg.CacheDir == "" {
		return
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > catalogHardMaxBytes || os.MkdirAll(s.cfg.CacheDir, 0700) != nil {
		return
	}
	file, err := os.CreateTemp(s.cfg.CacheDir, ".catalog-*")
	if err != nil {
		return
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return
	}
	if file.Close() != nil {
		return
	}
	_ = os.Rename(name, s.catalogPath(category))
}

// A capacity increase resumes the page that hit its bound. Legacy snapshots
// did not record that page, so preserve their members while restarting cursors.
func resumeCatalog(snapshot catalogSnapshot, maxItems, maxBytes int) catalogSnapshot {
	if !snapshot.Limited {
		return snapshot
	}
	legacy := snapshot.LimitReason == ""
	if legacy {
		if len(snapshot.Items) >= 5000 {
			snapshot.LimitReason = "items"
		} else {
			snapshot.LimitReason = "bytes"
		}
		snapshot.ItemLimit, snapshot.ByteLimit = 5000, 8<<20
	}
	if (snapshot.LimitReason == "items" && maxItems <= snapshot.ItemLimit) || (snapshot.LimitReason == "bytes" && maxBytes <= snapshot.ByteLimit) {
		return snapshot
	}
	if snapshot.LimitReason != "items" && snapshot.LimitReason != "bytes" {
		return snapshot
	}
	snapshot = copyCatalog(snapshot)
	if legacy {
		for key, cursor := range snapshot.Sources {
			snapshot.Sources[key] = catalogCursor{PreviousIDs: uniqueStrings(cursor.IDs, cursor.PreviousIDs)}
		}
	}
	snapshot.Limited = false
	snapshot.LimitReason = ""
	snapshot.ItemLimit, snapshot.ByteLimit = 0, 0
	return snapshot
}

func (s *Service) seedCatalog(category string) catalogSnapshot {
	if snapshot, ok := s.loadCatalog(category); ok {
		return snapshot
	}
	snapshot := catalogSnapshot{Version: 1, Items: []Item{}, Sources: make(map[string]catalogCursor)}
	for _, collection := range []string{"hot", "new", "top"} {
		if cached, ok := s.loadList(category, collection); ok {
			snapshot.Items = mergeCatalogItems(snapshot.Items, cached.Items, "")
			if cached.UpdatedAt.After(snapshot.UpdatedAt) {
				snapshot.UpdatedAt = cached.UpdatedAt
			}
		}
	}
	// Publish seeds once. The page fetch remains singleflight so simultaneous
	// first visitors cannot publish older seeds over a completed source page.
	s.catalogMu.Lock()
	if current, ok := s.catalogs[category]; ok {
		snapshot = current
	} else {
		s.catalogs[category] = snapshot
	}
	s.catalogMu.Unlock()
	return snapshot
}

func (s *Service) library(ctx context.Context, category string) (Response, error) {
	snapshot := s.seedCatalog(category)
	if len(snapshot.Items) == 0 {
		var err error
		snapshot, err = s.advanceCatalog(ctx, category)
		if err != nil {
			return Response{}, err
		}
		if len(snapshot.Items) == 0 {
			return Response{}, errors.New("catalog seed temporarily unavailable")
		}
	} else if catalogIndexing(snapshot, catalogSources(category), time.Now()) {
		s.queueCatalog(category)
	}
	response := s.response(cacheEntry{Items: snapshot.Items, UpdatedAt: snapshot.UpdatedAt}, "library", false)
	return response, nil
}

func sourceTTL(source catalogSource) time.Duration {
	if source.Collection == "hot" || source.Collection == "new" {
		return 30 * time.Minute
	}
	return 6 * time.Hour
}

func catalogIndexing(snapshot catalogSnapshot, sources []catalogSource, now time.Time) bool {
	if snapshot.Limited {
		return false
	}
	for _, source := range sources {
		cursor, ok := snapshot.Sources[source.Key]
		if !ok || !cursor.Done || now.Sub(cursor.CompletedAt) >= sourceTTL(source) {
			return true
		}
	}
	return false
}

func (s *Service) queueCatalog(category string) {
	s.enqueue(s.catalogQueue, backgroundTask{key: "catalog:" + category, run: func(ctx context.Context) { _, _ = s.advanceCatalog(ctx, category) }})
}

// advanceCatalog fetches exactly one source page. Round-robin source/category
// scheduling visits each first page before diving deeper into any single tag.
func (s *Service) advanceCatalog(ctx context.Context, category string) (catalogSnapshot, error) {
	result := s.catalogFlight.DoChan(category, func() (any, error) {
		s.lifecycleMu.Lock()
		if s.closed {
			s.lifecycleMu.Unlock()
			return catalogSnapshot{}, context.Canceled
		}
		s.catalogWork.Add(1)
		s.lifecycleMu.Unlock()
		defer s.catalogWork.Done()
		snapshot := s.seedCatalog(category)
		sources := catalogSources(category)
		if snapshot.Limited || len(sources) == 0 {
			return snapshot, nil
		}
		now := time.Now()
		for _, index := range catalogSourceOrder(snapshot, sources) {
			source := sources[index]
			cursor := snapshot.Sources[source.Key]
			if cursor.Done && now.Sub(cursor.CompletedAt) < sourceTTL(source) {
				continue
			}
			failureKey := "catalog:" + category + ":" + source.Key
			s.mu.Lock()
			retryAt := s.failures[failureKey]
			s.mu.Unlock()
			if now.Before(retryAt) {
				continue
			}
			if cursor.Done {
				cursor = catalogCursor{PreviousIDs: cursor.IDs}
			}
			fetchCtx, cancel := context.WithTimeout(s.ctx, 28*time.Second)
			if err := s.waitCatalogPage(fetchCtx); err != nil {
				cancel()
				return snapshot, err
			}
			page, err := s.fetchCatalogPage(fetchCtx, category, source, cursor.NextStart, 50)
			cancel()
			if err != nil {
				s.mu.Lock()
				s.failures[failureKey] = time.Now().Add(time.Minute)
				s.mu.Unlock()
				// Rotate past the failed provider but preserve its cursor and data.
				snapshot = copyCatalog(snapshot)
				snapshot.NextSource = (index + 1) % len(sources)
				s.saveCatalog(category, snapshot)
				return snapshot, errors.New("catalog source temporarily unavailable")
			}
			snapshot = mergeCatalogPage(snapshot, source, cursor, page, time.Now().UTC(), s.cfg.CatalogMaxItems, s.cfg.CatalogMaxBytes)
			snapshot.NextSource = (index + 1) % len(sources)
			s.saveCatalog(category, snapshot)
			return snapshot, nil
		}
		return snapshot, nil
	})
	select {
	case <-ctx.Done():
		return catalogSnapshot{}, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return catalogSnapshot{}, result.Err
		}
		return result.Val.(catalogSnapshot), nil
	}
}

func catalogSourceOrder(snapshot catalogSnapshot, sources []catalogSource) []int {
	order := make([]int, 0, len(sources))
	// Upgrades can add historical-year sources while old tags are still deep
	// in pagination. Give every new source an early first page, in plan order.
	for index, source := range sources {
		if _, seen := snapshot.Sources[source.Key]; !seen {
			order = append(order, index)
		}
	}
	for offset := range sources {
		start := snapshot.NextSource
		if start < 0 {
			start = 0
		}
		index := (start%len(sources) + offset) % len(sources)
		if _, seen := snapshot.Sources[sources[index].Key]; seen {
			order = append(order, index)
		}
	}
	return order
}

func (s *Service) waitCatalogPage(ctx context.Context) error {
	s.catalogMu.Lock()
	when := s.nextCatalogPage
	if now := time.Now(); when.Before(now) {
		when = now
	}
	s.nextCatalogPage = when.Add(s.catalogInterval)
	s.catalogMu.Unlock()
	timer := time.NewTimer(time.Until(when))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func copyCatalog(snapshot catalogSnapshot) catalogSnapshot {
	sources := make(map[string]catalogCursor, len(snapshot.Sources))
	for key, cursor := range snapshot.Sources {
		sources[key] = cursor
	}
	snapshot.Sources = sources
	return snapshot
}

func mergeCatalogPage(snapshot catalogSnapshot, source catalogSource, cursor catalogCursor, page catalogPage, now time.Time, maxItems, maxBytes int) catalogSnapshot {
	snapshot = copyCatalog(snapshot)
	originalItems := snapshot.Items
	originalCursor := cursor
	// Reducing an operator's configured limit pauses growth; it must never
	// delete the library already collected under an earlier configuration.
	if maxItems < len(originalItems) {
		maxItems = len(originalItems)
	}
	seen := make(map[string]bool, len(cursor.IDs))
	for _, id := range cursor.IDs {
		seen[id] = true
	}
	ids := append([]string{}, cursor.IDs...)
	added := 0
	for _, item := range page.Items {
		if !seen[item.ID] {
			seen[item.ID] = true
			ids = append(ids, item.ID)
			added++
		}
	}
	if page.NextStart <= cursor.NextStart || (len(page.Items) > 0 && added == 0) {
		page.Done = true
		// An ignored offset is an upstream pagination fault, not evidence that
		// the previously completed source has lost all of its later members.
		ids = uniqueStrings(ids, cursor.PreviousIDs)
	}
	cursor.IDs = ids
	cursor.NextStart = page.NextStart
	cursor.Done = page.Done
	cursor.Total = page.Total
	if cursor.Done {
		cursor.CompletedAt = now
		cursor.PreviousIDs = nil
	}
	snapshot.Sources[source.Key] = cursor
	merged := mergeCatalogItems(snapshot.Items, page.Items, source.Label)
	if len(merged) > maxItems {
		merged = merged[:maxItems]
		snapshot.Limited = true
		snapshot.LimitReason = "items"
		snapshot.ItemLimit, snapshot.ByteLimit = maxItems, maxBytes
		snapshot.Sources[source.Key] = originalCursor
	}
	snapshot.Items = merged
	snapshot.UpdatedAt = now
	// Trim only newly appended entries at a byte limit. Existing records survive
	// configuration reductions and a single unexpectedly large provider page.
	for {
		data, err := json.Marshal(snapshot)
		if err == nil && len(data) <= maxBytes {
			break
		}
		snapshot.Limited = true
		snapshot.LimitReason = "bytes"
		snapshot.ItemLimit, snapshot.ByteLimit = maxItems, maxBytes
		snapshot.Sources[source.Key] = originalCursor
		if len(snapshot.Items) <= len(originalItems) {
			snapshot.Items = originalItems
			break
		}
		remove := (len(snapshot.Items) - len(originalItems)) / 10
		if remove < 1 {
			remove = 1
		}
		snapshot.Items = snapshot.Items[:len(snapshot.Items)-remove]
	}
	return snapshot
}

func mergeCatalogItems(existing, incoming []Item, label string) []Item {
	out := append([]Item{}, existing...)
	positions := make(map[string]int, len(out))
	for i, item := range out {
		positions[item.ID] = i
	}
	for _, item := range incoming {
		if !idPattern.MatchString(item.ID) || item.Title == "" {
			continue
		}
		item.Overview = ""
		item.DetailsCached = false
		item.Tags = uniqueStrings(item.Tags, []string{label})
		if index, ok := positions[item.ID]; ok {
			out[index] = mergeCatalogItem(out[index], item)
		} else {
			positions[item.ID] = len(out)
			out = append(out, item)
		}
	}
	return out
}

func uniqueStrings(groups ...[]string) []string {
	out := []string{}
	seen := make(map[string]bool)
	for _, group := range groups {
		for _, value := range group {
			if value != "" && !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	return out
}

func mergeCatalogItem(old, item Item) Item {
	if item.Title == "" {
		item.Title = old.Title
	}
	if item.OriginalTitle == "" {
		item.OriginalTitle = old.OriginalTitle
	}
	if item.Year == "" {
		item.Year = old.Year
	}
	if item.ReleaseDate == "" {
		item.ReleaseDate = old.ReleaseDate
	}
	if item.PosterURL == "" {
		item.PosterURL = old.PosterURL
	}
	if item.MediaType == "" {
		item.MediaType = old.MediaType
	}
	if item.DoubanURL == "" {
		item.DoubanURL = old.DoubanURL
	}
	if item.MetadataSource == "" {
		item.MetadataSource = old.MetadataSource
	}
	if item.DoubanRating == nil {
		item.DoubanRating = old.DoubanRating
	}
	if item.DoubanVotes == nil {
		item.DoubanVotes = old.DoubanVotes
	}
	item.Genres = uniqueStrings(old.Genres, item.Genres)
	item.Tags = uniqueStrings(old.Tags, item.Tags)
	return item
}

// Current fast first pages lead the recommendation order. Expanded source
// membership supplies the tail, so a 30-minute refresh cannot shrink it to 50.
func (s *Service) recommendationPool(category, collection string, first []Item) []Item {
	snapshot, ok := s.peekCatalog(category)
	if !ok {
		return first
	}
	index := make(map[string]Item, len(snapshot.Items))
	for _, item := range snapshot.Items {
		index[item.ID] = item
	}
	pool := make([]Item, 0, len(snapshot.Items))
	seen := make(map[string]bool)
	appendItem := func(item Item) {
		if !seen[item.ID] {
			seen[item.ID] = true
			pool = append(pool, item)
		}
	}
	for _, item := range first {
		if known, ok := index[item.ID]; ok {
			item = mergeCatalogItem(known, item)
		}
		appendItem(item)
	}
	actual := collection
	if actual == "random" {
		actual = "top"
	}
	if actual == "top" && category != "movie" {
		for _, item := range snapshot.Items {
			appendItem(item)
		}
		return rankItems(pool, category)
	}
	for _, source := range catalogSources(category) {
		if source.Collection != actual {
			continue
		}
		cursor := snapshot.Sources[source.Key]
		ids := cursor.IDs
		if !cursor.Done {
			ids = append(append([]string{}, ids...), cursor.PreviousIDs...)
		}
		for _, id := range ids {
			if item, ok := index[id]; ok {
				appendItem(item)
			}
		}
	}
	if actual == "top" {
		return rankItems(pool, category)
	}
	return pool
}
