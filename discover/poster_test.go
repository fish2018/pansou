package discover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const posterTestAddress = "https://img9.doubanio.com/view/photo/s_ratio_poster/public/p123.jpg"

func posterTestImage(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: shade, A: 255})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func posterTestResponse(data []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}
}

func TestPosterDiskCacheSurvivesServiceRestart(t *testing.T) {
	dir := t.TempDir()
	want := posterTestImage(t, 10)
	first := New(Config{CacheDir: dir})
	first.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return posterTestResponse(want), nil
	})
	got, kind, err := first.Poster(context.Background(), posterTestAddress)
	if err != nil || kind != "image/png" || !bytes.Equal(got, want) {
		t.Fatalf("downloaded poster = %q / %v", kind, err)
	}
	stored, err := os.ReadFile(first.posterCachePath(posterTestAddress))
	if err != nil || !bytes.Equal(stored, want) {
		t.Fatalf("cache must contain raw image bytes: %v", err)
	}
	second := New(Config{CacheDir: dir})
	second.nextRequest = time.Now().Add(time.Hour)
	second.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("fresh persisted poster caused a network request")
		return nil, errors.New("unexpected network")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, kind, err = second.Poster(ctx, posterTestAddress)
	if err != nil || kind != "image/png" || !bytes.Equal(got, want) || !second.hasFreshPoster(posterTestAddress) {
		t.Fatalf("restarted service failed to serve its cache: %q / %v", kind, err)
	}
}

func TestPosterMergesConcurrentDownloads(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir()})
	want := posterTestImage(t, 20)
	var requests atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return posterTestResponse(want), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	const visitors = 24
	ready := make(chan struct{}, visitors)
	var visitorsDone sync.WaitGroup
	for range visitors {
		visitorsDone.Add(1)
		go func() {
			defer visitorsDone.Done()
			ready <- struct{}{}
			data, kind, err := s.Poster(context.Background(), posterTestAddress)
			if err != nil || kind != "image/png" || !bytes.Equal(data, want) {
				t.Errorf("shared image failed: %q / %v", kind, err)
			}
		}()
	}
	<-entered
	for range visitors {
		<-ready
	}
	close(release)
	visitorsDone.Wait()
	if got := requests.Load(); got != 1 {
		t.Fatalf("%d upstream requests for one poster", got)
	}
}

func TestPosterDisconnectedVisitorDoesNotCancelSharedDownload(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir()})
	want := posterTestImage(t, 30)
	entered := make(chan struct{})
	release := make(chan struct{})
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
			return posterTestResponse(want), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := s.Poster(ctx, posterTestAddress)
		firstDone <- err
	}()
	<-entered
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("disconnected visitor got %v", err)
	}
	secondDone := make(chan error, 1)
	go func() {
		data, _, err := s.Poster(context.Background(), posterTestAddress)
		if err == nil && !bytes.Equal(data, want) {
			err = errors.New("shared image changed")
		}
		secondDone <- err
	}()
	close(release)
	if err := <-secondDone; err != nil {
		t.Fatalf("other visitor lost the shared download: %v", err)
	}
}

func TestPosterExpiredCacheFallsBackWithoutExtendingTTL(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir()})
	want := posterTestImage(t, 40)
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return posterTestResponse(want), nil
	})
	if _, _, err := s.Poster(context.Background(), posterTestAddress); err != nil {
		t.Fatal(err)
	}
	path := s.posterCachePath(posterTestAddress)
	expired := time.Now().Add(-posterCacheTTL - time.Hour)
	if err := os.Chtimes(path, expired, expired); err != nil {
		t.Fatal(err)
	}
	if s.hasFreshPoster(posterTestAddress) {
		t.Fatal("expired image counted as fresh")
	}
	var requests atomic.Int32
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("upstream unavailable")
	})
	got, kind, err := s.Poster(context.Background(), posterTestAddress)
	if err != nil || kind != "image/png" || !bytes.Equal(got, want) || requests.Load() != 1 {
		t.Fatalf("failed to fall back after one refresh: %q / %v", kind, err)
	}
	for range 3 {
		got, _, err = s.Poster(context.Background(), posterTestAddress)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("backoff failed to serve stale image: %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("stale poster retried upstream during its failure backoff")
	}
	info, err := os.Stat(path)
	if err != nil || info.ModTime().Sub(expired).Abs() > time.Second {
		t.Fatalf("failure extended stale image TTL: %v", err)
	}
	refreshed := New(Config{CacheDir: s.cfg.CacheDir})
	newImage := posterTestImage(t, 50)
	refreshed.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return posterTestResponse(newImage), nil
	})
	got, _, err = refreshed.Poster(context.Background(), posterTestAddress)
	if err != nil || !bytes.Equal(got, newImage) || !refreshed.hasFreshPoster(posterTestAddress) {
		t.Fatalf("expired poster failed to refresh: %v", err)
	}
}

func TestPosterCacheEvictsOldestFilesAt128MiB(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir()})
	dir := filepath.Dir(s.posterCachePath(posterTestAddress))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var oldest string
	// Sparse files exercise the production 128 MiB limit without writing 129 MiB.
	for i := range 43 {
		path := s.posterCachePath(fmt.Sprintf("https://img9.doubanio.com/view/photo/public/old-%d.jpg", i))
		if i == 0 {
			oldest = path
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, posterMaxBytes); err != nil {
			t.Fatal(err)
		}
		date := time.Now().Add(-time.Hour + time.Duration(i)*time.Second)
		if err := os.Chtimes(path, date, date); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "README")
	if err := os.WriteFile(keep, []byte("unrelated file"), 0600); err != nil {
		t.Fatal(err)
	}
	want := posterTestImage(t, 60)
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return posterTestResponse(want), nil
	})
	if _, _, err := s.Poster(context.Background(), posterTestAddress); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("oldest cache file was retained: %v", err)
	}
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".img" {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			total += info.Size()
		}
	}
	if total > posterCacheMaxBytes || total < 126<<20 {
		t.Fatalf("cache size after eviction = %d", total)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("eviction touched an unrelated file: %v", err)
	}
	if !s.hasFreshPoster(posterTestAddress) {
		t.Fatal("eviction lost the newly downloaded image")
	}
}

func TestPosterRejectsUnsafeOrInvalidImagesWithoutCaching(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir()})
	var requests atomic.Int32
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return posterTestResponse(posterTestImage(t, 70)), nil
	})
	if _, _, err := s.Poster(context.Background(), "https://127.0.0.1/view/photo/a.png"); err == nil || requests.Load() != 0 {
		t.Fatal("unsafe URL reached the network")
	}
	for name, payload := range map[string][]byte{
		"svg":         []byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"),
		"png-header":  []byte("\x89PNG\r\n\x1a\n"),
		"webp-header": []byte("RIFF\x14\x00\x00\x00WEBPVP8 \x08\x00\x00\x00"),
		"too-large":   bytes.Repeat([]byte("a"), posterMaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			service := New(Config{CacheDir: t.TempDir()})
			service.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return posterTestResponse(payload), nil
			})
			if _, _, err := service.Poster(context.Background(), posterTestAddress); err == nil {
				t.Fatal("accepted invalid image")
			}
			if _, err := os.Stat(service.posterCachePath(posterTestAddress)); !os.IsNotExist(err) {
				t.Fatalf("invalid image entered disk cache: %v", err)
			}
		})
	}
	if err := os.MkdirAll(filepath.Dir(s.posterCachePath(posterTestAddress)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.posterCachePath(posterTestAddress), []byte("<svg>poisoned cache</svg>"), 0600); err != nil {
		t.Fatal(err)
	}
	data, kind, err := s.Poster(context.Background(), posterTestAddress)
	if err != nil || kind != "image/png" || len(data) == 0 || requests.Load() != 1 {
		t.Fatalf("invalid cached bytes were served: %q / %v", kind, err)
	}
}
