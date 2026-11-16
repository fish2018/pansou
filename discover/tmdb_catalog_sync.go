package discover

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const tmdbExportBase = "https://files.tmdb.org/p/exports/"

type tmdbExportRecord struct {
	ID            int     `json:"id"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	Popularity    float64 `json:"popularity"`
	Adult         bool    `json:"adult"`
}

type catalogCountingReader struct {
	reader io.Reader
	count  int64
}

func (reader *catalogCountingReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	reader.count += int64(n)
	return n, err
}

// StartFullCatalog has its own two bounded hydration workers. The ID import is
// a third streaming worker and does not consume the homepage's refresh queues.
func (s *Service) StartFullCatalog() {
	if !s.cfg.TMDBCatalogSyncEnabled {
		return
	}
	s.lifecycleMu.Lock()
	if s.closed || s.fullCatalogStarted {
		s.lifecycleMu.Unlock()
		return
	}
	s.fullCatalogStarted = true
	s.workers.Add(1)
	s.lifecycleMu.Unlock()
	go func() {
		defer s.workers.Done()
		store, err := s.openFullCatalog()
		if err != nil {
			return
		}
		if !waitCatalog(s.ctx, time.Second) {
			return
		}
		_ = store.refreshDatedVisibility(s.ctx)
		if !s.TMDBConfigured() {
			s.seedExistingFullCatalog()
			store.setStage("paused", "credentials_required")
			return
		}
		s.lifecycleMu.Lock()
		if s.closed {
			s.lifecycleMu.Unlock()
			return
		}
		s.workers.Add(2)
		go s.runFullHydration(store)
		go s.runFullHydration(store)
		s.lifecycleMu.Unlock()
		for s.ctx.Err() == nil {
			_ = store.refreshDatedVisibility(s.ctx)
			s.seedExistingFullCatalog()
			if store.checkCapacity() == nil {
				store.setStage("indexing", "")
				successful := true
				for _, media := range []string{"movie", "tv"} {
					if err = s.syncFullExport(s.ctx, store, media, time.Now().UTC()); err != nil {
						successful = false
						if errors.Is(err, errFullCatalogCapacity) {
							break
						}
						if s.ctx.Err() == nil {
							store.setStage("error", "export_unavailable_or_incomplete")
						}
					}
				}
				if successful {
					store.setStage("updating", "")
					for _, media := range []string{"movie", "tv"} {
						if s.syncFullChanges(s.ctx, store, media) != nil {
							successful = false
							if s.ctx.Err() == nil {
								store.setStage("error", "changes_temporarily_unavailable")
							}
							break
						}
					}
				}
				if successful {
					store.setStage("ready", "")
				}
				store.refreshStatus(s.ctx)
				store.invalidateFacets()
				store.checkpoint(s.ctx)
			}
			if !waitCatalog(s.ctx, time.Hour) {
				return
			}
		}
	}()
}

func waitCatalog(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) seedExistingFullCatalog() {
	for _, category := range []string{"movie", "tv", "animation", "variety", "documentary"} {
		if s.ctx.Err() != nil {
			return
		}
		if snapshot, ok := s.loadCatalog(category); ok {
			_ = s.SeedFullCatalog(category, snapshot.Items)
		}
	}
}

func (s *Service) syncFullExport(ctx context.Context, store *tmdbCatalogStore, media string, now time.Time) error {
	date := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if now.Hour() < 8 {
		date = date.AddDate(0, 0, -1)
	}
	for fallback := 0; fallback < 3; fallback++ {
		day := date.AddDate(0, 0, -fallback)
		dateKey := day.Format("2006-01-02")
		if store.meta(ctx, "export_done:"+media) == dateKey {
			return nil
		}
		err := s.importFullExport(ctx, store, media, day)
		var httpError fullCatalogHTTPError
		if errors.As(err, &httpError) && httpError.status == 404 {
			continue
		}
		return err
	}
	return errors.New("catalog export unavailable")
}

func (s *Service) importFullExport(ctx context.Context, store *tmdbCatalogStore, media string, date time.Time) error {
	if !contains([]string{"movie", "tv"}, media) {
		return errors.New("invalid catalog media type")
	}
	prefix := media + "_ids_"
	if media == "tv" {
		prefix = "tv_series_ids_"
	}
	dateKey := date.Format("2006-01-02")
	address := tmdbExportBase + prefix + date.Format("01_02_2006") + ".json.gz"
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("catalog export unavailable")
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "PanSou discovery catalog")
	// No API Authorization is ever attached to files.tmdb.org. The shared
	// transport keeps its connection/header timeouts, while the whole stream has
	// a separate 15-minute context instead of the 15-second JSON client timeout.
	client := *s.client
	client.Timeout = 0
	if transport, ok := client.Transport.(*http.Transport); ok {
		isolated := transport.Clone()
		isolated.ResponseHeaderTimeout = 30 * time.Second
		client.Transport = isolated
		defer isolated.CloseIdleConnections()
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("catalog export unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fullCatalogHTTPError{status: response.StatusCode}
	}
	if response.ContentLength > 256<<20 {
		return errors.New("catalog export too large")
	}
	if err = store.checkCapacity(); err != nil {
		return err
	}
	compressed, err := os.CreateTemp(filepath.Dir(store.path), ".tmdb-export-*.json.gz")
	if err != nil {
		return errors.New("catalog export download failed")
	}
	defer os.Remove(compressed.Name())
	defer compressed.Close()
	copied, copyErr := io.Copy(compressed, io.LimitReader(response.Body, (256<<20)+1))
	if copyErr != nil || copied > 256<<20 || (response.ContentLength >= 0 && copied != response.ContentLength) {
		return errors.New("catalog export incomplete")
	}
	// Download quickly before SQLite import. Slow indexed commits no longer
	// hold the provider connection open or consume the download's timeout.
	_ = response.Body.Close()
	cancel()
	if _, err = compressed.Seek(0, io.SeekStart); err != nil {
		return errors.New("catalog export download failed")
	}
	zip, err := gzip.NewReader(compressed)
	if err != nil {
		return errors.New("catalog export incomplete")
	}
	defer zip.Close()
	uncompressed := &catalogCountingReader{reader: io.LimitReader(zip, (1<<30)+1)}
	scanner := bufio.NewScanner(uncompressed)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	cursorKey := "export_cursor:" + media + ":" + dateKey
	cursor, _ := strconv.ParseInt(store.meta(ctx, cursorKey), 10, 64)
	var line int64
	batch := make([]tmdbExportRecord, 0, 512)
	lastCheckpoint := cursor
	for scanner.Scan() {
		line++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if line <= cursor {
			continue
		}
		var record tmdbExportRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.ID <= 0 {
			return errors.New("catalog export invalid")
		}
		if !record.Adult {
			batch = append(batch, record)
		}
		if line-lastCheckpoint >= 512 {
			if err = store.importExportBatch(ctx, media, dateKey, cursorKey, line, batch); err != nil {
				return err
			}
			lastCheckpoint = line
			batch = batch[:0]
			store.refreshStatus(ctx)
		}
	}
	if scanner.Err() != nil || uncompressed.count > 1<<30 {
		return errors.New("catalog export incomplete")
	}
	// The scanner must consume gzip through EOF: its CRC and uncompressed-size
	// trailer are checked before the completion marker can ever be committed.
	if line == 0 {
		return errors.New("catalog export empty")
	}
	if line < cursor {
		return errors.New("catalog export changed during resume")
	}
	if line > lastCheckpoint {
		if err = store.importExportBatch(ctx, media, dateKey, cursorKey, line, batch); err != nil {
			return err
		}
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("full catalog write failed")
	}
	defer tx.Rollback()
	for key, value := range map[string]string{"export_done:" + media: dateKey, "last_updated": strconv.FormatInt(time.Now().Unix(), 10)} {
		if err = setCatalogMeta(ctx, tx, key, value); err != nil {
			return errors.New("full catalog write failed")
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM sync_meta WHERE key LIKE ? AND key<>?", "export_cursor:"+media+":%", cursorKey); err != nil {
		return errors.New("full catalog write failed")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("full catalog write failed")
	}
	store.refreshStatus(ctx)
	store.invalidateFacets()
	return nil
}

func (store *tmdbCatalogStore) importExportBatch(ctx context.Context, media, date, cursorKey string, line int64, records []tmdbExportRecord) error {
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
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO entries(key,media_type,tmdb_id,origin,title,original_title,norm_title,norm_original,popularity,export_date) VALUES(?,?,?,'tmdb',?,?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET title=CASE WHEN entries.hydrated=0 THEN excluded.title ELSE entries.title END,original_title=excluded.original_title,norm_title=CASE WHEN entries.hydrated=0 THEN excluded.norm_title ELSE entries.norm_title END,norm_original=excluded.norm_original,popularity=excluded.popularity,export_date=excluded.export_date,removed=0`)
	if err != nil {
		return errors.New("full catalog write failed")
	}
	defer stmt.Close()
	category, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO entry_categories(key,category,source) VALUES(?,?,'tmdb')")
	if err != nil {
		return errors.New("full catalog write failed")
	}
	defer category.Close()
	for _, record := range records {
		title := record.OriginalTitle
		if media == "tv" {
			title = record.OriginalName
		}
		if title == "" {
			title = fmt.Sprintf("TMDB #%d", record.ID)
		}
		key := fullCatalogKey(media, record.ID)
		normalized := normalizeTitle(title)
		if _, err = stmt.ExecContext(ctx, key, media, record.ID, title, title, normalized, normalized, record.Popularity, date); err != nil {
			return errors.New("full catalog write failed")
		}
		if _, err = category.ExecContext(ctx, key, media); err != nil {
			return errors.New("full catalog write failed")
		}
	}
	if err = setCatalogMeta(ctx, tx, cursorKey, strconv.FormatInt(line, 10)); err != nil {
		return errors.New("full catalog write failed")
	}
	if err = setCatalogMeta(ctx, tx, "last_updated", strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return errors.New("full catalog write failed")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("full catalog write failed")
	}
	return nil
}

// TMDB's changes API accepts a maximum 14-day range. Persistent per-page
// checkpoints make retries idempotent and keep a failed page from skipping IDs.
func (s *Service) syncFullChanges(ctx context.Context, store *tmdbCatalogStore, media string) error {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	lastPoll, _ := strconv.ParseInt(store.meta(ctx, "changes_polled:"+media), 10, 64)
	if time.Since(time.Unix(lastPoll, 0)) < 55*time.Minute {
		return nil
	}
	start := now.AddDate(0, 0, -13)
	if completed, err := time.Parse("2006-01-02", store.meta(ctx, "changes_done:"+media)); err == nil && completed.After(start) {
		start = completed.AddDate(0, 0, 1)
	}
	if start.Format("2006-01-02") > today {
		start = now
	}
	for day := start; day.Format("2006-01-02") <= today; day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		if err := s.syncFullChangesDay(ctx, store, media, date, date < today); err != nil {
			return err
		}
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	_, err := store.db.ExecContext(ctx, "INSERT INTO sync_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "changes_polled:"+media, strconv.FormatInt(now.Unix(), 10))
	if err != nil {
		return errors.New("full catalog write failed")
	}
	return nil
}

func (s *Service) syncFullChangesDay(ctx context.Context, store *tmdbCatalogStore, media, date string, completedDay bool) error {
	cursorKey := "changes_cursor:" + media + ":" + date
	page, _ := strconv.Atoi(store.meta(ctx, cursorKey))
	if page < 1 {
		page = 1
	}
	for ; page <= 500; page++ {
		if err := store.checkCapacity(); err != nil {
			return err
		}
		var result struct {
			Results []struct {
				ID    int  `json:"id"`
				Adult bool `json:"adult"`
			} `json:"results"`
			Page       int `json:"page"`
			TotalPages int `json:"total_pages"`
		}
		requestCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := s.fullCatalogJSON(requestCtx, store, "/"+media+"/changes", url.Values{"start_date": {date}, "end_date": {date}, "page": {strconv.Itoa(page)}}, &result)
		cancel()
		if err != nil {
			return err
		}
		if result.TotalPages > 500 || result.Page != page || (result.TotalPages > 0 && page > result.TotalPages) {
			return errors.New("catalog changes pagination invalid")
		}
		store.writeMu.Lock()
		tx, txErr := store.db.BeginTx(ctx, nil)
		if txErr == nil {
			for _, changed := range result.Results {
				if changed.ID <= 0 {
					continue
				}
				key := fullCatalogKey(media, changed.ID)
				if changed.Adult {
					_, txErr = tx.ExecContext(ctx, "UPDATE entries SET removed=1,needs_refresh=0 WHERE key=?", key)
				} else {
					_, txErr = tx.ExecContext(ctx, "UPDATE entries SET needs_refresh=1,retry_at=0,removed=0 WHERE key=? AND origin='tmdb'", key)
				}
				if txErr != nil {
					break
				}
			}
			last := result.TotalPages == 0 || page >= result.TotalPages
			if txErr == nil {
				txErr = setCatalogMeta(ctx, tx, cursorKey, strconv.Itoa(page+1))
			}
			if txErr == nil && last {
				if completedDay {
					txErr = setCatalogMeta(ctx, tx, "changes_done:"+media, date)
				}
				if txErr == nil {
					_, txErr = tx.ExecContext(ctx, "DELETE FROM sync_meta WHERE key=?", cursorKey)
				}
			}
			if txErr == nil {
				txErr = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		store.writeMu.Unlock()
		if txErr != nil {
			return errors.New("full catalog write failed")
		}
		if result.TotalPages == 0 || page >= result.TotalPages {
			return nil
		}
	}
	return errors.New("catalog changes pagination invalid")
}
