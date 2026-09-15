# buerchen（不二资源搜索站）

站点：<https://buerchen.top/>。插件同时接入页面使用的联网搜索和“每日更新”，支持夸克、百度、迅雷分享。

插件名为 `buerchen`，优先级为 3，保留 Service 层关键词过滤。已加入主程序注册和 Docker 默认插件列表。已有部署需在自己的 `ENABLED_PLUGINS` 中加入 `buerchen`，使用包含此插件的新构建重启。

```bash
curl --get 'http://127.0.0.1:8888/api/search' \
  --data-urlencode 'kw=流浪地球' \
  --data-urlencode 'src=plugin' \
  --data-urlencode 'plugins=buerchen'
```

## 接口与限制

- SSE 搜索：`/api/other/web_search?title=关键词&is_type=0/2/4`。三种类型分别查询；该站对 `is_type=-1` 返回“暂无可用线路”，不能照搬其他同类站的全部类型参数。
- 支持 SSE 的多行数据、CRLF、心跳与 `[DONE]` 结束标志，并兼容站点混入的“线路：”状态行；跳过重复、失效及不匹配的资源。验证页面、验证码 JSON、损坏或中断的空流不会被当成成功的空搜索。
- 每日更新：使用 `/api2/content?keyword=关键词` 服务端过滤，避免下载超过 2 MiB 的完整目录；通过 `/api2/decrypt` 解密相应网盘字段，再走站点的链接解析流程。
- 加密搜索 token 或每日更新解密结果先按浏览器 `encodeURIComponent` 编码，再放入 JSON 的 `url` 字段，连同标题 POST 到 `/api/other/save_url`。搜索中已经提供的合法网盘直链直接返回。
- 每种网盘最多处理 6 项，其中每日更新最多占 2 项，总计最多 18 项。类型之间交替安排，实例内最多同时解析 3 项；不会调用资源失效标记或反馈接口。
- 返回前验证分享域名、路径及网盘类型，保留提取码并设置 `work_title`。以原始搜索 token 或每日更新的原始分享 ID 标识资源，避免以新生成的地址作为唯一 ID。
- 搜索阶段最多 10 秒，总处理时限为 28 秒，单个请求最多 12 秒，单次响应不超过 2 MiB。部分线路或资源失败时保留其他成功结果；全部失败会返回错误。
- 成功转存结果缓存 30 分钟，上限 256 项；另沿用框架的搜索缓存。`ext.refresh=true` 绕过两层缓存。缓存时长不代表链接有效期，不按“五分钟”提示强制丢弃分享。
- 遇到 `42901 / CHALLENGE_REQUIRED`、HTTP 403 或 HTTP 429 时，该实例暂停上游请求 1 分钟；不自动重试验证码，不把失败结果写入转存缓存。

## 验证

`testdata/` 保存脱敏的 SSE 和每日更新样本。离线测试覆盖两条解析链路、JSON 内的 URL 编码、直链、提取码、去重、并发上限、缓存刷新、限流及部分失败。

```bash
go test -race ./plugin/buerchen
BUERCHEN_LIVE_TEST=1 go test ./plugin/buerchen -run TestLiveSearch -count=1 -v
```

真实站点验证每条搜索线路最多尝试三项，找到一条可用分享后停止，另解析一条每日更新的百度分享。默认测试不访问外网。
