package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"pansou/discover"
)

func discoverCatalogTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	items := make([]discover.Item, 0, 30)
	for i := 1; i <= 30; i++ {
		items = append(items, discover.Item{
			ID: strconv.Itoa(i), Title: "片库作品 " + strconv.Itoa(i),
			Year: "2025", MediaType: "movie", Genres: []string{"剧情"}, Tags: []string{"华语"},
		})
	}
	return discoverCatalogTestRouterWithItems(t, items)
}

func discoverCatalogTestRouterWithItems(t *testing.T, items []discover.Item) *gin.Engine {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("movie_hot.json", map[string]any{"items": items, "updated_at": time.Now().UTC()})
	// A paused, persisted catalog exercises the HTTP contract without starting
	// a provider fetch or depending on current public network data.
	write("catalog_movie.json", map[string]any{
		"version": 1, "items": items, "sources": map[string]any{},
		"updated_at": time.Now().UTC(), "limited": true,
		"limit_reason": "items", "item_limit": 20000, "byte_limit": 32 << 20,
	})
	service := discover.New(discover.Config{CacheDir: dir})
	t.Cleanup(service.Close)
	router := gin.New()
	registerDiscoverRoutes(router.Group("/api"), service)
	return router
}

func TestDiscoverHandlerRecommendationPagesAndPreservesEntirePoolTotal(t *testing.T) {
	router := discoverCatalogTestRouter(t)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response discover.Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 24 || response.Total != 30 || response.PageSize != 24 || response.Page != 1 || !response.HasMore {
		t.Fatalf("recommendation pagination or full total is incorrect: %+v", response)
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?page=2&page_size=12", nil))
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		t.Fatalf("second recommendation page returned %d", recorder.Code)
	}
	if len(response.Items) != 12 || response.Total != 30 || response.PageSize != 12 || response.Page != 2 || !response.HasMore || response.Items[0].ID != "13" {
		t.Fatalf("explicit recommendation page skipped or repeated items: %+v", response)
	}
}

func TestDiscoverHandlerRecommendationFiltersBeforePaging(t *testing.T) {
	items := make([]discover.Item, 30)
	for i := range items {
		genre := "剧情"
		if i >= 24 {
			genre = "科幻"
		}
		items[i] = discover.Item{ID: strconv.Itoa(i + 1), Title: "时空作品 " + strconv.Itoa(i+1), Year: "1995", MediaType: "movie", Genres: []string{genre}, Tags: []string{"经典"}}
	}
	router := discoverCatalogTestRouterWithItems(t, items)
	filters := url.Values{"collection": {"hot"}, "genre": {"科幻"}, "q": {"时空"}, "tag": {"经典"}, "year_from": {"1990"}, "year_to": {"1999"}, "page": {"2"}, "page_size": {"3"}}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?"+filters.Encode(), nil))
	var response discover.Response
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		t.Fatalf("filtered recommendations returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if response.Total != 6 || response.CatalogTotal != 30 || response.Page != 2 || response.PageSize != 3 || response.HasMore || len(response.Items) != 3 || response.Items[0].ID != "28" || len(response.Genres) != 2 {
		t.Fatalf("recommendation filters did not cover the complete pool: %+v", response)
	}
}

func TestDiscoverHandlerLibraryClampsPageAndPreservesTotal(t *testing.T) {
	router := discoverCatalogTestRouter(t)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?collection=library&page=999&page_size=24", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response discover.Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Page != 2 || response.Total != 30 || response.CatalogTotal != 30 || response.PageSize != 24 || len(response.Items) != 6 || response.HasMore || response.Items[0].ID != "25" {
		t.Fatalf("unexpected last catalog page: %+v", response)
	}
}

func TestDiscoverHandlerRejectsInvalidCatalogParameters(t *testing.T) {
	router := discoverCatalogTestRouter(t)
	for _, key := range []string{"page", "page_size"} {
		values := []string{"0", "-1", "not-a-number", "99999999999999999999999999999"}
		if key == "page" {
			values = append(values, "1000001")
		} else {
			values = append(values, "61")
		}
		for _, value := range values {
			t.Run(key+"="+value, func(t *testing.T) {
				query := url.Values{"collection": {"library"}, key: {value}}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?"+query.Encode(), nil))
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("invalid %s returned %d", key, recorder.Code)
				}
			})
		}
	}
	query := url.Values{"collection": {"library"}, "q": {strings.Repeat("影", 129)}}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?"+query.Encode(), nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("overlong search returned %d", recorder.Code)
	}
}

func TestDiscoverHandlerValidatesAndFiltersLibraryYears(t *testing.T) {
	router := discoverCatalogTestRouter(t)
	for _, year := range []string{"1799", "0", "25", "2025-01", "20a5", " 2025", "UNKNOWN", strconv.Itoa(time.Now().UTC().Year() + 1)} {
		query := url.Values{"collection": {"library"}, "year": {year}}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?"+query.Encode(), nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("invalid year %q returned %d", year, recorder.Code)
		}
	}
	for _, year := range []string{"2025", "unknown"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?collection=library&year="+year, nil))
		var response discover.Response
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
			t.Fatalf("valid year %q returned %d", year, recorder.Code)
		}
		expected := 30
		if year == "unknown" {
			expected = 0
		}
		if response.Total != expected || response.CatalogTotal != 30 || len(response.Years) != 1 || response.Years[0] != "2025" {
			t.Fatalf("incorrect year filter response: %+v", response)
		}
	}
}

func TestDiscoverHandlerFiltersInclusiveLibraryYearRanges(t *testing.T) {
	items := []discover.Item{
		{ID: "1", Title: "范围之前", Year: "1989", MediaType: "movie"},
		{ID: "2", Title: "时空起点", Year: "1990", MediaType: "movie", Genres: []string{"科幻"}, Tags: []string{"经典"}},
		{ID: "3", Title: "中间喜剧", Year: "1995", MediaType: "movie", Genres: []string{"喜剧"}},
		{ID: "4", Title: "时空终点", Year: "1999", MediaType: "movie", Genres: []string{"科幻"}, Tags: []string{"经典"}},
		{ID: "5", Title: "范围之后", Year: "2000", MediaType: "movie"},
		{ID: "6", Title: "年份未知", MediaType: "movie"},
		{ID: "7", Title: "年份格式未知", Year: "不详", MediaType: "movie"},
	}
	router := discoverCatalogTestRouterWithItems(t, items)
	for _, test := range []struct {
		name, suffix string
		total, page  int
		ids          []string
	}{
		{name: "inclusive_range_before_pagination", suffix: "&year_from=1990&year_to=1999&page=2&page_size=2", total: 3, page: 2, ids: []string{"4"}},
		{name: "range_and_other_filters", suffix: "&year_from=1990&year_to=1999&genre=" + url.QueryEscape("科幻") + "&tag=" + url.QueryEscape("经典") + "&q=" + url.QueryEscape("时空"), total: 2, page: 1, ids: []string{"2", "4"}},
		{name: "single_year_range", suffix: "&year_from=1990&year_to=1990", total: 1, page: 1, ids: []string{"2"}},
		{name: "empty_range", suffix: "&year_from=1900&year_to=1909&page=999", total: 0, page: 1, ids: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?collection=library"+test.suffix, nil))
			var response discover.Response
			if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
				t.Fatalf("valid year range returned %d: %s", recorder.Code, recorder.Body.String())
			}
			if response.Total != test.total || response.Page != test.page || response.CatalogTotal != len(items) || len(response.Items) != len(test.ids) || len(response.Years) != 5 || response.Years[0] != "2000" || response.Years[4] != "1989" {
				t.Fatalf("incorrect range result or narrowed facets: %+v", response)
			}
			for i, id := range test.ids {
				if response.Items[i].ID != id {
					t.Fatalf("range item %d=%s, want %s", i, response.Items[i].ID, id)
				}
			}
		})
	}
}

func TestDiscoverHandlerRejectsInvalidLibraryYearRanges(t *testing.T) {
	router := discoverCatalogTestRouter(t)
	future := strconv.Itoa(time.Now().UTC().Year() + 1)
	for _, filters := range []url.Values{
		{"year_from": {"1990"}}, {"year_to": {"1999"}},
		{"year_from": {""}}, {"year_to": {""}}, {"year_from": {""}, "year_to": {""}},
		{"year_from": {"1990"}, "year_to": {""}}, {"year_from": {""}, "year_to": {"1999"}},
		{"year_from": {"2000"}, "year_to": {"1999"}},
		{"year_from": {"1799"}, "year_to": {"1999"}},
		{"year_from": {"1990"}, "year_to": {future}},
		{"year_from": {"199"}, "year_to": {"1999"}},
		{"year_from": {"19a0"}, "year_to": {"1999"}},
		{"year_from": {"1990"}, "year_to": {"１９９９"}},
		{"year_from": {" 1990"}, "year_to": {"1999"}},
		{"year_from": {"1990"}, "year_to": {"1999-01"}},
		{"year": {"1995"}, "year_from": {"1990"}, "year_to": {"1999"}},
		{"year": {"unknown"}, "year_from": {"1990"}, "year_to": {"1999"}},
	} {
		filters.Set("collection", "library")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/discover?"+filters.Encode(), nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("invalid range %q returned %d", filters.Encode(), recorder.Code)
		}
	}
}
