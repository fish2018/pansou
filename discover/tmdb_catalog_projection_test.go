package discover

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func projectionTestStore(t *testing.T) *tmdbCatalogStore {
	t.Helper()
	s := New(Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	t.Cleanup(s.Close)
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func projectionTestEntry(t *testing.T, store *tmdbCatalogStore, name string, year int, date string, removed bool, mapped string, categories ...string) string {
	t.Helper()
	key := "tmdb:movie:" + name
	_, err := store.db.Exec(`INSERT INTO entries(key,media_type,tmdb_id,origin,title,norm_title,year,release_date,popularity,removed,mapped_key) VALUES(?,'movie',1,'tmdb',?,?,?, ?,5,?,?)`, key, name, name, year, date, removed, mapped)
	if err != nil {
		t.Fatal(err)
	}
	for _, category := range categories {
		if _, err = store.db.Exec("INSERT INTO entry_categories(key,category,source) VALUES(?,?,'tmdb')", key, category); err != nil {
			t.Fatal(err)
		}
	}
	return key
}

func projectionCategoryCount(t *testing.T, store *tmdbCatalogStore, category string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow("SELECT COALESCE((SELECT total FROM category_counts WHERE category=?),0)", category).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestFullCatalogDatedVisibilityPromotesReleaseAndYear(t *testing.T) {
	store := projectionTestStore(t)
	ctx := context.Background()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	due := today.AddDate(0, 0, 1)
	later := today.AddDate(0, 0, 2)
	dateKey := projectionTestEntry(t, store, "date", due.Year(), due.Format("2006-01-02"), false, "", "movie", "animation")
	// Duplicate source memberships still represent one visible category row.
	if _, err := store.db.Exec("INSERT INTO entry_categories(key,category,source) VALUES(?,'movie','douban')", dateKey); err != nil {
		t.Fatal(err)
	}
	projectionTestEntry(t, store, "later", later.Year(), later.Format("2006-01-02"), false, "", "movie")
	projectionTestEntry(t, store, "year", today.Year()+1, "", false, "", "movie")
	projectionTestEntry(t, store, "mismatch", today.Year()+1, today.AddDate(0, 0, -1).Format("2006-01-02"), false, "", "movie")
	projectionTestEntry(t, store, "removed", due.Year(), due.Format("2006-01-02"), true, "", "movie")
	projectionTestEntry(t, store, "mapped", due.Year(), due.Format("2006-01-02"), false, "1292052", "movie")
	projectionTestEntry(t, store, "uncategorized", due.Year(), due.Format("2006-01-02"), false, "")

	if err := store.refreshDatedVisibilityAt(ctx, today); err != nil {
		t.Fatal(err)
	}
	if count := projectionCategoryCount(t, store, "movie"); count != 0 {
		t.Fatalf("future movie visible before release: %d", count)
	}
	if err := store.refreshDatedVisibilityAt(ctx, due); err != nil {
		t.Fatal(err)
	}
	wantDue := 1
	if due.Year() > today.Year() {
		// December 31 also crosses the year boundary in this first step.
		wantDue += 2
	}
	if count := projectionCategoryCount(t, store, "movie"); count != wantDue {
		t.Fatalf("movie count on due date = %d, want %d", count, wantDue)
	}
	if count := projectionCategoryCount(t, store, "animation"); count != 1 {
		t.Fatalf("animation count on due date = %d, want 1", count)
	}
	// Repeating the same day must not double-count or rewrite memberships.
	if err := store.refreshDatedVisibilityAt(ctx, due); err != nil {
		t.Fatal(err)
	}
	if count := projectionCategoryCount(t, store, "movie"); count != wantDue {
		t.Fatalf("repeat changed movie count: %d", count)
	}

	yearDue := time.Date(today.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
	if yearDue.Before(later) {
		yearDue = later
	}
	if err := store.refreshDatedVisibilityAt(ctx, yearDue); err != nil {
		t.Fatal(err)
	}
	if count := projectionCategoryCount(t, store, "movie"); count != 4 {
		t.Fatalf("movie count after year/date transitions = %d, want 4", count)
	}
}

func TestFullCatalogDatedVisibilityRepairsFirstRun(t *testing.T) {
	store := projectionTestStore(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	key := projectionTestEntry(t, store, "past", today.Year(), today.AddDate(0, 0, -1).Format("2006-01-02"), false, "", "movie")
	if _, err := store.db.Exec("DELETE FROM visible_categories WHERE key=?", key); err != nil {
		t.Fatal(err)
	}
	if err := store.refreshDatedVisibilityAt(context.Background(), today); err != nil {
		t.Fatal(err)
	}
	if count := projectionCategoryCount(t, store, "movie"); count != 1 {
		t.Fatalf("first run did not repair missing membership: %d", count)
	}
}

func TestFullCatalogDatedVisibilityRetriesAfterCancellation(t *testing.T) {
	store := projectionTestStore(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	tomorrow := today.AddDate(0, 0, 1)
	projectionTestEntry(t, store, "cancel", tomorrow.Year(), tomorrow.Format("2006-01-02"), false, "", "movie")
	if err := store.refreshDatedVisibilityAt(context.Background(), today); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.refreshDatedVisibilityAt(ctx, tomorrow); err == nil {
		t.Fatal("cancelled refresh succeeded")
	}
	if marker := store.meta(context.Background(), "visibility_date"); marker != today.Format("2006-01-02") {
		t.Fatal(fmt.Sprintf("cancelled refresh advanced marker to %q", marker))
	}
	if err := store.refreshDatedVisibilityAt(context.Background(), tomorrow); err != nil {
		t.Fatal(err)
	}
	if count := projectionCategoryCount(t, store, "movie"); count != 1 {
		t.Fatalf("retry did not promote due movie: %d", count)
	}
}
