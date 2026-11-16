package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

func catalogTestCollectionPage(count int, mediaType string, total int) string {
	items := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, map[string]any{
			"id": strconv.Itoa(i + 1), "title": "作品 " + strconv.Itoa(i+1),
			"type": mediaType, "year": "2025",
			"rating": map[string]any{"value": 8.5, "count": 2000},
		})
	}
	data, _ := json.Marshal(map[string]any{"subject_collection_items": items, "count": count, "total": total})
	return string(data)
}

func TestCatalogCollectionAdvancesByUpstreamPageSize(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("count") != "50" || r.URL.Query().Get("start") != "0" {
			t.Fatalf("request was not bounded: %s", r.URL.RawQuery)
		}
		// Even an upstream clamp below our requested size must advance safely.
		return jsonResponse(catalogTestCollectionPage(24, "movie", 250)), nil
	})
	page, err := s.fetchCatalogPage(context.Background(), "movie", catalogSources("movie")[2], 0, 100)
	if err != nil || page.NextStart != 24 || page.Done || page.Total != 250 || len(page.Items) != 24 {
		t.Fatalf("clamped page skipped positions: %+v error=%v", page, err)
	}
}

func TestCatalogCollectionShortPageWithTotalKeepsCursorSlots(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"count":50,"total":250,"subject_collection_items":[{"id":"1","title":"可见电影","type":"movie"}]}`), nil
	})
	page, err := s.fetchCatalogPage(context.Background(), "movie", catalogSources("movie")[0], 50, 50)
	if err != nil || page.NextStart != 100 || page.Done || len(page.Items) != 1 {
		t.Fatalf("hidden positions must not hide later pages: %+v error=%v", page, err)
	}
}

func TestCatalogEmptyPageAlwaysStops(t *testing.T) {
	for _, body := range []string{
		`{"count":50,"total":500,"subject_collection_items":[]}`,
		`{"subjects":[]}`,
	} {
		s := newTestService(t, Config{})
		s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })
		source := catalogSources("movie")[0]
		if body == `{"subjects":[]}` {
			source = doubanTagSource("movie", "最新", "time", "新片", "new")
		}
		page, err := s.fetchCatalogPage(context.Background(), "movie", source, 50, 50)
		if err != nil || !page.Done || len(page.Items) != 0 {
			t.Fatalf("empty page must stop: %+v error=%v", page, err)
		}
	}
}

func TestCatalogDedupAndTypeFilteringDoNotChangeCursor(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"count":5,"total":100,"subject_collection_items":[{"id":"1","title":"电影","type":"movie"},{"id":"1","title":"重复电影","type":"movie"},{"id":"2","title":"剧集","type":"tv"},{"id":"3","title":"其他","type":"book"},{"id":"bad","title":"无效"}]}`), nil
	})
	page, err := s.fetchCatalogPage(context.Background(), "movie", catalogSources("movie")[0], 10, 5)
	if err != nil || page.NextStart != 15 || page.Done || len(page.Items) != 1 || page.Items[0].MediaType != "movie" {
		t.Fatalf("filtering changed upstream positions: %+v error=%v", page, err)
	}
}

func TestCatalogTagListsPreserveUnknownMetadataAndUnratedItems(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("type") != "tv" || r.URL.Query().Get("tag") != "日本动画" || r.URL.Query().Get("page_start") != "24" {
			t.Fatalf("wrong tag type or cursor: %s", r.URL.RawQuery)
		}
		return jsonResponse(`{"subjects":[{"id":"1","title":"新番","rate":""},{"id":"2","title":"另一部","rate":"8.4"}]}`), nil
	})
	source := doubanTagSource("tv", "日本动画", "time", "近期日本动画", "new")
	page, err := s.fetchCatalogPage(context.Background(), "animation", source, 24, 50)
	if err != nil || !page.Done || page.NextStart != 26 || len(page.Items) != 2 {
		t.Fatalf("unexpected tag page: %+v error=%v", page, err)
	}
	for _, item := range page.Items {
		if item.Year != "" || item.DoubanVotes != nil || item.MediaType != "tv" || len(item.Tags) != 1 || item.Tags[0] != source.Label {
			t.Fatalf("invented metadata: %+v", item)
		}
	}
	if page.Items[0].DoubanRating != nil || page.Items[1].DoubanRating == nil || *page.Items[1].DoubanRating != 8.4 {
		t.Fatal("blank or real ratings were lost")
	}
}

func TestCatalogDocumentaryMovieKeepsMovieType(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("type") != "movie" {
			t.Fatal("documentary film queried as TV")
		}
		return jsonResponse(`{"subjects":[{"id":"1","title":"纪录电影","rate":"9.0"}]}`), nil
	})
	source := doubanTagSource("movie", "纪录片", "recommend", "纪录电影", "hot")
	page, err := s.fetchCatalogPage(context.Background(), "documentary", source, 0, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].MediaType != "movie" || len(page.Items[0].Genres) != 1 || page.Items[0].Genres[0] != "纪录片" {
		t.Fatalf("documentary type lost: %+v error=%v", page, err)
	}
}

func TestCatalogSourcesUseVerifiedCategoriesAndStableUniqueKeys(t *testing.T) {
	for _, category := range []string{"movie", "tv", "animation", "variety", "documentary"} {
		sources := catalogSources(category)
		if len(sources) == 0 || sources[0].collectionID == "" {
			t.Fatalf("%s lost its rich initial list", category)
		}
		seen := map[string]bool{}
		for _, source := range sources {
			if source.Key == "" || seen[source.Key] {
				t.Fatalf("duplicate %s source key %q", category, source.Key)
			}
			seen[source.Key] = true
			if source.tag == "国产动画" || source.tag == "动画" && source.mediaType == "tv" {
				t.Fatal("included an empty, unverified animation source")
			}
		}
	}
	if len(catalogSources("invalid")) != 0 {
		t.Fatal("unknown category should not have sources")
	}
}
