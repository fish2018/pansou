package discover

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTMDBConfigurationKeepsNewestExistingListCache(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured bool
		canonical  bool
		legacy     bool
		newer      string
	}{
		{name: "enable with Douban cache", configured: true, canonical: true, newer: "canonical"},
		{name: "legacy TMDB cache after disabling", legacy: true, newer: "legacy"},
		{name: "newer legacy cache", configured: true, canonical: true, legacy: true, newer: "legacy"},
		{name: "newer canonical cache", configured: true, canonical: true, legacy: true, newer: "canonical"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			seed := newTestService(t, Config{CacheDir: dir})
			now := time.Now().UTC()
			for _, source := range []struct {
				name    string
				key     string
				present bool
			}{
				{name: "canonical", key: "movie_hot", present: test.canonical},
				{name: "legacy", key: "movie_hot_tmdb", present: test.legacy},
			} {
				if !source.present {
					continue
				}
				updated := now.Add(-time.Hour)
				if source.name == test.newer {
					updated = now
				}
				seed.save(source.key, cacheEntry{Items: []Item{{ID: "123", Title: source.name, MediaType: "movie"}}, UpdatedAt: updated})
			}
			cfg := Config{CacheDir: dir}
			if test.configured {
				cfg.TMDBReadAccessToken = "test-token"
			}
			s := newTestService(t, cfg)
			s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("provider toggle caused a cold list fetch")
				return nil, errors.New("unexpected request")
			})
			got, err := s.Get(context.Background(), "movie", "hot")
			if err != nil || got.Stale || len(got.Items) != 1 || got.Items[0].Title != test.newer || !got.UpdatedAt.Equal(now) || got.TMDBConfigured != test.configured {
				t.Fatalf("existing list was lost or replaced: %+v %v", got, err)
			}
			if s.listKey("movie", "hot") != "movie_hot" {
				t.Fatal("list identity still depends on TMDB configuration")
			}
			if cached, ok := s.peek("movie_hot"); !ok || cached.Items[0].Title != test.newer {
				t.Fatal("legacy list was not adopted under the canonical key")
			}
			if test.legacy {
				if _, err := os.Stat(filepath.Join(dir, "movie_hot_tmdb.json")); err != nil {
					t.Fatalf("legacy recovery cache was removed: %v", err)
				}
			}
		})
	}
}

func TestTMDBEnableReturnsOldSubjectWhileEnrichmentIsBlocked(t *testing.T) {
	dir := t.TempDir()
	oldRating := 8.0
	old := cacheEntry{Items: []Item{{ID: "123", Title: "测试电影", Year: "2025", MediaType: "movie", Overview: "旧豆瓣简介", DoubanRating: &oldRating, MetadataSource: "douban"}}, UpdatedAt: time.Now().UTC()}
	newTestService(t, Config{CacheDir: dir}).save(baseSubjectKey("123", "movie"), old)
	s := newTestService(t, Config{CacheDir: dir, TMDBReadAccessToken: "test-token"})
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch r.URL.Path {
		case "/rexxar/api/v2/movie/123":
			if r.Header.Get("Authorization") != "" {
				t.Error("TMDB credentials were sent to Douban")
			}
			return jsonResponse(`{"id":"123","title":"测试电影","type":"movie","year":"2025","intro":"新豆瓣简介","rating":{"value":8.5,"count":1200}}`), nil
		case "/3/search/movie":
			if r.Header.Get("Authorization") != "Bearer test-token" || r.URL.Query().Get("api_key") != "" {
				t.Error("TMDB read token authentication changed")
			}
			close(started)
			select {
			case <-release:
				return jsonResponse(`{"results":[{"id":99,"title":"测试电影","release_date":"2025-03-01"}]}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		case "/3/movie/99":
			return jsonResponse(`{"id":99,"release_date":"2025-03-01","poster_path":"/poster.jpg","overview":"TMDB 简介"}`), nil
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		got, err := s.Subject(ctx, "123", "movie")
		cancel()
		if err != nil || got.Overview != "旧豆瓣简介" || got.TMDBID != 0 || got.DoubanRating == nil || *got.DoubanRating != oldRating {
			t.Fatalf("old subject waited for TMDB or changed: %+v %v", got, err)
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("enrichment was not queued")
	}
	if _, ok := s.load(s.subjectKey("123", "movie")); ok {
		t.Fatal("old Douban cache was copied into the enhanced cache before enrichment")
	}
	releaseOnce.Do(func() { close(release) })
	if _, err := s.refreshSubject(context.Background(), "123", "movie"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Subject(context.Background(), "123", "movie")
	if err != nil || got.Overview != "TMDB 简介" || got.TMDBID != 99 || got.MetadataSource != "tmdb" || got.DoubanRating == nil || *got.DoubanRating != 8.5 || calls.Load() != 3 {
		t.Fatalf("background enhancement was lost or duplicated: %+v %v calls=%d", got, err, calls.Load())
	}
	if original, ok := s.load(baseSubjectKey("123", "movie")); !ok || original.Items[0].Overview != "旧豆瓣简介" {
		t.Fatal("enrichment destroyed the original provider cache")
	}
}

func TestTMDBEnableKeepsOldSubjectWhenBackgroundRefreshFails(t *testing.T) {
	s := newTestService(t, Config{TMDBReadAccessToken: "test-token"})
	s.save(baseSubjectKey("123", "movie"), cacheEntry{Items: []Item{{ID: "123", Overview: "旧简介"}}, UpdatedAt: time.Now()})
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: http.NoBody}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	if _, err := s.Subject(context.Background(), "123", "movie"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background upgrade did not start")
	}
	releaseOnce.Do(func() { close(release) })
	if _, err := s.refreshSubject(context.Background(), "123", "movie"); err == nil {
		t.Fatal("upstream failure was hidden")
	}
	for range 10 {
		got, err := s.Subject(context.Background(), "123", "movie")
		if err != nil || got.Overview != "旧简介" {
			t.Fatalf("failed enrichment removed the fallback: %+v %v", got, err)
		}
	}
	if _, ok := s.load(s.subjectKey("123", "movie")); ok || calls.Load() != 1 {
		t.Fatalf("failed enrichment created an enhanced cache or bypassed backoff: calls=%d", calls.Load())
	}
}

func TestTMDBEnableUsesOldMetadataWithoutMarkingItEnhanced(t *testing.T) {
	s := newTestService(t, Config{TMDBReadAccessToken: "test-token"})
	oldRating, liveRating := 8.0, 8.5
	s.save(baseSubjectKey("123", "tv"), cacheEntry{Items: []Item{{ID: "123", Title: "整剧", Overview: "旧豆瓣简介", DoubanRating: &oldRating}}, UpdatedAt: time.Now()})
	got := s.response(cacheEntry{Items: []Item{{ID: "123", Title: "剧 第二季", MediaType: "tv", DoubanRating: &liveRating}}}, "hot", false)
	if len(got.Items) != 1 || got.Items[0].Overview != "旧豆瓣简介" || got.Items[0].Title != "剧 第二季" || got.Items[0].DoubanRating == nil || *got.Items[0].DoubanRating != liveRating || got.Items[0].DetailsCached || got.Items[0].TMDBID != 0 {
		t.Fatalf("old metadata hid pending enrichment or overwrote identity/rating: %+v", got.Items)
	}
}
