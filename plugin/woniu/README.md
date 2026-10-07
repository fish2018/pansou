# 蜗牛搜索插件

## 📖 简介

蜗牛是 PanSou 的搜索插件，用于抓取「蜗牛 I 4K」（默认站点 `https://wn4k.com`）的影视资源网盘链接。站点地址可在管理页里修改，登录、搜索、详情请求都基于当前 `base_url`。

## ✨ 核心特性

- ✅ **登录后抓取** - 站点详情页的网盘链接仅登录可见，插件带会话抓取
- ✅ **自定义域名** - 支持在管理页配置站点地址，换域名不必改代码
- ✅ **多账号支持** - 每个账号独立配置，搜索时优先使用最近活跃的账号
- ✅ **持久化存储** - 会话 Cookie 落盘（权限 0600），重启不丢失
- ✅ **Web 管理界面** - 一站式配置，简单易用
- ✅ **后台保活校验** - 每小时抽查会话有效性，失效在管理页直接可见
- ✅ **发布预算内交付** - 详情页并发抓取带截止时间，到点先交出已有结果

## 🚀 快速开始

### 步骤1: 启用插件

```bash
ENABLED_PLUGINS=woniu ASYNC_PLUGIN_ENABLED=true go run main.go
```

### 步骤2: 打开管理页

```
http://localhost:8888/woniu/你的账号标识
```

**示例**：

```
http://localhost:8888/woniu/pansou
```

系统会根据账号标识生成专属 64 位 hash，并重定向到：

```
http://localhost:8888/woniu/{hash}
```

**📌 提示**：请收藏 hash 后的 URL，方便下次访问。

### 步骤3: 登录

在「登录状态」区域输入蜗牛站点的用户名和密码，点击**登录**。

登录成功后管理页会显示用户名、登录时间与剩余有效期。插件会把会话 Cookie 保存到
`cache/woniu_users/{hash}.json`。

### 步骤4: 测试搜索

在「测试搜索」区域输入关键词（例如「凡人修仙传」），确认能抓到网盘链接。

## ⚠️ 为什么必须登录

站点对游客只放出「锁定预览」：影片标题和简介可见，链接位置一律替换成
`https://******（登录后可见）`。因此：

- 搜索页游客可访问，但**详情页必须登录**才有真实链接
- 插件在没有任何可用账号时**明确报错**，而不是返回一堆假链接或空结果

对应的错误信息会直接写进日志，便于定位：

```
[woniu] 未配置可用账号：站点详情页需登录才能查看网盘链接，请在 /woniu/{用户名} 登录后再试
```

## 🔗 链接解析

站点详情页按网盘类型分组（实测出现 `115离线下载`、`百度资源` 等），每组下是若干条分享链接。
插件从 `data-pan-item` 属性、`pan-link-meta` 文本、"打开"按钮的 `href` 与 `data-copy` 属性
四处取值并去重，因此站点小改一处不会导致整批抓不到。

支持的网盘类型：`115`、`baidu`、`quark`、`uc`、`aliyun`、`guangya`、`xunlei`、`tianyi`、
`123`、`mobile`、`pikpak`、`weiyun`、`lanzou`、`magnet`、`ed2k`，无法识别的站外链接标为 `others`。

### work_title 为什么要带上影片名

Service 层合并链接时，只要 `WorkTitle` 非空就只拿它做关键词过滤，不再回退到结果标题。
而蜗牛的「资源名」经常只是规格描述——实测「庆余年」的资源名是
`S02 4K WEB-DL DV HiveWeb-173.26GB`，一个「庆余年」都没有。若原样写进 `WorkTitle`，
两条链接会被关键词过滤整批丢掉，表现为 **`results` 里有结果、`merged_by_type` 却是空的**。

所以插件用 `composeWorkTitle` 把影片名和资源名拼起来（与 `gying` 的
`buildLinkWorkTitle` 同一语义），既保住关键词过滤，也保留了区分同影片多条线路的能力。

## 🧩 管理 API

所有操作都走 `POST /woniu/{hash}`，请求体带 `action` 字段。

| action | 说明 | 请求体 |
| --- | --- | --- |
| `get_status` | 查询登录状态 | `{"action":"get_status"}` |
| `login` | 用户名密码登录 | `{"action":"login","username":"xxx","password":"xxx"}` |
| `logout` | 退出登录并删除本地会话 | `{"action":"logout"}` |
| `get_config` | 查询站点地址 | `{"action":"get_config"}` |
| `update_config` | 修改站点地址 | `{"action":"update_config","base_url":"https://wn4k.com"}` |
| `test_search` | 用当前会话测试搜索 | `{"action":"test_search","keyword":"凡人修仙传"}` |

示例：

```bash
curl -X POST http://localhost:8888/woniu/{hash} \
  -H "Content-Type: application/json" \
  -d '{"action":"test_search","keyword":"凡人修仙传"}'
```

## ⚙️ 配置项

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CACHE_PATH` | `./cache` | 会话存储根目录，实际写入 `{CACHE_PATH}/woniu_users/` |
| `WONIU_HASH_SALT` | `pansou_woniu_secret_2026` | 账号标识 → hash 的加盐值，部署方应自行设置 |

站点地址不走环境变量，而是在管理页里配置，保存在 `cache/woniu_users/woniu_config.json`。
修改站点地址会**作废所有已保存的会话**（旧 Cookie 对新站无效），需要重新登录。

## 🧪 测试

```bash
# 单元测试（默认运行，使用本地假站点，不访问网络）
go test ./plugin/woniu/

# 真实站点联调（默认跳过）
WONIU_LIVE=1 WONIU_LIVE_USER=<账号> WONIU_LIVE_PASS=<密码> \
  go test ./plugin/woniu/ -run TestLiveWoniu -v
```

联调测试会验证登录、会话有效性、完整搜索链路、链接有效性，并断言搜索耗时落在发布预算内。

## 📌 已知边界

- **站点搜索页不提供翻页**：实测 `/vodsearch/-------------/page/2.html` 返回空页，
  一页最多 24 条，插件只取首页。
- **详情页必须登录**：见上文。
- **标题不含关键词的影片会被 Service 层过滤**：这是框架既有语义。例如搜索「三体」时，
  影片《北海》因剧情提及三体而命中搜索页，但其标题不含「三体」，其链接会被关键词过滤丢弃。
