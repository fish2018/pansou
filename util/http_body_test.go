package util

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
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

// 解压炸弹：压缩数据很小、解压后极大，必须被上限挡住，而不是把内存吃光。
func TestReadAllDecompressedStopsBomb(t *testing.T) {
	// 16 MiB 的零字节压成 gzip 后只有几十 KB——这就是炸弹的形状
	payload := make([]byte, 16<<20)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := buf.Bytes()
	t.Logf("压缩前(解压后) %d 字节 → 压缩后 %d 字节", len(payload), len(compressed))

	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	start := time.Now()
	out, err := ReadAllDecompressed(zr, int64(len(compressed)))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("炸弹必须被挡住，实际读出 %d 字节", len(out))
	}
	if !errors.Is(err, ErrDecompressionTooLarge) {
		t.Errorf("应可用 errors.Is 判定解压超限，实际: %v", err)
	}
	// 只读到上限就停，不应该把 16 MiB 全部解出来
	if int64(len(out)) > MaxDecompressRatio*int64(len(compressed))+minDecompressAllowance+1 {
		t.Errorf("读出的字节数超出限额: %d", len(out))
	}
	t.Logf("被挡下，耗时 %v，读出 %d 字节", elapsed, len(out))
}

// 对照：正常压缩的响应必须能正常读出来，不能被防炸弹逻辑误伤。
func TestReadAllDecompressedAllowsNormalPayload(t *testing.T) {
	payload := bytes.Repeat([]byte("正常的页面内容，重复若干次以形成可压缩的输入。"), 2000) // ~100 KB
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := buf.Bytes()

	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	out, err := ReadAllDecompressed(zr, int64(len(compressed)))
	if err != nil {
		t.Fatalf("正常负载不该被挡住: %v（压缩前 %d，解压后 %d）", err, len(compressed), len(payload))
	}
	if !bytes.Equal(out, payload) {
		t.Errorf("内容不一致：期望 %d 字节，实际 %d 字节", len(payload), len(out))
	}
}

// 压缩前大小未知（流式解压）时只受绝对上限约束：此时小压缩比不能被当成炸弹。
func TestReadAllDecompressedUnknownCompressedSize(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<20) // 1 MiB
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	out, err := ReadAllDecompressed(zr, 0)
	if err != nil {
		t.Fatalf("压缩前大小未知时不该按比值拦截: %v", err)
	}
	if len(out) != len(payload) {
		t.Errorf("内容长度不一致: %d", len(out))
	}
}

// 正好等于上限必须放行：边界多读的 1 字节只用于区分"恰好"与"超出"。
func TestCappedReaderAllowsExactLimit(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 1024)
	out, err := io.ReadAll(NewCappedReader(bytes.NewReader(data), 1024))
	if err != nil {
		t.Fatalf("正好等于上限不该报错: %v", err)
	}
	if len(out) != 1024 {
		t.Errorf("内容不完整: %d", len(out))
	}
}

// 超过上限必须**报错**而不是静默截断——静默截断会让解析器把半截数据当完整数据。
func TestCappedReaderErrorsInsteadOfTruncating(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 1025)
	_, err := io.ReadAll(NewCappedReader(bytes.NewReader(data), 1024))
	if err == nil {
		t.Fatal("超过上限必须报错，静默截断是最坏的结果")
	}
	if !errors.Is(err, ErrDecompressionTooLarge) {
		t.Errorf("应可用 errors.Is 判定: %v", err)
	}
}

// 下游是解析器（而非 io.ReadAll）时同样生效：goquery 之类的解析错误由此路透出。
func TestCappedReaderBoundsStreamingDecompression(t *testing.T) {
	payload := make([]byte, 8<<20) // 解压后 8 MiB
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	// 模拟插件里的写法：reader = gzReader 之后直接读
	if _, err := io.ReadAll(NewCappedReader(zr, 1<<20)); err == nil {
		t.Fatal("8 MiB 的解压流在 1 MiB 上限下必须报错")
	}
}
