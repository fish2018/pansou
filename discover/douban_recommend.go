package discover

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The public recommendation endpoint exposes at most 500 candidate slots per
// filter. Its year tags can include mismatches, so only each subject's explicit
// year/type and real genre metadata determine whether it belongs in a source.
const doubanRecommendMaxStart = 500

func doubanYearSources(category string) []catalogSource {
	var mediaTypes []string
	genre := ""
	switch category {
	case "movie":
		mediaTypes = []string{"movie"}
	case "tv":
		mediaTypes = []string{"tv"}
	case "animation":
		mediaTypes, genre = []string{"tv", "movie"}, "动画"
	case "documentary":
		mediaTypes, genre = []string{"tv", "movie"}, "纪录片"
	case "variety":
		mediaTypes, genre = []string{"tv"}, "综艺"
	default:
		return nil
	}
	var sources []catalogSource
	for _, year := range doubanRecommendYears(time.Now().UTC().Year()) {
		for _, mediaType := range mediaTypes {
			sources = append(sources, doubanYearSource(mediaType, genre, year))
		}
	}
	return sources
}

func doubanRecommendYears(current int) []int {
	var years []int
	seen := make(map[int]bool)
	add := func(year int) {
		if year >= 1950 && year <= current && !seen[year] {
			seen[year] = true
			years = append(years, year)
		}
	}
	for year := current; year > current-3; year-- {
		add(year)
	}
	for _, year := range []int{2020, 2015, 2010, 2005, 2000, 1990, 1980, 1970, 1960, 1950} {
		add(year)
	}
	for year := current; year >= 2000; year-- {
		add(year)
	}
	return years
}

func doubanYearSource(mediaType, genre string, year int) catalogSource {
	yearText := strconv.Itoa(year)
	kind, keyGenre := "电影", genre
	if mediaType == "tv" {
		kind = "剧集"
	}
	switch genre {
	case "动画":
		kind = "动画电影"
		if mediaType == "tv" {
			kind = "动画剧集"
		}
	case "纪录片":
		kind = "纪录电影"
		if mediaType == "tv" {
			kind = "纪录剧集"
		}
	case "综艺":
		kind = "综艺"
	case "":
		keyGenre = "all"
	}
	return catalogSource{
		Key:   "recommend_" + mediaType + "_" + yearText + "_" + keyGenre + "_U",
		Label: yearText + "年" + kind, mediaType: mediaType,
		recommendYear: year, recommendTag: genre, sort: "U",
	}
}

func (s *Service) fetchDoubanRecommendPage(ctx context.Context, category string, source catalogSource, start, count int) (catalogPage, error) {
	if start < 0 || !contains([]string{"movie", "tv"}, source.mediaType) || source.recommendYear < 1950 || source.recommendYear > time.Now().UTC().Year() || source.sort != "U" || !contains([]string{"", "动画", "纪录片", "综艺"}, source.recommendTag) {
		return catalogPage{}, errors.New("invalid discovery year source")
	}
	if start >= doubanRecommendMaxStart {
		return catalogPage{Items: []Item{}, NextStart: start, Done: true, Total: doubanRecommendMaxStart}, nil
	}
	if count <= 0 || count > 50 {
		count = 50
	}
	if count > doubanRecommendMaxStart-start {
		count = doubanRecommendMaxStart - start
	}
	year := strconv.Itoa(source.recommendYear)
	tags := year
	if source.recommendTag != "" {
		tags += "," + source.recommendTag
	}
	query := url.Values{"tags": {tags}, "sort": {source.sort}, "start": {strconv.Itoa(start)}, "count": {strconv.Itoa(count)}, "refresh": {"0"}}
	var payload struct {
		Items []doubanItem `json:"items"`
		Count int          `json:"count"`
		Total *int         `json:"total"`
	}
	address := "https://m.douban.com/rexxar/api/v2/" + source.mediaType + "/recommend?" + query.Encode()
	if err := s.getJSON(ctx, address, "douban", &payload); err != nil {
		return catalogPage{}, err
	}
	if payload.Items == nil {
		return catalogPage{}, errors.New("invalid discovery year response")
	}
	step := count
	if payload.Count > 0 && payload.Count <= count {
		step = payload.Count
	}
	page := catalogPage{Items: make([]Item, 0, len(payload.Items)), NextStart: start + step, Total: doubanRecommendMaxStart}
	if payload.Total != nil && *payload.Total >= 0 && *payload.Total < page.Total {
		page.Total = *payload.Total
	}
	// Short pages have hidden slots and can be followed by full pages. Advance
	// by the response count; only an actual empty page or the bound ends a scan.
	page.Done = len(payload.Items) == 0 || page.NextStart >= page.Total
	seen := make(map[string]bool)
	for _, raw := range payload.Items {
		if !validDoubanItem(raw.ID, raw.Title) || seen[raw.ID] || raw.Type != source.mediaType || strings.TrimSpace(raw.Year) != year {
			continue
		}
		item := mapDouban(raw, category)
		item.Year = strings.TrimSpace(raw.Year)
		if !doubanRecommendGenreMatches(item.Genres, source.recommendTag) {
			continue
		}
		if category == "tv" && (contains(item.Genres, "动画") || contains(item.Genres, "纪录片") || contains(item.Genres, "真人秀") || contains(item.Genres, "脱口秀")) {
			continue
		}
		seen[raw.ID] = true
		page.Items = append(page.Items, catalogSourceItem(item, source))
	}
	return page, nil
}

func doubanRecommendGenreMatches(genres []string, required string) bool {
	if required == "" {
		return true
	}
	if required == "综艺" {
		return contains(genres, "真人秀") || contains(genres, "脱口秀")
	}
	return contains(genres, required)
}
