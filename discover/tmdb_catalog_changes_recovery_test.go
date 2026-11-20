package discover

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"
)

const recoveryDay = "2026-11-19"
const recoveryPreviousDay = "2026-11-18"
const recoveryCursorKey = "changes_cursor:tv:" + recoveryDay
const recoveryWindowKey = "changes_window_start:tv:" + recoveryDay

func newChangesRecoveryService(t *testing.T, cursor int, ids ...int) (*Service, *tmdbCatalogStore) {
	t.Helper()
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token", TMDBCatalogRate: 8})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var records []tmdbExportRecord
	for _, id := range ids {
		records = append(records, tmdbExportRecord{ID: id, OriginalName: fmt.Sprintf("Series %d", id)})
	}
	if err = store.importExportBatch(s.ctx, "tv", recoveryDay, "recovery-seed", int64(len(records)), records); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("INSERT INTO sync_meta(key,value) VALUES(?,?)", recoveryCursorKey, strconv.Itoa(cursor)); err != nil {
		t.Fatal(err)
	}
	return s, store
}

func recoveryRequest(t *testing.T, request *http.Request) (string, int) {
	t.Helper()
	query := request.URL.Query()
	if request.URL.Path != "/3/tv/changes" || query.Get("end_date") != recoveryDay {
		t.Fatal("recovery requested a different endpoint or target day")
	}
	page, err := strconv.Atoi(query.Get("page"))
	if err != nil {
		t.Fatal(err)
	}
	return query.Get("start_date"), page
}

func assertChangesRefreshed(t *testing.T, store *tmdbCatalogStore, ids ...int) {
	t.Helper()
	for _, id := range ids {
		var refreshed int
		if err := store.db.QueryRow("SELECT needs_refresh FROM entries WHERE key=?", fullCatalogKey("tv", id)).Scan(&refreshed); err != nil || refreshed != 1 {
			t.Fatalf("changed ID %d was skipped: refreshed=%d err=%v", id, refreshed, err)
		}
	}
}

func assertChangesRecoveryComplete(t *testing.T, store *tmdbCatalogStore) {
	t.Helper()
	if store.meta(context.Background(), "changes_done:tv") != recoveryDay || store.meta(context.Background(), recoveryCursorKey) != "" || store.meta(context.Background(), recoveryWindowKey) != "" {
		t.Fatal("recovered day did not replace its checkpoint and window")
	}
}

func TestFullCatalogChangesInconsistentPageUsesOverlappingWindow(t *testing.T) {
	s, store := newChangesRecoveryService(t, 19, 19, 20, 21)
	var requested []string
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		start, page := recoveryRequest(t, request)
		requested = append(requested, fmt.Sprintf("%s/%d", start, page))
		if start == recoveryDay {
			return jsonResponse(`{"page":19,"total_pages":18,"results":[]}`), nil
		}
		if start != recoveryPreviousDay || page < 1 || page > 3 {
			t.Fatal("invalid overlapping recovery window")
		}
		return jsonResponse(fmt.Sprintf(`{"page":%d,"total_pages":3,"results":[{"id":%d}]}`, page, page+18)), nil
	})
	if err := s.syncFullChangesDay(s.ctx, store, "tv", recoveryDay, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(requested, []string{recoveryDay + "/19", recoveryPreviousDay + "/1", recoveryPreviousDay + "/2", recoveryPreviousDay + "/3"}) {
		t.Fatalf("recovery skipped IDs or kept retrying the stale page: %v", requested)
	}
	assertChangesRefreshed(t, store, 19, 20, 21)
	assertChangesRecoveryComplete(t, store)
}

func TestFullCatalogChangesShrunkFeedAndHTTP400RescanWithoutSkipping(t *testing.T) {
	for _, badRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("http400_%v", badRequest), func(t *testing.T) {
			s, store := newChangesRecoveryService(t, 19, 1, 2)
			requests := 0
			s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				start, page := recoveryRequest(t, request)
				requests++
				if start == recoveryDay {
					response := jsonResponse(`{"page":19,"total_pages":2,"results":[]}`)
					if badRequest {
						response.StatusCode = http.StatusBadRequest
					}
					return response, nil
				}
				if start != recoveryPreviousDay || page > 2 {
					t.Fatal("shrunk feed was not rescanned from the start")
				}
				return jsonResponse(fmt.Sprintf(`{"page":%d,"total_pages":2,"results":[{"id":%d}]}`, page, page)), nil
			})
			if err := s.syncFullChangesDay(s.ctx, store, "tv", recoveryDay, true); err != nil {
				t.Fatal(err)
			}
			if requests != 3 {
				t.Fatalf("rescan was not bounded: requests=%d", requests)
			}
			assertChangesRefreshed(t, store, 1, 2)
			assertChangesRecoveryComplete(t, store)
		})
	}
}

func TestFullCatalogChangesOverlappingCheckpointResumesAfterRestart(t *testing.T) {
	s, store := newChangesRecoveryService(t, 19, 1, 2)
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		start, page := recoveryRequest(t, request)
		if start == recoveryDay {
			return jsonResponse(`{"page":19,"total_pages":18,"results":[]}`), nil
		}
		if start == recoveryPreviousDay && page == 1 {
			return jsonResponse(`{"page":1,"total_pages":2,"results":[{"id":1}]}`), nil
		}
		response := jsonResponse(`{}`)
		response.StatusCode = http.StatusServiceUnavailable
		return response, nil
	})
	if err := s.syncFullChangesDay(s.ctx, store, "tv", recoveryDay, true); err == nil {
		t.Fatal("failed overlapping page was marked complete")
	}
	if store.meta(s.ctx, recoveryCursorKey) != "2" || store.meta(s.ctx, recoveryWindowKey) != recoveryPreviousDay || store.meta(s.ctx, "changes_done:tv") != "" {
		t.Fatal("overlapping checkpoint was not stored with its actual window")
	}
	cfg := s.cfg
	s.Close()
	restarted := newTestService(t, cfg)
	resumedStore, err := restarted.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	restarted.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		start, page := recoveryRequest(t, request)
		requests++
		if start != recoveryPreviousDay || page != 2 {
			t.Fatal("restart used an overlapping cursor against the original daily window")
		}
		return jsonResponse(`{"page":2,"total_pages":2,"results":[{"id":2}]}`), nil
	})
	if err = restarted.syncFullChangesDay(restarted.ctx, resumedStore, "tv", recoveryDay, true); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("restart did not resume its committed page")
	}
	assertChangesRefreshed(t, resumedStore, 1, 2)
	assertChangesRecoveryComplete(t, resumedStore)
}

func TestFullCatalogChangesUnstableOverlappingFeedStops(t *testing.T) {
	s, store := newChangesRecoveryService(t, 19, 1)
	requests := 0
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		start, page := recoveryRequest(t, request)
		requests++
		if start == recoveryDay {
			return jsonResponse(`{"page":19,"total_pages":18,"results":[]}`), nil
		}
		if page == 1 {
			return jsonResponse(`{"page":1,"total_pages":2,"results":[{"id":1}]}`), nil
		}
		return jsonResponse(`{"page":2,"total_pages":1,"results":[]}`), nil
	})
	if err := s.syncFullChangesDay(s.ctx, store, "tv", recoveryDay, true); err == nil {
		t.Fatal("inconsistent overlapping window was marked successful")
	}
	if requests != 3 || store.meta(s.ctx, "changes_done:tv") != "" || store.meta(s.ctx, recoveryCursorKey) != "2" || store.meta(s.ctx, recoveryWindowKey) != recoveryPreviousDay {
		t.Fatal("recovery repeated forever or discarded its checkpoint")
	}
	assertChangesRefreshed(t, store, 1)
}

func TestFullCatalogChangesInvalidPaginationNeverCompletes(t *testing.T) {
	for _, body := range []string{
		`{"page":19,"total_pages":-1,"results":[]}`,
		`{"page":19,"total_pages":501,"results":[]}`,
		`{"page":18,"total_pages":21,"results":[]}`,
		`{"page":19,"total_pages":21}`,
		`{"page":19,"total_pages":0,"results":[{"id":1}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			s, store := newChangesRecoveryService(t, 19)
			requests := 0
			s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				return jsonResponse(body), nil
			})
			if err := s.syncFullChangesDay(s.ctx, store, "tv", recoveryDay, true); err == nil {
				t.Fatal("invalid pagination was marked successful")
			}
			if requests != 1 || store.meta(s.ctx, "changes_done:tv") != "" || store.meta(s.ctx, recoveryCursorKey) != "19" || store.meta(s.ctx, recoveryWindowKey) != "" {
				t.Fatal("invalid response consumed progress or retried without a recovery signal")
			}
		})
	}
}

func TestFullCatalogChangesNormalDayKeepsSingleDayWindow(t *testing.T) {
	s, store := newChangesRecoveryService(t, 1, 1)
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		start, page := recoveryRequest(t, request)
		if start != recoveryDay || page != 1 {
			t.Fatal("normal request unnecessarily expanded its daily window")
		}
		return jsonResponse(`{"page":1,"total_pages":1,"results":[{"id":1}]}`), nil
	})
	if err := s.syncFullChangesDay(s.ctx, store, "tv", recoveryDay, false); err != nil {
		t.Fatal(err)
	}
	if store.meta(s.ctx, "changes_done:tv") != "" || store.meta(s.ctx, recoveryCursorKey) != "" || store.meta(s.ctx, recoveryWindowKey) != "" {
		t.Fatal("current partial day was completed or left an expanded window")
	}
	assertChangesRefreshed(t, store, 1)
}
