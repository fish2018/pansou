package discover

import (
	"context"
	"net/url"
	"strings"
	"time"
)

type backgroundTask struct {
	key string
	run func(context.Context)
}

// Start keeps all fifteen lists current without requiring page views. The
// scheduler visits one selection and one catalog page every five seconds.
func (s *Service) Start() { s.start(5 * time.Second) }

func (s *Service) start(interval time.Duration) {
	if !s.ensureWorkers() {
		return
	}
	s.StartFullCatalog()
	s.lifecycleMu.Lock()
	if s.closed || s.schedulerStarted {
		s.lifecycleMu.Unlock()
		return
	}
	s.schedulerStarted = true
	s.workers.Add(1)
	s.lifecycleMu.Unlock()
	// Load existing list snapshots first. Images and full subjects stay on disk
	// until needed; only the first twelve hot cards enter the warm queues.
	for _, category := range []string{"movie", "tv", "animation", "variety", "documentary"} {
		for _, collection := range []string{"hot", "new", "top"} {
			if entry, ok := s.loadList(category, collection); ok {
				s.queueHotPrefetch(category, entry, collection)
			}
		}
		s.loadCatalog(category)
	}
	go func() {
		defer s.workers.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for index := 0; ; index++ {
			categories := []string{"movie", "tv", "animation", "variety", "documentary"}
			category := categories[index%len(categories)]
			collection := []string{"hot", "new", "top"}[(index/len(categories))%3]
			entry, ok := s.loadList(category, collection)
			if !ok || time.Since(entry.UpdatedAt) >= listTTL(collection) {
				s.queueList(category, collection)
			} else {
				s.queueHotPrefetch(category, entry, collection)
			}
			s.queueCatalog(category)
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) ensureWorkers() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed {
		return false
	}
	if !s.workersStarted {
		s.workersStarted = true
		s.workers.Add(4)
		go s.runQueue(s.listQueue)
		go s.runQueue(s.catalogQueue)
		go s.runQueue(s.warmQueue)
		go s.runQueue(s.warmQueue)
	}
	return true
}

func (s *Service) runQueue(queue <-chan backgroundTask) {
	defer s.workers.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case task := <-queue:
			if s.ctx.Err() == nil {
				task.run(s.ctx)
			}
			s.mu.Lock()
			delete(s.pending, task.key)
			s.mu.Unlock()
		}
	}
}

func (s *Service) enqueue(queue chan backgroundTask, task backgroundTask) {
	if !s.ensureWorkers() {
		return
	}
	s.mu.Lock()
	if s.pending[task.key] || time.Now().Before(s.failures[task.key]) {
		s.mu.Unlock()
		return
	}
	s.pending[task.key] = true
	select {
	case queue <- task:
	default:
		delete(s.pending, task.key)
	}
	s.mu.Unlock()
}

func (s *Service) queueList(category, collection string) {
	s.enqueue(s.listQueue, backgroundTask{key: s.listKey(category, collection), run: func(ctx context.Context) { _, _ = s.refreshList(ctx, category, collection) }})
}

func (s *Service) queueSubject(id, mediaType string) {
	s.enqueue(s.warmQueue, backgroundTask{key: s.subjectKey(id, mediaType), run: func(ctx context.Context) { _, _ = s.refreshSubject(ctx, id, mediaType) }})
}

func (s *Service) queueHotPrefetch(category string, entry cacheEntry, collection string) {
	if collection != "hot" {
		return
	}
	s.lifecycleMu.Lock()
	enabled := s.schedulerStarted && !s.closed
	s.lifecycleMu.Unlock()
	if !enabled {
		return
	}
	s.enqueue(s.warmQueue, backgroundTask{key: "warm:" + s.listKey(category, collection), run: func(context.Context) { s.prefetchHot(entry, collection) }})
}

func (s *Service) prefetchHot(entry cacheEntry, collection string) {
	if collection != "hot" {
		return
	}
	s.lifecycleMu.Lock()
	enabled := s.schedulerStarted && !s.closed
	s.lifecycleMu.Unlock()
	if !enabled {
		return
	}
	items := releasedItems(entry.Items, time.Now().UTC())
	if len(items) > 12 {
		items = items[:12]
	}
	for _, item := range items {
		if ValidSubject(item.ID, item.MediaType) {
			if subject, ok := s.load(s.subjectKey(item.ID, item.MediaType)); !ok || len(subject.Items) != 1 || time.Since(subject.UpdatedAt) >= 24*time.Hour {
				s.queueSubject(item.ID, item.MediaType)
			}
		}
		if strings.HasPrefix(item.PosterURL, "/api/discover/poster?") {
			if parsed, err := url.Parse(item.PosterURL); err == nil {
				address := parsed.Query().Get("url")
				if ValidPosterURL(address) && !s.hasFreshPoster(address) {
					s.enqueue(s.warmQueue, backgroundTask{key: "poster:" + address, run: func(ctx context.Context) { _, _, _ = s.Poster(ctx, address) }})
				}
			}
		}
	}
}

// Close cancels queued/running refreshes and waits for the bounded workers and
// scheduler. It is safe to call more than once.
func (s *Service) Close() {
	s.lifecycleMu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
	}
	s.lifecycleMu.Unlock()
	s.workers.Wait()
	s.catalogWork.Wait()
	s.fullCatalogMu.Lock()
	if s.fullCatalog != nil {
		_ = s.fullCatalog.db.Close()
	}
	s.fullCatalogMu.Unlock()
	s.client.CloseIdleConnections()
}
