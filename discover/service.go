package discover

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"pansou/util"
)

const cacheTTL = 6 * time.Hour

// Config never appears in API responses or logs: its TMDB credentials remain server-side.
type Config struct {
	CacheDir               string
	TMDBAPIKey             string
	TMDBReadAccessToken    string
	CatalogMaxItems        int
	CatalogMaxBytes        int
	TMDBCatalogSyncEnabled bool
	TMDBCatalogMaxMB       int
	TMDBCatalogRate        int
}

func ConfigFromEnv() Config {
	dir := strings.TrimSpace(os.Getenv("DISCOVER_CACHE_DIR"))
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("DISCOVER_CACHE_PATH"))
	}
	if dir == "" {
		dir = "/app/data/discover"
	}
	items, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("DISCOVER_CATALOG_MAX_ITEMS")))
	megabytes, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("DISCOVER_CATALOG_MAX_MB")))
	items = boundedCatalogLimit(items, catalogMaxItems, catalogHardMaxItems)
	megabytes = boundedCatalogLimit(megabytes, catalogMaxBytes>>20, catalogHardMaxBytes>>20)
	fullMB, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("TMDB_CATALOG_MAX_MB")))
	rate, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("TMDB_CATALOG_RATE")))
	enabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("TMDB_CATALOG_SYNC_ENABLED")))
	return Config{CacheDir: dir, TMDBAPIKey: strings.TrimSpace(os.Getenv("TMDB_API_KEY")), TMDBReadAccessToken: strings.TrimSpace(os.Getenv("TMDB_READ_ACCESS_TOKEN")), CatalogMaxItems: items, CatalogMaxBytes: megabytes << 20, TMDBCatalogSyncEnabled: enabled, TMDBCatalogMaxMB: fullMB, TMDBCatalogRate: rate}
}

type Service struct {
	cfg                Config
	client             *http.Client
	mu                 sync.Mutex
	cache              map[string]cacheEntry
	failures           map[string]time.Time
	nextRequest        time.Time
	flight             singleflight.Group
	metadataFlight     singleflight.Group
	posterSlots        chan struct{}
	posterFlight       singleflight.Group
	posterCacheMu      sync.Mutex
	ctx                context.Context
	cancel             context.CancelFunc
	lifecycleMu        sync.Mutex
	workersStarted     bool
	schedulerStarted   bool
	closed             bool
	workers            sync.WaitGroup
	listQueue          chan backgroundTask
	warmQueue          chan backgroundTask
	pending            map[string]bool
	catalogMu          sync.Mutex
	catalogs           map[string]catalogSnapshot
	catalogFlight      singleflight.Group
	catalogQueue       chan backgroundTask
	catalogInterval    time.Duration
	nextCatalogPage    time.Time
	catalogWork        sync.WaitGroup
	fullCatalogMu      sync.Mutex
	fullCatalog        *tmdbCatalogStore
	fullCatalogError   string
	fullCatalogStarted bool
}

func New(cfg Config) *Service {
	if cfg.TMDBCatalogMaxMB <= 0 {
		cfg.TMDBCatalogMaxMB = 4096
	}
	if cfg.TMDBCatalogMaxMB > 65536 {
		cfg.TMDBCatalogMaxMB = 65536
	}
	if cfg.TMDBCatalogRate <= 0 {
		cfg.TMDBCatalogRate = 4
	}
	if cfg.TMDBCatalogRate > 8 {
		cfg.TMDBCatalogRate = 8
	}
	cfg.CatalogMaxItems = boundedCatalogLimit(cfg.CatalogMaxItems, catalogMaxItems, catalogHardMaxItems)
	cfg.CatalogMaxBytes = boundedCatalogLimit(cfg.CatalogMaxBytes, catalogMaxBytes, catalogHardMaxBytes)
	if cfg.CatalogMaxBytes < 1024 {
		cfg.CatalogMaxBytes = catalogMaxBytes
	}
	// Keep the default dial/proxy settings, but isolate discovery connections from
	// search traffic. This upstream's TLS/H2 negotiation stalls on some VPS exits.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{cfg: cfg, client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, cache: make(map[string]cacheEntry), failures: make(map[string]time.Time), posterSlots: make(chan struct{}, 8), ctx: ctx, cancel: cancel, listQueue: make(chan backgroundTask, 15), warmQueue: make(chan backgroundTask, 96), pending: make(map[string]bool), catalogs: make(map[string]catalogSnapshot), catalogQueue: make(chan backgroundTask, 5), catalogInterval: 5 * time.Second}
}

func ValidSelection(category, collection string) bool {
	return contains([]string{"movie", "tv", "animation", "variety", "documentary"}, category) && contains([]string{"hot", "top", "new", "random", "library"}, collection)
}

func contains(values []string, v string) bool {
	for _, value := range values {
		if value == v {
			return true
		}
	}
	return false
}

func (s *Service) TMDBConfigured() bool {
	return s.cfg.TMDBAPIKey != "" || s.cfg.TMDBReadAccessToken != ""
}

func listTTL(collection string) time.Duration {
	if collection == "hot" || collection == "new" {
		return 30 * time.Minute
	}
	return cacheTTL
}

func (s *Service) listKey(category, collection string) string {
	return category + "_" + collection
}

// Lists contain the same Douban candidates with or without TMDB enrichment.
// Adopt the newest legacy TMDB-named snapshot in memory without deleting its
// file; the next successful list refresh persists the canonical cache key.
func (s *Service) loadList(category, collection string) (cacheEntry, bool) {
	key := s.listKey(category, collection)
	entry, ok := s.load(key)
	legacy, hasLegacy := s.load(key + "_tmdb")
	if !hasLegacy {
		return entry, ok
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.cache[key]; exists && !legacy.UpdatedAt.After(current.UpdatedAt) {
		return current, true
	}
	s.cache[key] = legacy
	return legacy, true
}

// Get always returns existing lists immediately. Expired entries are refreshed
// in a bounded background queue, while genuinely cold requests share one fetch.
func (s *Service) Get(ctx context.Context, category, collection string) (Response, error) {
	response, err := s.get(ctx, category, collection)
	if err == nil {
		s.describeCatalog(category, collection, &response)
	}
	return response, err
}

func (s *Service) get(ctx context.Context, category, collection string) (Response, error) {
	if !ValidSelection(category, collection) {
		return Response{}, errors.New("invalid discovery selection")
	}
	if collection == "library" {
		return s.library(ctx, category)
	}
	cacheCollection := collection
	if collection == "random" {
		cacheCollection = "top"
	}
	if entry, ok := s.loadList(category, cacheCollection); ok {
		stale := time.Since(entry.UpdatedAt) >= listTTL(cacheCollection)
		if stale {
			s.queueList(category, cacheCollection)
		}
		entry.Items = s.recommendationPool(category, collection, entry.Items)
		return s.response(entry, collection, stale), nil
	}
	refreshed, err := s.refreshList(ctx, category, cacheCollection)
	if err != nil {
		return Response{}, err
	}
	refreshed.entry.Items = s.recommendationPool(category, collection, refreshed.entry.Items)
	return s.response(refreshed.entry, collection, refreshed.stale), nil
}

func (s *Service) refreshList(ctx context.Context, category, collection string) (refreshResult, error) {
	key := s.listKey(category, collection)
	result := s.flight.DoChan(key, func() (any, error) {
		old, hasOld := s.loadList(category, collection)
		if hasOld && time.Since(old.UpdatedAt) < listTTL(collection) {
			return refreshResult{entry: old}, nil
		}
		s.mu.Lock()
		retryAt := s.failures[key]
		s.mu.Unlock()
		if time.Now().Before(retryAt) {
			if hasOld {
				return refreshResult{entry: old, stale: true}, nil
			}
			return refreshResult{}, errors.New("discovery upstream temporarily unavailable")
		}
		refreshCtx, cancel := context.WithTimeout(s.ctx, 28*time.Second)
		defer cancel()
		items, err := s.fetchDouban(refreshCtx, category, collection)
		if err != nil && s.cfg.TMDBCatalogSyncEnabled && (collection == "hot" || collection == "new") {
			// A timed-out upstream context must not prevent the independent local
			// directory from publishing a current homepage selection.
			fallbackCtx, fallbackCancel := context.WithTimeout(s.ctx, 5*time.Second)
			if fallback, fallbackErr := s.catalogRecommendations(fallbackCtx, category, collection, time.Now().UTC()); fallbackErr == nil {
				items, err = fallback, nil
			}
			fallbackCancel()
		}
		if err != nil {
			s.mu.Lock()
			s.failures[key] = time.Now().Add(time.Minute)
			s.mu.Unlock()
			if hasOld {
				return refreshResult{entry: old, stale: true}, nil
			}
			return refreshResult{}, errors.New("discovery upstream temporarily unavailable")
		}
		entry := cacheEntry{Items: items, UpdatedAt: time.Now().UTC()}
		s.save(key, entry)
		s.queueHotPrefetch(category, entry, collection)
		return refreshResult{entry: entry}, nil
	})
	select {
	case <-ctx.Done():
		return refreshResult{}, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return refreshResult{}, result.Err
		}
		refreshed := result.Val.(refreshResult)
		return refreshed, nil
	}
}

type refreshResult struct {
	entry cacheEntry
	stale bool
}

func (s *Service) response(entry cacheEntry, collection string, stale bool) Response {
	items := make([]Item, 0, len(entry.Items))
	fromCatalog := isCatalogRecommendation(entry.Items)
	for _, item := range entry.Items {
		// Directory pools already carry a freshness-aware SQL cache flag. JSON
		// list snapshots do not; only a live directory pool may preserve it.
		item.DetailsCached = item.DetailsCached && fromCatalog
		metadata, ok := s.peek(s.subjectKey(item.ID, item.MediaType))
		pendingEnrichment := false
		if !ok && s.TMDBConfigured() {
			metadata, ok = s.peek(baseSubjectKey(item.ID, item.MediaType))
			pendingEnrichment = ok
		}
		if ok && len(metadata.Items) == 1 {
			item = applyMetadata(item, metadata.Items[0])
			item.DetailsCached = !pendingEnrichment && time.Since(metadata.UpdatedAt) < 24*time.Hour
		}
		items = append(items, item)
	}
	items = releasedItems(items, time.Now().UTC())
	if collection == "random" {
		rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
		for i := range items {
			items[i].Reason = "豆瓣高分 · 随机发现"
		}
	}
	response := Response{Items: items, UpdatedAt: entry.UpdatedAt, Stale: stale, TMDBConfigured: s.TMDBConfigured()}
	if stale {
		response.Warning = "正在后台更新推荐，先展示缓存内容"
	}
	return response
}

func releasedItems(items []Item, now time.Time) []Item {
	out := make([]Item, 0, len(items))
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, item := range items {
		if year, err := strconv.Atoi(item.Year); err == nil && year > now.Year() {
			continue
		}
		if date, err := time.Parse("2006-01-02", item.ReleaseDate); err == nil && date.After(today) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func (s *Service) load(key string) (cacheEntry, bool) {
	if entry, ok := s.peek(key); ok {
		return entry, true
	}
	if s.cfg.CacheDir == "" {
		return cacheEntry{}, false
	}
	file, err := os.Open(filepath.Join(s.cfg.CacheDir, key+".json"))
	if err != nil {
		return cacheEntry{}, false
	}
	defer file.Close()
	data, err := util.ReadAllLimited(file, 2<<20)
	var entry cacheEntry
	if err != nil || json.Unmarshal(data, &entry) != nil || entry.Items == nil || entry.UpdatedAt.IsZero() {
		return cacheEntry{}, false
	}
	// Disk I/O never holds the shared cache/rate-limit mutex. Another refresh
	// may have published a newer value while this snapshot was being read.
	s.mu.Lock()
	if current, ok := s.cache[key]; ok {
		s.mu.Unlock()
		return current, true
	}
	s.cache[key] = entry
	s.mu.Unlock()
	return entry, true
}

func (s *Service) peek(key string) (cacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	return entry, ok
}

func (s *Service) save(key string, entry cacheEntry) {
	s.mu.Lock()
	s.cache[key] = entry
	delete(s.failures, key)
	s.mu.Unlock()
	if s.cfg.CacheDir == "" {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil || os.MkdirAll(s.cfg.CacheDir, 0700) != nil {
		return
	}
	file, err := os.CreateTemp(s.cfg.CacheDir, ".discover-*")
	if err != nil {
		return
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return
	}
	if file.Close() != nil {
		return
	}
	_ = os.Rename(name, filepath.Join(s.cfg.CacheDir, key+".json"))
}

// Each request reserves one small slot; the bounded refresh worker pool prevents
// queued reservations from growing with the number of homepage clients.
func (s *Service) waitRate(ctx context.Context) error {
	s.mu.Lock()
	now := time.Now()
	when := s.nextRequest
	if when.Before(now) {
		when = now
	}
	s.nextRequest = when.Add(180 * time.Millisecond)
	s.mu.Unlock()
	timer := time.NewTimer(time.Until(when))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) getJSON(ctx context.Context, address, source string, target any) error {
	for attempt := 0; attempt < 2; attempt++ {
		retryable, err := s.getJSONOnce(ctx, address, source, target)
		if err == nil || !retryable || attempt == 1 || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("discovery upstream request failed")
}

// Retry transport/read failures once. HTTP rejections, oversized payloads and
// malformed JSON are final; repeating them would only burden the provider.
func (s *Service) getJSONOnce(ctx context.Context, address, source string, target any) (bool, error) {
	if err := s.waitRate(ctx); err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false, errors.New("invalid discovery upstream request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; PanSou discovery)")
	if source == "douban" {
		req.Header.Set("Referer", "https://m.douban.com/")
	}
	if source == "tmdb" && s.cfg.TMDBReadAccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.TMDBReadAccessToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return ctx.Err() == nil, errors.New("discovery upstream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("discovery upstream returned status %d", resp.StatusCode)
	}
	body, err := util.ReadAllLimited(resp.Body, 2<<20)
	if err != nil {
		return !errors.Is(err, util.ErrResponseTooLarge), errors.New("discovery upstream response too large or incomplete")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return false, errors.New("invalid discovery upstream response")
	}
	return false, nil
}

func rankItems(items []Item, category string) []Item {
	threshold := 100
	if category == "movie" {
		threshold = 500
	}
	out := make([]Item, 0, len(items))
	for _, item := range items {
		if item.DoubanRating != nil && item.DoubanVotes != nil && *item.DoubanRating >= 7.5 && *item.DoubanVotes >= threshold {
			item.Reason = "豆瓣高分 · 榜单精选"
			out = append(out, item)
		}
	}
	// Shrink low-sample ratings toward 7, preserving the original order for ties.
	score := func(item Item) float64 {
		votes := float64(*item.DoubanVotes)
		return (votes**item.DoubanRating + float64(threshold)*7) / (votes + float64(threshold))
	}
	sort.SliceStable(out, func(i, j int) bool { return score(out[i]) > score(out[j]) })
	return out
}
