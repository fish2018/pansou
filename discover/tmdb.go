package discover

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type tmdbCandidate struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	Name          string `json:"name"`
	OriginalTitle string `json:"original_title"`
	OriginalName  string `json:"original_name"`
	ReleaseDate   string `json:"release_date"`
	FirstAirDate  string `json:"first_air_date"`
}

type tmdbDetails struct {
	ID            int    `json:"id"`
	PosterPath    string `json:"poster_path"`
	Overview      string `json:"overview"`
	OriginalTitle string `json:"original_title"`
	OriginalName  string `json:"original_name"`
	ReleaseDate   string `json:"release_date"`
	FirstAirDate  string `json:"first_air_date"`
	AirDate       string `json:"air_date"`
	Genres        []struct {
		Name string `json:"name"`
	} `json:"genres"`
}

func (s *Service) enrich(ctx context.Context, items []Item) []Item {
	if !s.TMDBConfigured() {
		return items
	}
	out := append([]Item{}, items...)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if item, err := s.enrichItem(ctx, out[i]); err == nil {
					out[i] = item
				}
			}
		}()
	}
	for i := range out {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return out
		}
	}
	close(jobs)
	wg.Wait()
	return out
}

func (s *Service) enrichItem(ctx context.Context, item Item) (Item, error) {
	key := "metadata_" + item.ID
	if cached, ok := s.load(key); ok && len(cached.Items) == 1 && time.Since(cached.UpdatedAt) < 7*24*time.Hour {
		return applyMetadata(item, cached.Items[0]), nil
	}
	result := s.metadataFlight.DoChan(key, func() (any, error) {
		full := item
		// The lightweight new-TV list has no year. Fetch the official subject
		// before matching, rather than guessing a namesake's year from TMDB.
		if full.Year == "" {
			var details doubanItem
			if err := s.getJSON(ctx, "https://m.douban.com/rexxar/api/v2/"+item.MediaType+"/"+item.ID, "douban", &details); err == nil && details.ID == item.ID {
				full = applyMetadata(full, mapDouban(details, item.MediaType))
			}
		}
		if len(full.Year) != 4 {
			return full, nil
		}
		matched, err := s.fetchTMDB(ctx, full)
		if err != nil {
			return full, err
		}
		s.save(key, cacheEntry{Items: []Item{matched}, UpdatedAt: time.Now().UTC()})
		return matched, nil
	})
	select {
	case <-ctx.Done():
		return item, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return item, result.Err
		}
		return applyMetadata(item, result.Val.(Item)), nil
	}
}

// Keep the Douban title (including season) and score even when TMDB metadata exists.
func applyMetadata(item, metadata Item) Item {
	if metadata.PosterURL != "" {
		item.PosterURL = metadata.PosterURL
	}
	if metadata.Overview != "" {
		item.Overview = metadata.Overview
	}
	if len(metadata.Genres) > 0 {
		item.Genres = append([]string{}, metadata.Genres...)
	}
	if metadata.OriginalTitle != "" {
		item.OriginalTitle = metadata.OriginalTitle
	}
	if metadata.Year != "" {
		item.Year = metadata.Year
	}
	if metadata.ReleaseDate != "" {
		item.ReleaseDate = metadata.ReleaseDate
	}
	if item.DoubanRating == nil {
		item.DoubanRating = metadata.DoubanRating
	}
	if item.DoubanVotes == nil {
		item.DoubanVotes = metadata.DoubanVotes
	}
	if metadata.TMDBID != 0 {
		item.TMDBID = metadata.TMDBID
		item.MetadataSource = "tmdb"
	}
	return item
}

func (s *Service) tmdbAddress(path string, query url.Values) string {
	query.Set("language", "zh-CN")
	if s.cfg.TMDBReadAccessToken == "" {
		query.Set("api_key", s.cfg.TMDBAPIKey)
	}
	return "https://api.themoviedb.org/3" + path + "?" + query.Encode()
}

func (s *Service) fetchTMDB(ctx context.Context, item Item) (Item, error) {
	queryTitle, season := splitSeason(item.Title)
	if item.MediaType != "tv" {
		queryTitle = item.Title
		season = 0
	}
	var payload struct {
		Results []tmdbCandidate `json:"results"`
	}
	query := url.Values{"query": {queryTitle}, "include_adult": {"false"}}
	if season == 0 {
		if item.MediaType == "movie" {
			query.Set("year", item.Year)
		} else {
			query.Set("first_air_date_year", item.Year)
		}
	}
	if err := s.getJSON(ctx, s.tmdbAddress("/search/"+item.MediaType, query), "tmdb", &payload); err != nil {
		return item, err
	}
	var candidates []tmdbCandidate
	for _, candidate := range payload.Results {
		if matchCandidate(item, candidate, season > 0) {
			candidates = append(candidates, candidate)
		}
	}
	// Never choose by popularity when two different works have the same name/year.
	if len(candidates) != 1 {
		return item, nil
	}
	candidate := candidates[0]
	path := "/" + item.MediaType + "/" + strconv.Itoa(candidate.ID)
	if season > 0 {
		path += "/season/" + strconv.Itoa(season)
	}
	var details tmdbDetails
	if err := s.getJSON(ctx, s.tmdbAddress(path, url.Values{}), "tmdb", &details); err != nil {
		return item, err
	}
	date := details.ReleaseDate
	if item.MediaType == "tv" {
		date = details.FirstAirDate
	}
	if season > 0 {
		date = details.AirDate
	}
	if len(date) < 4 || date[:4] != item.Year {
		return item, nil
	}
	if strings.HasPrefix(details.PosterPath, "/") && !strings.Contains(details.PosterPath, "..") {
		item.PosterURL = "https://image.tmdb.org/t/p/w500" + details.PosterPath
	}
	item.Overview = details.Overview
	item.ReleaseDate = date
	if item.OriginalTitle == "" {
		item.OriginalTitle = details.OriginalTitle
		if item.OriginalTitle == "" {
			item.OriginalTitle = details.OriginalName
		}
	}
	if len(details.Genres) > 0 {
		item.Genres = []string{}
	}
	for _, genre := range details.Genres {
		if genre.Name != "" {
			item.Genres = append(item.Genres, genre.Name)
		}
	}
	item.TMDBID = candidate.ID
	item.MetadataSource = "tmdb"
	return item, nil
}

func matchCandidate(item Item, candidate tmdbCandidate, season bool) bool {
	if candidate.ID <= 0 {
		return false
	}
	names := []string{item.Title, item.OriginalTitle}
	if season {
		for i := range names {
			names[i], _ = splitSeason(names[i])
		}
	}
	candidateNames := []string{candidate.Title, candidate.OriginalTitle}
	date := candidate.ReleaseDate
	if item.MediaType == "tv" {
		candidateNames = []string{candidate.Name, candidate.OriginalName}
		date = candidate.FirstAirDate
	}
	if !season && (len(date) < 4 || date[:4] != item.Year) {
		return false
	}
	for _, name := range names {
		if normalizeTitle(name) == "" {
			continue
		}
		for _, candidateName := range candidateNames {
			if normalizeTitle(name) == normalizeTitle(candidateName) {
				return true
			}
		}
	}
	return false
}

func normalizeTitle(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
}

var seasonPattern = regexp.MustCompile(`(?i)\s*(?:第\s*([一二三四五六七八九十百0-9]+)\s*季|season\s+(\d+))\s*$`)

func splitSeason(title string) (string, int) {
	match := seasonPattern.FindStringSubmatchIndex(title)
	if match == nil {
		return title, 0
	}
	for _, index := range []int{2, 4} {
		if match[index] < 0 {
			continue
		}
		part := title[match[index]:match[index+1]]
		if number, err := strconv.Atoi(part); err == nil && number > 0 && number < 100 {
			return strings.TrimSpace(title[:match[0]]), number
		}
		number, err := chineseSeason(part)
		if err == nil {
			return strings.TrimSpace(title[:match[0]]), number
		}
	}
	return title, 0
}

func chineseSeason(part string) (int, error) {
	digits := map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	if part == "十" {
		return 10, nil
	}
	n := 0
	for _, r := range part {
		if r == '十' {
			if n == 0 {
				n = 1
			}
			n *= 10
			continue
		}
		d, ok := digits[r]
		if !ok {
			return 0, errors.New("unsupported season")
		}
		n += d
	}
	if n > 0 && n < 100 {
		return n, nil
	}
	return 0, errors.New("unsupported season")
}
