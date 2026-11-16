package discover

import (
	"context"
	"testing"
	"time"
)

func TestFullCatalogSubjectKeepsTMDBAndDoubanIdentitiesSeparate(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{CacheDir: dir, TMDBCatalogSyncEnabled: true})
	t.Cleanup(s.Close)
	store, err := s.openFullCatalog()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO entries(key,media_type,tmdb_id,origin,title,norm_title,hydrated,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"tmdb:movie:278", "movie", 278, "tmdb", "TMDB 独立电影", "tmdb独立电影", 1, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	rating := 8.2
	s.save(s.subjectKey("278", "movie"), cacheEntry{Items: []Item{{ID: "278", Title: "豆瓣另一部电影", MediaType: "movie", DoubanRating: &rating, MetadataSource: "douban"}}, UpdatedAt: time.Now()})
	item, err := s.Subject(context.Background(), "tmdb:movie:278", "movie")
	if err != nil || item.ID != "tmdb:movie:278" || item.TMDBID != 278 || item.Title != "TMDB 独立电影" || item.DoubanRating != nil || item.DoubanURL != "" {
		t.Fatalf("TMDB identity or unknown Douban score changed: %+v, %v", item, err)
	}
	item, err = s.Subject(context.Background(), "278", "movie")
	if err != nil || item.Title != "豆瓣另一部电影" || item.DoubanRating == nil || *item.DoubanRating != rating {
		t.Fatalf("numeric Douban identity was replaced: %+v, %v", item, err)
	}
	for _, id := range []string{"tmdb:tv:278", "tmdb:movie:0278", "tmdb:movie:0", "tmdb:movie:-1", "tmdb:movie:4294967296", "tmdb:movie:1/../../123", "tmdb:movie:278:extra"} {
		if _, err := s.Subject(context.Background(), id, "movie"); err == nil {
			t.Errorf("invalid or cross-media identity was accepted: %q", id)
		}
	}
	s.Close()
	restarted := New(Config{CacheDir: dir, TMDBCatalogSyncEnabled: true})
	t.Cleanup(restarted.Close)
	item, err = restarted.Subject(context.Background(), "tmdb:movie:278", "movie")
	if err != nil || item.Title != "TMDB 独立电影" {
		t.Fatalf("full-catalog detail did not survive restart: %+v, %v", item, err)
	}
}

func TestFullCatalogLibraryUsesDiskFiltersAndPreservesDoubanSeasons(t *testing.T) {
	s := newTestService(t, Config{CacheDir: t.TempDir(), TMDBCatalogSyncEnabled: true})
	rating := 9.2
	items := []Item{
		{ID: "2373195", Title: "绝命毒师 第一季", Year: "2008", MediaType: "tv", Genres: []string{"犯罪"}, DoubanRating: &rating, DoubanURL: "https://movie.douban.com/subject/2373195/", MetadataSource: "douban"},
		{ID: "2373196", Title: "另一部犯罪剧", Year: "2011", MediaType: "tv", Genres: []string{"犯罪"}, MetadataSource: "douban"},
	}
	if err := s.SeedFullCatalog("tv", items); err != nil {
		t.Fatal(err)
	}
	response, err := s.Query(context.Background(), "tv", "library", ListOptions{Page: 1, PageSize: 24, Query: "命毒师", Genre: "犯罪", YearFrom: "2000", YearTo: "2009"})
	if err != nil || response.Total != 1 || response.CatalogTotal != 2 || len(response.Items) != 1 || response.Items[0].ID != "2373195" || response.Items[0].Title != "绝命毒师 第一季" || response.Items[0].DoubanRating == nil || *response.Items[0].DoubanRating != rating {
		t.Fatalf("full-catalog filtering or original season score was lost: %+v, %v", response, err)
	}
	if response.CatalogSync == nil || !response.CatalogSync.Enabled || len(response.Years) != 2 {
		t.Fatalf("full-catalog progress or whole-category facets were lost: %+v", response)
	}
}
