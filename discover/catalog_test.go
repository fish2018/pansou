package discover

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func completedCatalog(category string, items []Item) catalogSnapshot {
	now := time.Now().UTC()
	snapshot := catalogSnapshot{Version: 1, Items: items, Sources: make(map[string]catalogCursor), UpdatedAt: now}
	for _, source := range catalogSources(category) {
		snapshot.Sources[source.Key] = catalogCursor{Done: true, CompletedAt: now}
	}
	return snapshot
}

func TestLibraryFiltersAndFacetsUseEntireDeduplicatedPool(t *testing.T) {
	s := newTestService(t, Config{})
	items := make([]Item, 60)
	for i := range items {
		items[i] = Item{ID: strconv.Itoa(i + 1), Title: fmt.Sprintf("电影%d", i+1), MediaType: "movie", Genres: []string{"剧情"}, Tags: []string{"经典"}}
	}
	items[58].Title = "哈利·波特"
	items[59].Genres = []string{"科幻"}
	s.saveCatalog("movie", completedCatalog("movie", items))
	s.save(s.subjectKey("60", "movie"), cacheEntry{Items: []Item{{ID: "60", Genres: []string{"科幻", "冒险"}}}, UpdatedAt: time.Now()})
	page1, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 1, PageSize: 24})
	if err != nil || page1.Total != 60 || page1.CatalogTotal != 60 || !page1.HasMore || page1.Indexing || len(page1.Items) != 24 || !contains(page1.Genres, "冒险") {
		t.Fatalf("incorrect full-library counts/facets: %+v %v", page1, err)
	}
	page2, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 2, PageSize: 24})
	if err != nil || page1.Items[23].ID == page2.Items[0].ID || page2.Items[0].ID != "25" {
		t.Fatalf("pages overlap or skip: %+v %v", page2, err)
	}
	filtered, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 999, PageSize: 24, Genre: "科幻", Tag: "经典"})
	if err != nil || filtered.Total != 1 || filtered.CatalogTotal != 60 || filtered.Page != 1 || filtered.HasMore || filtered.Items[0].ID != "60" {
		t.Fatalf("filter did not precede paging/clamp: %+v %v", filtered, err)
	}
	matched, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 1, PageSize: 24, Query: "哈利波特"})
	if err != nil || matched.Total != 1 || matched.Items[0].ID != "59" {
		t.Fatalf("normalized title search missed deep entry: %+v %v", matched, err)
	}
}

func TestRecommendationQueriesPageAndFilterTheWholePool(t *testing.T) {
	s := newTestService(t, Config{})
	rating, votes := 8.5, 2000
	items, ids := make([]Item, 65), make([]string, 60)
	for i := range items {
		genre := "剧情"
		if i >= 24 {
			genre = "喜剧"
		}
		if i >= 48 {
			genre = "科幻"
		}
		items[i] = Item{ID: strconv.Itoa(i + 1), Title: "片库作品 " + strconv.Itoa(i+1), Year: "2025", MediaType: "tv", Genres: []string{genre}, Tags: []string{"经典"}}
		if i < len(ids) {
			ids[i] = items[i].ID
			items[i].DoubanRating, items[i].DoubanVotes = &rating, &votes
		} else {
			items[i].Genres = []string{"仅在片库"}
		}
	}
	snapshot := completedCatalog("tv", items)
	for _, source := range catalogSources("tv") {
		if source.Collection == "hot" || source.Collection == "new" {
			cursor := snapshot.Sources[source.Key]
			cursor.IDs = ids
			snapshot.Sources[source.Key] = cursor
		}
	}
	s.saveCatalog("tv", snapshot)
	for _, collection := range []string{"hot", "new", "top"} {
		s.save(s.listKey("tv", collection), cacheEntry{Items: items[:12], UpdatedAt: time.Now()})
	}
	for _, collection := range []string{"hot", "new", "top", "random"} {
		t.Run(collection, func(t *testing.T) {
			first, err := s.Query(context.Background(), "tv", collection, ListOptions{Page: 1, PageSize: 12})
			if err != nil || len(first.Items) != 12 || first.Total != 60 || first.CatalogTotal != 65 || !first.HasMore || !reflect.DeepEqual(first.Genres, []string{"剧情", "喜剧", "科幻"}) {
				t.Fatalf("page lost the complete recommendation pool or its genres: %+v %v", first, err)
			}
			if collection != "random" {
				second, err := s.Query(context.Background(), "tv", collection, ListOptions{Page: 2, PageSize: 12})
				if err != nil || second.Total != 60 || second.Page != 2 || second.Items[0].ID != "13" {
					t.Fatalf("second recommendation page skipped or repeated items: %+v %v", second, err)
				}
			}
			filtered, err := s.Query(context.Background(), "tv", collection, ListOptions{Page: 2, PageSize: 6, Genre: "科幻", Year: "2025", Query: "片库作品", Tag: "经典"})
			if err != nil || len(filtered.Items) != 6 || filtered.Total != 12 || filtered.CatalogTotal != 65 || filtered.Page != 2 || filtered.HasMore || !reflect.DeepEqual(filtered.Genres, first.Genres) {
				t.Fatalf("filters were applied to the current page or narrowed genre choices: %+v %v", filtered, err)
			}
			seen := make(map[string]bool)
			for _, item := range filtered.Items {
				if !contains(item.Genres, "科幻") || seen[item.ID] {
					t.Fatalf("filtered recommendation is incorrect or duplicated: %+v", item)
				}
				seen[item.ID] = true
			}
			if collection != "random" && filtered.Items[0].ID != "55" {
				t.Fatalf("deep genre match was omitted: %+v", filtered.Items)
			}
			full, err := s.Get(context.Background(), "tv", collection)
			if err != nil || len(full.Items) != 60 || full.Total != 60 {
				t.Fatalf("Get no longer supplies the complete internal pool: %+v %v", full, err)
			}
		})
	}
}

func TestCatalogPageMergesMetadataAndStopsRepeatedOffsetsWithoutLosingOldTail(t *testing.T) {
	rating, votes := 8.6, 1500
	source := catalogSources("movie")[0]
	old := Item{ID: "1", Title: "电影", Year: "2001", MediaType: "movie", Genres: []string{"剧情"}, DoubanRating: &rating, DoubanVotes: &votes, Overview: "不存入基本片库"}
	snapshot := catalogSnapshot{Version: 1, Items: []Item{}, Sources: map[string]catalogCursor{}}
	snapshot = mergeCatalogPage(snapshot, source, catalogCursor{}, catalogPage{Items: []Item{old, old}, NextStart: 50}, time.Now(), catalogMaxItems, catalogMaxBytes)
	if len(snapshot.Items) != 1 || len(snapshot.Sources[source.Key].IDs) != 1 || snapshot.Items[0].Overview != "" {
		t.Fatalf("duplicate/basic snapshot merge failed: %+v", snapshot)
	}
	cursor := snapshot.Sources[source.Key]
	cursor.PreviousIDs = []string{"1", "2", "3"}
	snapshot = mergeCatalogPage(snapshot, source, cursor, catalogPage{Items: []Item{{ID: "1", Title: "更新片名"}}, NextStart: 100}, time.Now(), catalogMaxItems, catalogMaxBytes)
	got := snapshot.Items[0]
	if !snapshot.Sources[source.Key].Done || len(snapshot.Sources[source.Key].IDs) != 3 || got.Year != "2001" || got.DoubanVotes == nil || *got.DoubanVotes != votes || !contains(got.Genres, "剧情") {
		t.Fatalf("repeated page dropped known metadata/membership: %+v", snapshot)
	}
}

func TestCatalogCursorResumesAndRotatesAfterRestartAndFailure(t *testing.T) {
	dir := t.TempDir()
	s := newTestService(t, Config{CacheDir: dir})
	s.catalogInterval = 0
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "movie_hot_gaia") || r.URL.Query().Get("start") != "0" {
			t.Errorf("first source did not begin at zero: %s", r.URL.Path)
		}
		return jsonResponse(`{"count":50,"total":100,"subject_collection_items":[{"id":"1","title":"第一部","type":"movie"}]}`), nil
	})
	first, err := s.advanceCatalog(context.Background(), "movie")
	if err != nil || len(first.Items) != 1 || first.NextSource != 1 || first.Sources[catalogSources("movie")[0].Key].NextStart != 50 {
		t.Fatalf("first cursor incorrect: %+v %v", first, err)
	}
	restarted := newTestService(t, Config{CacheDir: dir})
	restarted.catalogInterval = 0
	var calls atomic.Int32
	restarted.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if !strings.Contains(r.URL.Path, "movie_latest") {
			t.Errorf("restart did not resume next source: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: http.NoBody}, nil
	})
	if _, err := restarted.advanceCatalog(context.Background(), "movie"); err == nil {
		t.Fatal("failed source was reported as success")
	}
	after, _ := restarted.peekCatalog("movie")
	if calls.Load() != 1 || len(after.Items) != 1 || after.Items[0].ID != "1" || after.NextSource != 2 || after.Sources[catalogSources("movie")[0].Key].NextStart != 50 {
		t.Fatalf("failure retried HTTP rejection or lost progress: %+v calls=%d", after, calls.Load())
	}
	third := newTestService(t, Config{CacheDir: dir})
	loaded, ok := third.loadCatalog("movie")
	if !ok || loaded.NextSource != 2 || len(loaded.Items) != 1 {
		t.Fatalf("failure cursor/data not persisted: %+v", loaded)
	}
}

func TestLibrarySeedReturnsImmediatelyWhileIndexingAndColdSeedFetchesOnlyOnePage(t *testing.T) {
	s := newTestService(t, Config{})
	s.save("movie_hot", cacheEntry{Items: []Item{{ID: "1", Title: "缓存电影", MediaType: "movie"}}, UpdatedAt: time.Now()})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, err := s.Query(ctx, "movie", "library", ListOptions{Page: 1, PageSize: 24})
	if err != nil || got.Total != 1 || !got.Indexing {
		t.Fatalf("seed blocked on full index: %+v %v", got, err)
	}
	cold := newTestService(t, Config{})
	var calls atomic.Int32
	cold.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(`{"count":50,"total":250,"subject_collection_items":[{"id":"2","title":"首批电影","type":"movie"}]}`), nil
	})
	got, err = cold.Query(context.Background(), "movie", "library", ListOptions{Page: 1, PageSize: 24})
	if err != nil || got.Total != 1 || !got.Indexing || calls.Load() != 1 {
		t.Fatalf("cold request attempted full crawl: %+v calls=%d %v", got, calls.Load(), err)
	}
	unavailable := newTestService(t, Config{})
	for _, source := range catalogSources("movie") {
		unavailable.failures["catalog:movie:"+source.Key] = time.Now().Add(time.Minute)
	}
	if _, err := unavailable.Get(context.Background(), "movie", "library"); err == nil {
		t.Fatal("a cold catalog with all sources unavailable became a successful empty library")
	}
}

func TestExpandedRecommendationsSurviveFastRefreshAndRequireRealVotes(t *testing.T) {
	s := newTestService(t, Config{})
	rating, votes := 8.9, 2000
	items, ids := make([]Item, 250), make([]string, 250)
	for i := range items {
		ids[i] = strconv.Itoa(i + 1)
		items[i] = Item{ID: ids[i], Title: "电影", MediaType: "movie", DoubanRating: &rating, DoubanVotes: &votes}
	}
	snapshot := completedCatalog("movie", items)
	source := catalogSources("movie")[2]
	cursor := snapshot.Sources[source.Key]
	cursor.IDs = ids
	snapshot.Sources[source.Key] = cursor
	s.saveCatalog("movie", snapshot)
	s.save("movie_top", cacheEntry{Items: items[:50], UpdatedAt: time.Now()})
	got, err := s.Get(context.Background(), "movie", "top")
	if err != nil || len(got.Items) != 250 || got.Total != 250 {
		t.Fatalf("fast refresh shrank the expanded recommendation pool: total=%d %v", len(got.Items), err)
	}
	tv := []Item{{ID: "1", Title: "真实票数", DoubanRating: &rating, DoubanVotes: &votes}, {ID: "2", Title: "票数未知", DoubanRating: &rating}}
	s.saveCatalog("tv", completedCatalog("tv", tv))
	if ranked := s.recommendationPool("tv", "top", nil); len(ranked) != 1 || ranked[0].ID != "1" {
		t.Fatalf("top pool invented vote eligibility: %+v", ranked)
	}
}

func TestCatalogBoundsAndPageValidation(t *testing.T) {
	items := make([]Item, catalogMaxItems+1)
	for i := range items {
		items[i] = Item{ID: strconv.Itoa(i + 1), Title: "电影"}
	}
	snapshot := mergeCatalogPage(catalogSnapshot{Version: 1, Items: []Item{}, Sources: map[string]catalogCursor{}}, catalogSources("movie")[0], catalogCursor{}, catalogPage{Items: items, NextStart: len(items)}, time.Now(), catalogMaxItems, catalogMaxBytes)
	if len(snapshot.Items) != catalogMaxItems || !snapshot.Limited || catalogIndexing(snapshot, catalogSources("movie"), time.Now()) {
		t.Fatal("catalog resource cap/indexing state incorrect")
	}
	for _, options := range []ListOptions{{Page: 0, PageSize: 24}, {Page: 1000001, PageSize: 24}, {Page: 1, PageSize: 61}, {Page: 1, PageSize: 24, Query: strings.Repeat("字", 129)}} {
		if options.Valid() {
			t.Fatalf("unbounded options accepted: %+v", options)
		}
	}
}

func TestCatalogGlobalPageRateCannotBeBypassedByVisitors(t *testing.T) {
	s := newTestService(t, Config{})
	s.catalogInterval = 40 * time.Millisecond
	if err := s.waitCatalogPage(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := s.waitCatalogPage(ctx); err == nil {
		t.Fatal("visitor bypassed global catalog page interval")
	}
}

func TestLibraryYearFilteringUsesEnrichedYearsBeforePaging(t *testing.T) {
	s := newTestService(t, Config{})
	items := []Item{
		{ID: "1", Title: "旧电影", Year: "1999", MediaType: "movie"},
		{ID: "2", Title: "另一部旧电影", Year: "1999", MediaType: "movie"},
		{ID: "3", Title: "年份未知", MediaType: "movie"},
		{ID: "4", Title: "待补年份", MediaType: "movie"},
		{ID: "5", Title: "年份格式未知", Year: "不详", MediaType: "movie"},
		{ID: "6", Title: "未来作品", Year: strconv.Itoa(time.Now().UTC().Year() + 1), MediaType: "movie"},
	}
	s.saveCatalog("movie", completedCatalog("movie", items))
	s.save("movie_hot", cacheEntry{Items: items[:1], UpdatedAt: time.Now()})
	s.save(s.subjectKey("4", "movie"), cacheEntry{Items: []Item{{ID: "4", Year: "2005"}}, UpdatedAt: time.Now()})
	page, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 2, PageSize: 1, Year: "1999"})
	if err != nil || page.Total != 2 || page.CatalogTotal != 5 || len(page.Items) != 1 || page.Items[0].ID != "2" || !reflect.DeepEqual(page.Years, []string{"2005", "1999"}) {
		t.Fatalf("year filter/facets did not precede pagination: %+v %v", page, err)
	}
	for _, collection := range []string{"library", "hot"} {
		got, err := s.Get(context.Background(), "movie", collection)
		if err != nil || !reflect.DeepEqual(got.Years, page.Years) {
			t.Fatalf("%s facets disagree with enriched full library: %+v %v", collection, got, err)
		}
	}
	enriched, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 1, PageSize: 24, Year: "2005"})
	if err != nil || enriched.Total != 1 || enriched.Items[0].ID != "4" {
		t.Fatalf("real subject year was not searchable: %+v %v", enriched, err)
	}
	unknown, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 1, PageSize: 24, Year: "unknown"})
	if err != nil || unknown.Total != 2 || unknown.Items[0].ID != "3" || unknown.Items[1].ID != "5" || !reflect.DeepEqual(unknown.Years, page.Years) {
		t.Fatalf("unknown year was guessed or included in numeric facets: %+v %v", unknown, err)
	}
	for _, year := range []string{"1799", "999", "20000", "2025-01-01", "20a5", " 2005", "UNKNOWN", strconv.Itoa(time.Now().UTC().Year() + 1)} {
		if (ListOptions{Page: 1, PageSize: 24, Year: year}).Valid() {
			t.Errorf("invalid year %q accepted", year)
		}
	}
}

func TestLibraryYearRangesCombineFiltersBeforePagingAndPreserveFacets(t *testing.T) {
	s := newTestService(t, Config{})
	currentYear := strconv.Itoa(time.Now().UTC().Year())
	items := []Item{
		{ID: "1", Title: "范围之前", Year: "1989", MediaType: "movie"},
		{ID: "2", Title: "时空起点", Year: "1990", MediaType: "movie", Genres: []string{"科幻"}, Tags: []string{"经典"}},
		{ID: "3", Title: "中间喜剧", Year: "1995", MediaType: "movie", Genres: []string{"喜剧"}},
		{ID: "4", Title: "时空终点", Year: "1999", MediaType: "movie", Genres: []string{"科幻"}, Tags: []string{"经典"}},
		{ID: "5", Title: "范围之后", Year: "2000", MediaType: "movie"},
		{ID: "6", Title: "年份未知", MediaType: "movie"},
		{ID: "7", Title: "年份格式未知", Year: "不详", MediaType: "movie"},
		{ID: "8", Title: "时空补全", MediaType: "movie", Genres: []string{"科幻"}, Tags: []string{"经典"}},
		{ID: "9", Title: "最早边界", Year: "1800", MediaType: "movie"},
		{ID: "10", Title: "当前年份", Year: currentYear, MediaType: "movie"},
	}
	s.saveCatalog("movie", completedCatalog("movie", items))
	s.save(s.subjectKey("8", "movie"), cacheEntry{Items: []Item{{ID: "8", Year: "1994"}}, UpdatedAt: time.Now()})
	wantYears := []string{currentYear, "2000", "1999", "1995", "1994", "1990", "1989", "1800"}
	page, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 2, PageSize: 2, YearFrom: "1990", YearTo: "1999"})
	if err != nil || page.Total != 4 || page.CatalogTotal != len(items) || len(page.Items) != 2 || page.Items[0].ID != "4" || page.Items[1].ID != "8" || page.Page != 2 || page.HasMore || !reflect.DeepEqual(page.Years, wantYears) {
		t.Fatalf("year range did not include boundaries/enrichment before paging: %+v %v", page, err)
	}
	combined, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 1, PageSize: 24, YearFrom: "1990", YearTo: "1999", Genre: "科幻", Tag: "经典", Query: "时空"})
	if err != nil || combined.Total != 3 || len(combined.Items) != 3 || combined.Items[0].ID != "2" || combined.Items[1].ID != "4" || combined.Items[2].ID != "8" || !reflect.DeepEqual(combined.Years, wantYears) {
		t.Fatalf("range/genre/tag/title filters did not combine: %+v %v", combined, err)
	}
	for _, bounds := range []struct{ from, to, id string }{{"1800", "1800", "9"}, {currentYear, currentYear, "10"}} {
		got, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 1, PageSize: 24, YearFrom: bounds.from, YearTo: bounds.to})
		if err != nil || got.Total != 1 || len(got.Items) != 1 || got.Items[0].ID != bounds.id {
			t.Fatalf("inclusive range boundary %s..%s rejected: %+v %v", bounds.from, bounds.to, got, err)
		}
	}
	empty, err := s.Query(context.Background(), "movie", "library", ListOptions{Page: 999, PageSize: 24, YearFrom: "1900", YearTo: "1909"})
	if err != nil || empty.Total != 0 || empty.Page != 1 || len(empty.Items) != 0 || empty.HasMore || empty.CatalogTotal != len(items) || !reflect.DeepEqual(empty.Years, wantYears) {
		t.Fatalf("empty range included unknown years or lost full facets: %+v %v", empty, err)
	}
}

func TestLibraryRejectsInvalidYearRangeOptions(t *testing.T) {
	future := strconv.Itoa(time.Now().UTC().Year() + 1)
	for _, filters := range []struct{ year, from, to string }{
		{"", "1990", ""}, {"", "", "1999"}, {"", "2000", "1999"},
		{"", "1799", "1999"}, {"", "1990", future}, {"", "199", "1999"},
		{"", "19a0", "1999"}, {"", "1990", "１９９９"}, {"", " 1990", "1999"},
		{"", "1990", "1999-01"}, {"1995", "1990", "1999"}, {"unknown", "1990", "1999"},
	} {
		options := ListOptions{Page: 1, PageSize: 24, Year: filters.year, YearFrom: filters.from, YearTo: filters.to}
		if options.Valid() {
			t.Errorf("invalid range options accepted: %+v", options)
		}
	}
}

func TestNewCatalogSourcesGetAnEarlyFirstPageAfterUpgrade(t *testing.T) {
	sources := []catalogSource{{Key: "old_hot"}, {Key: "new_2025"}, {Key: "new_1990"}, {Key: "old_deep"}}
	snapshot := catalogSnapshot{NextSource: 3, Sources: map[string]catalogCursor{"old_hot": {NextStart: 200}, "old_deep": {NextStart: 450}}}
	if order := catalogSourceOrder(snapshot, sources); !reflect.DeepEqual(order, []int{1, 2, 3, 0}) {
		t.Fatalf("old deep cursor postponed new historical first pages: %v", order)
	}
	snapshot.Sources["new_2025"] = catalogCursor{NextStart: 50}
	if order := catalogSourceOrder(snapshot, sources); !reflect.DeepEqual(order, []int{2, 3, 0, 1}) {
		t.Fatalf("first-page priority did not retain fair deep-page rotation: %v", order)
	}
}

func TestCatalogCapacityResumesTheTruncatedPageAndLowerLimitsKeepExistingData(t *testing.T) {
	source := catalogSources("movie")[0]
	items := []Item{{ID: "1", Title: "一"}, {ID: "2", Title: "二"}, {ID: "3", Title: "三"}}
	originalCursor := catalogCursor{NextStart: 50, IDs: []string{"1", "2", "3"}}
	snapshot := catalogSnapshot{Version: 1, Items: items, Sources: map[string]catalogCursor{source.Key: originalCursor}, UpdatedAt: time.Now()}
	page := catalogPage{Items: []Item{{ID: "4", Title: "四"}, {ID: "5", Title: "五"}}, NextStart: 100}
	limited := mergeCatalogPage(snapshot, source, originalCursor, page, time.Now(), 4, catalogMaxBytes)
	if !limited.Limited || limited.LimitReason != "items" || len(limited.Items) != 4 || limited.Sources[source.Key].NextStart != 50 {
		t.Fatalf("item limit advanced beyond a truncated page: %+v", limited)
	}
	dir := t.TempDir()
	s := newTestService(t, Config{CacheDir: dir, CatalogMaxItems: 4})
	s.saveCatalog("movie", limited)
	raised := newTestService(t, Config{CacheDir: dir, CatalogMaxItems: 5})
	loaded, ok := raised.loadCatalog("movie")
	if !ok || loaded.Limited || len(loaded.Items) != 4 || loaded.Sources[source.Key].NextStart != 50 {
		t.Fatalf("raising capacity did not restore the persisted boundary cursor: %+v", loaded)
	}
	expanded := mergeCatalogPage(loaded, source, loaded.Sources[source.Key], page, time.Now(), 5, catalogMaxBytes)
	if expanded.Limited || len(expanded.Items) != 5 || expanded.Items[4].ID != "5" {
		t.Fatalf("resuming omitted a truncated tail entry: %+v", expanded)
	}
	raised.saveCatalog("movie", expanded)
	lowered := newTestService(t, Config{CacheDir: dir, CatalogMaxItems: 2})
	retained, ok := lowered.loadCatalog("movie")
	if !ok || len(retained.Items) != 5 {
		t.Fatal("lower configuration invalidated a previously collected snapshot")
	}
	retained = mergeCatalogPage(retained, source, retained.Sources[source.Key], catalogPage{Items: []Item{{ID: "6", Title: "六"}}, NextStart: 150}, time.Now(), 2, catalogMaxBytes)
	if !retained.Limited || len(retained.Items) != 5 || retained.Items[4].ID != "5" {
		t.Fatal("lower item capacity deleted existing data or admitted new data")
	}

	longItem := Item{ID: "9", Title: strings.Repeat("长", 600)}
	byteLimited := mergeCatalogPage(catalogSnapshot{Version: 1, Items: []Item{}, Sources: map[string]catalogCursor{}}, source, catalogCursor{}, catalogPage{Items: []Item{longItem}, NextStart: 50}, time.Now(), 10, 1024)
	if !byteLimited.Limited || byteLimited.LimitReason != "bytes" || byteLimited.Sources[source.Key].NextStart != 0 {
		t.Fatal("byte limit lost its reason or boundary cursor")
	}
	if resumeCatalog(byteLimited, 20, 1024).Limited != true || resumeCatalog(byteLimited, 10, 4096).Limited {
		t.Fatal("only a matching capacity increase should resume indexing")
	}
	oldLarge := catalogSnapshot{Version: 1, Items: []Item{longItem}, Sources: map[string]catalogCursor{}}
	lowerBytes := mergeCatalogPage(oldLarge, source, catalogCursor{}, catalogPage{Items: []Item{{ID: "10", Title: "新作"}}, NextStart: 50}, time.Now(), 10, 1024)
	if !lowerBytes.Limited || len(lowerBytes.Items) != 1 || lowerBytes.Items[0].Title != longItem.Title {
		t.Fatal("lower byte capacity removed an existing large record")
	}
}

func TestLegacyLimitedCatalogPreservesMembersWhileCapacityIncreaseRestartsItsPages(t *testing.T) {
	legacy := catalogSnapshot{Version: 1, Items: make([]Item, 5000), Sources: map[string]catalogCursor{"old": {NextStart: 100, IDs: []string{"1", "2"}}}, Limited: true}
	resumed := resumeCatalog(legacy, catalogMaxItems, catalogMaxBytes)
	if resumed.Limited || len(resumed.Items) != 5000 || resumed.Sources["old"].NextStart != 0 || !reflect.DeepEqual(resumed.Sources["old"].PreviousIDs, []string{"1", "2"}) {
		t.Fatal("legacy capacity increase lost data or source membership")
	}
	legacy.Items = legacy.Items[:2]
	if !resumeCatalog(legacy, catalogMaxItems, 4<<20).Limited || resumeCatalog(legacy, catalogMaxItems, catalogMaxBytes).Limited {
		t.Fatal("legacy byte bound resumed without sufficient byte capacity")
	}
}

func TestCatalogEnvironmentCapacityHasSafeDefaultsAndHardMaximums(t *testing.T) {
	for _, test := range []struct {
		items, mb            string
		wantItems, wantBytes int
	}{
		{"", "", catalogMaxItems, catalogMaxBytes},
		{"20000", "32", catalogHardMaxItems, catalogHardMaxBytes},
		{"20001", "33", catalogMaxItems, catalogMaxBytes},
		{"-1", "not-a-number", catalogMaxItems, catalogMaxBytes},
	} {
		t.Setenv("DISCOVER_CATALOG_MAX_ITEMS", test.items)
		t.Setenv("DISCOVER_CATALOG_MAX_MB", test.mb)
		cfg := ConfigFromEnv()
		if cfg.CatalogMaxItems != test.wantItems || cfg.CatalogMaxBytes != test.wantBytes {
			t.Fatalf("unsafe capacity config: %+v", cfg)
		}
	}
}
