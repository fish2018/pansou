package cache

import (
	"bytes"
	"encoding/gob"
	"sync"
	"time"

	"pansou/model"
)

// 初始化函数，注册model包中的类型到gob
func init() {
	// 注册SearchResult类型
	gob.Register(model.SearchResult{})

	// 注册SearchResponse类型
	gob.Register(model.SearchResponse{})

	// 注册MergedLinks类型
	gob.Register(model.MergedLinks{})

	// 注册[]model.SearchResult类型
	gob.Register([]model.SearchResult{})

	// 注册map[string][]model.SearchResult类型
	gob.Register(map[string][]model.SearchResult{})

	// 注册time.Time类型
	gob.Register(time.Time{})
}

// Serializer 序列化接口
type Serializer interface {
	Serialize(v interface{}) ([]byte, error)
	Deserialize(data []byte, v interface{}) error
}

// GobSerializer 使用gob进行序列化/反序列化
type GobSerializer struct {
	bufferPool sync.Pool
}

// NewGobSerializer 创建新的gob序列化器
func NewGobSerializer() *GobSerializer {
	return &GobSerializer{
		bufferPool: sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
	}
}

// Serialize 序列化数据
func (s *GobSerializer) Serialize(v interface{}) ([]byte, error) {
	buf := s.bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer s.bufferPool.Put(buf)

	enc := gob.NewEncoder(buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// Deserialize 反序列化数据
func (s *GobSerializer) Deserialize(data []byte, v interface{}) error {
	buf := s.bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer s.bufferPool.Put(buf)

	buf.Write(data)
	dec := gob.NewDecoder(buf)
	return dec.Decode(v)
}

// JSONSerializer 使用JSON进行序列化/反序列化
// 为了保持向后兼容性
type JSONSerializer struct {
	bufferPool *sync.Pool
}

// NewJSONSerializer 创建新的JSON序列化器
func NewJSONSerializer() *JSONSerializer {
	return &JSONSerializer{
		bufferPool: &bufferPool, // 使用已有的缓冲区池
	}
}

// Serialize 序列化数据
func (s *JSONSerializer) Serialize(v interface{}) ([]byte, error) {
	return SerializeWithPool(v)
}

// Deserialize 反序列化数据
func (s *JSONSerializer) Deserialize(data []byte, v interface{}) error {
	return DeserializeWithPool(data, v)
}
