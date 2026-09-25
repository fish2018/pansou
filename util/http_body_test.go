package util

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadAllLimitedPassesThroughWithinLimit(t *testing.T) {
	want := []byte("正常响应体")
	got, err := ReadAllLimited(bytes.NewReader(want), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("内容被改动: %q", got)
	}
}

// 边界：正好等于上限必须成功，多 1 字节必须失败。
// 只读 limit 字节无法区分"流结束"与"被截断"，所以实现要读 limit+1 字节。
func TestReadAllLimitedBoundaryIsExact(t *testing.T) {
	const limit = 64

	exact := bytes.Repeat([]byte("a"), limit)
	if _, err := ReadAllLimited(bytes.NewReader(exact), limit); err != nil {
		t.Errorf("正好等于上限不该失败: %v", err)
	}

	over := bytes.Repeat([]byte("a"), limit+1)
	_, err := ReadAllLimited(bytes.NewReader(over), limit)
	if err == nil {
		t.Fatal("超过上限 1 字节必须失败，否则上限形同虚设")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("错误应可判定: %v", err)
	}
}

// 超限时必须停下来，不能先把整个流读完再报错（那样上限就白设了）。
func TestReadAllLimitedStopsReading(t *testing.T) {
	const limit = 32
	// 一个"永远读不完"的流：如果实现把它读完，用例会超时/耗尽内存
	endless := io.LimitReader(zeroReader{}, 1<<30)

	_, err := ReadAllLimited(endless, limit)
	if err == nil {
		t.Fatal("应当报超限")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestReadAllLimitedRejectsNilAndZeroLimit(t *testing.T) {
	if _, err := ReadAllLimited(nil, 1024); err == nil {
		t.Error("nil 响应体应报错而不是 panic")
	}
	// limit <= 0 时回落到默认上限，而不是拒绝一切
	if _, err := ReadAllLimited(strings.NewReader("x"), 0); err != nil {
		t.Errorf("limit 为 0 应回落默认上限: %v", err)
	}
}
