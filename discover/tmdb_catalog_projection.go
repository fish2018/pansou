package discover

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// refreshDatedVisibility promotes scheduled works after their release date or
// year arrives. SQL triggers cannot observe the clock changing by itself.
func (store *tmdbCatalogStore) refreshDatedVisibility(ctx context.Context) error {
	return store.refreshDatedVisibilityAt(ctx, time.Now().UTC())
}

func (store *tmdbCatalogStore) refreshDatedVisibilityAt(ctx context.Context, now time.Time) error {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	now = now.UTC()
	today := now.Format("2006-01-02")
	previous := store.meta(ctx, "visibility_date")
	previousYear := 0
	if parsed, err := time.Parse("2006-01-02", previous); err == nil {
		if previous >= today {
			return nil
		}
		previousYear = parsed.Year()
	} else {
		previous = "0000-00-00"
	}
	if err := store.checkCapacity(); err != nil {
		return err
	}

	// This maintenance runs outside request handling. Bound its write-lock time;
	// a failed/cancelled attempt leaves the date marker unchanged for a retry.
	writeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(writeCtx, nil)
	if err != nil {
		return errors.New("full catalog visibility update failed")
	}
	defer tx.Rollback()

	const releaseQuery = `INSERT OR IGNORE INTO visible_categories(key,category,year,popularity)
SELECT e.key,c.category,e.year,e.popularity
FROM entries e INDEXED BY entries_release_date
JOIN entry_categories c ON c.key=e.key
WHERE e.release_date>? AND e.release_date<=?
 AND e.removed=0 AND e.mapped_key='' AND (e.year=0 OR e.year<=?)
 AND NOT EXISTS(SELECT 1 FROM visible_categories v WHERE v.key=e.key AND v.category=c.category)`
	result, err := tx.ExecContext(writeCtx, releaseQuery, previous, today, now.Year())
	if err != nil {
		return errors.New("full catalog visibility update failed")
	}
	changed, _ := result.RowsAffected()

	// Year-only records, and records with an earlier release date but a future
	// catalog year, need a separate indexed range when January arrives.
	const yearQuery = `INSERT OR IGNORE INTO visible_categories(key,category,year,popularity)
SELECT e.key,c.category,e.year,e.popularity
FROM entries e INDEXED BY entries_year
JOIN entry_categories c ON c.key=e.key
WHERE e.year>? AND e.year<=?
 AND e.removed=0 AND e.mapped_key='' AND (e.release_date='' OR e.release_date<=?)
 AND NOT EXISTS(SELECT 1 FROM visible_categories v WHERE v.key=e.key AND v.category=c.category)`
	result, err = tx.ExecContext(writeCtx, yearQuery, previousYear, now.Year(), today)
	if err != nil {
		return errors.New("full catalog visibility update failed")
	}
	additional, _ := result.RowsAffected()
	changed += additional
	if err = setCatalogMeta(writeCtx, tx, "visibility_date", today); err != nil {
		return errors.New("full catalog visibility update failed")
	}
	if changed > 0 {
		if err = setCatalogMeta(writeCtx, tx, "last_updated", strconv.FormatInt(now.Unix(), 10)); err != nil {
			return errors.New("full catalog visibility update failed")
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("full catalog visibility update failed")
	}
	if changed > 0 {
		store.invalidateFacets()
		store.refreshStatus(ctx)
	}
	return nil
}
