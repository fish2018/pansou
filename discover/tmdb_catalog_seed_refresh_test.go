package discover

import (
	"fmt"
	"testing"
	"time"
)

func seedSnapshotForTest(t *testing.T, s *Service, category string, updated time.Time, items []Item) {
	t.Helper()
	s.saveCatalog(category, catalogSnapshot{Version: 1, Items: items, Sources: map[string]catalogCursor{}, UpdatedAt: updated})
}

func catalogChangesForTest(t *testing.T, store *tmdbCatalogStore) int {
	t.Helper()
	var changes int
	if err := store.db.QueryRow("SELECT total_changes()").Scan(&changes); err != nil {
		t.Fatal(err)
	}
	return changes
}

func TestFullCatalogSeedUnchangedSnapshotDoesNotWrite(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	items := make([]Item, 513)
	for index := range items {
		items[index] = Item{ID: fmt.Sprintf("%d", index+1), Title: fmt.Sprintf("Series %d", index+1), MediaType: "tv"}
	}
	if err := s.SeedFullCatalog("tv", items); err != nil {
		t.Fatal(err)
	}
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	// total_changes is connection-local. Keep one connection for this assertion;
	// 513 items also exercises the boundary between two batched lookup queries.
	store.db.SetMaxOpenConns(1)
	seedSnapshotForTest(t, s, "tv", time.Now().Add(-time.Minute), items)
	before := catalogChangesForTest(t, store)
	s.seedExistingFullCatalog()
	if after := catalogChangesForTest(t, store); after != before {
		t.Fatalf("unchanged snapshot caused SQLite writes: before=%d after=%d", before, after)
	}
}

func TestFullCatalogSeedNewSnapshotUpdatesRatingAndKeepsSeason(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	rating := 8.1
	item := Item{ID: "100", Title: "测试剧 第二季", MediaType: "tv", Year: "2026", DoubanRating: &rating}
	if err := s.SeedFullCatalog("tv", []Item{item}); err != nil {
		t.Fatal(err)
	}
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	store.db.SetMaxOpenConns(1)
	if _, err = store.db.Exec("UPDATE entries SET updated_at=? WHERE key='100'", time.Now().Add(-2*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	rating = 9.2
	seedSnapshotForTest(t, s, "tv", time.Now().Add(-time.Minute), []Item{item})
	s.seedExistingFullCatalog()
	var actual float64
	var season int
	if err = store.db.QueryRow("SELECT douban_rating,season FROM entries WHERE key='100'").Scan(&actual, &season); err != nil || actual != 9.2 || season != 2 {
		t.Fatalf("new snapshot did not refresh rating or flattened season: rating=%v season=%d err=%v", actual, season, err)
	}
	before := catalogChangesForTest(t, store)
	s.seedExistingFullCatalog()
	if after := catalogChangesForTest(t, store); after != before {
		t.Fatal("already merged new snapshot was written again")
	}
}

func TestFullCatalogSeedFillsMissingIDAndCategoryOnly(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	rating := 8.8
	current := Item{ID: "101", Title: "已有剧", MediaType: "tv"}
	missing := Item{ID: "102", Title: "新收录", MediaType: "tv"}
	otherCategory := Item{ID: "103", Title: "动画剧 第三季", MediaType: "tv", DoubanRating: &rating}
	if err := s.SeedFullCatalog("tv", []Item{current}); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedFullCatalog("animation", []Item{otherCategory}); err != nil {
		t.Fatal(err)
	}
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`CREATE TABLE seed_writes(key TEXT);
CREATE TRIGGER record_seed_update AFTER UPDATE ON entries BEGIN INSERT INTO seed_writes VALUES(new.key); END;
CREATE TRIGGER record_seed_insert AFTER INSERT ON entries BEGIN INSERT INTO seed_writes VALUES(new.key); END;`); err != nil {
		t.Fatal(err)
	}
	seedSnapshotForTest(t, s, "tv", time.Now().Add(-time.Minute), []Item{current, missing, otherCategory})
	s.seedExistingFullCatalog()
	var count int
	if err = store.db.QueryRow("SELECT COUNT(*) FROM entry_categories WHERE category='tv' AND source='douban'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("missing ID or category was not seeded: count=%d err=%v", count, err)
	}
	if err = store.db.QueryRow("SELECT COUNT(*) FROM seed_writes WHERE key='101'").Scan(&count); err != nil || count != 0 {
		t.Fatal("unmodified existing category member was rewritten")
	}
	var actual float64
	var season int
	if err = store.db.QueryRow("SELECT douban_rating,season FROM entries WHERE key='103'").Scan(&actual, &season); err != nil || actual != 8.8 || season != 3 {
		t.Fatal("category repair lost the Douban rating or season")
	}
}
