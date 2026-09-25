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
