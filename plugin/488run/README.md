# 488run（夸搜盘）

站点：<https://488.run>（备用域名 `b46.cn`）。基于 `wechat-cinehunt` 主题的聚合网盘搜索站，同时聚合 TG 频道（`tg`）、全网检索（`web`）、站内资源（`resource`）和用户提交（`submission`），搜索结果条目中直接携带各网盘分享直链与提取码。

2026-09-27 实测《甄嬛传》《凡人修仙传》单次搜索可返回 50~99 条有效网盘链接，覆盖夸克、百度、UC、迅雷等网盘类型。

## 使用

插件名为 `488run`，优先级为 2，保留 Service 层关键词过滤。已加入主程序注册和 Docker 默认插件列表。已有部署需要在自己的 `ENABLED_PLUGINS` 列表中加入 `488run`，然后用包含此插件的新构建重启。

```bash
curl --get 'http://127.0.0.1:8888/api/search' \
  --data-urlencode 'kw=甄嬛传' \
  --data-urlencode 'src=plugin' \
  --data-urlencode 'plugins=488run'
```

## 解析方式

- **会话 Cookie 预热与缓存**：站点接口需要携带首页下发的 `yd_frontend`（`HttpOnly`，有效期 12 小时）。插件在服务启动后后台预热并缓存 6 小时，使用 `singleflight` 合并并发获取请求；遇到 `401/403`（非频控）时自动刷新一次 Cookie 并重试。
- **两阶段搜索与增量轮询**：
  - 首轮请求 `/api/frontend/search?q=<关键词>&limit=24&theme=wechat-cinehunt` 获取初始结果及 `search_id`、`complete` 状态。
  - 若 `complete=false`，继续轮询 `/api/frontend/search/poll?id=<search_id>&limit=24&theme=wechat-cinehunt`。该接口为服务端增量消费队列（每次仅返回上次轮询之后的新增条目），插件对初始批次与历次轮询结果按唯一键去重合并，直至 `complete=true` 或到达 `plugin.PublishBudget()` 发布预算上限。
- **频控冷却保护**：避免触发前端页面自动发起的 `/api/frontend/resource-check` 并发风暴；若接口返回 `429` 或 `403 访问过于频繁`，插件自动进入 20 秒冷却期。
- **链接与标题清洗**：直接从条目 `share_link` / `link` / `url` / `magnet` 及描述文本中提取网盘链接，截断非 ASCII 尾随粘连字符（如 `?pwd=6666提取码:6666`），清理标题末尾无意义的空提取码后缀（如 `提取码： 26天前`），并补齐 `WorkTitle`、封面图与分类标签。

## 验证

离线单元测试使用 `httptest.Server` 模拟完整 Cookie 下发、多轮 `poll` 增量合并与会话过期重签，常规测试不访问外网。

```bash
go test ./plugin/488run
RUN_LIVE_488RUN_TEST=1 go test ./plugin/488run -run TestLive488RunSearch -count=1 -v
```
