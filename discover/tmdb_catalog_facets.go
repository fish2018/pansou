package discover

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const catalogFacetSnapshotMaxBytes = 64 << 10

type catalogFacetSnapshot struct {
	Version    int                                  `json:"version"`
	Indexed    int                                  `json:"indexed"`
	Categories map[string]catalogFacetSnapshotEntry `json:"categories"`
}

type catalogFacetSnapshotEntry struct {
	UpdatedAt time.Time `json:"updated_at"`
	Genres    []string  `json:"genres"`
	Years     []string  `json:"years"`
	Tags      []string  `json:"tags"`
}

// Facets change less often than list pages. Expired values remain available
// while a single shared refresh runs outside the request's critical path.
func (store *tmdbCatalogStore) categoryFacets(ctx context.Context, category string) (tmdbCatalogFacets, error) {
	store.facetMu.Lock()
	cached, ok := store.facets[category]
	store.facetMu.Unlock()
	if ok && time.Since(cached.at) < time.Minute {
		return cached, nil
	}
	refreshed := store.facetFlight.DoChan(category, func() (any, error) {
		s := store.owner
		s.lifecycleMu.Lock()
		if s.closed {
			s.lifecycleMu.Unlock()
			return nil, context.Canceled
		}
		s.workers.Add(1)
		s.lifecycleMu.Unlock()
		defer s.workers.Done()
		refreshCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		result, err := store.fetchCategoryFacets(refreshCtx, category)
		if err != nil {
			return nil, err
		}
		store.facetMu.Lock()
		store.facets[category] = result
		store.facetMu.Unlock()
		store.saveFacetSnapshot()
		return result, nil
	})
	if ok {
		return cached, nil
	}
	select {
	case <-ctx.Done():
		return tmdbCatalogFacets{}, ctx.Err()
	case result := <-refreshed:
		if result.Err != nil {
			return tmdbCatalogFacets{}, errors.New("full catalog facets unavailable")
		}
		return result.Val.(tmdbCatalogFacets), nil
	}
}

func (store *tmdbCatalogStore) loadFacetSnapshot() {
	file, err := os.Open(store.path + "-facets.json")
	if err != nil {
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, catalogFacetSnapshotMaxBytes+1))
	if err != nil || len(data) > catalogFacetSnapshotMaxBytes {
		return
	}
	var snapshot catalogFacetSnapshot
	if json.Unmarshal(data, &snapshot) != nil || snapshot.Version != 1 || snapshot.Indexed < 0 || len(snapshot.Categories) > 5 {
		return
	}
	store.statusMu.RLock()
	indexed := store.status.Indexed
	store.statusMu.RUnlock()
	// A replaced/empty database must not adopt another directory's old facets.
	if snapshot.Indexed > indexed {
		return
	}
	for category, entry := range snapshot.Categories {
		if !contains([]string{"movie", "tv", "animation", "variety", "documentary"}, category) || len(entry.Genres) > 256 || len(entry.Years) > 256 || len(entry.Tags) > 256 {
			continue
		}
		valid := true
		for _, values := range [][]string{entry.Genres, entry.Years, entry.Tags} {
			for _, value := range values {
				if value == "" || len(value) > 256 {
					valid = false
				}
			}
		}
		if !valid {
			continue
		}
		at := entry.UpdatedAt
		if at.After(time.Now()) {
			at = time.Time{}
		}
		store.facets[category] = tmdbCatalogFacets{at: at, genres: nonNilStrings(entry.Genres), years: nonNilStrings(entry.Years), tags: nonNilStrings(entry.Tags)}
	}
}

func (store *tmdbCatalogStore) saveFacetSnapshot() {
	store.facetPersistMu.Lock()
	defer store.facetPersistMu.Unlock()
	snapshot := catalogFacetSnapshot{Version: 1, Categories: make(map[string]catalogFacetSnapshotEntry)}
	store.statusMu.RLock()
	snapshot.Indexed = store.status.Indexed
	store.statusMu.RUnlock()
	store.facetMu.Lock()
	for category, entry := range store.facets {
		snapshot.Categories[category] = catalogFacetSnapshotEntry{UpdatedAt: entry.at, Genres: entry.genres, Years: entry.years, Tags: entry.tags}
	}
	store.facetMu.Unlock()
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > catalogFacetSnapshotMaxBytes {
		return
	}
	file, err := os.CreateTemp(filepath.Dir(store.path), ".catalog-facets-*")
	if err != nil {
		return
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return
	}
	if file.Close() != nil {
		return
	}
	_ = os.Rename(temporary, store.path+"-facets.json")
}
