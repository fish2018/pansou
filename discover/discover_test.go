package discover

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	s := New(cfg)
	t.Cleanup(s.Close)
	return s
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const listFixture = `{"subject_collection_items":[{"id":"123","title":"测试剧 第二季","original_title":"Example Season 2","year":"2025","type":"tv","rating":{"value":8.5,"count":1200},"pic":{"large":"https://img9.doubanio.com/view/photo/s_ratio_poster/public/p123.jpg"}}]}`

func TestDoubanRatingMappingDoesNotInventScoreOrVotes(t *testing.T) {
	value := doubanItem{ID: "123", Title: "电影", Year: "2025"}
	item := mapDouban(value, "movie")
	if item.DoubanRating != nil || item.DoubanVotes != nil || item.MetadataSource != "douban" {
		t.Fatalf("unrated subject: %+v", item)
	}
	value.Rating = &struct {
		Value float64 `json:"value"`
		Count int     `json:"count"`
	}{Value: 8.1, Count: 2000}
	value.Year = ""
	value.CardSubtitle = "1999 / 中国大陆 / 剧情"
	item = mapDouban(value, "movie")
	if *item.DoubanRating != 8.1 || *item.DoubanVotes != 2000 || item.Year != "1999" {
		t.Fatalf("real rating lost: %+v", item)
	}
	value.Rating.Value = 0
	if item = mapDouban(value, "movie"); item.DoubanRating != nil {
		t.Fatal("zero is not a rating")
	}
}

func TestMovieAndTVMatchingRequiresCorrectNameYearAndType(t *testing.T) {
	item := Item{Title: "The Example", Year: "2024", MediaType: "movie"}
	candidate := tmdbCandidate{ID: 1, Title: "The Example", ReleaseDate: "2024-05-06"}
	if !matchCandidate(item, candidate, false) {
		t.Fatal("expected exact match")
	}
	candidate.ReleaseDate = "1994-05-06"
	if matchCandidate(item, candidate, false) {
		t.Fatal("matched a different year's namesake")
	}
	candidate.ReleaseDate = "2024-05-06"
	candidate.Title = "A Different Example"
	if matchCandidate(item, candidate, false) {
		t.Fatal("matched a different title")
	}
	candidate.Title = ""
	candidate.Name = "The Example"
	candidate.FirstAirDate = "2024-05-06"
	if matchCandidate(item, candidate, false) {
		t.Fatal("matched TV fields on movie endpoint")
	}
}

func TestSeasonMatchingPreservesTitleScoreAndSeasonYear(t *testing.T) {
	s := newTestService(t, Config{TMDBReadAccessToken: "test-token"})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.URL.Query().Get("language") != "zh-CN" {
			t.Error("missing TMDB authentication or language")
		}
		switch r.URL.Path {
		case "/3/search/tv":
			if r.URL.Query().Get("query") != "测试剧" {
				t.Error("search did not remove season suffix")
			}
			return jsonResponse(`{"results":[{"id":99,"name":"测试剧","original_name":"Example","first_air_date":"2022-01-01"}]}`), nil
		case "/3/tv/99/season/2":
			return jsonResponse(`{"air_date":"2025-03-01","poster_path":"/poster.jpg","overview":"第二季简介"}`), nil
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	rating := 8.5
	item := Item{ID: "123", Title: "测试剧 第二季", OriginalTitle: "Example Season 2", Year: "2025", MediaType: "tv", DoubanRating: &rating}
	got, err := s.fetchTMDB(context.Background(), item)
	if err != nil || got.Title != item.Title || got.OriginalTitle != item.OriginalTitle || *got.DoubanRating != rating || got.TMDBID != 99 || got.MetadataSource != "tmdb" {
		t.Fatalf("season metadata: %+v, %v", got, err)
	}
}

func TestSeasonNumbers(t *testing.T) {
	for title, want := range map[string]int{"剧名 第二季": 2, "Example Season 12": 12, "剧名 第十一季": 11, "剧名 第二十三季": 23, "剧名": 0} {
		_, got := splitSeason(title)
		if got != want {
			t.Errorf("%q season=%d want=%d", title, got, want)
		}
	}
}

func TestConcurrentTopAndRandomShareFetchButKeepOwnReasons(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir()})
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return jsonResponse(listFixture), nil
	})
	var hot, random Response
	var hotErr, randomErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); hot, hotErr = s.Get(context.Background(), "tv", "top") }()
	<-started
	go func() { defer wg.Done(); random, randomErr = s.Get(context.Background(), "tv", "random") }()
	close(release)
	wg.Wait()
	if hotErr != nil || randomErr != nil || calls.Load() != 1 {
		t.Fatalf("calls=%d hot=%v random=%v", calls.Load(), hotErr, randomErr)
	}
	if hot.Items[0].Reason != "豆瓣高分 · 榜单精选" || random.Items[0].Reason != "豆瓣高分 · 随机发现" {
		t.Fatalf("shared response mutated: hot=%s random=%s", hot.Items[0].Reason, random.Items[0].Reason)
	}
}

func TestFailedRefreshUsesPersistentStaleCache(t *testing.T) {
	dir := t.TempDir()
	old := cacheEntry{Items: []Item{{ID: "123", Title: "缓存电影", Genres: []string{}}}, UpdatedAt: time.Now().UTC().Add(-7 * time.Hour)}
	newTestService(t, Config{CacheDir: dir}).save("movie_hot", old)
	s := newTestService(t, Config{CacheDir: dir})
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("down") })
	for range 2 {
		refreshed, err := s.refreshList(context.Background(), "movie", "hot")
		got := s.response(refreshed.entry, "hot", refreshed.stale)
		if err != nil || !got.Stale || got.Warning == "" || len(got.Items) != 1 || got.Items[0].Title != "缓存电影" || !got.UpdatedAt.Equal(old.UpdatedAt) {
			t.Fatalf("stale cache lost: %+v %v", got, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("failure backoff did not suppress requests: %d", calls.Load())
	}
}

func TestNoCacheFailureIsAnErrorAndNeverAnEmptySuccess(t *testing.T) {
	for _, body := range []string{`{}`, `{"subject_collection_items":[]}`, `{"subject_collection_items":[{"id":"bad","title":"Injected"}]}`} {
		s := newTestService(t, Config{})
		s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })
		if _, err := s.Get(context.Background(), "movie", "hot"); err == nil {
			t.Errorf("accepted invalid upstream: %s", body)
		}
	}
}

func TestNewTVUsesSupportedTimeListAndDoesNotInventVotes(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("tag") != "热门" || r.URL.Query().Get("sort") != "time" {
			t.Errorf("unsupported new-TV query: %s", r.URL.RawQuery)
		}
		return jsonResponse(`{"subjects":[{"id":"123","title":"新剧","rate":"8.1","cover":"https://img9.doubanio.com/view/photo/s_ratio_poster/public/p123.jpg"}]}`), nil
	})
	got, err := s.Get(context.Background(), "tv", "new")
	if err != nil || len(got.Items) != 1 || *got.Items[0].DoubanRating != 8.1 || got.Items[0].DoubanVotes != nil || got.TMDBConfigured {
		t.Fatalf("new-TV response: %+v %v", got, err)
	}
}

func TestTopRankNeedsBothRatingAndEnoughVotes(t *testing.T) {
	a, b := 8.5, 9.5
	many, few := 2000, 20
	items := []Item{{ID: "large", DoubanRating: &a, DoubanVotes: &many}, {ID: "tiny", DoubanRating: &b, DoubanVotes: &few}, {ID: "unknown", DoubanRating: &b}}
	got := rankItems(items, "movie")
	if len(got) != 1 || got[0].ID != "large" {
		t.Fatalf("rank admits unproven ratings: %+v", got)
	}
}

func TestPosterWhitelistRejectsArbitraryTargets(t *testing.T) {
	for _, address := range []string{"http://img9.doubanio.com/view/photo/a.jpg", "https://127.0.0.1/view/photo/a.jpg", "https://img9.doubanio.com.evil.test/view/photo/a.jpg", "https://img9.doubanio.com:443/view/photo/a.jpg", "https://user@img9.doubanio.com/view/photo/a.jpg", "https://img9.doubanio.com/view/photo/../../a", "https://img9.doubanio.com/view/photo/a.jpg?url=http://127.0.0.1", "https://img9.doubanio.com/api/private"} {
		if ValidPosterURL(address) {
			t.Errorf("accepted dangerous poster URL: %s", address)
		}
	}
	if !ValidPosterURL("https://img9.doubanio.com/view/photo/s_ratio_poster/public/p123.jpg") {
		t.Fatal("rejected real Douban poster")
	}
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse("<svg xmlns='http://www.w3.org/2000/svg'></svg>"), nil
	})
	if _, _, err := s.Poster(context.Background(), "https://img9.doubanio.com/view/photo/s_ratio_poster/public/p123.jpg"); err == nil {
		t.Fatal("accepted non-raster poster")
	}
}

func TestTMDBErrorsDoNotLeakAPIKey(t *testing.T) {
	s := newTestService(t, Config{TMDBAPIKey: "secret-test-key"})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New(r.URL.String()) })
	var result any
	err := s.getJSON(context.Background(), s.tmdbAddress("/search/movie", make(map[string][]string)), "tmdb", &result)
	if err == nil || strings.Contains(err.Error(), "secret-test-key") {
		t.Fatalf("credential leaked: %v", err)
	}
}

func TestSubjectLoadsFullDoubanMetadataOnDemandAndPersistsIt(t *testing.T) {
	dir := t.TempDir()
	s := newTestService(t, Config{CacheDir: dir})
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path != "/rexxar/api/v2/tv/123" {
			t.Errorf("unexpected subject request: %s", r.URL.Path)
		}
		return jsonResponse(`{"id":"123","type":"tv","title":"测试剧 第二季","original_title":"Example Season 2","year":"2025","intro":"真实简介","genres":["剧情","悬疑"],"rating":{"value":8.5,"count":1200},"pubdate":["2025-03-01(中国大陆)"]}`), nil
	})
	got, err := s.Subject(context.Background(), "123", "tv")
	if err != nil || got.Overview != "真实简介" || len(got.Genres) != 2 || got.ReleaseDate != "2025-03-01" || *got.DoubanRating != 8.5 || got.TMDBID != 0 {
		t.Fatalf("subject metadata: %+v %v", got, err)
	}
	loaded := newTestService(t, Config{CacheDir: dir})
	loaded.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("cached subject hit upstream")
		return nil, errors.New("unexpected")
	})
	if item, err := loaded.Subject(context.Background(), "123", "tv"); err != nil || item.Title != got.Title {
		t.Fatalf("subject cache: %+v %v", item, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("subject requests=%d", calls.Load())
	}
}

func TestGenresAreParsedOnlyFromKnownSubtitleGenreField(t *testing.T) {
	item := mapDouban(doubanItem{ID: "123", Title: "剧", CardSubtitle: "2025 / 中国大陆 / 剧情 悬疑 / 演员名"}, "tv")
	if strings.Join(item.Genres, ",") != "剧情,悬疑" {
		t.Fatalf("missing genres: %+v", item.Genres)
	}
	if len(subtitleGenres("2025 / 悬疑 / 演员名 / 喜剧")) != 0 {
		t.Fatal("parsed genre from the wrong subtitle field")
	}
}

func TestFutureReleasesAreExcludedOnlyWhenDateOrYearIsKnown(t *testing.T) {
	items := []Item{{ID: "next-year", Year: "2027"}, {ID: "tomorrow", Year: "2026", ReleaseDate: "2026-11-14"}, {ID: "today", ReleaseDate: "2026-11-13"}, {ID: "unknown"}}
	got := releasedItems(items, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	if len(got) != 2 || got[0].ID != "today" || got[1].ID != "unknown" {
		t.Fatalf("release filter guessed or kept future data: %+v", got)
	}
}

func TestDiscoveryTransportIsIsolatedAndPreservesConfiguredProxy(t *testing.T) {
	original := http.DefaultTransport
	base := original.(*http.Transport).Clone()
	proxyURL, _ := url.Parse("http://example.invalid:3128")
	base.Proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	http.DefaultTransport = base
	t.Cleanup(func() { http.DefaultTransport = original })
	s := newTestService(t, Config{})
	transport := s.client.Transport.(*http.Transport)
	if transport == base || s.client.Timeout != 15*time.Second {
		t.Fatal("discovery shares the global transport or lacks the request deadline")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	proxy, err := transport.Proxy(req)
	if err != nil || proxy.String() != proxyURL.String() {
		t.Fatal("configured proxy was not preserved")
	}
	if transport.ForceAttemptHTTP2 || len(transport.TLSNextProto) != 0 || strings.Join(transport.TLSClientConfig.NextProtos, ",") != "http/1.1" || transport.Protocols.HTTP2() || !transport.Protocols.HTTP1() {
		t.Fatal("HTTP/2 remains enabled")
	}
	if base.Protocols != nil && !base.Protocols.HTTP2() {
		t.Fatal("changed the default transport protocols")
	}
}

func TestDiscoveryUsesHTTP1AgainstAnHTTP2CapableTLSServer(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("negotiated %s", r.Proto)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	s := newTestService(t, Config{})
	transport := s.client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	t.Cleanup(transport.CloseIdleConnections)
	var payload struct {
		OK bool `json:"ok"`
	}
	if err := s.getJSON(context.Background(), server.URL, "douban", &payload); err != nil || !payload.OK {
		t.Fatalf("HTTP/1.1 TLS request failed: %v", err)
	}
}

func TestNetworkFailureRetriesOnlyOnce(t *testing.T) {
	for _, recover := range []bool{true, false} {
		s := newTestService(t, Config{})
		var calls atomic.Int32
		s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			n := calls.Add(1)
			if recover && n == 2 {
				return jsonResponse(`{"ok":true}`), nil
			}
			return nil, errors.New("connection reset")
		})
		var payload struct {
			OK bool `json:"ok"`
		}
		err := s.getJSON(context.Background(), "https://example.test/list", "douban", &payload)
		if calls.Load() != 2 || (recover && (err != nil || !payload.OK)) || (!recover && err == nil) {
			t.Fatalf("recover=%t requests=%d result=%+v error=%v", recover, calls.Load(), payload, err)
		}
	}
}

func TestHTTPRejectionAndInvalidJSONAreNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusOK} {
		s := newTestService(t, Config{})
		var calls atomic.Int32
		s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			response := jsonResponse("invalid JSON")
			response.StatusCode = status
			return response, nil
		})
		var payload any
		if err := s.getJSON(context.Background(), "https://example.test/list", "douban", &payload); err == nil || calls.Load() != 1 {
			t.Fatalf("status=%d requests=%d error=%v", status, calls.Load(), err)
		}
	}
}

func TestRetryWaitHonorsTheTotalRequestDeadline(t *testing.T) {
	s := newTestService(t, Config{})
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("connection reset") })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var payload any
	if err := s.getJSON(ctx, "https://example.test/list", "douban", &payload); !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("ignored request deadline: requests=%d error=%v", calls.Load(), err)
	}
}

func TestMovieTopRequestsOneFastPageOf50Subjects(t *testing.T) {
	s := newTestService(t, Config{})
	calls := 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.Contains(r.URL.Path, "movie_top250") || r.URL.Query().Get("count") != "50" || r.URL.Query().Get("start") != "0" {
			t.Errorf("unexpected top list page: %s", r.URL.Path)
		}
		return jsonResponse(catalogTestCollectionPage(50, "movie", 250)), nil
	})
	items, err := s.fetchDouban(context.Background(), "movie", "top")
	if err != nil || calls != 1 || len(items) != 50 {
		t.Fatalf("top list should retain its first page: count=%d calls=%d error=%v", len(items), calls, err)
	}
}
