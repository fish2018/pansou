package discover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func cacheAuditItem(id int) Item {
	return Item{ID: fullCatalogKey("movie", id), TMDBID: id, MediaType: "movie", Title: "缓存作品", OriginalTitle: "Cached work", Year: "2025", ReleaseDate: "2025-01-01", Overview: "已保存的完整简介", MetadataSource: "tmdb", Genres: []string{"剧情"}, DetailsCached: true}
}

func TestCatalogFacetsServeStaleAndShareOneRefresh(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	item := cacheAuditItem(11)
	if err = store.saveHydrated(s.ctx, item, 10, []string{"movie"}); err != nil {
		t.Fatal(err)
	}
	store.facets["movie"] = tmdbCatalogFacets{at: time.Now().Add(-2 * time.Minute), genres: []string{"旧类型"}, years: []string{"2024"}, tags: []string{}}
	// Occupy the pool: one background query may wait, but existing facets and all
	// concurrent visitors must remain available without starting duplicate SQL.
	connections := make([]*sql.Conn, 0, 4)
	for range 4 {
		c, e := store.db.Conn(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		connections = append(connections, c)
	}
	before := store.db.Stats().WaitCount
	started := time.Now()
	for range 20 {
		result, e := store.categoryFacets(context.Background(), "movie")
		if e != nil || len(result.genres) != 1 || result.genres[0] != "旧类型" {
			t.Fatalf("stale facets unavailable: %+v %v", result, e)
		}
	}
	if time.Since(started) > 200*time.Millisecond {
		t.Fatal("stale requests waited for facet SQL")
	}
	deadline := time.Now().Add(time.Second)
	for store.db.Stats().WaitCount == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := store.db.Stats().WaitCount - before; got != 1 {
		t.Fatalf("expected one shared refresh, got %d pool waiters", got)
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.facetMu.Lock()
		result := store.facets["movie"]
		store.facetMu.Unlock()
		if len(result.genres) == 1 && result.genres[0] == "剧情" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stale facets were never refreshed")
}

func TestCatalogFacetSnapshotSurvivesRestartAndInvalidation(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{CacheDir: dir, TMDBCatalogSyncEnabled: true})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.saveHydrated(s.ctx, cacheAuditItem(11), 10, []string{"movie"}); err != nil {
		t.Fatal(err)
	}
	result, err := store.categoryFacets(context.Background(), "movie")
	if err != nil || len(result.years) != 1 || result.years[0] != "2025" {
		t.Fatalf("cold facets: %+v %v", result, err)
	}
	s.Close()
	restarted := newTestService(t, Config{CacheDir: dir, TMDBCatalogSyncEnabled: true})
	store, err = restarted.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	store.facetMu.Lock()
	saved, ok := store.facets["movie"]
	store.facetMu.Unlock()
	if !ok || len(saved.genres) != 1 || saved.genres[0] != "剧情" {
		t.Fatalf("facet snapshot not reused: %+v", saved)
	}
	info, err := os.Stat(store.path + "-facets.json")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot permissions: %v %v", info, err)
	}
	store.invalidateFacets()
	store.facetMu.Lock()
	saved, ok = store.facets["movie"]
	store.facetMu.Unlock()
	if !ok || !saved.at.IsZero() || len(saved.genres) != 1 {
		t.Fatal("invalidation discarded usable facets")
	}
	// Shutting down during a shared SQL wait must cancel it before closing DB.
	connections := make([]*sql.Conn, 0, 4)
	for range 4 {
		c, e := store.db.Conn(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		connections = append(connections, c)
	}
	_, err = store.categoryFacets(context.Background(), "movie")
	if err != nil {
		t.Fatal(err)
	}
	restarted.cancel()
	for _, c := range connections {
		_ = c.Close()
	}
	done := make(chan struct{})
	go func() { restarted.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("facet refresh leaked past Close")
	}
}

func TestNumericSubjectUsesCompleteSQLCacheWithoutWaitingForDouban(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token"})
	rating := 8.9
	original := Item{ID: "12345", Title: "缓存作品", OriginalTitle: "Cached work", Year: "2025", MediaType: "movie", MetadataSource: "douban", DoubanRating: &rating, DoubanURL: "https://movie.douban.com/subject/12345/"}
	if err := s.SeedFullCatalog("movie", []Item{original}); err != nil {
		t.Fatal(err)
	}
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.saveHydrated(s.ctx, cacheAuditItem(11), 10, []string{"movie"}); err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, err := s.Subject(ctx, original.ID, "movie")
	if err != nil || got.ID != original.ID || got.Overview != "已保存的完整简介" || got.DoubanRating == nil || *got.DoubanRating != rating {
		t.Fatalf("local numeric details lost: %+v %v", got, err)
	}
	// A directory-backed homepage keeps the complete cache hint; direct subject
	// calls also remain usable for clients that do not understand that hint.
	got.Reason = catalogRecommendationHotReason
	response := s.response(cacheEntry{Items: []Item{got}, UpdatedAt: time.Now()}, "hot", false)
	if !response.Items[0].DetailsCached {
		t.Fatal("homepage discarded its complete directory details")
	}
	wrongCtx, wrongCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer wrongCancel()
	if _, err = s.Subject(wrongCtx, original.ID, "tv"); err == nil {
		t.Fatal("cross-media SQL subject was accepted")
	}
}

func TestStaleTMDBDetailsAreRefreshableAndFailureReturnsExplicitStaleFlag(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token"})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{11, 12, 13} {
		if err = store.saveHydrated(s.ctx, cacheAuditItem(id), 10, []string{"movie"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.db.Exec("UPDATE entries SET updated_at=? WHERE key='tmdb:movie:11'", time.Now().Add(-8*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("UPDATE entries SET needs_refresh=1 WHERE key='tmdb:movie:12'"); err != nil {
		t.Fatal(err)
	}
	response, err := s.QueryFullCatalog(s.ctx, "movie", ListOptions{Page: 1, PageSize: 24})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range response.Items {
		if item.DetailsCached != (item.ID == "tmdb:movie:13") {
			t.Fatalf("incorrect list cache hint: %+v", item)
		}
	}
	requests := 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if strings.HasSuffix(r.URL.Path, "/12") {
			return jsonResponse(`{"id":12,"title":"已更新资料","original_title":"Cached work","release_date":"2025-01-01","overview":"新简介"}`), nil
		}
		return nil, errors.New("simulated outage")
	})
	refreshed, err := s.FullCatalogSubject(s.ctx, "tmdb:movie:12", "movie")
	if err != nil || refreshed.Title != "已更新资料" || !refreshed.DetailsCached || requests != 1 {
		t.Fatalf("needs_refresh did not renew metadata: %+v %v %d", refreshed, err, requests)
	}
	stale, err := s.FullCatalogSubject(s.ctx, "tmdb:movie:11", "movie")
	if err != nil || stale.Overview != "已保存的完整简介" || stale.DetailsCached {
		t.Fatalf("complete stale fallback lost: %+v %v", stale, err)
	}
	encoded, _ := json.Marshal(stale)
	if !strings.Contains(string(encoded), `"details_cached":false`) {
		t.Fatal("stale response did not explicitly disable long browser caching")
	}
}

func TestJSONSubjectHintsIgnoreHistoricalBooleansAndReflectFreshness(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured bool
		source     string
		age        time.Duration
		storedHint bool
		expected   bool
	}{
		{"fresh legacy file without hint", false, "douban", time.Hour, false, true},
		{"expired historical true hint", false, "douban", 25 * time.Hour, true, false},
		{"pending TMDB enhancement", true, "douban", time.Hour, true, false},
		{"fresh enhanced subject", true, "tmdb", time.Hour, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{CacheDir: t.TempDir()}
			if tc.configured {
				cfg.TMDBReadAccessToken = "test-token"
			}
			s := newTestService(t, cfg)
			s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				response := jsonResponse(`{}`)
				response.StatusCode = 503
				return response, nil
			})
			s.save(s.subjectKey("123", "movie"), cacheEntry{Items: []Item{{ID: "123", Title: "已缓存电影", MediaType: "movie", MetadataSource: tc.source, DetailsCached: tc.storedHint}}, UpdatedAt: time.Now().Add(-tc.age)})
			got, err := s.Subject(context.Background(), "123", "movie")
			if err != nil || got.DetailsCached != tc.expected {
				t.Fatalf("historical hint was trusted: %+v %v", got, err)
			}
			if tc.age > 24*time.Hour {
				got, err = s.refreshSubject(context.Background(), "123", "movie")
				if err != nil || got.DetailsCached {
					t.Fatalf("failed refresh did not mark stale data: %+v %v", got, err)
				}
			}
		})
	}
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"id":"123","title":"新详情","type":"movie","year":"2025","intro":"完整简介"}`), nil
	})
	item, err := s.refreshSubject(context.Background(), "123", "movie")
	if err != nil || !item.DetailsCached {
		t.Fatalf("fresh successful details should be reusable: %+v %v", item, err)
	}
}
