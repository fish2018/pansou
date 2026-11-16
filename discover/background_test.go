package discover

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpiredListReturnsWhileItsSharedRefreshIsBlocked(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir()})
	old := cacheEntry{Items: []Item{{ID: "123", Title: "旧榜单", Genres: []string{}}}, UpdatedAt: time.Now().UTC().Add(-time.Hour)}
	s.save("tv_hot", old)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return jsonResponse(listFixture), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		got, err := s.Get(ctx, "tv", "hot")
		cancel()
		if err != nil || !got.Stale || len(got.Items) != 1 || got.Items[0].Title != "旧榜单" || !got.UpdatedAt.Equal(old.UpdatedAt) {
			t.Fatalf("expired list waited or changed: %+v %v", got, err)
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no background list refresh")
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate stale refreshes: %d", calls.Load())
	}
	close(release)
	if _, err := s.refreshList(context.Background(), "tv", "hot"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), "tv", "hot")
	if err != nil || got.Stale || got.Items[0].Title != "测试剧 第二季" {
		t.Fatalf("new snapshot was not published: %+v %v", got, err)
	}
}

func TestListFreshnessUses30MinutesForHotNewAndSixHoursForTop(t *testing.T) {
	for _, collection := range []string{"hot", "new", "top"} {
		s := newTestService(t, Config{})
		s.save(s.listKey("movie", collection), cacheEntry{Items: []Item{{ID: "123", Title: "缓存", Genres: []string{}}}, UpdatedAt: time.Now().Add(-time.Hour)})
		s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
		got, err := s.Get(context.Background(), "movie", collection)
		if err != nil || got.Stale != (collection != "top") {
			t.Fatalf("collection %s freshness=%t error=%v", collection, got.Stale, err)
		}
		s.Close()
	}
}

func TestExpiredSubjectReturnsImmediatelyAndRefreshesInBackground(t *testing.T) {
	s := newTestService(t, Config{})
	s.save(s.subjectKey("123", "movie"), cacheEntry{Items: []Item{{ID: "123", Title: "旧标题", Overview: "旧简介"}}, UpdatedAt: time.Now().Add(-25 * time.Hour)})
	started, release := make(chan struct{}), make(chan struct{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			return jsonResponse(`{"id":"123","title":"新标题","type":"movie","year":"2025","intro":"新简介"}`), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, err := s.Subject(ctx, "123", "movie")
	if err != nil || got.Overview != "旧简介" {
		t.Fatalf("stale subject blocked: %+v %v", got, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("subject refresh did not start")
	}
	close(release)
	if _, err := s.refreshSubject(context.Background(), "123", "movie"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Subject(context.Background(), "123", "movie")
	if err != nil || got.Overview != "新简介" {
		t.Fatalf("subject was not renewed: %+v %v", got, err)
	}
}

func TestSchedulerRefreshesWithoutPageViewsAndCloseCancelsIt(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir()})
	started, canceled := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var cancelOnce sync.Once
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-r.Context().Done()
		cancelOnce.Do(func() { close(canceled) })
		return nil, r.Context().Err()
	})
	s.start(10 * time.Millisecond)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("unvisited list was not refreshed")
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not stop refresh workers")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("Close did not cancel upstream request")
	}
	if calls.Load() > 2 {
		t.Fatalf("scheduler ran beyond its independent list/catalog workers: %d", calls.Load())
	}
}

func TestHotPrefetchWarmsOnlyFirst12SubjectsWithTwoWorkersAndDeduplicates(t *testing.T) {
	s := newTestService(t, Config{})
	s.lifecycleMu.Lock()
	s.schedulerStarted = true
	s.lifecycleMu.Unlock()
	var items []Item
	for i := 1; i <= 20; i++ {
		items = append(items, Item{ID: strconv.Itoa(i), Title: "剧", Year: "2025", MediaType: "tv"})
	}
	var active, maximum, total atomic.Int32
	var seenMu sync.Mutex
	seen := make(map[string]bool)
	done := make(chan struct{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		id := path.Base(r.URL.Path)
		seenMu.Lock()
		seen[id] = true
		seenMu.Unlock()
		if total.Add(1) == 12 {
			close(done)
		}
		return jsonResponse(fmt.Sprintf(`{"id":%q,"title":"预取剧","year":"2025","type":"tv","intro":"已缓存简介"}`, id)), nil
	})
	for range 10 {
		s.prefetchHot(cacheEntry{Items: items}, "hot")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hot subjects did not finish warming")
	}
	if _, err := s.refreshSubject(context.Background(), "12", "tv"); err != nil {
		t.Fatal(err)
	}
	seenMu.Lock()
	if len(seen) != 12 || seen["13"] {
		t.Errorf("prefetched outside the first 12: %v", seen)
	}
	seenMu.Unlock()
	if maximum.Load() > 2 || total.Load() != 12 {
		t.Fatalf("prefetch concurrency=%d total=%d", maximum.Load(), total.Load())
	}
	got, err := s.Subject(context.Background(), "1", "tv")
	if err != nil || got.Overview != "已缓存简介" || total.Load() != 12 {
		t.Fatalf("warmed detail was not a cache hit: %+v %v", got, err)
	}
}

func TestWarmQueueIsBoundedAndDoesNotBlockListRefreshes(t *testing.T) {
	s := newTestService(t, Config{})
	block := func(ctx context.Context) { <-ctx.Done() }
	for i := 0; i < 300; i++ {
		s.enqueue(s.warmQueue, backgroundTask{key: fmt.Sprintf("warm-%d", i), run: block})
	}
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if len(s.warmQueue) > 96 || pending > 98 {
		t.Fatalf("unbounded warm queue: queued=%d pending=%d", len(s.warmQueue), pending)
	}
	started := make(chan struct{})
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { close(started); return jsonResponse(listFixture), nil })
	s.queueList("movie", "hot")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("prefetch queue blocked list updates")
	}
}

func TestListCombinesCachedMetadataButKeepsNewDoubanRating(t *testing.T) {
	s := newTestService(t, Config{})
	oldRating, newRating := 8.0, 8.5
	s.save(s.subjectKey("123", "tv"), cacheEntry{Items: []Item{{ID: "123", Title: "整剧", Overview: "缓存简介", DoubanRating: &oldRating, Genres: []string{"剧情"}}}, UpdatedAt: time.Now()})
	got := s.response(cacheEntry{Items: []Item{{ID: "123", Title: "剧 第二季", MediaType: "tv", DoubanRating: &newRating}}, UpdatedAt: time.Now()}, "hot", false)
	if len(got.Items) != 1 || got.Items[0].Title != "剧 第二季" || got.Items[0].Overview != "缓存简介" || *got.Items[0].DoubanRating != 8.5 {
		t.Fatalf("cached metadata overwrote live identity/score: %+v", got.Items)
	}
	if !got.Items[0].DetailsCached {
		t.Fatal("fresh complete subject was not marked cached")
	}
	stale := cacheEntry{Items: []Item{{ID: "123", Overview: "过期但可用的简介"}}, UpdatedAt: time.Now().Add(-25 * time.Hour)}
	s.save(s.subjectKey("123", "tv"), stale)
	got = s.response(cacheEntry{Items: []Item{{ID: "123", MediaType: "tv"}, {ID: "456", MediaType: "tv", DetailsCached: true}}}, "hot", false)
	if len(got.Items) != 2 || got.Items[0].DetailsCached || got.Items[0].Overview != "过期但可用的简介" || got.Items[1].DetailsCached {
		t.Fatalf("stale or missing subject was incorrectly marked complete: %+v", got.Items)
	}
}
