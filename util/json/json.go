package json

import (
	"github.com/bytedance/sonic"
)

// API是sonic的全局配置实例
var API = sonic.ConfigDefault

// 初始化sonic配置
func init() {
	// 根据需要配置sonic选项
	API = sonic.Config{
		UseNumber:   true,
		EscapeHTML:  true,
		SortMapKeys: false, // 生产环境设为false提高性能
	}.Froze()
}

// Marshal 使用sonic序列化对象到JSON
func Marshal(v interface{}) ([]byte, error) {
	return API.Marshal(v)
}

// Unmarshal 使用sonic反序列化JSON到对象
func Unmarshal(data []byte, v interface{}) error {
	return API.Unmarshal(data, v)
}

// MarshalString 序列化对象到JSON字符串
func MarshalString(v interface{}) (string, error) {
	bytes, err := API.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// UnmarshalString 反序列化JSON字符串到对象
func UnmarshalString(str string, v interface{}) error {
	return API.Unmarshal([]byte(str), v)
}

// MarshalIndent 序列化对象到格式化的JSON
func MarshalIndent(v interface{}, prefix, indent string) ([]byte, error) {
	// 使用sonic的格式化功能
	return API.MarshalIndent(v, prefix, indent)
}
