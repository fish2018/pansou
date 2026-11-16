package discover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type fullTMDBDetails struct {
	ID            int     `json:"id"`
	Adult         bool    `json:"adult"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	Popularity    float64 `json:"popularity"`
	Genres        []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"genres"`
}

type fullCatalogHTTPError struct{ status int }

func (err fullCatalogHTTPError) Error() string { return "catalog provider rejected request" }

func (s *Service) fullCatalogJSON(ctx context.Context, store *tmdbCatalogStore, path string, query url.Values, target any) error {
	if query == nil {
		query = url.Values{}
	}
	if err := store.waitRequest(ctx, s.cfg.TMDBCatalogRate); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.tmdbAddress(path, query), nil)
	if err != nil {
		return errors.New("catalog request failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "PanSou discovery catalog")
	if s.cfg.TMDBReadAccessToken != "" {
		request.Header.Set("Authorization", "Bearer "+s.cfg.TMDBReadAccessToken)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return errors.New("catalog provider unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == 429 || response.StatusCode == 401 || response.StatusCode == 403 {
			delay := time.Minute
			if seconds, parseErr := strconv.Atoi(response.Header.Get("Retry-After")); parseErr == nil && seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			} else if at, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil && time.Until(at) > 0 {
				delay = time.Until(at)
			}
			if delay > 15*time.Minute {
				delay = 15 * time.Minute
			}
			if response.StatusCode != 429 {
				delay = 30 * time.Minute
				store.setStage("paused", "credentials_rejected")
			}
			store.rateMu.Lock()
			until := time.Now().Add(delay)
			if until.After(store.pauseUntil) {
				store.pauseUntil = until
			}
			store.rateMu.Unlock()
		}
		return fullCatalogHTTPError{status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return errors.New("catalog response invalid")
	}
	if json.Unmarshal(body, target) != nil {
		return errors.New("catalog response invalid")
	}
	return nil
}

func (store *tmdbCatalogStore) waitRequest(ctx context.Context, rate int) error {
	if rate < 1 {
		rate = 1
	}
	if rate > 8 {
		rate = 8
	}
	store.rateMu.Lock()
	when := store.nextRequest
	if now := time.Now(); when.Before(now) {
		when = now
	}
	if when.Before(store.pauseUntil) {
		when = store.pauseUntil
	}
	store.nextRequest = when.Add(time.Second / time.Duration(rate))
	store.rateMu.Unlock()
	for {
		timer := time.NewTimer(time.Until(when))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		store.rateMu.Lock()
		pause := store.pauseUntil
		store.rateMu.Unlock()
		if !pause.After(time.Now()) {
			return nil
		}
		when = pause
	}
}

func (s *Service) hydrateFullCatalog(ctx context.Context, store *tmdbCatalogStore, mediaType string, id int) (Item, error) {
	key := fullCatalogKey(mediaType, id)
	result := s.metadataFlight.DoChan("full:"+key, func() (any, error) {
		requestCtx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
		defer cancel()
		var details fullTMDBDetails
		err := s.fullCatalogJSON(requestCtx, store, "/"+mediaType+"/"+strconv.Itoa(id), url.Values{}, &details)
		if err != nil {
			store.recordHydrationFailure(s.ctx, key, err)
			return Item{}, err
		}
		if details.ID != id {
			store.recordHydrationFailure(s.ctx, key, errors.New("invalid id"))
			return Item{}, errors.New("catalog response invalid")
		}
		if details.Adult {
			store.writeMu.Lock()
			_, _ = store.db.ExecContext(s.ctx, "UPDATE entries SET removed=1,needs_refresh=0,retry_at=? WHERE key=?", time.Now().Add(30*24*time.Hour).Unix(), key)
			store.writeMu.Unlock()
			store.invalidateFacets()
			return Item{}, errors.New("discovery metadata unavailable")
		}
		item := Item{ID: key, TMDBID: id, MediaType: mediaType, Title: details.Title, OriginalTitle: details.OriginalTitle, MetadataSource: "tmdb", Overview: details.Overview, Genres: []string{}, DetailsCached: true, Reason: "TMDB 完整目录"}
		date := details.ReleaseDate
		if mediaType == "tv" {
			item.Title = details.Name
			item.OriginalTitle = details.OriginalName
			date = details.FirstAirDate
		}
		if item.Title == "" {
			item.Title = item.OriginalTitle
		}
		if item.Title == "" {
			item.Title = "TMDB #" + strconv.Itoa(id)
		}
		if parsed, parseErr := time.Parse("2006-01-02", date); parseErr == nil {
			item.ReleaseDate = date
			if parsed.Year() >= 1800 {
				item.Year = strconv.Itoa(parsed.Year())
			}
		}
		if strings.HasPrefix(details.PosterPath, "/") && !strings.Contains(details.PosterPath, "..") {
			item.PosterURL = "https://image.tmdb.org/t/p/w500" + details.PosterPath
		}
		categories := []string{mediaType}
		for _, genre := range details.Genres {
			if genre.Name != "" {
				item.Genres = append(item.Genres, genre.Name)
			}
			switch genre.ID {
			case 16:
				categories = append(categories, "animation")
			case 99:
				categories = append(categories, "documentary")
			case 10764, 10767:
				if mediaType == "tv" {
					categories = append(categories, "variety")
				}
			}
		}
		if err = store.saveHydrated(requestCtx, item, details.Popularity, uniqueStrings(categories)); err != nil {
			return Item{}, err
		}
		store.refreshStatus(s.ctx)
		return item, nil
	})
	select {
	case <-ctx.Done():
		return Item{}, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return Item{}, result.Err
		}
		return result.Val.(Item), nil
	}
}

func (store *tmdbCatalogStore) saveHydrated(ctx context.Context, item Item, popularity float64, categories []string) error {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	if err := store.checkCapacity(); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("full catalog write failed")
	}
	defer tx.Rollback()
	genres, _ := json.Marshal(nonNilStrings(item.Genres))
	year, _ := strconv.Atoi(item.Year)
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `INSERT INTO entries(key,media_type,tmdb_id,origin,title,original_title,norm_title,norm_original,year,release_date,poster_url,overview,genres_json,metadata_source,reason,hydrated,popularity,updated_at) VALUES(?,?,?,'tmdb',?,?,?,?,?,?,?,?,?,'tmdb',?,1,?,?) ON CONFLICT(key) DO UPDATE SET title=excluded.title,original_title=excluded.original_title,norm_title=excluded.norm_title,norm_original=excluded.norm_original,year=excluded.year,release_date=excluded.release_date,poster_url=excluded.poster_url,overview=excluded.overview,genres_json=excluded.genres_json,hydrated=1,popularity=excluded.popularity,updated_at=excluded.updated_at,retry_at=0,failures=0,needs_refresh=0,removed=0`, item.ID, item.MediaType, item.TMDBID, item.Title, item.OriginalTitle, normalizeTitle(item.Title), normalizeTitle(item.OriginalTitle), year, item.ReleaseDate, item.PosterURL, item.Overview, string(genres), item.Reason, popularity, now)
	if err != nil {
		return errors.New("full catalog write failed")
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM entry_categories WHERE key=? AND source='tmdb'", item.ID); err != nil {
		return errors.New("full catalog write failed")
	}
	for _, category := range categories {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO entry_categories(key,category,source) VALUES(?,?,'tmdb')", item.ID, category); err != nil {
			return errors.New("full catalog write failed")
		}
	}
	if err = writeCatalogFacets(ctx, tx, item); err != nil {
		return errors.New("full catalog write failed")
	}
	if err = store.matchDouban(ctx, tx, item, year); err != nil {
		return errors.New("full catalog write failed")
	}
	if err = setCatalogMeta(ctx, tx, "last_updated", strconv.FormatInt(now, 10)); err != nil {
		return errors.New("full catalog write failed")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("full catalog write failed")
	}
	return nil
}

// Match a complete series only to a complete-series Douban subject. Explicit
// season titles are never flattened into the parent series, even for season 1.
func (store *tmdbCatalogStore) matchDouban(ctx context.Context, tx *sql.Tx, item Item, year int) error {
	var previousMapping string
	_ = tx.QueryRowContext(ctx, "SELECT mapped_key FROM entries WHERE key=?", item.ID).Scan(&previousMapping)
	if _, err := tx.ExecContext(ctx, "UPDATE entries SET mapped_key='' WHERE key=?", item.ID); err != nil {
		return err
	}
	if year < 1800 {
		if previousMapping != "" {
			return store.restoreDoubanMapping(ctx, tx, previousMapping)
		}
		return nil
	}
	title, original := normalizeTitle(item.Title), normalizeTitle(item.OriginalTitle)
	if title == "" {
		if previousMapping != "" {
			return store.restoreDoubanMapping(ctx, tx, previousMapping)
		}
		return nil
	}
	if original == "" {
		original = title
	}
	matches, err := catalogMatchingKeys(ctx, tx, "douban", item.MediaType, year, title, original)
	if err != nil {
		return err
	}
	if len(matches) != 1 {
		if previousMapping != "" {
			return store.restoreDoubanMapping(ctx, tx, previousMapping)
		}
		return nil
	}
	// A second TMDB namesake makes the correspondence ambiguous in both
	// directions. Undo earlier tentative mappings instead of hiding either work.
	namesakes, err := catalogMatchingKeys(ctx, tx, "tmdb", item.MediaType, year, title, original)
	if err != nil {
		return err
	}
	if len(namesakes) != 1 {
		_, err = tx.ExecContext(ctx, "UPDATE entries SET mapped_key='' WHERE mapped_key=?", matches[0])
		if err != nil {
			return err
		}
		return store.restoreDoubanMapping(ctx, tx, matches[0])
	}
	if previousMapping != "" && previousMapping != matches[0] {
		if err = store.restoreDoubanMapping(ctx, tx, previousMapping); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE entries SET mapped_key=? WHERE key=?", matches[0], item.ID); err != nil {
		return err
	}
	var oldGenres string
	if err = tx.QueryRowContext(ctx, "SELECT genres_json FROM entries WHERE key=?", matches[0]).Scan(&oldGenres); err != nil {
		return err
	}
	var existingGenres []string
	_ = json.Unmarshal([]byte(oldGenres), &existingGenres)
	genres := uniqueStrings(existingGenres, item.Genres)
	encodedGenres, _ := json.Marshal(genres)
	_, err = tx.ExecContext(ctx, "UPDATE entries SET tmdb_id=?,poster_url=CASE WHEN ?<>'' THEN ? ELSE poster_url END,overview=CASE WHEN ?<>'' THEN ? ELSE overview END,release_date=CASE WHEN ?<>'' THEN ? ELSE release_date END,genres_json=?,popularity=(SELECT popularity FROM entries WHERE key=?),metadata_source='tmdb',hydrated=1 WHERE key=?", item.TMDBID, item.PosterURL, item.PosterURL, item.Overview, item.Overview, item.ReleaseDate, item.ReleaseDate, string(encodedGenres), item.ID, matches[0])
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM entry_categories WHERE key=? AND source='tmdb'", matches[0]); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO entry_categories(key,category,source) SELECT ?,category,'tmdb' FROM entry_categories WHERE key=?", matches[0], item.ID); err != nil {
		return err
	}
	for _, genre := range genres {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO entry_genres(key,genre) VALUES(?,?)", matches[0], genre); err != nil {
			return err
		}
	}
	return nil
}

func (store *tmdbCatalogStore) restoreDoubanMapping(ctx context.Context, tx *sql.Tx, key string) error {
	var snapshotJSON string
	if err := tx.QueryRowContext(ctx, "SELECT douban_snapshot FROM entries WHERE key=? AND origin='douban'", key).Scan(&snapshotJSON); err != nil {
		return err
	}
	var item Item
	_ = json.Unmarshal([]byte(snapshotJSON), &item)
	item.ID = key
	genres, _ := json.Marshal(nonNilStrings(item.Genres))
	hydrated := 0
	if item.DetailsCached {
		hydrated = 1
	}
	source := item.MetadataSource
	if source == "" {
		source = "douban"
	}
	_, err := tx.ExecContext(ctx, "UPDATE entries SET tmdb_id=?,poster_url=?,overview=?,release_date=?,genres_json=?,popularity=0,metadata_source=?,hydrated=? WHERE key=?", item.TMDBID, item.PosterURL, item.Overview, item.ReleaseDate, string(genres), source, hydrated, key)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM entry_categories WHERE key=? AND source='tmdb'", key); err != nil {
		return err
	}
	return writeCatalogFacets(ctx, tx, item)
}

// Two independent index lookups avoid SQLite choosing a media-wide scan for
// an OR across the title and original-title indexes.
func catalogMatchingKeys(ctx context.Context, tx *sql.Tx, origin, media string, year int, title, original string) ([]string, error) {
	const query = `SELECT key FROM entries INDEXED BY entries_title WHERE norm_title IN (?,?) AND year=? AND media_type=? AND origin=? AND season=0 AND removed=0 UNION SELECT key FROM entries INDEXED BY entries_original WHERE norm_original IN (?,?) AND year=? AND media_type=? AND origin=? AND season=0 AND removed=0 LIMIT 2`
	rows, err := tx.QueryContext(ctx, query, title, original, year, media, origin, title, original, year, media, origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (store *tmdbCatalogStore) recordHydrationFailure(ctx context.Context, key string, providerError error) {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	var failures int
	_ = store.db.QueryRowContext(ctx, "SELECT failures FROM entries WHERE key=?", key).Scan(&failures)
	if failures > 8 {
		failures = 8
	}
	delay := time.Duration(1<<failures) * 10 * time.Minute
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	retry := time.Now().Add(delay).Unix()
	var httpError fullCatalogHTTPError
	if errors.As(providerError, &httpError) && httpError.status == 404 {
		_, _ = store.db.ExecContext(ctx, "UPDATE entries SET removed=1,retry_at=?,failures=failures+1,needs_refresh=0 WHERE key=?", time.Now().Add(30*24*time.Hour).Unix(), key)
		store.invalidateFacets()
		return
	}
	_, _ = store.db.ExecContext(ctx, "UPDATE entries SET retry_at=?,failures=failures+1 WHERE key=?", retry, key)
}

func (store *tmdbCatalogStore) nextHydration(ctx context.Context) (string, int, error) {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	var key, media string
	var id int
	err := store.db.QueryRowContext(ctx, `SELECT key,media_type,tmdb_id FROM entries INDEXED BY entries_hydrate_pending WHERE origin='tmdb' AND removed=0 AND retry_at<=? AND (hydrated=0 OR needs_refresh=1) ORDER BY needs_refresh DESC,hydrated,popularity DESC LIMIT 1`, time.Now().Unix()).Scan(&key, &media, &id)
	if err != nil {
		return "", 0, err
	}
	// A lease prevents the second bounded worker from choosing the same ID.
	if _, err = store.db.ExecContext(ctx, "UPDATE entries SET retry_at=? WHERE key=?", time.Now().Add(time.Minute).Unix(), key); err != nil {
		return "", 0, err
	}
	return media, id, nil
}

func (s *Service) runFullHydration(store *tmdbCatalogStore) {
	defer s.workers.Done()
	for s.ctx.Err() == nil {
		if store.checkCapacity() != nil {
			if !waitCatalog(s.ctx, time.Minute) {
				return
			}
			continue
		}
		media, id, err := store.nextHydration(s.ctx)
		if err == sql.ErrNoRows {
			if !waitCatalog(s.ctx, 2*time.Second) {
				return
			}
			continue
		}
		if err != nil {
			store.setStage("error", "storage_read_failed")
			if !waitCatalog(s.ctx, 10*time.Second) {
				return
			}
			continue
		}
		_, _ = s.hydrateFullCatalog(s.ctx, store, media, id)
	}
}
