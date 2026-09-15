# pan365（365 聚合站）

站点：<https://pan.365wp.top/>。站点使用自己的搜索及转存接口，可以作为独立来源接入 PanSou。

插件名为 `pan365`，优先级为 3，保留 Service 层关键词过滤。已加入主程序注册和 Docker 默认插件列表。已有部署需在自己的 `ENABLED_PLUGINS` 中加入 `pan365`，使用包含此插件的新构建重启。

```bash
curl --get 'http://127.0.0.1:8888/api/search' \
  --data-urlencode 'kw=流浪地球' \
  --data-urlencode 'src=plugin' \
  --data-urlencode 'plugins=pan365'
```

## 接口与限制

- 分别请求 `/api/interface/search` 的 `disk_type=quark` 和 `disk_type=baidu`，每条线路取第一页 20 项。未指定类型时首页主要是夸克，分开查询可保留百度结果。
- 去重、过滤无关标题后，每种网盘最多解析 6 项，总计最多 12 项。不同类型交替排入解析队列；实例内最多同时解析 3 项。
- 将搜索返回的加密 `url` 原样作为 `encrypted_url` 提交给 `/api/transfer-share/transfer-share`，只返回通过域名和分享路径校验的 `share_url`。只读取必要字段，不记录 `file_info` 等网盘内部信息。
- URL 中的提取码优先于 `passcode`，保留其他查询参数及片段，清除空的尾部 `?`。每条链接设置 `work_title`。
- 优先以 `original_url` 中的原始分享 ID 标识资源，避免重新转存生成新地址后重复；原始地址缺失时使用线路、来源和标题。
- 搜索阶段最多 10 秒，包含解析的总时限为 28 秒；单个请求最多 12 秒，单次响应不超过 2 MiB。部分线路或资源失败时保留其他成功结果，所有线路失败会返回错误。
- 成功转存结果缓存 30 分钟，上限 256 项；另沿用框架的搜索缓存。`ext.refresh=true` 绕过两层缓存重新获取链接。缓存时长用于减少请求，不代表网盘链接的有效期；不按站点的“五分钟”文案主动判定链接失效。
- HTTP 403、429 或验证码状态会使该实例暂停上游请求 1 分钟。失败结果不会进入转存缓存。

## 验证

`testdata/` 保存脱敏的搜索响应。离线测试覆盖多网盘查询、解析数量与并发、提取码、域名校验、缓存刷新、稳定 ID、部分失败及限流。

```bash
go test -race ./plugin/pan365
PAN365_LIVE_TEST=1 go test ./plugin/pan365 -run TestLiveSearch -count=1 -v
```

真实站点验证每种网盘最多尝试三项，找到一条可用分享后停止。默认测试不访问外网。
