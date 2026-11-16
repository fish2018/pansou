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
		return entry.Items[0], nil
	}
	if s.TMDBConfigured() {
		if entry, ok := s.load(baseSubjectKey(id, mediaType)); ok && len(entry.Items) == 1 {
			// Keep the old subject immediately available while the new provider
			// cache is built. Copying it to the TMDB key would suppress enrichment.
			s.queueSubject(id, mediaType)
			return entry.Items[0], nil
		}
	}
	return s.refreshSubject(ctx, id, mediaType)
}

func (s *Service) refreshSubject(ctx context.Context, id, mediaType string) (Item, error) {
	key := s.subjectKey(id, mediaType)
	result := s.flight.DoChan(key, func() (any, error) {
		old, hasOld := s.load(key)
		if hasOld && len(old.Items) == 1 && time.Since(old.UpdatedAt) < 24*time.Hour {
			return old.Items[0], nil
		}
		s.mu.Lock()
		retryAt := s.failures[key]
		s.mu.Unlock()
		if time.Now().Before(retryAt) {
			if hasOld && len(old.Items) == 1 {
				return old.Items[0], nil
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
				return old.Items[0], nil
			}
			return Item{}, errors.New("discovery subject temporarily unavailable")
		}
		item := mapDouban(details, mediaType)
		if s.TMDBConfigured() {
			if enhanced, err := s.enrichItem(refreshCtx, item); err == nil {
				item = enhanced
			}
		}
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
