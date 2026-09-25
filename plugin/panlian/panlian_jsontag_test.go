package panlian

import (
	"reflect"
	"testing"

	utiljson "pansou/util/json"
)

// VideoItem 曾有两个字段共用 json:"type_name"。
//
// 惨痛的教训：我曾按 encoding/json 的文档推断"两个字段都会被整体忽略"，随后又
// 用一个探针"推翻"了自己的结论——但那个探针跑在**已经修好 tag 的结构**上，
// 根本没构成冲突，所以它证明不了任何事。重新用真正构造冲突的结构分别实测：
//
//	修复前结构（Type 与 TypeName 同为 json:"type_name"）+ sonic:  Type="" TypeName=""   ← 两个都丢
//	修复后结构（Type 为 json:"type"）+ sonic 给两个 key:           Type="电影" TypeName="科幻片"
//
// 结论：sonic（pansou/util/json）与 encoding/json 在这一点上行为一致，都是全部忽略
// 且不报错，最初"接口返回的 type_name 谁也没拿到"的判断成立。
//
// 下面两个用例锁住修正后的行为，第三个用例把冲突语义钉住。
func TestVideoItemTypeFieldsUnmarshal(t *testing.T) {
	var item VideoItem
	payload := `{"type":"电影","type_name":"科幻片"}`
	if err := utiljson.Unmarshal([]byte(payload), &item); err != nil {
		t.Fatal(err)
	}
	if item.Type != "电影" {
		t.Errorf("Type = %q, 期望 电影（json key 应为 type）", item.Type)
	}
	if item.TypeName != "科幻片" {
		t.Errorf("TypeName = %q, 期望 科幻片（json key 应为 type_name）", item.TypeName)
	}
}

// 只给 type_name 时必须能被 TypeName 接住——这正是修复前完全丢失的场景。
func TestVideoItemTypeNameOnly(t *testing.T) {
	var item VideoItem
	if err := utiljson.Unmarshal([]byte(`{"type_name":"科幻片"}`), &item); err != nil {
		t.Fatal(err)
	}
	if item.TypeName != "科幻片" {
		t.Errorf("TypeName = %q, 期望 科幻片", item.TypeName)
	}
	if item.Type != "" {
		t.Errorf("Type = %q, 期望空", item.Type)
	}
}

// 把"同层同名 tag → 全部忽略且不报错"钉住，并且真的构造出冲突。
//
// 类型用 reflect.StructOf 在运行时构造，而不是在源码里写两个同 tag 的字段：
// 后者会触发 go vet 的 "struct field repeats json tag"，让这条刻意构造的用例
// 污染 vet 输出；运行时构造同样能走到 sonic 的字段解析逻辑，冲突一点不打折。
func TestDuplicateTagAtSameDepthIsDropped(t *testing.T) {
	typ := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: reflect.TypeOf(""), Tag: `json:"same"`},
		{Name: "B", Type: reflect.TypeOf(""), Tag: `json:"same"`},
	})
	v := reflect.New(typ)
	if err := utiljson.Unmarshal([]byte(`{"same":"v"}`), v.Interface()); err != nil {
		t.Fatal(err)
	}
	a := v.Elem().FieldByName("A").String()
	b := v.Elem().FieldByName("B").String()
	if a != "" || b != "" {
		t.Errorf("sonic 未按预期丢弃冲突字段: A=%q B=%q；若 sonic 升级改变了该行为，需重新评估同类 tag 冲突", a, b)
	}
}
