package discover

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

func ValidSubject(id, mediaType string) bool {
	if !contains([]string{"movie", "tv"}, mediaType) {
		return false
	}
	if idPattern.MatchString(id) {
		return true
	}
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != "tmdb" || parts[1] != mediaType || len(parts[2]) > 10 || strings.HasPrefix(parts[2], "0") {
		return false
	}
	number, err := strconv.ParseUint(parts[2], 10, 32)
	return err == nil && number > 0 && strconv.FormatUint(number, 10) == parts[2]
}

func baseSubjectKey(id, mediaType string) string {
	return "subject_" + mediaType + "_" + id
}

func (s *Service) subjectKey(id, mediaType string) string {
	key := baseSubjectKey(id, mediaType)
	if s.TMDBConfigured() {
		key += "_tmdb"
	}
	return key
}

// Subject returns stored metadata immediately and renews old entries in the
// background. Only an uncached card waits for the provider.
func (s *Service) Subject(ctx context.Context, id, mediaType string) (Item, error) {
	if !ValidSubject(id, mediaType) {
		return Item{}, errors.New("invalid discovery subject")
	}
	if strings.HasPrefix(id, "tmdb:") {
		return s.FullCatalogSubject(ctx, id, mediaType)
	}
	if entry, ok := s.load(s.subjectKey(id, mediaType)); ok && len(entry.Items) == 1 {
		if time.Since(entry.UpdatedAt) >= 24*time.Hour {
			s.queueSubject(id, mediaType)
		}
		return s.subjectCacheHint(entry.Items[0], entry.UpdatedAt), nil
	}
	if s.TMDBConfigured() {
		if entry, ok := s.load(baseSubjectKey(id, mediaType)); ok && len(entry.Items) == 1 {
			// Keep the old subject immediately available while the new provider
			// cache is built. Copying it to the TMDB key would suppress enrichment.
			s.queueSubject(id, mediaType)
			item := entry.Items[0]
			item.DetailsCached = false
			return item, nil
		}
	}
	// The persistent directory can already contain a complete matched Douban
	// subject even when it was never prefetched into the older JSON cache.
	if s.cfg.TMDBCatalogSyncEnabled {
		if store, err := s.openFullCatalog(); err == nil {
			item, err := scanFullItem(store.db.QueryRowContext(ctx, "SELECT "+fullItemColumns+" FROM entries e WHERE e.key=? AND e.media_type=? AND e.origin='douban' AND e.removed=0 AND e.hydrated=1", id, mediaType))
			if err == nil {
				s.queueSubject(id, mediaType)
				return item, nil
			}
		}
	}
	return s.refreshSubject(ctx, id, mediaType)
}

func (s *Service) refreshSubject(ctx context.Context, id, mediaType string) (Item, error) {
	key := s.subjectKey(id, mediaType)
	result := s.flight.DoChan(key, func() (any, error) {
		old, hasOld := s.load(key)
		if hasOld && len(old.Items) == 1 && time.Since(old.UpdatedAt) < 24*time.Hour {
			return s.subjectCacheHint(old.Items[0], old.UpdatedAt), nil
		}
		s.mu.Lock()
		retryAt := s.failures[key]
		s.mu.Unlock()
		if time.Now().Before(retryAt) {
			if hasOld && len(old.Items) == 1 {
				item := old.Items[0]
				item.DetailsCached = false
				return item, nil
			}
			return Item{}, errors.New("discovery subject temporarily unavailable")
		}
		refreshCtx, cancel := context.WithTimeout(s.ctx, 28*time.Second)
		defer cancel()
		var details doubanItem
		err := s.getJSON(refreshCtx, "https://m.douban.com/rexxar/api/v2/"+mediaType+"/"+id, "douban", &details)
		if err != nil || details.ID != id || details.Title == "" || (details.Type != "" && details.Type != mediaType) {
			s.mu.Lock()
			s.failures[key] = time.Now().Add(time.Minute)
			s.mu.Unlock()
			if hasOld && len(old.Items) == 1 {
				item := old.Items[0]
				item.DetailsCached = false
				return item, nil
			}
			return Item{}, errors.New("discovery subject temporarily unavailable")
		}
		item := mapDouban(details, mediaType)
		if s.TMDBConfigured() {
			if enhanced, err := s.enrichItem(refreshCtx, item); err == nil {
				item = enhanced
			}
		}
		item = s.subjectCacheHint(item, time.Now().UTC())
		s.save(key, cacheEntry{Items: []Item{item}, UpdatedAt: time.Now().UTC()})
		return item, nil
	})
	select {
	case <-ctx.Done():
		return Item{}, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return Item{}, result.Err
		}
		return result.Val.(Item), nil
	}
}

// Old cache files may contain obsolete or absent hints. Derive freshness from
// their timestamp and provider state rather than trusting the persisted bool.
func (s *Service) subjectCacheHint(item Item, updated time.Time) Item {
	item.DetailsCached = time.Since(updated) < 24*time.Hour && (!s.TMDBConfigured() || item.MetadataSource == "tmdb")
	return item
}
