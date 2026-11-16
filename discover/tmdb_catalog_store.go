package discover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var errFullCatalogDisabled = errors.New("full catalog disabled")
var errFullCatalogCapacity = errors.New("full catalog capacity reached")

// The full directory is disk-backed. Only the current SQL page, counters and
// small facets are held in memory; export readers never build an ID map.
type tmdbCatalogStore struct {
	db          *sql.DB
	path        string
	maxBytes    int64
	writeMu     sync.Mutex
	statusMu    sync.RWMutex
	status      CatalogSyncStatus
	rateMu      sync.Mutex
	nextRequest time.Time
	pauseUntil  time.Time
	facetMu     sync.Mutex
	facets      map[string]tmdbCatalogFacets
}

type tmdbCatalogFacets struct {
	at                  time.Time
	total               int
	genres, years, tags []string
}

const tmdbCatalogSchema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;
PRAGMA cache_size=-6144;
PRAGMA temp_store=FILE;
PRAGMA wal_autocheckpoint=2048;
PRAGMA journal_size_limit=16777216;
CREATE TABLE IF NOT EXISTS sync_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS entries (
 key TEXT PRIMARY KEY, media_type TEXT NOT NULL, tmdb_id INTEGER NOT NULL DEFAULT 0,
 origin TEXT NOT NULL, title TEXT NOT NULL, original_title TEXT NOT NULL DEFAULT '',
 norm_title TEXT NOT NULL, norm_original TEXT NOT NULL DEFAULT '', year INTEGER NOT NULL DEFAULT 0,
 release_date TEXT NOT NULL DEFAULT '', poster_url TEXT NOT NULL DEFAULT '', overview TEXT NOT NULL DEFAULT '',
 genres_json TEXT NOT NULL DEFAULT '[]', tags_json TEXT NOT NULL DEFAULT '[]',
 douban_rating REAL, douban_votes INTEGER, douban_url TEXT NOT NULL DEFAULT '',
 metadata_source TEXT NOT NULL DEFAULT 'tmdb', reason TEXT NOT NULL DEFAULT '',
 season INTEGER NOT NULL DEFAULT 0, popularity REAL NOT NULL DEFAULT 0,
 hydrated INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
 retry_at INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0,
 needs_refresh INTEGER NOT NULL DEFAULT 0, mapped_key TEXT NOT NULL DEFAULT '',
 export_date TEXT NOT NULL DEFAULT '', removed INTEGER NOT NULL DEFAULT 0, douban_snapshot TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS entries_title ON entries(norm_title,year,media_type);
CREATE INDEX IF NOT EXISTS entries_original ON entries(norm_original,year,media_type);
CREATE INDEX IF NOT EXISTS entries_year ON entries(year,popularity DESC);
CREATE INDEX IF NOT EXISTS entries_release_date ON entries(release_date,year);
CREATE INDEX IF NOT EXISTS entries_media ON entries(media_type,popularity DESC);
CREATE INDEX IF NOT EXISTS entries_popularity ON entries(popularity DESC,key);
CREATE INDEX IF NOT EXISTS entries_known_year ON entries(year) WHERE year>0;
CREATE INDEX IF NOT EXISTS entries_hydrate_pending ON entries(needs_refresh DESC,hydrated,popularity DESC) WHERE origin='tmdb' AND removed=0 AND (hydrated=0 OR needs_refresh=1);
CREATE INDEX IF NOT EXISTS entries_tmdb ON entries(media_type,tmdb_id);
CREATE TABLE IF NOT EXISTS entry_categories(key TEXT NOT NULL REFERENCES entries(key) ON DELETE CASCADE,category TEXT NOT NULL,source TEXT NOT NULL,PRIMARY KEY(key,category,source));
CREATE INDEX IF NOT EXISTS category_entries ON entry_categories(category,key);
CREATE TABLE IF NOT EXISTS visible_categories(key TEXT NOT NULL REFERENCES entries(key) ON DELETE CASCADE,category TEXT NOT NULL,year INTEGER NOT NULL,popularity REAL NOT NULL,PRIMARY KEY(key,category));
CREATE INDEX IF NOT EXISTS visible_category_order ON visible_categories(category,popularity DESC,key);
CREATE INDEX IF NOT EXISTS visible_category_year ON visible_categories(category,year,popularity DESC,key);
CREATE TABLE IF NOT EXISTS category_counts(category TEXT PRIMARY KEY,total INTEGER NOT NULL DEFAULT 0);
CREATE TRIGGER IF NOT EXISTS visible_category_insert AFTER INSERT ON visible_categories BEGIN
 INSERT INTO category_counts(category,total) VALUES(new.category,1) ON CONFLICT(category) DO UPDATE SET total=total+1;
END;
CREATE TRIGGER IF NOT EXISTS visible_category_delete AFTER DELETE ON visible_categories BEGIN
 UPDATE category_counts SET total=MAX(0,total-1) WHERE category=old.category;
END;
CREATE TRIGGER IF NOT EXISTS category_insert AFTER INSERT ON entry_categories BEGIN
 INSERT OR IGNORE INTO visible_categories(key,category,year,popularity)
 SELECT key,new.category,year,popularity FROM entries WHERE key=new.key AND removed=0 AND mapped_key='' AND (year=0 OR year<=CAST(strftime('%Y','now') AS INTEGER)) AND (release_date='' OR release_date<=date('now'));
END;
CREATE TRIGGER IF NOT EXISTS category_delete AFTER DELETE ON entry_categories WHEN NOT EXISTS(SELECT 1 FROM entry_categories WHERE key=old.key AND category=old.category) BEGIN
 DELETE FROM visible_categories WHERE key=old.key AND category=old.category;
END;
CREATE TRIGGER IF NOT EXISTS entry_visibility_update AFTER UPDATE OF year,release_date,removed,mapped_key ON entries WHEN old.year<>new.year OR old.release_date<>new.release_date OR old.removed<>new.removed OR old.mapped_key<>new.mapped_key BEGIN
 DELETE FROM visible_categories WHERE key=new.key;
 INSERT OR IGNORE INTO visible_categories(key,category,year,popularity)
 SELECT new.key,category,new.year,new.popularity FROM entry_categories WHERE key=new.key AND new.removed=0 AND new.mapped_key='' AND (new.year=0 OR new.year<=CAST(strftime('%Y','now') AS INTEGER)) AND (new.release_date='' OR new.release_date<=date('now'));
END;
CREATE TRIGGER IF NOT EXISTS entry_popularity_update AFTER UPDATE OF popularity ON entries WHEN old.popularity<>new.popularity BEGIN
 UPDATE visible_categories SET popularity=new.popularity WHERE key=new.key;
 UPDATE entries SET popularity=new.popularity WHERE key=new.mapped_key AND origin='douban' AND new.mapped_key<>'';
END;
CREATE TABLE IF NOT EXISTS entry_genres(key TEXT NOT NULL REFERENCES entries(key) ON DELETE CASCADE,genre TEXT NOT NULL,PRIMARY KEY(key,genre));
CREATE INDEX IF NOT EXISTS genre_entries ON entry_genres(genre,key);
CREATE TABLE IF NOT EXISTS entry_tags(key TEXT NOT NULL REFERENCES entries(key) ON DELETE CASCADE,tag TEXT NOT NULL,PRIMARY KEY(key,tag));
CREATE INDEX IF NOT EXISTS tag_entries ON entry_tags(tag,key);
CREATE TABLE IF NOT EXISTS counters(id INTEGER PRIMARY KEY CHECK(id=1),indexed INTEGER NOT NULL,hydrated INTEGER NOT NULL);
INSERT OR IGNORE INTO counters VALUES(1,0,0);
CREATE VIRTUAL TABLE IF NOT EXISTS entries_search USING fts5(norm_title,norm_original,content='entries',content_rowid='rowid',tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS entries_insert AFTER INSERT ON entries BEGIN
 INSERT INTO entries_search(rowid,norm_title,norm_original) VALUES(new.rowid,new.norm_title,new.norm_original);
 UPDATE counters SET indexed=indexed+1,hydrated=hydrated+new.hydrated WHERE id=1;
END;
CREATE TRIGGER IF NOT EXISTS entries_delete AFTER DELETE ON entries BEGIN
 INSERT INTO entries_search(entries_search,rowid,norm_title,norm_original) VALUES('delete',old.rowid,old.norm_title,old.norm_original);
 UPDATE counters SET indexed=indexed-1,hydrated=hydrated-old.hydrated WHERE id=1;
END;
CREATE TRIGGER IF NOT EXISTS entries_search_update AFTER UPDATE OF norm_title,norm_original ON entries WHEN old.norm_title<>new.norm_title OR old.norm_original<>new.norm_original BEGIN
 INSERT INTO entries_search(entries_search,rowid,norm_title,norm_original) VALUES('delete',old.rowid,old.norm_title,old.norm_original);
 INSERT INTO entries_search(rowid,norm_title,norm_original) VALUES(new.rowid,new.norm_title,new.norm_original);
END;
CREATE TRIGGER IF NOT EXISTS entries_hydrated_update AFTER UPDATE OF hydrated ON entries WHEN old.hydrated<>new.hydrated BEGIN
 UPDATE counters SET hydrated=hydrated+new.hydrated-old.hydrated WHERE id=1;
END;
`

func (s *Service) openFullCatalog() (*tmdbCatalogStore, error) {
	if !s.cfg.TMDBCatalogSyncEnabled {
		return nil, errFullCatalogDisabled
	}
	s.fullCatalogMu.Lock()
	defer s.fullCatalogMu.Unlock()
	if s.fullCatalog != nil {
		return s.fullCatalog, nil
	}
	if s.ctx.Err() != nil {
		return nil, context.Canceled
	}
	if s.cfg.CacheDir == "" {
		s.fullCatalogError = "storage_unavailable"
		return nil, errors.New("full catalog storage unavailable")
	}
	if err := os.MkdirAll(s.cfg.CacheDir, 0700); err != nil {
		s.fullCatalogError = "storage_unavailable"
		return nil, errors.New("full catalog storage unavailable")
	}
	path := filepath.Join(s.cfg.CacheDir, "tmdb-catalog.sqlite")
	// Connection-local pragmas apply to every pool connection. Four readers plus
	// one writer stay within a small, explicit page-cache budget.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=cache_size(-6144)")
	if err != nil {
		s.fullCatalogError = "storage_unavailable"
		return nil, errors.New("full catalog storage unavailable")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	if _, err = db.ExecContext(ctx, tmdbCatalogSchema); err != nil {
		db.Close()
		s.fullCatalogError = "storage_initialization_failed"
		return nil, errors.New("full catalog initialization failed")
	}
	_ = os.Chmod(path, 0600)
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0600)
	}
	store := &tmdbCatalogStore{db: db, path: path, maxBytes: int64(s.cfg.TMDBCatalogMaxMB) << 20, facets: make(map[string]tmdbCatalogFacets), status: CatalogSyncStatus{Enabled: true, Stage: "starting"}}
	store.refreshStatus(ctx)
	s.fullCatalog = store
	s.fullCatalogError = ""
	return store, nil
}

func (s *Service) FullCatalogStatus() *CatalogSyncStatus {
	if !s.cfg.TMDBCatalogSyncEnabled {
		return &CatalogSyncStatus{Enabled: false, Stage: "disabled"}
	}
	s.fullCatalogMu.Lock()
	store, safeError := s.fullCatalog, s.fullCatalogError
	s.fullCatalogMu.Unlock()
	if store == nil {
		stage := "starting"
		if safeError != "" {
			stage = "error"
		}
		return &CatalogSyncStatus{Enabled: true, Stage: stage, Error: safeError}
	}
	store.statusMu.RLock()
	status := store.status
	store.statusMu.RUnlock()
	return &status
}

func (store *tmdbCatalogStore) setStage(stage, safeError string) {
	store.statusMu.Lock()
	store.status.Stage = stage
	store.status.Error = safeError
	store.statusMu.Unlock()
}

func (store *tmdbCatalogStore) refreshStatus(ctx context.Context) {
	var indexed, hydrated int
	if store.db.QueryRowContext(ctx, "SELECT indexed,hydrated FROM counters WHERE id=1").Scan(&indexed, &hydrated) != nil {
		return
	}
	var updated int64
	_ = store.db.QueryRowContext(ctx, "SELECT CAST(value AS INTEGER) FROM sync_meta WHERE key='last_updated'").Scan(&updated)
	store.statusMu.Lock()
	store.status.Indexed = indexed
	store.status.Hydrated = hydrated
	if updated > 0 {
		store.status.UpdatedAt = time.Unix(updated, 0).UTC()
	}
	store.statusMu.Unlock()
}

func (store *tmdbCatalogStore) meta(ctx context.Context, key string) string {
	var value string
	_ = store.db.QueryRowContext(ctx, "SELECT value FROM sync_meta WHERE key=?", key).Scan(&value)
	return value
}
func setCatalogMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO sync_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

func (store *tmdbCatalogStore) checkpoint(ctx context.Context) {
	// PASSIVE never blocks active readers. WAL frames are reused automatically.
	_, _ = store.db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
}

func (store *tmdbCatalogStore) invalidateFacets() {
	store.facetMu.Lock()
	store.facets = make(map[string]tmdbCatalogFacets)
	store.facetMu.Unlock()
}

func (s *Service) SeedFullCatalog(category string, items []Item) error {
	if !contains([]string{"movie", "tv", "animation", "variety", "documentary"}, category) {
		return errors.New("invalid discovery category")
	}
	store, err := s.openFullCatalog()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	// Existing bounded Douban snapshots may contain tens of thousands of rows;
	// commit small batches so HTTP readers and the importer can make progress.
	for start := 0; start < len(items); start += 256 {
		end := start + 256
		if end > len(items) {
			end = len(items)
		}
		if err = store.seedBatch(s.ctx, category, items[start:end]); err != nil {
			return err
		}
	}
	store.refreshStatus(s.ctx)
	store.invalidateFacets()
	return nil
}

func (store *tmdbCatalogStore) seedBatch(ctx context.Context, category string, items []Item) error {
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
	const statement = `INSERT INTO entries(key,media_type,tmdb_id,origin,title,original_title,norm_title,norm_original,year,release_date,poster_url,overview,genres_json,tags_json,douban_rating,douban_votes,douban_url,metadata_source,reason,season,hydrated,updated_at) VALUES(?,?,?,'douban',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET title=excluded.title,original_title=CASE WHEN excluded.original_title<>'' THEN excluded.original_title ELSE entries.original_title END,norm_title=excluded.norm_title,norm_original=CASE WHEN excluded.norm_original<>'' THEN excluded.norm_original ELSE entries.norm_original END,year=CASE WHEN excluded.year>0 THEN excluded.year ELSE entries.year END,release_date=CASE WHEN excluded.release_date<>'' THEN excluded.release_date ELSE entries.release_date END,poster_url=CASE WHEN entries.metadata_source='tmdb' AND entries.tmdb_id>0 THEN entries.poster_url WHEN excluded.poster_url<>'' THEN excluded.poster_url ELSE entries.poster_url END,overview=CASE WHEN entries.metadata_source='tmdb' AND entries.tmdb_id>0 THEN entries.overview WHEN excluded.overview<>'' THEN excluded.overview ELSE entries.overview END,genres_json=CASE WHEN entries.metadata_source='tmdb' AND entries.tmdb_id>0 THEN entries.genres_json WHEN excluded.genres_json<>'[]' THEN excluded.genres_json ELSE entries.genres_json END,tags_json=excluded.tags_json,douban_rating=COALESCE(excluded.douban_rating,entries.douban_rating),douban_votes=COALESCE(excluded.douban_votes,entries.douban_votes),douban_url=excluded.douban_url,reason=excluded.reason,season=excluded.season,hydrated=MAX(entries.hydrated,excluded.hydrated),updated_at=excluded.updated_at`
	stmt, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return errors.New("full catalog write failed")
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, item := range items {
		if !idPattern.MatchString(item.ID) || item.Title == "" || !contains([]string{"movie", "tv"}, item.MediaType) {
			continue
		}
		genres, _ := json.Marshal(nonNilStrings(item.Genres))
		tags, _ := json.Marshal(nonNilStrings(item.Tags))
		year, _ := strconv.Atoi(item.Year)
		if year < 1800 || year > time.Now().UTC().Year() {
			year = 0
		}
		_, season := splitSeason(item.Title)
		hydrated := 0
		if item.DetailsCached {
			hydrated = 1
		}
		source := item.MetadataSource
		if source == "" {
			source = "douban"
		}
		_, err = stmt.ExecContext(ctx, item.ID, item.MediaType, item.TMDBID, item.Title, item.OriginalTitle, normalizeTitle(item.Title), normalizeTitle(item.OriginalTitle), year, item.ReleaseDate, item.PosterURL, item.Overview, string(genres), string(tags), item.DoubanRating, item.DoubanVotes, item.DoubanURL, source, item.Reason, season, hydrated, now)
		if err != nil {
			return errors.New("full catalog write failed")
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO entry_categories(key,category,source) VALUES(?,?,'douban')", item.ID, category); err != nil {
			return errors.New("full catalog write failed")
		}
		snapshotJSON, _ := json.Marshal(item)
		if _, err = tx.ExecContext(ctx, "UPDATE entries SET douban_snapshot=? WHERE key=?", string(snapshotJSON), item.ID); err != nil {
			return errors.New("full catalog write failed")
		}
		var finalGenres, finalTags string
		if err = tx.QueryRowContext(ctx, "SELECT genres_json,tags_json FROM entries WHERE key=?", item.ID).Scan(&finalGenres, &finalTags); err != nil {
			return errors.New("full catalog write failed")
		}
		_ = json.Unmarshal([]byte(finalGenres), &item.Genres)
		_ = json.Unmarshal([]byte(finalTags), &item.Tags)
		if err = writeCatalogFacets(ctx, tx, item); err != nil {
			return errors.New("full catalog write failed")
		}

		if season == 0 && year > 0 {
			keys, queryErr := catalogMatchingKeys(ctx, tx, "tmdb", item.MediaType, year, normalizeTitle(item.Title), normalizeTitle(item.OriginalTitle))
			if queryErr != nil {
				return errors.New("full catalog write failed")
			}
			var matching []Item
			for _, key := range keys {
				candidate, scanErr := scanFullItem(tx.QueryRowContext(ctx, "SELECT "+fullItemColumns+" FROM entries e WHERE e.key=? AND e.hydrated=1", key))
				if scanErr == sql.ErrNoRows {
					continue
				}
				if scanErr != nil {
					return errors.New("full catalog write failed")
				}
				matching = append(matching, candidate)
			}
			for _, candidate := range matching {
				if err = store.matchDouban(ctx, tx, candidate, year); err != nil {
					return errors.New("full catalog write failed")
				}
			}
		}
	}
	if err = setCatalogMeta(ctx, tx, "last_updated", strconv.FormatInt(now, 10)); err != nil {
		return errors.New("full catalog write failed")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("full catalog write failed")
	}
	return nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func writeCatalogFacets(ctx context.Context, tx *sql.Tx, item Item) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM entry_genres WHERE key=?", item.ID); err != nil {
		return err
	}
	for _, genre := range uniqueStrings(item.Genres) {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO entry_genres(key,genre) VALUES(?,?)", item.ID, genre); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM entry_tags WHERE key=?", item.ID); err != nil {
		return err
	}
	for _, tag := range uniqueStrings(item.Tags) {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO entry_tags(key,tag) VALUES(?,?)", item.ID, tag); err != nil {
			return err
		}
	}
	return nil
}

const fullItemColumns = `e.key,e.title,e.original_title,e.year,e.release_date,e.poster_url,e.overview,e.genres_json,e.tags_json,e.douban_rating,e.douban_votes,e.douban_url,e.tmdb_id,e.metadata_source,e.reason,e.media_type,e.hydrated`

type catalogScanner interface{ Scan(...any) error }

func scanFullItem(row catalogScanner) (Item, error) {
	var item Item
	var year, hydrated int
	var genres, tags string
	var rating sql.NullFloat64
	var votes sql.NullInt64
	err := row.Scan(&item.ID, &item.Title, &item.OriginalTitle, &year, &item.ReleaseDate, &item.PosterURL, &item.Overview, &genres, &tags, &rating, &votes, &item.DoubanURL, &item.TMDBID, &item.MetadataSource, &item.Reason, &item.MediaType, &hydrated)
	if err != nil {
		return Item{}, err
	}
	if year > 0 {
		item.Year = strconv.Itoa(year)
	}
	_ = json.Unmarshal([]byte(genres), &item.Genres)
	_ = json.Unmarshal([]byte(tags), &item.Tags)
	if item.Genres == nil {
		item.Genres = []string{}
	}
	if rating.Valid {
		value := rating.Float64
		item.DoubanRating = &value
	}
	if votes.Valid {
		value := int(votes.Int64)
		item.DoubanVotes = &value
	}
	item.DetailsCached = hydrated == 1
	return item, nil
}

func fullCatalogKey(mediaType string, id int) string { return fmt.Sprintf("tmdb:%s:%d", mediaType, id) }
func parseFullCatalogKey(value, mediaType string) (int, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "tmdb" || parts[1] != mediaType || !contains([]string{"movie", "tv"}, mediaType) {
		return 0, false
	}
	id, err := strconv.Atoi(parts[2])
	return id, err == nil && id > 0 && int64(id) <= 2147483647 && strconv.Itoa(id) == parts[2]
}
