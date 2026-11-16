package discover

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func (store *tmdbCatalogStore) checkCapacity() error {
	var size int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(store.path + suffix); err == nil {
			size += info.Size()
		}
	}
	if size >= store.maxBytes {
		store.setStage("paused", "storage_limit")
		return errFullCatalogCapacity
	}
	var fs unix.Statfs_t
	if unix.Statfs(filepath.Dir(store.path), &fs) == nil && uint64(fs.Bavail)*uint64(fs.Bsize) < 1<<30 {
		store.setStage("paused", "disk_space_low")
		return errFullCatalogCapacity
	}
	return nil
}
