package discover

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fullExportFixture(t *testing.T, records []tmdbExportRecord) []byte {
	t.Helper()
	var output bytes.Buffer
	zip := gzip.NewWriter(&output)
	encoder := json.NewEncoder(zip)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
func fullBinaryResponse(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func TestFullCatalogExportCRCResumeAndNamespaces(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token"})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	records := make([]tmdbExportRecord, 601)
	for index := range records {
		records[index] = tmdbExportRecord{ID: index + 1, OriginalTitle: fmt.Sprintf("Movie %d", index), Popularity: float64(index)}
	}
	records[600].Adult = true
	valid := fullExportFixture(t, records)
	corrupted := append([]byte{}, valid...)
	corrupted[len(corrupted)-5] ^= 0xff
	var correct bool
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "" || request.URL.Query().Get("api_key") != "" {
			t.Fatal("API credentials sent to export host")
		}
		if correct {
			return fullBinaryResponse(valid), nil
		}
		return fullBinaryResponse(corrupted), nil
	})
	date := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	if err = s.importFullExport(context.Background(), store, "movie", date); err == nil {
		t.Fatal("corrupt gzip marked successful")
	}
	if store.meta(s.ctx, "export_done:movie") != "" {
		t.Fatal("incomplete gzip completion marker persisted")
	}
	if got := store.meta(s.ctx, "export_cursor:movie:2026-11-16"); got != "512" {
		t.Fatalf("batch checkpoint = %q", got)
	}
	correct = true
	if err = s.importFullExport(context.Background(), store, "movie", date); err != nil {
		t.Fatal(err)
	}
	if store.meta(s.ctx, "export_done:movie") != "2026-11-16" {
		t.Fatal("valid gzip completion missing")
	}
	var count int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count)
	if count != 600 {
		t.Fatalf("resumed count=%d", count)
	}
	if err = store.importExportBatch(s.ctx, "tv", "2026-11-16", "test_tv_cursor", 1, []tmdbExportRecord{{ID: 1, OriginalName: "TV same number"}}); err != nil {
		t.Fatal(err)
	}
	_ = store.db.QueryRow("SELECT COUNT(*) FROM entries WHERE tmdb_id=1").Scan(&count)
	if count != 2 {
		t.Fatal("movie/TV namespace collision")
	}
	movie, err := s.QueryFullCatalog(s.ctx, "movie", ListOptions{Page: 1, PageSize: 24, Year: "unknown"})
	if err != nil || movie.Total != 600 || len(movie.Items) != 24 || movie.Items[0].Year != "" || movie.Items[0].DoubanRating != nil {
		t.Fatalf("stub query: %+v %v", movie, err)
	}
}

func TestFullCatalogExportFallsBackOneDay(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	body := fullExportFixture(t, []tmdbExportRecord{{ID: 1, OriginalName: "Series"}})
	var paths []string
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if strings.Contains(request.URL.Path, "11_16_2026") {
			response := jsonResponse("missing")
			response.StatusCode = 404
			return response, nil
		}
		return fullBinaryResponse(body), nil
	})
	if err = s.syncFullExport(s.ctx, store, "tv", time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || store.meta(s.ctx, "export_done:tv") != "2026-11-15" {
		t.Fatalf("fallback: %v", paths)
	}
}

func TestFullCatalogHydrationMergesOnlyUniqueWorkAndKeepsSeasons(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token", TMDBCatalogRate: 8})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	rating := 9.7
	original := Item{ID: "1292052", Title: "示例动画", OriginalTitle: "Example Animation", Year: "1994", MediaType: "movie", DoubanRating: &rating, DoubanURL: "https://movie.douban.com/subject/1292052/", PosterURL: "old-poster", Overview: "old-overview", MetadataSource: "douban"}
	if err = s.SeedFullCatalog("movie", []Item{original}); err != nil {
		t.Fatal(err)
	}
	if err = s.SeedFullCatalog("tv", []Item{{ID: "2373195", Title: "示例剧 第一季", OriginalTitle: "Example Series Season 1", Year: "2008", MediaType: "tv", DoubanRating: &rating}}); err != nil {
		t.Fatal(err)
	}
	if err = store.importExportBatch(s.ctx, "movie", "2026-11-16", "m", 1, []tmdbExportRecord{{ID: 278, OriginalTitle: "Example Animation", Popularity: 100}}); err != nil {
		t.Fatal(err)
	}
	if err = store.importExportBatch(s.ctx, "tv", "2026-11-16", "t", 1, []tmdbExportRecord{{ID: 1396, OriginalName: "Example Series", Popularity: 90}}); err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("missing TMDB authorization")
		}
		if strings.Contains(request.URL.Path, "/tv/") {
			return jsonResponse(`{"id":1396,"name":"示例剧","original_name":"Example Series","first_air_date":"2008-01-20","overview":"TV overview","popularity":90,"genres":[{"id":80,"name":"犯罪"}]}`), nil
		}
		id := 278
		if strings.HasSuffix(request.URL.Path, "/279") {
			id = 279
		}
		return jsonResponse(fmt.Sprintf(`{"id":%d,"title":"示例动画","original_title":"Example Animation","release_date":"1994-09-22","poster_path":"/new.jpg","overview":"TMDB overview","popularity":100,"vote_average":1.2,"genres":[{"id":16,"name":"动画"}]}`, id)), nil
	})
	item, err := s.FullCatalogSubject(s.ctx, "tmdb:movie:278", "movie")
	if err != nil || item.DoubanRating != nil {
		t.Fatalf("TMDB vote copied as Douban rating: %+v %v", item, err)
	}
	response, err := s.QueryFullCatalog(s.ctx, "animation", ListOptions{Page: 1, PageSize: 24})
	if err != nil || response.Total != 1 || response.Items[0].ID != "1292052" || *response.Items[0].DoubanRating != rating {
		t.Fatalf("merged animation: %+v %v", response, err)
	}
	if err = s.SeedFullCatalog("movie", []Item{original}); err != nil {
		t.Fatal(err)
	}
	response, err = s.QueryFullCatalog(s.ctx, "movie", ListOptions{Page: 1, PageSize: 24})
	if err != nil || response.Total != 1 || response.Items[0].PosterURL != "https://image.tmdb.org/t/p/w500/new.jpg" || response.Items[0].Overview != "TMDB overview" || len(response.Items[0].Genres) != 1 || response.Items[0].MetadataSource != "tmdb" {
		t.Fatalf("reseed degraded metadata: %+v %v", response, err)
	}
	var popularity float64
	_ = store.db.QueryRow("SELECT popularity FROM entries WHERE key='1292052'").Scan(&popularity)
	if popularity != 100 {
		t.Fatal("matched work lost popularity")
	}
	if _, err = s.FullCatalogSubject(s.ctx, "tmdb:tv:1396", "tv"); err != nil {
		t.Fatal(err)
	}
	response, err = s.QueryFullCatalog(s.ctx, "tv", ListOptions{Page: 1, PageSize: 24})
	if err != nil || response.Total != 2 {
		t.Fatalf("season was flattened: %+v %v", response, err)
	}
	// A second indistinguishable TMDB namesake makes the mapping ambiguous.
	if err = store.importExportBatch(s.ctx, "movie", "2026-11-16", "m", 2, []tmdbExportRecord{{ID: 279, OriginalTitle: "Example Animation"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FullCatalogSubject(s.ctx, "tmdb:movie:279", "movie"); err != nil {
		t.Fatal(err)
	}
	response, err = s.QueryFullCatalog(s.ctx, "movie", ListOptions{Page: 1, PageSize: 24})
	if err != nil || response.Total != 3 {
		t.Fatalf("ambiguous mapping hid a work: %+v %v", response, err)
	}
	for _, entry := range response.Items {
		if entry.ID == original.ID && (entry.PosterURL != original.PosterURL || entry.Overview != original.Overview || entry.MetadataSource != "douban" || entry.DetailsCached) {
			t.Fatalf("ambiguous mapping did not restore original Douban fields: %+v", entry)
		}
	}
}

func TestFullCatalogBackoffCapacityAndRestoredIDs(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token"})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.importExportBatch(s.ctx, "movie", "2026-11-16", "m", 2, []tmdbExportRecord{{ID: 1, OriginalTitle: "First", Popularity: 100}, {ID: 2, OriginalTitle: "Second", Popularity: 90}}); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		response := jsonResponse("{}")
		response.StatusCode = 404
		return response, nil
	})
	if _, err = s.FullCatalogSubject(s.ctx, "tmdb:movie:1", "movie"); err == nil {
		t.Fatal("failed hydration returned an incomplete stub as successful details")
	}
	if _, err = s.FullCatalogSubject(s.ctx, "tmdb:movie:1", "movie"); err == nil {
		t.Fatal("removed work should be hidden")
	}
	if requests.Load() != 1 {
		t.Fatal("negative cache did not prevent repeated requests")
	}
	media, id, err := store.nextHydration(s.ctx)
	if err != nil || media != "movie" || id != 2 {
		t.Fatal("failed first record blocked cursor")
	}
	if err = store.importExportBatch(s.ctx, "movie", "2026-11-17", "m2", 1, []tmdbExportRecord{{ID: 1, OriginalTitle: "First"}}); err != nil {
		t.Fatal(err)
	}
	var removed int
	_ = store.db.QueryRow("SELECT removed FROM entries WHERE key='tmdb:movie:1'").Scan(&removed)
	if removed != 0 {
		t.Fatal("restored valid export ID remained permanently hidden")
	}
	store.maxBytes = 1
	if err = store.importExportBatch(s.ctx, "movie", "2026-11-17", "m2", 2, []tmdbExportRecord{{ID: 3, OriginalTitle: "Third"}}); !errors.Is(err, errFullCatalogCapacity) {
		t.Fatal("capacity limit ignored")
	}
	if status := s.FullCatalogStatus(); status.Stage != "paused" || status.Error != "storage_limit" {
		t.Fatalf("unsafe or missing pause status: %+v", status)
	}
	if response, err := s.QueryFullCatalog(s.ctx, "movie", ListOptions{Page: 1, PageSize: 24}); err != nil || response.Total != 2 {
		t.Fatal("capacity pause blocked existing library reads")
	}
	store.maxBytes = 4 << 30
	if err = store.importExportBatch(s.ctx, "movie", "2026-11-17", "m2", 2, []tmdbExportRecord{{ID: 3, OriginalTitle: "Third"}}); err != nil {
		t.Fatal("capacity increase did not resume")
	}
}

func TestFullCatalogRateLimitAndChangesDailyWindow(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true, TMDBReadAccessToken: "test-token", TMDBCatalogRate: 8})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := jsonResponse("{}")
		response.StatusCode = 429
		response.Header.Set("Retry-After", "30")
		return response, nil
	})
	var value any
	if err = s.fullCatalogJSON(s.ctx, store, "/movie/1", nil, &value); err == nil {
		t.Fatal("429 ignored")
	}
	store.rateMu.Lock()
	pause := store.pauseUntil
	store.pauseUntil = time.Time{}
	store.rateMu.Unlock()
	if time.Until(pause) < 29*time.Second {
		t.Fatal("Retry-After ignored")
	}
	today := time.Now().UTC().Format("2006-01-02")
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("start_date") != today || request.URL.Query().Get("end_date") != today {
			t.Fatal("changes request was not split into daily windows")
		}
		return jsonResponse(`{"page":1,"total_pages":501,"results":[]}`), nil
	})
	if err = s.syncFullChangesDay(s.ctx, store, "movie", today, false); err == nil {
		t.Fatal("unreachable changes page silently accepted")
	}
	s.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(`{"page":1,"total_pages":1,"results":[]}`), nil
	})
	if err = s.syncFullChangesDay(s.ctx, store, "movie", today, false); err != nil {
		t.Fatal(err)
	}
	if store.meta(s.ctx, "changes_done:movie") == today {
		t.Fatal("partial current day marked permanently complete")
	}
}

// Run explicitly during directory changes. The normal test suite stays small.
func TestFullCatalogLargeDirectoryQueries(t *testing.T) {
	if os.Getenv("PANSOU_CATALOG_BENCH") != "1" {
		t.Skip("set PANSOU_CATALOG_BENCH=1 for the 100000-row disk benchmark")
	}
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	size := 100000
	if requested, _ := strconv.Atoi(os.Getenv("PANSOU_CATALOG_BENCH_ROWS")); requested >= 100000 && requested <= 1600000 {
		size = requested
	}
	for offset := 0; offset < size; offset += 512 {
		records := make([]tmdbExportRecord, 0, 512)
		for index := offset; index < offset+512 && index < size; index++ {
			title := fmt.Sprintf("Catalog Movie %06d", index)
			if index == 321 {
				title = "冰雪奇缘 Frozen"
			}
			records = append(records, tmdbExportRecord{ID: index + 1, OriginalTitle: title, Popularity: float64(size - index)})
		}
		if err = store.importExportBatch(s.ctx, "movie", "2026-11-16", "large", int64(offset+len(records)), records); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.saveHydrated(s.ctx, Item{ID: "tmdb:movie:322", TMDBID: 322, Title: "冰雪奇缘", OriginalTitle: "Frozen", Year: "2013", ReleaseDate: "2013-11-27", MediaType: "movie", Genres: []string{"动画"}, MetadataSource: "tmdb"}, 10000, []string{"movie", "animation"}); err != nil {
		t.Fatal(err)
	}
	store.refreshStatus(s.ctx)
	t.Logf("%d-row indexed import: %s", size, time.Since(started))
	var totalBytes int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, statErr := os.Stat(store.path + suffix); statErr == nil {
			totalBytes += info.Size()
			t.Logf("database%s bytes=%d", suffix, info.Size())
		}
	}
	t.Logf("database+WAL+SHM=%.2f MiB (%.1f bytes/title)", float64(totalBytes)/(1<<20), float64(totalBytes)/float64(size))
	for _, sample := range []struct {
		name    string
		options ListOptions
	}{{"first", ListOptions{Page: 1, PageSize: 24}}, {"second", ListOptions{Page: 2, PageSize: 24}}, {"3char", ListOptions{Page: 1, PageSize: 24, Query: "雪奇缘"}}, {"2char", ListOptions{Page: 1, PageSize: 24, Query: "冰雪"}}, {"year_genre", ListOptions{Page: 1, PageSize: 24, Year: "2013", Genre: "动画"}}} {
		started = time.Now()
		response, queryErr := s.QueryFullCatalog(s.ctx, "movie", sample.options)
		elapsed := time.Since(started)
		if queryErr != nil || len(response.Items) == 0 {
			t.Fatalf("%s query failed: %+v %v", sample.name, response, queryErr)
		}
		t.Logf("%s: %s, total=%d", sample.name, elapsed, response.Total)
		if elapsed > 2*time.Second {
			t.Errorf("%s query exceeded 2 seconds", sample.name)
		}
		where, args := catalogWhere("movie", sample.options)
		rows, queryErr := store.db.Query("EXPLAIN QUERY PLAN SELECT e.key FROM "+catalogQueryFrom(sample.options)+" WHERE "+where+" ORDER BY v.popularity DESC,v.key LIMIT 24", args...)
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			_ = rows.Scan(&id, &parent, &unused, &detail)
			t.Logf("%s plan: %s", sample.name, detail)
		}
		rows.Close()
	}
}
