package pool

import (
	"sync"

	"pansou/model"
)

// LinkPool 网盘链接对象池
var LinkPool = sync.Pool{
	New: func() interface{} {
		return &model.Link{}
	},
}

// SearchResultPool 搜索结果对象池
var SearchResultPool = sync.Pool{
	New: func() interface{} {
		return &model.SearchResult{
			Links: make([]model.Link, 0, 4),
			Tags:  make([]string, 0, 8),
		}
	},
}

// MergedLinkPool 合并链接对象池
var MergedLinkPool = sync.Pool{
	New: func() interface{} {
		return &model.MergedLink{}
	},
}

// GetLink 从对象池获取Link对象
func GetLink() *model.Link {
	return LinkPool.Get().(*model.Link)
}

// ReleaseLink 释放Link对象回对象池
func ReleaseLink(l *model.Link) {
	l.Type = ""
	l.URL = ""
	l.Password = ""
	LinkPool.Put(l)
}

// GetSearchResult 从对象池获取SearchResult对象
func GetSearchResult() *model.SearchResult {
	return SearchResultPool.Get().(*model.SearchResult)
}

// ReleaseSearchResult 释放SearchResult对象回对象池
func ReleaseSearchResult(sr *model.SearchResult) {
	sr.MessageID = ""
	sr.Channel = ""
	sr.Title = ""
	sr.Content = ""
	sr.Links = sr.Links[:0]
	sr.Tags = sr.Tags[:0]
	// 不重置时间，因为会被重新赋值
	SearchResultPool.Put(sr)
}

// GetMergedLink 从对象池获取MergedLink对象
func GetMergedLink() *model.MergedLink {
	return MergedLinkPool.Get().(*model.MergedLink)
}

// ReleaseMergedLink 释放MergedLink对象回对象池
func ReleaseMergedLink(ml *model.MergedLink) {
	ml.URL = ""
	ml.Password = ""
	ml.Note = ""
	// 不重置时间，因为会被重新赋值
	MergedLinkPool.Put(ml)
}
