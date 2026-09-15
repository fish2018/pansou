# hjzhencai（花卷资源）

站点：<https://www.hjzhencai.top>。这是提供网盘分享的影视资源站，详情页的“选集下载”直接给出分享链接，无需进入播放器。

2026-09-15 实测《遮天》提供夸克、百度网盘，《流浪地球2》另有天翼云盘。插件只提取下载区中的网盘分享，不返回站内播放地址。

## 使用

插件名为 `hjzhencai`，优先级为 3，保留 Service 层关键词过滤。已加入主程序注册和 Docker 默认插件列表。已有部署需在自己的 `ENABLED_PLUGINS` 列表中加入 `hjzhencai`，然后使用包含此插件的新构建重启。

```bash
curl --get 'http://127.0.0.1:8888/api/search' \
  --data-urlencode 'kw=流浪地球' \
  --data-urlencode 'src=plugin' \
  --data-urlencode 'plugins=hjzhencai'
```

## 解析方式

- 搜索入口为 `/index.php/vod/search.html?wd=关键词`，从 `.module-card-item-title a` 提取详情页和稳定的影片 ID。
- 只跟随分页中标题为“下一页”的链接；“尾页”也带 `page-next` 样式，不能据此直接跳转。
- 每次最多搜索 3 页、处理 48 个详情页，插件实例共享 3 个详情并发名额，总处理超时 25 秒。
- 从 `.down-wrap .down-card` 提取链接。相同下载项中的地址和下载按钮会去重；URL 查询参数中的提取码优先，其次读取同一卡片的文字，天翼的 `code` 是分享 ID，不当成提取码。
- 返回标题、简介、分类、封面和站点更新时间。每条链接带 `work_title`，同线路有多个下载项时保留集数标签；单个整部分享的默认“第1集”标签省略。
- 成功详情缓存 30 分钟，上限 512 条，`ext.refresh=true` 绕过详情缓存。HTTP 错误、验证页面和没有网盘链接的详情不进入缓存。
- 链接按分享域名和路径验证；实测覆盖夸克、百度、天翼，兼容常见阿里、UC、迅雷、115、123、光鸭的 `/s/` 分享格式。

## 验证

离线样本保存在 `testdata/`，常规测试不访问外网。

```bash
go test ./plugin/hjzhencai
HJZHENCAI_LIVE_TEST=1 go test ./plugin/hjzhencai -run TestLiveSearch -count=1 -v
```
