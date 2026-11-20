package discover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pansou/util"
)

const (
	posterCacheTTL      = 7 * 24 * time.Hour
	posterCacheMaxBytes = 128 << 20
	posterMaxBytes      = 3 << 20
)

type posterResult struct {
	data      []byte
	kind      string
	updatedAt time.Time
}

// Exact hosts and image paths prevent this endpoint becoming an arbitrary URL
// proxy. Redirects are disabled on the HTTP client, including redirects to LANs.
func ValidPosterURL(address string) bool {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if !strings.HasPrefix(u.Path, "/view/photo/") || strings.Contains(u.Path, "..") {
		return false
	}
	return contains([]string{"img1.doubanio.com", "img2.doubanio.com", "img3.doubanio.com", "img9.doubanio.com"}, strings.ToLower(u.Hostname()))
}

func (s *Service) Poster(ctx context.Context, address string) ([]byte, string, error) {
	if !ValidPosterURL(address) {
		return nil, "", errors.New("invalid poster URL")
	}
	if cached, ok := s.loadPoster(address); ok && time.Since(cached.updatedAt) < posterCacheTTL {
		return cached.data, cached.kind, nil
	}
	result := s.posterFlight.DoChan(address, func() (any, error) {
		cached, hasCached := s.loadPoster(address)
		if hasCached && time.Since(cached.updatedAt) < posterCacheTTL {
			return cached, nil
		}
		failureKey := "poster:" + address
		s.mu.Lock()
		retryAt := s.failures[failureKey]
		s.mu.Unlock()
		if time.Now().Before(retryAt) {
			if hasCached {
				return cached, nil
			}
			return posterResult{}, errors.New("poster temporarily unavailable")
		}
		// A disconnected visitor must not cancel the image shared by other visitors.
		fetchCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		poster, err := s.fetchPoster(fetchCtx, address)
		if err != nil {
			s.mu.Lock()
			s.failures[failureKey] = time.Now().Add(time.Minute)
			s.mu.Unlock()
			if hasCached {
				return cached, nil
			}
			return posterResult{}, err
		}
		s.mu.Lock()
		delete(s.failures, failureKey)
		s.mu.Unlock()
		s.savePoster(address, poster.data)
		return poster, nil
	})
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return nil, "", result.Err
		}
		poster := result.Val.(posterResult)
		return poster.data, poster.kind, nil
	}
}

func (s *Service) fetchPoster(ctx context.Context, address string) (posterResult, error) {
	select {
	case s.posterSlots <- struct{}{}:
	case <-ctx.Done():
		return posterResult{}, ctx.Err()
	}
	defer func() { <-s.posterSlots }()
	if err := s.waitRate(ctx); err != nil {
		return posterResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return posterResult{}, errors.New("invalid poster request")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://movie.douban.com/")
	response, err := s.client.Do(req)
	if err != nil {
		return posterResult{}, errors.New("poster unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return posterResult{}, errors.New("poster unavailable")
	}
	data, err := util.ReadAllLimited(response.Body, posterMaxBytes)
	if err != nil {
		return posterResult{}, errors.New("poster too large or incomplete")
	}
	kind, ok := posterContentType(data)
	if !ok {
		return posterResult{}, errors.New("invalid poster content")
	}
	return posterResult{data: data, kind: kind, updatedAt: time.Now()}, nil
}

func posterContentType(data []byte) (string, bool) {
	kind := http.DetectContentType(data)
	switch kind {
	case "image/jpeg", "image/png", "image/gif":
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		return kind, err == nil && config.Width > 0 && config.Height > 0
	case "image/webp":
		// WebP is not decoded by the standard library; validate its bounded RIFF
		// chunks rather than accepting the MIME signature alone.
		if len(data) < 20 || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
			return "", false
		}
		hasFrame := false
		for offset := 12; offset < len(data); {
			if len(data)-offset < 8 {
				return "", false
			}
			size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
			end := uint64(offset) + 8 + size + size%2
			if end > uint64(len(data)) {
				return "", false
			}
			switch string(data[offset : offset+4]) {
			case "VP8 ", "VP8L", "ANMF":
				hasFrame = hasFrame || size >= 5
			}
			offset = int(end)
		}
		return kind, hasFrame
	default:
		return "", false
	}
}

func (s *Service) posterCachePath(address string) string {
	key := sha256.Sum256([]byte(address))
	return filepath.Join(s.cfg.CacheDir, "posters", hex.EncodeToString(key[:])+".img")
}

// Background prefetch can check freshness without loading an image into memory.
func (s *Service) hasFreshPoster(address string) bool {
	if s.cfg.CacheDir == "" || !ValidPosterURL(address) {
		return false
	}
	info, err := os.Stat(s.posterCachePath(address))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= posterMaxBytes && time.Since(info.ModTime()) < posterCacheTTL
}

func (s *Service) loadPoster(address string) (posterResult, bool) {
	if s.cfg.CacheDir == "" {
		return posterResult{}, false
	}
	file, err := os.Open(s.posterCachePath(address))
	if err != nil {
		return posterResult{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > posterMaxBytes {
		return posterResult{}, false
	}
	data, err := util.ReadAllLimited(file, posterMaxBytes)
	if err != nil {
		return posterResult{}, false
	}
	kind, ok := posterContentType(data)
	if !ok {
		return posterResult{}, false
	}
	return posterResult{data: data, kind: kind, updatedAt: info.ModTime()}, true
}

func (s *Service) savePoster(address string, data []byte) {
	if s.cfg.CacheDir == "" {
		return
	}
	s.posterCacheMu.Lock()
	defer s.posterCacheMu.Unlock()
	path := s.posterCachePath(address)
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	file, err := os.CreateTemp(dir, ".poster-*")
	if err != nil {
		return
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return
	}
	if file.Close() != nil || os.Rename(temporary, path) != nil {
		return
	}
	s.prunePosterCache(dir)
}

// Called under posterCacheMu. Atomic replacement keeps concurrent readers from
// observing partial files; eviction only removes our hashed image filenames.
func (s *Service) prunePosterCache(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cachedFile struct {
		name string
		size int64
		date time.Time
	}
	var files []cachedFile
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 68 || !strings.HasSuffix(name, ".img") {
			continue
		}
		if _, err := hex.DecodeString(name[:64]); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, cachedFile{name: name, size: info.Size(), date: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].date.Equal(files[j].date) {
			return files[i].name < files[j].name
		}
		return files[i].date.Before(files[j].date)
	})
	for _, file := range files {
		if total <= posterCacheMaxBytes {
			break
		}
		if os.Remove(filepath.Join(dir, file.name)) == nil {
			total -= file.size
		}
	}
}
