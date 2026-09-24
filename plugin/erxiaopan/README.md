# erxiaopan（二小盘）

站点：<https://www.2xiaopan.one>。maccms + DYXS2 模板的影视资源站，详情页“影片下载”直接给出各网盘分享地址，不需要进入播放器。

2026-09-15 实测《狂飙》同时提供夸克、百度（含 `?pwd=` 提取码）和天翼云盘（访问码写在链接后面），《流浪地球2》为夸克多版本分享。

## 使用

插件名为 `erxiaopan`，优先级为 2，保留 Service 层关键词过滤。已加入主程序注册和 Docker 默认插件列表。已有部署需要在自己的 `ENABLED_PLUGINS` 列表中加入 `erxiaopan`，然后用包含此插件的新构建重启。

```bash
curl --get 'http://127.0.0.1:8888/api/search' \
  --data-urlencode 'kw=狂飙' \
  --data-urlencode 'src=plugin' \
  --data-urlencode 'plugins=erxiaopan'
```

## 解析方式

- 搜索入口为 `/index.php/vod/search/wd/关键词.html`，翻页为 `/index.php/vod/search/page/{n}/wd/关键词.html`，从 `.video-info-header h3 a` 提取干净标题与详情页 ID（`.module-item-pic` 里的标题带“立刻播放”前缀，不能用）。
- 只跟随分页中标题为“下一页”的链接；“尾页”也带 `page-next` 样式，不能据此跳转。
- 每次最多搜索 2 页、处理 20 个详情页，插件实例共享 5 个详情并发名额，整体超时 25 秒。
- 从 `#download-list .module-row-one` 提取分享地址，优先读 `data-clipboard-text`，缺失时回退下载按钮的 `href`。站内跳转（如 `/list/share`）和非法地址一律丢弃。
- 链接按分享域名与路径校验，实测覆盖夸克、百度、天翼；兼容阿里、UC、迅雷、115、123、光鸭、PikPak、移动云盘的同类分享格式，并保留磁力/ed2k 兜底。
- 提取码优先取 URL 参数（`?pwd=`），其次读链接旁文本（“提取码：”“访问码：”）。天翼把访问码直接拼在地址后面，插件会先截出合法 URL 再解析访问码。
- 返回标题、分类、年份、地区、导演、主演（最多 6 位）、剧情、封面与来源地址。站点会把多个地区或类别合成一个链接（如“美国 / 日本”“剧情 / 动作 / 冒险 / 运动”），插件按分隔符拆成独立标签。
- 每条链接带 `work_title`：同一详情页存在多种集数标签（如第 1 集、第 2 集）时追加集数，整部分享只有一种标签时不加后缀。
- 成功详情缓存 30 分钟，上限 512 条，`ext.refresh=true` 绕过详情缓存。HTTP 错误、结构异常和没有网盘链接的详情不进入缓存。

## 域名说明

该域名使用 share-dns 轮换（`www.2xiaopan.one` → `*.vipcf.3666888.xyz` → Cloudflare，CNAME 的 TTL 只有 1 秒），DNS 解析会间歇失败。插件对每个页面做 3 次带退避的重试，每次重试都会重新建连并重新解析；实测改前 1/3 成功率、改后连续通过。站点换域名时只需修改 `defaultBaseURL`。

## 验证

离线样本保存在 `testdata/`，常规测试不访问外网。

```bash
go test ./plugin/erxiaopan
ERXIAOPAN_LIVE_TEST=1 go test ./plugin/erxiaopan -run TestLiveSearch -count=1 -v
```