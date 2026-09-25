package util

import (
	"errors"
	"fmt"
	"io"
)

// MaxUpstreamResponseBytes 是单次读取上游响应体的上限。
//
// 全仓原先有 70+ 处裸 io.ReadAll(resp.Body)：上游是第三方网盘站点，返回体量完全由对方
// 决定（被投毒、被镜像放大、或只是页面异常膨胀），裸读会把任意大小的响应整包物化进内存，
// 而插件是并发跑的——几十个插件同时撞上一个大响应就足以把进程内存打满。
//
// 取 16 MiB 是因为正常插件响应（HTML 页面、JSON 列表）在一两 MiB 量级，留了一个数量级的
// 余量；真出现超过它的合法响应，宁可让该插件按"读取失败"报错，也不要静默地撑爆内存
// ——失败是可见的，OOM 不是。
const MaxUpstreamResponseBytes int64 = 16 << 20

// ErrResponseTooLarge 表示响应体超过上限被截断（读取未完成）。
var ErrResponseTooLarge = errors.New("响应体超过上限")

// ReadAllLimited 读取响应体，超过 limit 字节即报错而不是继续读。
//
// 用 io.LimitReader 读 limit+1 字节：多读的那 1 字节是用来区分"正好等于上限"与"超过上限"
// 的——只读 limit 字节时无法判断流是结束了还是被截断了。
func ReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if r == nil {
		return nil, errors.New("响应体为空")
	}
	if limit <= 0 {
		limit = MaxUpstreamResponseBytes
	}

	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w（上限 %d 字节）", ErrResponseTooLarge, limit)
	}
	return data, nil
}

// 解压炸弹（compression bomb）防护参数。
//
// 上游响应体封顶只挡住第一层：压缩数据本身可以很小，解压后却极大（16 MiB 的 gzip 可解出
// 数十 GB），而解压后的内容通常还要整体物化进内存再解析。因此解压路径需要**第二道**约束。
const (
	// MaxDecompressedBytes 是解压后允许的最大字节数，取 64 MiB——比响应体上限(16 MiB)高 4 倍，
	// 因为压缩率正常的页面解压后本就会变大；真正异常的膨胀会远超这个量级。
	MaxDecompressedBytes int64 = 64 << 20

	// MaxDecompressRatio 是压缩比上限：解压后不得超过压缩前的 100 倍。
	// 业内成熟实现普遍同时约束"绝对大小 + 压缩比"，比值常取 100:1。
	MaxDecompressRatio int64 = 100

	// minDecompressAllowance 是小输入的保护下限：压缩前只有几 KB 时比值会剧烈波动
	// （例如 1 KB 压到 20 字节就是 50 倍），只看比值会误伤正常响应。
	// 输出在 400 KiB 以下一律放行，仍受 MaxDecompressedBytes 约束。
	minDecompressAllowance int64 = 400 << 10
)

// ErrDecompressionTooLarge 表示解压后的数据超过上限或压缩比异常。
var ErrDecompressionTooLarge = errors.New("解压后数据超过上限")

// ReadAllDecompressed 读取**解压后**的数据流，同时受绝对上限与压缩比上限约束。
//
// compressedSize 是压缩前（解压前）的字节数；传 0 表示未知（例如直接对响应体做流式解压，
// 压缩前的字节数事先不可知），此时只受绝对上限约束。
//
// 取舍与 ReadAllLimited 一致：宁可让该插件按"读取失败"报错，也不要静默撑爆内存
// ——失败是可见的，OOM 不是。
func ReadAllDecompressed(r io.Reader, compressedSize int64) ([]byte, error) {
	limit := MaxDecompressedBytes
	if compressedSize > 0 {
		ratioLimit := compressedSize * MaxDecompressRatio
		switch {
		case ratioLimit < minDecompressAllowance:
			limit = minDecompressAllowance
		case ratioLimit < limit:
			limit = ratioLimit
		}
	}

	data, err := ReadAllLimited(r, limit)
	if err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return nil, fmt.Errorf("%w（上限 %d 字节，压缩前 %d 字节）: %w",
				ErrDecompressionTooLarge, limit, compressedSize, err)
		}
		return nil, err
	}
	return data, nil
}

// cappedReader 是"超限即报错"的读取器。
//
// 为什么不用 io.LimitReader：它在到达上限后返回 EOF，调用方看到的是**正常结束**——
// 解析器会把半截数据当完整数据解析，得到错误结论却毫无察觉。这里改成超限时返回错误，
// 于是下游既有的错误分支（io.ReadAll 的 err、goquery 的解析错误）自动生效，
// 调用点无需改动就能获得"失败可见"。
type cappedReader struct {
	r     io.Reader
	limit int64
	read  int64
}

// NewCappedReader 包装 r，读取超过 limit 字节时返回 ErrDecompressionTooLarge。
// limit <= 0 时使用 MaxDecompressedBytes。
func NewCappedReader(r io.Reader, limit int64) io.Reader {
	if limit <= 0 {
		limit = MaxDecompressedBytes
	}
	return &cappedReader{r: r, limit: limit}
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.read > c.limit {
		return 0, fmt.Errorf("%w（上限 %d 字节）", ErrDecompressionTooLarge, c.limit)
	}
	// 只允许读到 limit+1 字节：多读的那 1 字节用来区分"正好等于上限"与"超过上限"，
	// 与 ReadAllLimited 同一套办法。同时避免一次读进远超额度的数据。
	remaining := c.limit + 1 - c.read
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return n, fmt.Errorf("%w（上限 %d 字节）", ErrDecompressionTooLarge, c.limit)
	}
	return n, err
}
