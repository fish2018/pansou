package json

import (
	"strings"
	"testing"
)

// 补齐的能力必须真的可用，而不是只有签名能编译。
// 这几个用例同时把 util/json 从 0% 覆盖率抬起来。

func TestEncoderDecoderRoundTrip(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	var buf strings.Builder
	if err := NewEncoder(&buf).Encode(payload{Name: "仙逆", Age: 3}); err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}

	var got payload
	if err := NewDecoder(strings.NewReader(buf.String())).Decode(&got); err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	if got.Name != "仙逆" || got.Age != 3 {
		t.Errorf("往返结果 = %+v, 期望 {仙逆 3}", got)
	}
}

// 确认 sonic 真的支持解到 RawMessage——这是迁移那 6 处调用点的前提，
// 不能只靠"签名能编译"就认为可用。
func TestRawMessageUnmarshal(t *testing.T) {
	var raw RawMessage
	if err := Unmarshal([]byte(`{"a":1}`), &raw); err != nil {
		t.Fatalf("解到 RawMessage 失败: %v", err)
	}
	if string(raw) != `{"a":1}` {
		t.Errorf("RawMessage = %s, 期望 {\"a\":1}", raw)
	}

	// 作为结构体字段时同样可用
	var outer struct {
		Inner RawMessage `json:"inner"`
	}
	if err := Unmarshal([]byte(`{"inner":{"x":[1,2]}}`), &outer); err != nil {
		t.Fatalf("作为字段解到 RawMessage 失败: %v", err)
	}
	if string(outer.Inner) != `{"x":[1,2]}` {
		t.Errorf("字段 RawMessage = %s", outer.Inner)
	}
}

// API 配置了 UseNumber: true，Number 类型必须与之配套工作。
func TestNumberWithUseNumberConfig(t *testing.T) {
	var v Number
	if err := Unmarshal([]byte(`12345678901234567890`), &v); err != nil {
		t.Fatalf("解到 Number 失败: %v", err)
	}
	if v.String() != "12345678901234567890" {
		t.Errorf("Number = %s，大整数被改写了", v)
	}
}

// 包裹数字的 Map 场景：UseNumber 下必须给出 Number 而不是 float64，
// 否则大整数精度会丢——这正是要统一走封装的原因。
func TestUseNumberKeepsPrecisionInMap(t *testing.T) {
	var m map[string]interface{}
	if err := Unmarshal([]byte(`{"id":12345678901234567890}`), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["id"].(Number); !ok {
		t.Errorf("UseNumber 下 map 值类型 = %T, 期望 json.Number", m["id"])
	}
}
