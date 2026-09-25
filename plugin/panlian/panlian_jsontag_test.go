package panlian

import (
	"encoding/json"
	"testing"
)

// VideoItem 曾出现两个字段共用 json:"type_name"。同层出现同名 tag 时，
// encoding/json 会把这两个字段**全部忽略且不报错**，接口返回的 type_name
// 谁也没拿到，normalize 里的 firstNonEmpty(TypeName, Type) 因此永远落空。
// 这个用例锁住修正后的行为：两个不同 key 各自落到各自字段。
func TestVideoItemTypeFieldsUnmarshal(t *testing.T) {
	var item VideoItem
	payload := `{"type":"电影","type_name":"科幻片"}`
	if err := json.Unmarshal([]byte(payload), &item); err != nil {
		t.Fatal(err)
	}
	if item.Type != "电影" {
		t.Errorf("Type = %q, 期望 电影（json key 应为 type）", item.Type)
	}
	if item.TypeName != "科幻片" {
		t.Errorf("TypeName = %q, 期望 科幻片（json key 应为 type_name）", item.TypeName)
	}
}

// 只有 type_name 时也要能拿到值：这是修复前完全丢失的场景。
func TestVideoItemTypeNameOnly(t *testing.T) {
	var item VideoItem
	if err := json.Unmarshal([]byte(`{"type_name":"科幻片"}`), &item); err != nil {
		t.Fatal(err)
	}
	if item.TypeName != "科幻片" {
		t.Errorf("TypeName = %q, 期望 科幻片", item.TypeName)
	}
	if item.Type != "" {
		t.Errorf("Type = %q, 期望空", item.Type)
	}
}
