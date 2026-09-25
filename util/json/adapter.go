package json

import (
	stdjson "encoding/json"
	"io"

	"github.com/bytedance/sonic"
)

// 本文件补齐调用方需要、但原先没有暴露的能力。
//
// 项目约定：业务代码一律使用 pansou/util/json，不要直接 import encoding/json，
// 以保证序列化行为统一由这里的 sonic 配置决定（见 json.go 的 init：
// UseNumber/EscapeHTML/SortMapKeys）。但 RawMessage、Number、NewDecoder 这些是
// 业务代码实际会用到的类型与构造函数，封装里没有就会逼着调用方绕开封装，
// 所以在这里补齐，让"只用封装"这条约定在全部调用点都能成立。

// Decoder 是 sonic 的解码器。
// 注意 sonic v1.14 的 NewDecoder 返回**值**而不是指针（与 encoding/json 不同）。
type Decoder = sonic.Decoder

// Encoder 是 sonic 的编码器。
type Encoder = sonic.Encoder

// RawMessage 与 encoding/json 的同名类型语义一致，sonic 原生支持对它编解码。
type RawMessage = stdjson.RawMessage

// Number 与 encoding/json 的同名类型一致，且与 API 的 UseNumber: true 配置配套。
type Number = stdjson.Number

// NewDecoder 返回一个从 r 读取的解码器。
func NewDecoder(r io.Reader) Decoder {
	return API.NewDecoder(r)
}

// NewEncoder 返回一个写入 w 的编码器。
func NewEncoder(w io.Writer) Encoder {
	return API.NewEncoder(w)
}
