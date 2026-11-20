package discover

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// catalogSource describes one public Douban list. Key is stable across restarts;
// Collection is empty for library-only tags rather than a recommendation list.
type catalogSource struct {
	Key           string
	Label         string
	Collection    string
	collectionID  string
	mediaType     string
	tag           string
	sort          string
	recommendYear int
	recommendTag  string
}

type catalogPage struct {
	Items     []Item
	NextStart int
	Done      bool
	Total     int
}

func catalogSources(category string) []catalogSource {
	sources := doubanCatalogSources(category)
	if len(sources) == 0 {
		return nil
	}
	// Keep the fast recommendation lists first, then explore distinct years
	// before revisiting the narrower legacy tags. Stable keys retain cursors.
	main := 1
	if category == "movie" {
		main = 3
	}
	result := append([]catalogSource{}, sources[:main]...)
	result = append(result, doubanYearSources(category)...)
	return append(result, sources[main:]...)
}

func (s *Service) fetchCatalogPage(ctx context.Context, category string, source catalogSource, start, count int) (catalogPage, error) {
	return s.fetchDoubanCatalogPage(ctx, category, source, start, count)
}

func doubanCollectionSource(id, label, collection, mediaType string) catalogSource {
	return catalogSource{Key: "rexxar_" + id, Label: label, Collection: collection, collectionID: id, mediaType: mediaType}
}

func doubanTagSource(mediaType, tag, sort, label, collection string) catalogSource {
	return catalogSource{Key: "search_" + mediaType + "_" + tag + "_" + sort, Label: label, Collection: collection, mediaType: mediaType, tag: tag, sort: sort}
}

func doubanCatalogSources(category string) []catalogSource {
	var sources []catalogSource
	switch category {
	case "movie":
		sources = []catalogSource{
			doubanCollectionSource("movie_hot_gaia", "热门电影", "hot", "movie"),
			doubanCollectionSource("movie_latest", "近期新片", "new", "movie"),
			doubanCollectionSource("movie_top250", "豆瓣 Top250", "top", "movie"),
			doubanCollectionSource("movie_weekly_best", "一周口碑", "", "movie"),
			doubanCollectionSource("movie_showing", "影院热映", "", "movie"),
			doubanTagSource("movie", "热门", "recommend", "热门电影", "hot"),
			doubanTagSource("movie", "最新", "time", "近期新片", "new"),
			doubanTagSource("movie", "豆瓣高分", "rank", "豆瓣高分", ""),
			doubanTagSource("movie", "冷门佳片", "rank", "冷门佳片", ""),
		}
		for _, tag := range []string{"华语", "欧美", "韩国", "日本"} {
			sources = append(sources, doubanTagSource("movie", tag, "recommend", tag, ""))
		}
	case "tv":
		sources = []catalogSource{
			doubanCollectionSource("tv_hot", "热门剧集", "hot", "tv"),
			doubanCollectionSource("tv_domestic", "国产剧", "", "tv"),
			doubanCollectionSource("tv_american", "美剧", "", "tv"),
			doubanTagSource("tv", "热门", "recommend", "热门剧集", "hot"),
			doubanTagSource("tv", "热门", "time", "近期新剧", "new"),
			doubanTagSource("tv", "热门", "rank", "经典剧集", ""),
		}
		for _, tag := range []string{"国产剧", "美剧", "英剧", "韩剧", "日剧", "港剧"} {
			sources = append(sources, doubanTagSource("tv", tag, "recommend", tag, ""))
		}
	case "animation":
		sources = []catalogSource{
			// This public list includes animation from multiple countries. The
			// optional raw type preserves films when Douban includes them.
			doubanCollectionSource("tv_animation", "热门动漫", "hot", ""),
			doubanTagSource("tv", "日本动画", "recommend", "日本动画", "hot"),
			doubanTagSource("tv", "日本动画", "rank", "经典日本动画", ""),
			doubanTagSource("tv", "日本动画", "time", "近期日本动画", "new"),
			doubanTagSource("movie", "动画", "recommend", "动画电影", "hot"),
			doubanTagSource("movie", "动画", "rank", "经典动画电影", ""),
			doubanTagSource("movie", "动画", "time", "近期动画电影", "new"),
		}
	case "variety":
		sources = []catalogSource{
			doubanCollectionSource("tv_variety_show", "热门综艺", "hot", "tv"),
			doubanTagSource("tv", "综艺", "recommend", "综艺", "hot"),
			doubanTagSource("tv", "综艺", "rank", "经典综艺", ""),
			doubanTagSource("tv", "综艺", "time", "近期综艺", "new"),
		}
	case "documentary":
		sources = []catalogSource{
			doubanCollectionSource("tv_documentary", "热门纪录片", "hot", ""),
			doubanTagSource("tv", "纪录片", "recommend", "纪录片", "hot"),
			doubanTagSource("tv", "纪录片", "rank", "经典纪录片", ""),
			doubanTagSource("tv", "纪录片", "time", "近期纪录片", "new"),
			doubanTagSource("movie", "纪录片", "recommend", "纪录电影", "hot"),
			doubanTagSource("movie", "纪录片", "rank", "经典纪录电影", ""),
			doubanTagSource("movie", "纪录片", "time", "近期纪录电影", "new"),
		}
	}
	return sources
}

// The web list omits year, media type, genres and vote counts. Its requested
// type is the only type we can assign; the missing fields stay unknown.
type doubanSearchItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Rate  string `json:"rate"`
	Cover string `json:"cover"`
}

func (s *Service) fetchDoubanCatalogPage(ctx context.Context, category string, source catalogSource, start, count int) (catalogPage, error) {
	if start < 0 || len(doubanCatalogSources(category)) == 0 {
		return catalogPage{}, errors.New("invalid discovery source page")
	}
	// Public rexxar lists clamp larger requests to 50. Use the same conservative
	// size for web tag lists, keeping upstream responses bounded.
	if count <= 0 || count > 50 {
		count = 50
	}
	if source.recommendYear != 0 {
		return s.fetchDoubanRecommendPage(ctx, category, source, start, count)
	}
	if source.collectionID != "" {
		var payload struct {
			Items []doubanItem `json:"subject_collection_items"`
			Count int          `json:"count"`
			Total *int         `json:"total"`
		}
		query := url.Values{"start": {strconv.Itoa(start)}, "count": {strconv.Itoa(count)}}
		address := "https://m.douban.com/rexxar/api/v2/subject_collection/" + source.collectionID + "/items?" + query.Encode()
		if err := s.getJSON(ctx, address, "douban", &payload); err != nil {
			return catalogPage{}, err
		}
		if payload.Items == nil {
			return catalogPage{}, errors.New("invalid discovery collection response")
		}
		step := count
		if payload.Count > 0 && payload.Count <= count {
			step = payload.Count
		}
		page := catalogPage{Items: make([]Item, 0, len(payload.Items)), NextStart: start + step}
		if payload.Total != nil && *payload.Total >= 0 {
			page.Total = *payload.Total
		}
		page.Done = len(payload.Items) == 0
		if payload.Total != nil && *payload.Total >= 0 {
			page.Done = page.Done || page.NextStart >= *payload.Total
		} else {
			page.Done = page.Done || len(payload.Items) < step
		}
		seen := make(map[string]bool)
		for _, raw := range payload.Items {
			if !validDoubanItem(raw.ID, raw.Title) || seen[raw.ID] {
				continue
			}
			if raw.Type != "" && raw.Type != "movie" && raw.Type != "tv" {
				continue
			}
			if source.mediaType != "" && raw.Type != "" && raw.Type != source.mediaType {
				continue
			}
			seen[raw.ID] = true
			item := mapDouban(raw, category)
			if source.mediaType != "" {
				item.MediaType = source.mediaType
			}
			page.Items = append(page.Items, catalogSourceItem(item, source))
		}
		return page, nil
	}
	if !contains([]string{"movie", "tv"}, source.mediaType) || source.tag == "" || !contains([]string{"recommend", "rank", "time"}, source.sort) {
		return catalogPage{}, errors.New("invalid discovery tag source")
	}
	query := url.Values{"type": {source.mediaType}, "tag": {source.tag}, "sort": {source.sort}, "page_limit": {strconv.Itoa(count)}, "page_start": {strconv.Itoa(start)}}
	var payload struct {
		Subjects []doubanSearchItem `json:"subjects"`
	}
	if err := s.getJSON(ctx, "https://movie.douban.com/j/search_subjects?"+query.Encode(), "douban", &payload); err != nil {
		return catalogPage{}, err
	}
	if payload.Subjects == nil {
		return catalogPage{}, errors.New("invalid discovery tag response")
	}
	page := catalogPage{Items: make([]Item, 0, len(payload.Subjects)), NextStart: start + len(payload.Subjects), Done: len(payload.Subjects) < count}
	seen := make(map[string]bool)
	for _, raw := range payload.Subjects {
		if !validDoubanItem(raw.ID, raw.Title) || seen[raw.ID] {
			continue
		}
		seen[raw.ID] = true
		item := mapDouban(doubanItem{ID: raw.ID, Title: raw.Title, CoverURL: raw.Cover, Type: source.mediaType}, source.mediaType)
		if value, err := strconv.ParseFloat(raw.Rate, 64); err == nil && value > 0 && value <= 10 {
			item.DoubanRating = &value
		}
		if category == "animation" {
			item.Genres = []string{"动画"}
		}
		if category == "documentary" {
			item.Genres = []string{"纪录片"}
		}
		page.Items = append(page.Items, catalogSourceItem(item, source))
	}
	return page, nil
}

func validDoubanItem(id, title string) bool {
	return idPattern.MatchString(id) && strings.TrimSpace(title) != ""
}

func catalogSourceItem(item Item, source catalogSource) Item {
	item.Tags = []string{source.Label}
	item.Reason = "豆瓣 · " + source.Label
	return item
}
