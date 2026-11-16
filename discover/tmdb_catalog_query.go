package discover

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Service) QueryFullCatalog(ctx context.Context, category string, options ListOptions) (Response, error) {
	if !options.Valid() || !contains([]string{"movie", "tv", "animation", "variety", "documentary"}, category) {
		return Response{}, errors.New("invalid discovery page or filters")
	}
	store, err := s.openFullCatalog()
	if err != nil {
		return Response{}, err
	}
	facets, err := store.categoryFacets(ctx, category)
	if err != nil {
		return Response{}, errors.New("full catalog query unavailable")
	}
	where, args := catalogWhere(category, options)
	from := catalogQueryFrom(options)
	total := 0
	if err = store.db.QueryRowContext(ctx, "SELECT total FROM category_counts WHERE category=?", category).Scan(&total); err != nil && err != sql.ErrNoRows {
		return Response{}, errors.New("full catalog query unavailable")
	}
	facets.total = total
	if normalizeTitle(options.Query) != "" || options.Genre != "" || options.Tag != "" || options.Year != "" || options.YearFrom != "" {
		countFrom := from
		if normalizeTitle(options.Query) == "" && options.Genre == "" && options.Tag == "" {
			countFrom = "visible_categories v"
		}
		if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+countFrom+" WHERE "+where, args...).Scan(&total); err != nil {
			return Response{}, errors.New("full catalog query unavailable")
		}
	}
	page := options.Page
	last := (total + options.PageSize - 1) / options.PageSize
	if last < 1 {
		last = 1
	}
	if page > last {
		page = last
	}
	queryArgs := append(append([]any{}, args...), options.PageSize, (page-1)*options.PageSize)
	rows, err := store.db.QueryContext(ctx, "SELECT "+fullItemColumns+" FROM "+from+" WHERE "+where+" ORDER BY v.popularity DESC,v.key LIMIT ? OFFSET ?", queryArgs...)
	if err != nil {
		return Response{}, errors.New("full catalog query unavailable")
	}
	defer rows.Close()
	items := make([]Item, 0, options.PageSize)
	for rows.Next() {
		item, scanErr := scanFullItem(rows)
		if scanErr != nil {
			return Response{}, errors.New("full catalog query unavailable")
		}
		if idPattern.MatchString(item.ID) && !item.DetailsCached {
			cached := s.response(cacheEntry{Items: []Item{item}, UpdatedAt: time.Now().UTC()}, "library", false)
			if len(cached.Items) == 1 {
				item = cached.Items[0]
			}
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return Response{}, errors.New("full catalog query unavailable")
	}
	status := s.FullCatalogStatus()
	response := Response{Items: items, UpdatedAt: status.UpdatedAt, TMDBConfigured: s.TMDBConfigured(), Total: total, CatalogTotal: facets.total, Page: page, PageSize: options.PageSize, HasMore: page*options.PageSize < total, Genres: facets.genres, Years: facets.years, Tags: facets.tags, Indexing: status.Stage != "ready" && status.Stage != "disabled", CatalogSync: status}
	if status.Stage == "paused" || status.Stage == "error" {
		response.Warning = "目录同步暂缓，已收录作品仍可浏览"
	}
	return response, nil
}

func catalogQueryFrom(options ListOptions) string {
	if normalizeTitle(options.Query) != "" || options.Genre != "" || options.Tag != "" {
		return "entries e CROSS JOIN visible_categories v ON v.key=e.key"
	}
	return "visible_categories v JOIN entries e ON e.key=v.key"
}

func catalogWhere(category string, options ListOptions) (string, []any) {
	clauses := []string{"v.category=?"}
	args := []any{category}
	if options.Year == "unknown" {
		clauses = append(clauses, "v.year=0")
	} else if options.Year != "" {
		clauses = append(clauses, "v.year=?")
		year, _ := strconv.Atoi(options.Year)
		args = append(args, year)
	}
	if options.YearFrom != "" {
		clauses = append(clauses, "v.year BETWEEN ? AND ?")
		from, _ := strconv.Atoi(options.YearFrom)
		to, _ := strconv.Atoi(options.YearTo)
		args = append(args, from, to)
	}
	if options.Genre != "" {
		clauses = append(clauses, "e.key IN(SELECT key FROM entry_genres WHERE genre=?)")
		args = append(args, options.Genre)
	}
	if options.Tag != "" {
		clauses = append(clauses, "e.key IN(SELECT key FROM entry_tags WHERE tag=?)")
		args = append(args, options.Tag)
	}
	query := normalizeTitle(options.Query)
	if query != "" {
		if utf8.RuneCountInString(query) >= 3 {
			// Quoting the complete normalized token prevents MATCH operators, quotes,
			// or user punctuation from becoming FTS syntax.
			clauses = append(clauses, "e.rowid IN(SELECT rowid FROM entries_search WHERE entries_search MATCH ?)")
			args = append(args, "\""+strings.ReplaceAll(query, "\"", "\"\"")+"\"")
		} else {
			// Trigram indexes cannot answer one/two-character substrings. Use two
			// B-tree prefix ranges instead of silently scanning the entire database.
			clauses = append(clauses, "e.rowid IN(SELECT rowid FROM entries WHERE norm_title>=? AND norm_title<? UNION SELECT rowid FROM entries WHERE norm_original>=? AND norm_original<?)")
			args = append(args, query, query+"\U0010ffff", query, query+"\U0010ffff")
		}
	}
	return strings.Join(clauses, " AND "), args
}

func (store *tmdbCatalogStore) categoryFacets(ctx context.Context, category string) (tmdbCatalogFacets, error) {
	store.facetMu.Lock()
	cached, ok := store.facets[category]
	store.facetMu.Unlock()
	if ok && time.Since(cached.at) < time.Minute {
		return cached, nil
	}
	result := tmdbCatalogFacets{at: time.Now(), genres: []string{}, years: []string{}, tags: []string{}}
	if err := store.db.QueryRowContext(ctx, "SELECT total FROM category_counts WHERE category=?", category).Scan(&result.total); err != nil && err != sql.ErrNoRows {
		return result, err
	}
	rows, err := store.db.QueryContext(ctx, "SELECT DISTINCT year FROM visible_categories WHERE category=? AND year>0 ORDER BY year DESC", category)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var year int
		if err = rows.Scan(&year); err != nil {
			rows.Close()
			return result, err
		}
		result.years = append(result.years, strconv.Itoa(year))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = store.db.QueryContext(ctx, "SELECT DISTINCT g.genre FROM entry_genres g INDEXED BY genre_entries CROSS JOIN visible_categories v ON v.key=g.key WHERE v.category=? ORDER BY g.genre", category)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var genre string
		if err = rows.Scan(&genre); err != nil {
			rows.Close()
			return result, err
		}
		result.genres = append(result.genres, genre)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = store.db.QueryContext(ctx, "SELECT DISTINCT t.tag FROM entry_tags t INDEXED BY tag_entries CROSS JOIN visible_categories v ON v.key=t.key WHERE v.category=? ORDER BY t.tag", category)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var tag string
		if err = rows.Scan(&tag); err != nil {
			rows.Close()
			return result, err
		}
		result.tags = append(result.tags, tag)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	store.facetMu.Lock()
	store.facets[category] = result
	store.facetMu.Unlock()
	return result, nil
}

func (s *Service) FullCatalogSubject(ctx context.Context, id, mediaType string) (Item, error) {
	numericID, valid := parseFullCatalogKey(id, mediaType)
	if !valid {
		return Item{}, errors.New("invalid discovery subject")
	}
	store, err := s.openFullCatalog()
	if err != nil {
		return Item{}, err
	}
	var updated, retryAt int64
	var removed int
	_ = store.db.QueryRowContext(ctx, "SELECT removed,retry_at FROM entries WHERE key=?", id).Scan(&removed, &retryAt)
	if removed == 1 && retryAt > time.Now().Unix() {
		return Item{}, errors.New("discovery metadata unavailable")
	}
	item, err := scanFullItem(store.db.QueryRowContext(ctx, "SELECT "+fullItemColumns+" FROM entries e WHERE e.key=? AND e.removed=0", id))
	if err != nil && err != sql.ErrNoRows {
		return Item{}, errors.New("full catalog query unavailable")
	}
	if err == nil {
		_ = store.db.QueryRowContext(ctx, "SELECT updated_at FROM entries WHERE key=?", id).Scan(&updated)
	}
	if err == sql.ErrNoRows {
		return Item{}, errors.New("discovery metadata unavailable")
	}
	if err == nil && retryAt > time.Now().Unix() {
		if item.DetailsCached {
			return item, nil
		}
		return Item{}, errors.New("discovery metadata unavailable")
	}
	if err == nil && item.DetailsCached && time.Since(time.Unix(updated, 0)) < 7*24*time.Hour {
		return item, nil
	}
	if !s.TMDBConfigured() {
		if err == nil && item.DetailsCached {
			return item, nil
		}
		return Item{}, errors.New("discovery metadata unavailable")
	}
	refreshed, refreshErr := s.hydrateFullCatalog(ctx, store, mediaType, numericID)
	if refreshErr == nil {
		return refreshed, nil
	}
	if err == nil && item.DetailsCached {
		_ = store.db.QueryRowContext(ctx, "SELECT removed FROM entries WHERE key=?", id).Scan(&removed)
		if removed == 0 {
			return item, nil
		}
	}
	return Item{}, errors.New("discovery metadata unavailable")
}
