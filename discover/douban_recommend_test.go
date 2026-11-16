package discover

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRecommendPageUsesRealYearTypeAndRating(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if r.URL.Path != "/rexxar/api/v2/movie/recommend" || q.Get("tags") != "2000" || q.Get("sort") != "U" || q.Get("start") != "20" || q.Get("count") != "50" {
			t.Fatalf("wrong year query: %s", r.URL)
		}
		return jsonResponse(`{"count":20,"total":500,"items":[
			{"id":"1","title":"真实电影","type":"movie","year":"2000","card_subtitle":"2000 / 中国大陆 / 剧情","rating":{"value":8.6,"count":2400}},
			{"id":"1","title":"重复电影","type":"movie","year":"2000"},
			{"id":"2","title":"标签误年","type":"movie","year":"1999"},
			{"id":"3","title":"未知年份","type":"movie","card_subtitle":"2000 / 中国大陆 / 剧情"},
			{"id":"4","title":"错误类型","type":"tv","year":"2000"},
			{"id":"5","title":"未评分电影","type":"movie","year":"2000","rating":{"value":0,"count":0}},
			{"id":"6","title":"播放单","type":"playlist","year":"2000"},
			{"id":"7","title":"未知类型","year":"2000"}
		]}`), nil
	})
	source := doubanYearSource("movie", "", 2000)
	page, err := s.fetchCatalogPage(context.Background(), "movie", source, 20, 100)
	if err != nil || page.NextStart != 40 || page.Done || page.Total != 500 || len(page.Items) != 2 {
		t.Fatalf("unsafe candidate mapping: %+v err=%v", page, err)
	}
	item := page.Items[0]
	if item.Year != "2000" || item.MediaType != "movie" || item.DoubanRating == nil || *item.DoubanRating != 8.6 || item.DoubanVotes == nil || *item.DoubanVotes != 2400 || !contains(item.Genres, "剧情") {
		t.Fatalf("real metadata lost: %+v", item)
	}
	if page.Items[1].DoubanRating != nil || page.Items[1].DoubanVotes != nil || page.Items[1].Year != "2000" {
		t.Fatal("unrated movie was lost or received a fabricated score")
	}
}

func TestRecommendHiddenShortPagesAndFilteredPagesKeepAdvancing(t *testing.T) {
	for _, body := range []string{
		`{"count":20,"total":500,"items":[{"id":"1","title":"电影","type":"movie","year":"1990"}]}`,
		`{"count":20,"total":500,"items":[{"id":"1","title":"错误标签","type":"movie","year":"1989"}]}`,
	} {
		s := newTestService(t, Config{})
		s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })
		page, err := s.fetchCatalogPage(context.Background(), "movie", doubanYearSource("movie", "", 1990), 20, 20)
		if err != nil || page.Done || page.NextStart != 40 {
			t.Fatalf("hidden candidate positions stopped exploration: %+v err=%v", page, err)
		}
	}
}

func TestRecommendSpecialCategoriesRequireRealGenresAndMediaType(t *testing.T) {
	for _, tc := range []struct {
		category, mediaType, tag, genres string
	}{
		{"animation", "tv", "动画", "剧情 动画 奇幻"},
		{"animation", "movie", "动画", "动画 短片"},
		{"documentary", "tv", "纪录片", "纪录片"},
		{"documentary", "movie", "纪录片", "纪录片 音乐"},
		{"variety", "tv", "综艺", "脱口秀"},
	} {
		t.Run(tc.category+tc.mediaType, func(t *testing.T) {
			s := newTestService(t, Config{})
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("tags") != "2024,"+tc.tag {
					t.Fatalf("wrong special category filter: %s", r.URL.RawQuery)
				}
				body := `{"count":50,"total":500,"items":[{"id":"1","title":"准确分类","type":"` + tc.mediaType + `","year":"2024","card_subtitle":"2024 / 中国大陆 / ` + tc.genres + `"},{"id":"2","title":"用户标签误分类","type":"` + tc.mediaType + `","year":"2024","card_subtitle":"2024 / 美国 / 剧情 喜剧"},{"id":"3","title":"只有标签没有类型","type":"` + tc.mediaType + `","year":"2024"},{"id":"4","title":"不支持的类型","type":"book","year":"2024","genres":["` + tc.tag + `"]}]}`
				return jsonResponse(body), nil
			})
			page, err := s.fetchCatalogPage(context.Background(), tc.category, doubanYearSource(tc.mediaType, tc.tag, 2024), 0, 50)
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != "1" || page.Items[0].MediaType != tc.mediaType || !contains(page.Items[0].Genres, strings.Fields(tc.genres)[0]) {
				t.Fatalf("category mixed unrelated candidates: %+v err=%v", page, err)
			}
		})
	}
}

func TestRecommendDramaSourceExcludesKnownOtherTVGenres(t *testing.T) {
	s := newTestService(t, Config{})
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"count":50,"total":500,"items":[
			{"id":"1","title":"电视剧","type":"tv","year":"2024","genres":["剧情","喜剧"]},
			{"id":"2","title":"动画","type":"tv","year":"2024","genres":["动画"]},
			{"id":"3","title":"纪录片","type":"tv","year":"2024","genres":["纪录片"]},
			{"id":"4","title":"真人秀","type":"tv","year":"2024","genres":["真人秀"]},
			{"id":"5","title":"脱口秀","type":"tv","year":"2024","genres":["脱口秀"]}
		]}`), nil
	})
	page, err := s.fetchCatalogPage(context.Background(), "tv", doubanYearSource("tv", "", 2024), 0, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "1" {
		t.Fatalf("drama year source mixed animation/documentary/variety: %+v err=%v", page, err)
	}
}

func TestRecommendEmptyInvalidAndBoundedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		start      int
		wantErr    bool
	}{
		{"empty", `{"count":50,"total":500,"items":[]}`, 0, false},
		{"malformed", `{"message":"upstream unavailable"}`, 0, true},
		{"last page", `{"count":50,"total":1000,"items":[{"id":"1","title":"电影","type":"movie","year":"2000"}]}`, 450, false},
		{"past bound", "", 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(t, Config{})
			calls := 0
			s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return jsonResponse(tc.body), nil
			})
			page, err := s.fetchCatalogPage(context.Background(), "movie", doubanYearSource("movie", "", 2000), tc.start, 50)
			if (err != nil) != tc.wantErr || !tc.wantErr && !page.Done || tc.start >= 500 && calls != 0 {
				t.Fatalf("invalid stop/error handling: %+v err=%v calls=%d", page, err, calls)
			}
		})
	}
}

func TestRecommendYearSourcesBalanceRecentAndHistoricCoverage(t *testing.T) {
	years := doubanRecommendYears(2026)
	wantPrefix := []int{2026, 2025, 2024, 2020, 2015, 2010, 2005, 2000, 1990, 1980, 1970, 1960, 1950}
	if len(years) != 32 {
		t.Fatalf("unexpected year coverage %v", years)
	}
	for i, year := range wantPrefix {
		if years[i] != year {
			t.Fatalf("historic coverage delayed: %v", years)
		}
	}
	seenYears := make(map[int]bool)
	for _, year := range years {
		if seenYears[year] {
			t.Fatal("duplicate year")
		}
		seenYears[year] = true
	}
	for year := 2000; year <= 2026; year++ {
		if !seenYears[year] {
			t.Fatalf("missing recent year %d", year)
		}
	}
	for _, category := range []string{"movie", "tv", "animation", "variety", "documentary"} {
		fresh := doubanYearSources(category)
		expected := len(doubanRecommendYears(time.Now().UTC().Year()))
		if category == "animation" || category == "documentary" {
			expected *= 2
		}
		if len(fresh) != expected {
			t.Fatalf("%s missing yearly media sources: %d", category, len(fresh))
		}
		seen := map[string]bool{}
		for _, source := range catalogSources(category) {
			if seen[source.Key] {
				t.Fatalf("duplicate source %s", source.Key)
			}
			seen[source.Key] = true
			if source.recommendYear != 0 && (source.Collection != "" || !strings.HasPrefix(source.Key, "recommend_")) {
				t.Fatal("year source changed a curated recommendation collection")
			}
		}
		for _, legacy := range doubanCatalogSources(category) {
			if !seen[legacy.Key] {
				t.Fatalf("lost existing source %s", legacy.Key)
			}
		}
	}
}
