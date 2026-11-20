package discover

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func fallbackFixture(t *testing.T, s *Service, id int, date string, popularity float64, category string) Item {
	t.Helper()
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	media := "tv"
	if category == "movie" {
		media = "movie"
	}
	year := ""
	if len(date) >= 4 {
		year = date[:4]
	}
	item := Item{ID: fullCatalogKey(media, id), TMDBID: id, Title: fmt.Sprintf("目录作品%d", id), OriginalTitle: fmt.Sprintf("Catalog Work %d", id), Year: year, ReleaseDate: date, Genres: []string{"剧情"}, MediaType: media, MetadataSource: "tmdb", Overview: "已经补齐的简介"}
	if err := store.saveHydrated(s.ctx, item, popularity, []string{category}); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestCatalogRecommendationFallbackUsesDatesCategoryAndKnownScores(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	now := time.Now().UTC()
	date := func(days int) string { return now.AddDate(0, 0, days).Format("2006-01-02") }
	fallbackFixture(t, s, 1, date(0), 2, "tv")
	fallbackFixture(t, s, 2, date(-1), 100, "tv")
	fallbackFixture(t, s, 3, date(0), 10, "tv")
	fallbackFixture(t, s, 4, date(1), 2000, "tv")
	fallbackFixture(t, s, 5, "", 500, "tv")
	fallbackFixture(t, s, 6, date(-91), 400, "tv")
	fallbackFixture(t, s, 7, date(0), 3000, "movie")
	fallbackFixture(t, s, 8, date(0), 4000, "tv")
	store, _ := s.openFullCatalog()
	if _, err := store.db.Exec("UPDATE entries SET removed=1 WHERE key='tmdb:tv:8'"); err != nil {
		t.Fatal(err)
	}
	rating := 8.6
	if err := s.SeedFullCatalog("tv", []Item{{ID: "123", Title: "有豆瓣评分的新剧", MediaType: "tv", Year: strconv.Itoa(now.Year()), ReleaseDate: date(0), Genres: []string{"剧情"}, DoubanRating: &rating, DoubanURL: "https://movie.douban.com/subject/123/", MetadataSource: "douban"}}); err != nil {
		t.Fatal(err)
	}
	items, err := s.catalogRecommendations(s.ctx, "tv", "new", now)
	if err != nil || len(items) != 4 {
		t.Fatalf("recent candidates: %+v %v", items, err)
	}
	for index, id := range []string{"tmdb:tv:3", "tmdb:tv:1", "123", "tmdb:tv:2"} {
		item := items[index]
		if item.ID != id || item.Reason != catalogRecommendationNewReason {
			t.Fatalf("date/popularity order or source changed at %d: %+v", index, item)
		}
		if item.ID == "123" {
			if item.DoubanRating == nil || *item.DoubanRating != rating || item.DoubanURL == "" {
				t.Fatalf("known Douban rating lost: %+v", item)
			}
		} else if item.DoubanRating != nil || item.DoubanURL != "" {
			t.Fatalf("TMDB score or association invented: %+v", item)
		}
	}
	items, err = s.catalogRecommendations(s.ctx, "tv", "hot", now)
	if err != nil || len(items) != 5 || items[0].ID != "tmdb:tv:5" || items[1].ID != "tmdb:tv:6" || items[2].ID != "tmdb:tv:2" {
		t.Fatalf("hot candidates ignore popularity or visibility: %+v %v", items, err)
	}
	for _, item := range items {
		if item.Reason != catalogRecommendationHotReason {
			t.Fatalf("fallback misrepresented as Douban list: %+v", item)
		}
	}
	if _, err := s.catalogRecommendations(s.ctx, "tv", "top", now); err == nil {
		t.Fatal("Douban top ratings were replaced by a popularity fallback")
	}
}

func TestFailedHomepageRefreshPublishesDirectoryAndDoesNotAppendStaleSources(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	today := time.Now().UTC().Format("2006-01-02")
	for id := 1; id <= 30; id++ {
		fallbackFixture(t, s, id, today, float64(100-id), "tv")
	}
	old := Item{ID: "456", Title: "旧日期或日期未知的豆瓣来源", MediaType: "tv", MetadataSource: "douban"}
	snapshot := completedCatalog("tv", []Item{old})
	for _, source := range catalogSources("tv") {
		if source.Collection == "new" {
			snapshot.Sources[source.Key] = catalogCursor{Done: true, IDs: []string{old.ID}, CompletedAt: time.Now()}
		}
	}
	s.saveCatalog("tv", snapshot)
	s.save("tv_new", cacheEntry{Items: []Item{old}, UpdatedAt: time.Now().Add(-48 * time.Hour)})
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		response := jsonResponse(`{"error":"temporarily unavailable"}`)
		response.StatusCode = http.StatusServiceUnavailable
		return response, nil
	})
	refreshed, err := s.refreshList(context.Background(), "tv", "new")
	if err != nil || refreshed.stale || len(refreshed.entry.Items) != 30 || time.Since(refreshed.entry.UpdatedAt) > time.Minute {
		t.Fatalf("directory did not replace old homepage after upstream failure: %+v %v", refreshed, err)
	}
	page1, err := s.Query(context.Background(), "tv", "new", ListOptions{Page: 1, PageSize: 12})
	if err != nil || page1.Stale || page1.Total != 30 || len(page1.Items) != 12 || page1.Items[0].Reason != catalogRecommendationNewReason {
		t.Fatalf("fallback page 1: %+v %v", page1, err)
	}
	page2, err := s.Query(context.Background(), "tv", "new", ListOptions{Page: 2, PageSize: 12})
	if err != nil || len(page2.Items) != 12 || page2.Items[0].ID == page1.Items[0].ID {
		t.Fatalf("fallback paging regressed: %+v %v", page2, err)
	}
	for _, item := range append(page1.Items, page2.Items...) {
		if item.ID == old.ID || item.ReleaseDate != today || item.DoubanRating != nil {
			t.Fatalf("stale source or invented score entered current directory pool: %+v", item)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("local recommendations fetched per-item metadata or retried a fresh cache: %d HTTP requests", calls.Load())
	}
	// A namespaced card keeps its complete SQLite detail path even without
	// credentials or an external provider, including the separate numeric ID.
	item, err := s.Subject(context.Background(), page1.Items[0].ID, "tv")
	if err != nil || item.Overview != "已经补齐的简介" || item.DoubanRating != nil {
		t.Fatalf("fallback detail failed: %+v %v", item, err)
	}
}

func TestCatalogFallbackKeepsDoubanPriorityAndDisabledBehavior(t *testing.T) {
	t.Run("Douban remains first choice", func(t *testing.T) {
		s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
		fallbackFixture(t, s, 1, time.Now().UTC().Format("2006-01-02"), 100, "tv")
		s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) { return jsonResponse(listFixture), nil })
		got, err := s.Get(context.Background(), "tv", "hot")
		if err != nil || len(got.Items) != 1 || got.Items[0].ID != "123" || isCatalogRecommendation(got.Items) {
			t.Fatalf("working Douban source was replaced: %+v %v", got, err)
		}
	})
	t.Run("disabled catalog retains original stale data", func(t *testing.T) {
		s := newTestService(t, Config{CacheDir: t.TempDir()})
		old := cacheEntry{Items: []Item{{ID: "123", Title: "旧的有效数据", MediaType: "tv"}}, UpdatedAt: time.Now().Add(-48 * time.Hour)}
		s.save("tv_hot", old)
		s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := jsonResponse(`{}`)
			response.StatusCode = http.StatusServiceUnavailable
			return response, nil
		})
		got, err := s.refreshList(context.Background(), "tv", "hot")
		if err != nil || !got.stale || !got.entry.UpdatedAt.Equal(old.UpdatedAt) || got.entry.Items[0].ID != "123" {
			t.Fatalf("disabled behavior changed: %+v %v", got, err)
		}
	})
}

func TestCatalogFallbackHotDoesNotPrefetchTMDBIDsThroughDouban(t *testing.T) {
	s := newTestService(t, Config{})
	s.lifecycleMu.Lock()
	s.schedulerStarted = true
	s.lifecycleMu.Unlock()
	requests := make(chan string, 4)
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests <- request.URL.Path
		return jsonResponse(`{"id":"123","title":"数字豆瓣条目","type":"tv"}`), nil
	})
	s.prefetchHot(cacheEntry{Items: []Item{{ID: "tmdb:tv:203483", Title: "目录新剧", MediaType: "tv", DetailsCached: true, MetadataSource: "tmdb"}}}, "hot")
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	s.lifecycleMu.Lock()
	workersStarted := s.workersStarted
	s.lifecycleMu.Unlock()
	if pending != 0 || workersStarted || len(s.warmQueue) != 0 {
		t.Fatal("TMDB namespaced detail entered the Douban prefetch queue")
	}
	s.prefetchHot(cacheEntry{Items: []Item{{ID: "123", Title: "数字豆瓣条目", MediaType: "tv"}}}, "hot")
	select {
	case path := <-requests:
		if path != "/rexxar/api/v2/tv/123" {
			t.Fatalf("unexpected prefetch endpoint: %s", path)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary Douban hot-card prefetch was disabled")
	}
}
