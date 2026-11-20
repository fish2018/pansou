package discover

import (
	"testing"
	"time"
)

func TestFullCatalogSeedSharedCategoriesUseNewerSnapshot(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	initial := 7.0
	item := Item{ID: "1001", Title: "共享动画剧 第二季", Year: "2026", MediaType: "tv", DoubanRating: &initial}
	for _, category := range []string{"tv", "animation"} {
		if err := s.SeedFullCatalog(category, []Item{item}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.db.Exec("UPDATE entries SET updated_at=? WHERE key='1001'", now.Add(-5*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	olderRating, newerRating := 8.1, 9.2
	older, newer := item, item
	older.DoubanRating = &olderRating
	newer.DoubanRating = &newerRating
	seedSnapshotForTest(t, s, "tv", now.Add(-4*time.Minute), []Item{older})
	seedSnapshotForTest(t, s, "animation", now.Add(-time.Minute), []Item{newer})
	s.seedExistingFullCatalog()
	assertSeedScore := func() {
		t.Helper()
		var score float64
		var season int
		if err := store.db.QueryRow("SELECT douban_rating,season FROM entries WHERE key='1001'").Scan(&score, &season); err != nil || score != newerRating || season != 2 {
			t.Fatalf("newer source skipped or season lost: score=%v season=%d err=%v", score, season, err)
		}
	}
	assertSeedScore()
	// A later TV snapshot is still older than the already applied animation
	// snapshot; applying that category must not regress the shared row.
	intermediateRating := 8.5
	older.DoubanRating = &intermediateRating
	seedSnapshotForTest(t, s, "tv", now.Add(-2*time.Minute), []Item{older})
	s.seedExistingFullCatalog()
	assertSeedScore()
	if got := store.meta(s.ctx, snapshotSeedKey("tv")); got != now.Add(-2*time.Minute).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("TV source watermark not advanced: %q", got)
	}
}

func TestFullCatalogSeedLegacyAdoptionDoesNotRewriteEntries(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	item := Item{ID: "1002", Title: "旧版已收录剧", MediaType: "tv"}
	if err := s.SeedFullCatalog("tv", []Item{item}); err != nil {
		t.Fatal(err)
	}
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	store.db.SetMaxOpenConns(1)
	if _, err = store.db.Exec(`CREATE TABLE seed_audit(key TEXT); CREATE TRIGGER audit_entry_seed AFTER UPDATE ON entries BEGIN INSERT INTO seed_audit VALUES(new.key); END;`); err != nil {
		t.Fatal(err)
	}
	seedSnapshotForTest(t, s, "tv", time.Now().Add(-time.Minute), []Item{item})
	s.seedExistingFullCatalog()
	var count int
	if err = store.db.QueryRow("SELECT COUNT(*) FROM seed_audit").Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy adoption rewrote data rows: %d %v", count, err)
	}
	if store.meta(s.ctx, snapshotSeedKey("tv")) == "" {
		t.Fatal("legacy source watermark not adopted")
	}
	before := catalogChangesForTest(t, store)
	s.seedExistingFullCatalog()
	if after := catalogChangesForTest(t, store); after != before {
		t.Fatal("unchanged source rewrote SQLite after adoption")
	}
	// Even with a current watermark, a missing category membership must heal.
	if _, err = store.db.Exec("DELETE FROM entry_categories WHERE key='1002' AND category='tv'"); err != nil {
		t.Fatal(err)
	}
	s.seedExistingFullCatalog()
	if err = store.db.QueryRow("SELECT COUNT(*) FROM entry_categories WHERE key='1002' AND category='tv' AND source='douban'").Scan(&count); err != nil || count != 1 {
		t.Fatal("current watermark prevented category repair")
	}
}

func TestFullCatalogSeedFailureNeverAdvancesSourceWatermark(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Now().Add(-time.Minute)
	seedSnapshotForTest(t, s, "tv", updated, []Item{{ID: "1003", Title: "待收录剧", MediaType: "tv"}})
	if _, err = store.db.Exec(`CREATE TRIGGER fail_seed BEFORE INSERT ON entries WHEN new.key='1003' BEGIN SELECT RAISE(ABORT,'test failure'); END;`); err != nil {
		t.Fatal(err)
	}
	s.seedExistingFullCatalog()
	if store.meta(s.ctx, snapshotSeedKey("tv")) != "" {
		t.Fatal("failed source merge was marked successful")
	}
	if _, err = store.db.Exec("DROP TRIGGER fail_seed"); err != nil {
		t.Fatal(err)
	}
	s.seedExistingFullCatalog()
	if got := store.meta(s.ctx, snapshotSeedKey("tv")); got != updated.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("retry did not complete source: %q", got)
	}
}

func TestFullCatalogSeedLegacyRecentArrivalRepairsOnlyNewestKnownScore(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	oldRating, newRating := 6.9, 7.0
	oldVotes, newVotes := 120, 140
	item := Item{ID: "1004", Title: "共享旧剧", MediaType: "tv", DoubanRating: &oldRating, DoubanVotes: &oldVotes}
	unchanged := Item{ID: "1005", Title: "已正确合入", MediaType: "tv", DoubanRating: &newRating, DoubanVotes: &newVotes}
	for _, category := range []string{"tv", "animation"} {
		if err := s.SeedFullCatalog(category, []Item{item, unchanged}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`CREATE TABLE score_seed_audit(key TEXT); CREATE TRIGGER audit_score_seed AFTER UPDATE ON entries BEGIN INSERT INTO score_seed_audit VALUES(new.key); END;`); err != nil {
		t.Fatal(err)
	}
	newer := item
	newer.DoubanRating = &newRating
	newer.DoubanVotes = &newVotes
	now := time.Now()
	seedSnapshotForTest(t, s, "tv", now.Add(-2*time.Minute), []Item{item, unchanged})
	seedSnapshotForTest(t, s, "animation", now.Add(-time.Minute), []Item{newer, unchanged})
	s.seedExistingFullCatalog()
	var rating float64
	var votes, count int
	if err = store.db.QueryRow("SELECT douban_rating,douban_votes FROM entries WHERE key='1004'").Scan(&rating, &votes); err != nil || rating != newRating || votes != newVotes {
		t.Fatalf("legacy arrival clock hid actual known-value changes: %v/%d err=%v", rating, votes, err)
	}
	if err = store.db.QueryRow("SELECT COUNT(*) FROM score_seed_audit WHERE key='1005'").Scan(&count); err != nil || count != 0 {
		t.Fatal("migration rewrote an already correct item")
	}
}
