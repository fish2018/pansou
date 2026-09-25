package cache

import (
	"bytes"
	"sync"

	"pansou/util/json"
)

// 缓冲区对象池
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// SerializeWithPool 使用对象池序列化数据
func SerializeWithPool(v interface{}) ([]byte, error) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	// 使用sonic直接编码到缓冲区
	encoder := json.API.NewEncoder(buf)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}

	// 复制结果以避免池化对象被修改
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// DeserializeWithPool 使用对象池反序列化数据
func DeserializeWithPool(data []byte, v interface{}) error {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	// 写入数据到缓冲区
	buf.Write(data)

	// 使用sonic从缓冲区解码
	decoder := json.API.NewDecoder(buf)
	return decoder.Decode(v)
}
