package cache

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pansou/util/json"
)

// 磁盘缓存项元数据
type diskCacheMetadata struct {
	Key          string    `json:"key"`
	Expiry       time.Time `json:"expiry"`
	LastUsed     time.Time `json:"last_used"`
	Size         int       `json:"size"`
	LastModified time.Time `json:"last_modified"` // 添加最后修改时间字段
}

// DiskCache 磁盘缓存
type DiskCache struct {
	path      string
	maxSizeMB int
	metadata  map[string]*diskCacheMetadata
	mutex     sync.RWMutex
	currSize  int64
}

// NewDiskCache 创建新的磁盘缓存
func NewDiskCache(path string, maxSizeMB int) (*DiskCache, error) {
	// 确保缓存目录存在
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}

	cache := &DiskCache{
		path:      path,
		maxSizeMB: maxSizeMB,
		metadata:  make(map[string]*diskCacheMetadata),
	}

	// 加载现有缓存元数据
	cache.loadMetadata()

	// 启动周期性清理
	go cache.startCleanupTask()

	return cache, nil
}

// 加载元数据
func (c *DiskCache) loadMetadata() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// 遍历缓存目录
	files, err := ioutil.ReadDir(c.path)
	if err != nil {
		return
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		// 跳过元数据文件
		if file.Name() == "metadata.json" {
			continue
		}

		// 读取元数据
		metadataFile := filepath.Join(c.path, file.Name()+".meta")
		data, err := ioutil.ReadFile(metadataFile)
		if err != nil {
			continue
		}

		var meta diskCacheMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}

		// 更新总大小
		c.currSize += int64(meta.Size)

		// 存储元数据
		c.metadata[meta.Key] = &meta
	}
}

// 保存元数据
func (c *DiskCache) saveMetadata(key string, meta *diskCacheMetadata) error {
	metadataFile := filepath.Join(c.path, c.getFilename(key)+".meta")
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// 元数据同样要原子写：半截元数据会让缓存项"存在但读不出"
	return writeFileAtomic(metadataFile, data, 0644)
}

// writeFileAtomic 原子落盘：写临时文件 → fsync → rename。
//
// 原先直接 ioutil.WriteFile 到最终路径：进程在写一半时被 kill（容器滚动更新、OOM、
// 宿主机重启）会留下截断的文件，而元数据仍然有效，Get 就会把半截 JSON 交给反序列化
// ——故障表现是"结果是空的/残缺的"而不是缓存未命中，排查时极易误判为上游插件故障。
//
// rename 在同一文件系统内是原子的：读者要么看到旧内容、要么看到完整新内容。
// fsync 是为了防止"rename 已生效但数据还在页缓存里"时断电留下空文件。
func writeFileAtomic(filePath string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(filePath)
	tmp, err := os.CreateTemp(dir, filepath.Base(filePath)+".tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	// 无论从哪条路径返回，都别留下临时文件
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("刷盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, filePath); err != nil {
		return fmt.Errorf("重命名到目标路径失败: %w", err)
	}
	tmpName = "" // rename 成功，临时文件已不存在
	return nil
}

// 获取文件名
func (c *DiskCache) getFilename(key string) string {
	hash := md5.Sum([]byte(key))
	return hex.EncodeToString(hash[:])
}

// Set 设置缓存
func (c *DiskCache) Set(key string, data []byte, ttl time.Duration) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// 如果已存在，先减去旧项的大小。
	// 这里不再预删旧文件：getFilename 是键的哈希，新旧文件同路径，rename 会整体覆盖；
	// 预先 os.Remove 反而把"写一半崩溃"的窗口重新打开，且删不掉时错误被丢弃。
	if meta, exists := c.metadata[key]; exists {
		c.currSize -= int64(meta.Size)
	}

	// 检查空间
	maxSize := int64(c.maxSizeMB) * 1024 * 1024
	if c.currSize+int64(len(data)) > maxSize {
		// 清理空间
		c.evictLRU(int64(len(data)))
	}

	// 获取文件名
	filename := c.getFilename(key)
	filePath := filepath.Join(c.path, filename)

	// 确保目录存在（防止外部删除缓存目录）
	if err := os.MkdirAll(c.path, 0755); err != nil {
		return fmt.Errorf("创建缓存目录失败: %v", err)
	}

	// 原子写入文件
	if err := writeFileAtomic(filePath, data, 0644); err != nil {
		return err
	}

	// 创建元数据
	now := time.Now()
	meta := &diskCacheMetadata{
		Key:          key,
		Expiry:       now.Add(ttl),
		LastUsed:     now,
		LastModified: now, // 设置最后修改时间
		Size:         len(data),
	}

	// 保存元数据
	if err := c.saveMetadata(key, meta); err != nil {
		// 元数据没落盘就等于这项不存在（Get 只认内存元数据表），
		// 数据文件留着只会占空间，删掉；删除失败只记日志不回滚主错误。
		if rmErr := os.Remove(filePath); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Printf("[DISK_CACHE] 元数据保存失败后清理数据文件也未成功: %s | %v\n", filePath, rmErr)
		}
		return err
	}

	// 更新内存中的元数据
	c.metadata[key] = meta
	c.currSize += int64(len(data))

	return nil
}

// Get 获取缓存
func (c *DiskCache) Get(key string) ([]byte, bool, error) {
	c.mutex.RLock()
	meta, exists := c.metadata[key]
	c.mutex.RUnlock()

	if !exists {
		return nil, false, nil
	}

	// 检查是否过期
	if time.Now().After(meta.Expiry) {
		c.Delete(key)
		return nil, false, nil
	}

	// 获取文件路径
	filePath := filepath.Join(c.path, c.getFilename(key))

	// 读取文件
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		// 如果文件不存在，删除元数据
		if os.IsNotExist(err) {
			c.Delete(key)
		}
		return nil, false, err
	}

	// 更新最后使用时间
	c.mutex.Lock()
	meta.LastUsed = time.Now()
	c.saveMetadata(key, meta)
	c.mutex.Unlock()

	return data, true, nil
}

// Delete 删除缓存
func (c *DiskCache) Delete(key string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	meta, exists := c.metadata[key]
	if !exists {
		return nil
	}

	// 删除文件。文件本就不存在是正常情况（过期清理、重复删除），不算失败；
	// 其它错误要报出来，否则会变成"元数据已删但文件永远留在磁盘上"的静默泄漏。
	filename := c.getFilename(key)
	var firstErr error
	for _, name := range []string{filename, filename + ".meta"} {
		if err := os.Remove(filepath.Join(c.path, name)); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("删除缓存文件 %s 失败: %w", name, err)
			} else {
				fmt.Printf("[DISK_CACHE] 删除缓存文件失败: %s | %v\n", name, err)
			}
		}
	}

	// 更新元数据（无论文件是否删干净都要做，否则内存表会一直引用不存在的项）
	c.currSize -= int64(meta.Size)
	delete(c.metadata, key)

	return nil
}

// Has 检查缓存是否存在
func (c *DiskCache) Has(key string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	meta, exists := c.metadata[key]
	if !exists {
		return false
	}

	// 检查是否过期
	if time.Now().After(meta.Expiry) {
		// 异步删除过期项
		go c.Delete(key)
		return false
	}

	return true
}

// 清理过期项
func (c *DiskCache) cleanExpired() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := time.Now()
	for key, meta := range c.metadata {
		if now.After(meta.Expiry) {
			// 删除文件
			filename := c.getFilename(key)
			err := os.Remove(filepath.Join(c.path, filename))
			if err == nil || os.IsNotExist(err) {
				os.Remove(filepath.Join(c.path, filename+".meta"))
				c.currSize -= int64(meta.Size)
				delete(c.metadata, key)
			}
		}
	}
}

// 驱逐策略 - LRU
func (c *DiskCache) evictLRU(requiredSpace int64) {
	// 按最后使用时间排序
	type cacheItem struct {
		key      string
		lastUsed time.Time
		size     int
	}

	items := make([]cacheItem, 0, len(c.metadata))
	for k, v := range c.metadata {
		items = append(items, cacheItem{
			key:      k,
			lastUsed: v.LastUsed,
			size:     v.Size,
		})
	}

	// 按最后使用时间排序
	// 使用冒泡排序保持简单
	for i := 0; i < len(items); i++ {
		for j := 0; j < len(items)-i-1; j++ {
			if items[j].lastUsed.After(items[j+1].lastUsed) {
				items[j], items[j+1] = items[j+1], items[j]
			}
		}
	}

	// 从最久未使用开始删除，直到有足够空间
	maxSize := int64(c.maxSizeMB) * 1024 * 1024
	for _, item := range items {
		if c.currSize+requiredSpace <= maxSize {
			break
		}

		// 删除文件
		filename := c.getFilename(item.key)
		err := os.Remove(filepath.Join(c.path, filename))
		if err == nil || os.IsNotExist(err) {
			os.Remove(filepath.Join(c.path, filename+".meta"))
			c.currSize -= int64(item.size)
			delete(c.metadata, item.key)
		}
	}
}

// 启动定期清理任务
func (c *DiskCache) startCleanupTask() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		c.cleanExpired()
	}
}

// Clear 清空缓存
func (c *DiskCache) Clear() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// 删除所有缓存文件
	files, err := ioutil.ReadDir(c.path)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		os.Remove(filepath.Join(c.path, file.Name()))
	}

	// 重置元数据
	c.metadata = make(map[string]*diskCacheMetadata)
	c.currSize = 0

	return nil
}

// GetExpiry 获取缓存项的过期时间（写入时按 ttl 算出的那个时刻）。
//
// 存在的理由是上层需要它：EnhancedTwoLevelCache 从磁盘回填内存时，原先一律用
// CacheTTLMinutes 重新计时，于是磁盘上按 3 分钟短 TTL 落盘的 partial 结果
// 会在内存里获得完整 60 分钟寿命，短 TTL 分流被整个绕过。
func (c *DiskCache) GetExpiry(key string) (time.Time, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	meta, exists := c.metadata[key]
	if !exists {
		return time.Time{}, false
	}

	return meta.Expiry, true
}

// GetLastModified 获取缓存项的最后修改时间
func (c *DiskCache) GetLastModified(key string) (time.Time, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	meta, exists := c.metadata[key]
	if !exists {
		return time.Time{}, false
	}

	return meta.LastModified, true
}
