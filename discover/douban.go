package discover

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

type doubanItem struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	Year          string `json:"year"`
	Type          string `json:"type"`
	CardSubtitle  string `json:"card_subtitle"`
	CoverURL      string `json:"cover_url"`
	Cover         struct {
		URL string `json:"url"`
	} `json:"cover"`
	Pic struct {
		Large  string `json:"large"`
		Normal string `json:"normal"`
	} `json:"pic"`
	Rating *struct {
		Value float64 `json:"value"`
		Count int     `json:"count"`
	} `json:"rating"`
	Genres  []string `json:"genres"`
	Intro   string   `json:"intro"`
	Pubdate []string `json:"pubdate"`
}

var yearPattern = regexp.MustCompile(`(?:^|\D)((?:19|20)\d{2})(?:\D|$)`)
var idPattern = regexp.MustCompile(`^\d{1,12}$`)

func (s *Service) fetchDouban(ctx context.Context, category, collection string) ([]Item, error) {
	if collection == "new" && category != "movie" {
		return s.fetchDoubanNew(ctx, category)
	}
	list := map[string]string{"movie": "movie_hot_gaia", "tv": "tv_hot", "animation": "tv_animation", "variety": "tv_variety_show", "documentary": "tv_documentary"}[category]
	if category == "movie" && collection == "top" {
		list = "movie_top250"
	}
	if category == "movie" && collection == "new" {
		list = "movie_latest"
	}
	mediaType := "tv"
	if category == "movie" {
		mediaType = "movie"
	}
	if category == "animation" || category == "documentary" {
		mediaType = ""
	}
	page, err := s.fetchCatalogPage(ctx, category, doubanCollectionSource(list, "热门榜单", collection, mediaType), 0, 50)
	if err != nil {
		return nil, err
	}
	items := page.Items
	for i := range items {
		items[i].Reason = "豆瓣热门榜单"
		if collection == "new" {
			items[i].Reason = "豆瓣近期新作"
		}
	}
	if len(items) == 0 {
		return nil, errors.New("douban returned no valid discovery items")
	}
	if collection == "top" {
		items = rankItems(items, category)
	}
	return items, nil
}

func mapDouban(raw doubanItem, category string) Item {
	item := Item{ID: raw.ID, Title: raw.Title, OriginalTitle: raw.OriginalTitle, Year: raw.Year, Genres: raw.Genres, Overview: raw.Intro, DoubanURL: "https://movie.douban.com/subject/" + raw.ID + "/", MetadataSource: "douban", MediaType: "tv"}
	if category == "movie" {
		item.MediaType = "movie"
	}
	if raw.Type == "movie" || raw.Type == "tv" {
		item.MediaType = raw.Type
	} // Animation and documentary collections can include films.
	if item.Year == "" {
		if match := yearPattern.FindStringSubmatch(raw.CardSubtitle); len(match) > 1 {
			item.Year = match[1]
		}
	}
	if item.Genres == nil {
		item.Genres = subtitleGenres(raw.CardSubtitle)
	}
	if raw.Rating != nil && raw.Rating.Value > 0 && raw.Rating.Value <= 10 {
		value, count := raw.Rating.Value, raw.Rating.Count
		item.DoubanRating = &value
		if count > 0 {
			item.DoubanVotes = &count
		}
	}
	for _, candidate := range []string{raw.Pic.Large, raw.Cover.URL, raw.CoverURL, raw.Pic.Normal} {
		if ValidPosterURL(candidate) {
			item.PosterURL = "/api/discover/poster?url=" + url.QueryEscape(candidate)
			break
		}
	}
	for _, date := range raw.Pubdate {
		if len(date) >= 10 && date[4] == '-' && date[7] == '-' {
			item.ReleaseDate = date[:10]
			break
		}
	}
	return item
}

// Collection cards contain "year / regions / genres / cast" rather than a
// genres array. Only recognize known genres in that third field.
func subtitleGenres(subtitle string) []string {
	parts := strings.Split(subtitle, "/")
	genres := []string{}
	if len(parts) < 3 {
		return genres
	}
	known := []string{"剧情", "喜剧", "爱情", "动作", "科幻", "动画", "悬疑", "惊悚", "恐怖", "犯罪", "同性", "音乐", "歌舞", "传记", "历史", "战争", "西部", "奇幻", "冒险", "灾难", "武侠", "古装", "运动", "真人秀", "脱口秀", "短片", "纪录片", "家庭", "儿童", "戏曲", "黑色电影", "舞台艺术"}
	for _, value := range strings.Fields(strings.TrimSpace(parts[2])) {
		if contains(known, value) {
			genres = append(genres, value)
		}
	}
	return genres
}

func (s *Service) fetchDoubanNew(ctx context.Context, category string) ([]Item, error) {
	tag := map[string]string{"tv": "热门", "animation": "日本动画", "variety": "综艺", "documentary": "纪录片"}[category]
	page, err := s.fetchCatalogPage(ctx, category, doubanTagSource("tv", tag, "time", tag, "new"), 0, 50)
	if err != nil {
		return nil, err
	}
	items := page.Items
	for i := range items {
		items[i].Reason = "豆瓣新剧 · 按时间排序"
		if category == "animation" {
			items[i].Reason = "豆瓣新番 · 日本动画"
		}
		if category == "variety" {
			items[i].Reason = "豆瓣综艺 · 按时间排序"
		}
		if category == "documentary" {
			items[i].Reason = "豆瓣纪录片 · 按时间排序"
		}
	}
	if len(items) == 0 {
		return nil, errors.New("douban returned no valid new releases")
	}
	return items, nil
}
