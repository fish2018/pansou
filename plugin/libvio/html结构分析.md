# libvio插件HTML结构分析

> **2026-10 入口变更**：主域名现为 `https://libvio.host`（`www.libvio.mov` 已失效）。
> 站点在入口套了一层 **CDN 浏览器验证**：纯 HTTP 客户端一律 403，响应头带
> `x-cdn-challenge: required`，正文是一段浏览器端 SHA-256 前导零挑战。插件需解出
> nonce、提交后拿到 `__cdn_verified` cookie（Max-Age 1800）才能访问搜索/详情/播放三级页面，
> 实现见 `libvio.go` 中 `solveCDNChallenge` / `solvePowNonce` / `cdnGuard`。
> 下文页面结构经实测（携带该 cookie 后）仍然适用。

## 网站信息
- 网站名称：LIBVIO
- 主域名：https://libvio.host
- 网站类型：影视资源在线播放/下载网站
- 特点：提供网盘下载链接（夸克、UC等）

## 访问流程说明
该网站需要通过三步来获取网盘链接：
1. **搜索页面** → 获取详情页链接
2. **详情页面** → 获取网盘下载页链接（注意选择"下载"而非"播放"）
3. **播放页面** → 从JavaScript对象中提取网盘URL

## 1. 搜索页面结构

### 搜索URL格式
```
https://www.libvio.mov/search/-------------.html?wd={关键词}&submit=
```

### 搜索结果容器
- 主容器：`<ul class="stui-vodlist clearfix">`
- 单个结果项：`<li class="col-md-6 col-sm-4 col-xs-3">`

### 搜索结果项结构
```html
<li class="col-md-6 col-sm-4 col-xs-3">
    <div class="stui-vodlist__box">
        <a class="stui-vodlist__thumb lazyload" href="/detail/4095.html" title="瑞克和莫蒂 第五季" 
           data-original="https://xxx.jpg">
            <span class="play hidden-xs"></span>
            <span class="pic-text text-right">10集全</span>
            <span class="pic-tag pic-tag-top">9.6</span>
        </a>
        <div class="stui-vodlist__detail">
            <h4 class="title text-overflow">
                <a href="/detail/4095.html" title="瑞克和莫蒂 第五季">瑞克和莫蒂 第五季</a>
            </h4>
        </div>
    </div>
</li>
```

### 字段提取要点
- **标题**：`.stui-vodlist__detail h4 a` 的文本内容或 `title` 属性
- **详情页链接**：`.stui-vodlist__thumb` 或 `.stui-vodlist__detail h4 a` 的 `href` 属性
- **封面图片**：`.stui-vodlist__thumb` 的 `data-original` 属性
- **集数信息**：`.pic-text` 的文本内容
- **评分**：`.pic-tag` 的文本内容

## 2. 详情页面结构

### 详情页URL格式
```
https://www.libvio.mov/detail/{id}.html
```

### 下载链接容器
详情页包含多个播放/下载源，我们需要查找带有"下载"关键字的源：

```html
<div class="stui-vodlist__head">
    <div class="stui-pannel__head clearfix">
        <span class="more text-muted pull-right"></span>
        <h3 class="iconfont icon-iconfontplay2">视频下载(UC) </h3>
    </div>
    <ul class="stui-content__playlist clearfix">
        <li>
            <a href="/play/714892571-2-1.html">合集</a>
        </li>
    </ul>
</div>

<div class="stui-vodlist__head">
    <div class="stui-pannel__head clearfix">
        <span class="more text-muted pull-right"></span>
        <h3 class="iconfont icon-iconfontplay2">视频下载 (夸克) </h3>
    </div>
    <ul class="stui-content__playlist clearfix">
        <li>
            <a href="/play/714892571-1-1.html">合集</a>
        </li>
    </ul>
</div>
```

### 字段提取要点
- **下载源标题**：`h3` 标签内容，需要包含"下载"关键字
- **播放页链接**：`.stui-content__playlist li a` 的 `href` 属性
- **网盘类型识别**：从标题中提取（如"UC"、"夸克"等）

### 筛选规则
- 只提取标题包含"下载"的播放源
- 忽略标题只有"播放"的源（这些通常是在线播放链接）

## 3. 播放页面结构（获取网盘链接）

### 播放页URL格式
```
https://www.libvio.mov/play/{id}-{sid}-{nid}.html
```
- `id`：影片ID
- `sid`：播放源ID
- `nid`：集数ID

### 网盘链接提取方式
网盘链接存储在页面内的JavaScript对象中：

```javascript
<script type="text/javascript">
    var player_aaaa = {
        "flag": "play",
        "encrypt": 3,
        "trysee": 10,
        "points": 0,
        "link": "/play/714892571-1-1.html",
        "link_next": "",
        "link_pre": "",
        "url": "https://drive.uc.cn/s/132a6339c94d4?public=1",
        "url_next": "",
        "from": "uc",
        "server": "no",
        "note": "",
        "id": "714892571",
        "sid": 2,
        "nid": 1
    }
</script>
```

### 字段提取要点
- **网盘链接**：`player_aaaa.url` 字段
- **网盘类型**：`player_aaaa.from` 字段（如 "uc"、"quark" 等）
- **集数信息**：`player_aaaa.nid` 字段

### 提取方法
1. 使用正则表达式匹配 `var player_aaaa = {...}` 内容
2. 解析JSON对象
3. 提取 `url` 字段即为网盘链接

## 4. 支持的网盘类型

根据HTML结构分析，网站主要支持：
- UC网盘（drive.uc.cn）
- 夸克网盘（pan.quark.cn）

## 5. 特殊处理事项

### JavaScript对象解析
- 需要处理转义字符（如 `\/` → `/`）
- 注意JSON对象的格式可能不标准，需要容错处理

### 多集处理
- 电视剧/动漫可能有多集，每集有独立的播放页链接
- 需要处理"合集"类型的资源（通常包含整季资源）

### 错误处理
- 需要处理播放页没有player_aaaa对象的情况
- 需要处理URL字段为空的情况
- 需要处理网站改版导致的结构变化

## 6. 实现建议

### 搜索流程
1. 发送搜索请求，解析HTML获取搜索结果
2. 提取每个结果的详情页链接

### 详情页处理
1. 访问详情页，查找所有播放源
2. 筛选出标题包含"下载"的源
3. 提取对应的播放页链接

### 网盘链接提取
1. 访问播放页
2. 使用正则表达式提取player_aaaa对象
3. 解析JSON获取网盘URL
4. 根据from字段确定网盘类型

### 并发优化
- 可以对多个详情页访问进行并发
- 可以对多个播放页访问进行并发
- 使用缓存避免重复请求

### 链接类型映射
```
from字段 → 网盘类型
"uc" → "UC网盘"
"quark" → "夸克网盘"
```