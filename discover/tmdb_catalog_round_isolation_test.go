package discover

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupIsolatedRound(t *testing.T, handler func(http.ResponseWriter, *http.Request)) (*Service, *tmdbCatalogStore) {
	t.Helper()
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token", TMDBCatalogRate: 8})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for _, media := range []string{"movie", "tv"} {
		if _, err = store.db.Exec("INSERT INTO sync_meta(key,value) VALUES(?,?)", "changes_done:"+media, yesterday); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	destination, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		address := *r.URL
		clone.URL = &address
		clone.URL.Scheme = destination.Scheme
		clone.URL.Host = destination.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	return s, store
}

func TestFullCatalogRoundExportFailureStillChecksBothChanges(t *testing.T) {
	for _, downloadStatus := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
		t.Run(fmt.Sprintf("download_%d", downloadStatus), func(t *testing.T) {
			var mu sync.Mutex
			seen := map[string]bool{}
			body := fullExportFixture(t, []tmdbExportRecord{{ID: 99, OriginalTitle: "Round Movie", OriginalName: "Round Series"}})
			s, store := setupIsolatedRound(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen[r.URL.Path] = true
				mu.Unlock()
				if strings.Contains(r.URL.Path, "movie_ids_") {
					http.Error(w, "unavailable", downloadStatus)
					return
				}
				if strings.Contains(r.URL.Path, "/exports/") {
					w.Write(body)
					return
				}
				fmt.Fprint(w, `{"page":1,"total_pages":1,"results":[]}`)
			})
			s.syncFullCatalogRound(s.ctx, store)
			mu.Lock()
			defer mu.Unlock()
			if !seen["/3/movie/changes"] || !seen["/3/tv/changes"] {
				t.Fatalf("export failure blocked independent changes: %v", seen)
			}
			for _, media := range []string{"movie", "tv"} {
				if store.meta(s.ctx, "changes_polled:"+media) == "" {
					t.Fatalf("%s changes were not completed", media)
				}
			}
			status := s.FullCatalogStatus()
			if status.Stage != "error" || status.Error != "export_unavailable_or_incomplete" {
				t.Fatalf("export failure hidden or mistaken for rejected credentials: %+v", status)
			}
		})
	}
}

func TestFullCatalogRoundOneChangeFailureStillChecksOtherMedia(t *testing.T) {
	body := fullExportFixture(t, []tmdbExportRecord{{ID: 99, OriginalTitle: "Round Movie", OriginalName: "Round Series"}})
	s, store := setupIsolatedRound(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/exports/") {
			w.Write(body)
			return
		}
		if r.URL.Path == "/3/movie/changes" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"page":1,"total_pages":1,"results":[]}`)
	})
	s.syncFullCatalogRound(s.ctx, store)
	if store.meta(s.ctx, "changes_polled:movie") != "" || store.meta(s.ctx, "changes_polled:tv") == "" {
		t.Fatal("movie changes failure blocked TV or was marked completed")
	}
	status := s.FullCatalogStatus()
	if status.Stage != "error" || status.Error != "changes_temporarily_unavailable" {
		t.Fatalf("changes failure hidden: %+v", status)
	}
}

func TestFullCatalogRoundPreservesSharedPausesAndResumesCapacity(t *testing.T) {
	t.Run("capacity", func(t *testing.T) {
		calls := 0
		body := fullExportFixture(t, []tmdbExportRecord{{ID: 99, OriginalTitle: "Round Movie", OriginalName: "Round Series"}})
		s, store := setupIsolatedRound(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if strings.Contains(r.URL.Path, "/exports/") {
				w.Write(body)
				return
			}
			fmt.Fprint(w, `{"page":1,"total_pages":1,"results":[]}`)
		})
		store.maxBytes = 1
		s.syncFullCatalogRound(s.ctx, store)
		status := s.FullCatalogStatus()
		if calls != 0 || status.Stage != "paused" || status.Error != "storage_limit" {
			t.Fatalf("capacity pause replaced or upstream requested: calls=%d status=%+v", calls, status)
		}
		store.maxBytes = 4 << 30
		s.syncFullCatalogRound(s.ctx, store)
		if status = s.FullCatalogStatus(); status.Stage != "ready" || status.Error != "" {
			t.Fatalf("recovered capacity remained paused: %+v", status)
		}
	})
	t.Run("credentials", func(t *testing.T) {
		body := fullExportFixture(t, []tmdbExportRecord{{ID: 99, OriginalTitle: "Round Movie", OriginalName: "Round Series"}})
		tvRequested := false
		s, store := setupIsolatedRound(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/exports/") {
				w.Write(body)
				return
			}
			if r.URL.Path == "/3/tv/changes" {
				tvRequested = true
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
		s.syncFullCatalogRound(s.ctx, store)
		status := s.FullCatalogStatus()
		if tvRequested || status.Stage != "paused" || status.Error != "credentials_rejected" {
			t.Fatalf("authentication pause replaced: tv=%v status=%+v", tvRequested, status)
		}
		s.syncFullCatalogRound(s.ctx, store)
		if status = s.FullCatalogStatus(); status.Stage != "paused" || status.Error != "credentials_rejected" {
			t.Fatalf("next round erased active auth pause: %+v", status)
		}
	})
	t.Run("concurrent_hydration_pause", func(t *testing.T) {
		s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
		store, err := s.openFullCatalog()
		if err != nil {
			t.Fatal(err)
		}
		store.setStage("paused", "credentials_rejected")
		store.setCatalogRoundStage("ready", "")
		if status := s.FullCatalogStatus(); status.Stage != "paused" || status.Error != "credentials_rejected" {
			t.Fatalf("task completion erased hydration pause: %+v", status)
		}
	})
}
