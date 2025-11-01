# Zhizhen HTML 数据结构分析

## 基本信息
- **数据源类型**: HTML 网页
- **搜索URL格式**: `https://xiaomi666.fun/index.php/vod/search/wd/{关键词}.html`
- **详情URL格式**: `https://xiaomi666.fun/index.php/vod/detail/id/{资源ID}.html`
- **数据特点**: 视频点播(VOD)系统网页，提供HTML格式的影视资源数据
- **特殊说明**: 使用独立域名，HTML结构与muou插件相同

## HTML 页面结构

### 搜索结果页面 (`.module-search-item`)
搜索结果页面包含多个搜索项，每个搜索项的HTML结构如下：

```html
<div class="module-search-item">
    <div class="module-item-pic">
        <img data-src="https://..." />
    </div>
    <div class="video-info-header">
        <h3>
            <a href="/index.php/vod/detail/id/12345.html">资源标题</a>
        </h3>
    </div>
    <div class="video-serial">更新至11集</div>
    <div class="video-info-aux">
        <span class="tag-link">
            <a>分类1</a>
            <a>分类2</a>
        </span>
    </div>
    <div class="video-info-items">
        <div>
            <span class="video-info-itemtitle">导演：</span>
            <a class="video-info-actor">导演名</a>
        </div>
        <div>
            <span class="video-info-itemtitle">主演：</span>
            <a class="video-info-actor">演员1</a>
            <a class="video-info-actor">演员2</a>
        </div>
        <div>
            <span class="video-info-itemtitle">剧情：</span>
            <span class="video-info-item">剧情简介内容</span>
        </div>
    </div>
</div>
```

### 详情页面 (`.module-row-one`)
详情页面包含下载链接区域，每个链接的HTML结构如下：

```html
<div id="download-list">
    <div class="module-row-one">
        <button data-clipboard-text="https://pan.quark.cn/s/xxx">复制链接</button>
        <a href="https://pan.quark.cn/s/xxx">打开链接</a>
    </div>
</div>
```

## 插件所需字段映射

| 源字段 | 目标字段 | 说明 |
|--------|----------|------|
| 详情页URL中的ID | `UniqueID` | 格式: `zhizhen-{id}` |
| `.video-info-header h3 a` 文本 | `Title` | 资源标题 |
| 质量、导演、主演、剧情 | `Content` | 组合描述信息 |
| `.video-info-aux .tag-link a` | `Tags` | 标签数组 |
| 详情页 `#download-list` 中的链接 | `Links` | 解析为Link数组 |
| `.module-item-pic > img` 的 `data-src` | `Images` | 封面图片 |
| `""` | `Channel` | 插件搜索结果Channel为空 |
| `time.Time{}` | `Datetime` | 使用零值 |

## 下载链接解析

### 链接提取方式
- **从 `data-clipboard-text` 属性**: 优先从按钮的 `data-clipboard-text` 属性提取链接
- **从 `href` 属性**: 如果没有 `data-clipboard-text`，则从 `<a>` 标签的 `href` 属性提取
- **去重处理**: 避免重复添加相同的链接

### 链接类型识别
通过正则表达式匹配URL来自动识别网盘类型，支持16种网盘类型：

```go
// 主流网盘
quark:      https://pan.quark.cn/s/...
baidu:      https://pan.baidu.com/s/...?pwd=...
aliyun:     https://aliyundrive.com/s/... 或 https://www.alipan.com/s/...
uc:         https://drive.uc.cn/s/...
xunlei:     https://pan.xunlei.com/s/...

// 运营商网盘
tianyi:     https://cloud.189.cn/t/...
mobile:     https://caiyun.feixin.10086.cn/...

// 专业网盘
115:        https://115.com/s/...
weiyun:     https://share.weiyun.com/...
lanzou:     https://lanzou.com/... 或其他变体
jianguoyun: https://jianguoyun.com/p/...
123:        https://123pan.com/s/...
pikpak:     https://mypikpak.com/s/...

// 其他协议
magnet:     magnet:?xt=urn:btih:...
ed2k:       ed2k://|file|...|
```

### 密码提取
从URL中提取 `?pwd=` 参数作为密码，例如：
```
https://pan.baidu.com/s/1kOWHnazfGFe6wJ-tin2pNQ?pwd=b2s4
提取密码: b2s4
```

## 支持的网盘类型（16种）

### 主流网盘
- **baidu (百度网盘)**: `https://pan.baidu.com/s/{分享码}?pwd={密码}`
- **quark (夸克网盘)**: `https://pan.quark.cn/s/{分享码}`
- **aliyun (阿里云盘)**: `https://aliyundrive.com/s/{分享码}`, `https://www.alipan.com/s/{分享码}`
- **uc (UC网盘)**: `https://drive.uc.cn/s/{分享码}`
- **xunlei (迅雷网盘)**: `https://pan.xunlei.com/s/{分享码}`

### 运营商网盘
- **tianyi (天翼云盘)**: `https://cloud.189.cn/t/{分享码}`
- **mobile (移动网盘)**: `https://caiyun.feixin.10086.cn/{分享码}`

### 专业网盘
- **115 (115网盘)**: `https://115.com/s/{分享码}`
- **weiyun (微云)**: `https://share.weiyun.com/{分享码}`
- **lanzou (蓝奏云)**: `https://lanzou.com/{分享码}`
- **jianguoyun (坚果云)**: `https://jianguoyun.com/{分享码}`
- **123 (123网盘)**: `https://123pan.com/s/{分享码}`
- **pikpak (PikPak)**: `https://mypikpak.com/s/{分享码}`

### 其他协议
- **magnet (磁力链接)**: `magnet:?xt=urn:btih:{hash}`
- **ed2k (电驴链接)**: `ed2k://|file|{filename}|{size}|{hash}|/`
- **others (其他类型)**: 其他不在上述分类中的链接

## 插件开发指导

### 搜索请求示例
```go
searchURL := fmt.Sprintf("https://xiaomi666.fun/index.php/vod/search/wd/%s.html", url.QueryEscape(keyword))
```

### 详情页请求示例
```go
detailURL := fmt.Sprintf("https://xiaomi666.fun/index.php/vod/detail/id/%s.html", itemID)
```

### HTML解析流程
1. **搜索页面解析**: 使用 goquery 解析搜索结果页面
2. **提取搜索项**: 遍历 `.module-search-item` 元素
3. **提取基本信息**: 从搜索项中提取标题、分类、导演、主演等
4. **异步获取详情**: 并发请求详情页面获取下载链接
5. **缓存管理**: 使用 sync.Map 缓存详情页结果，TTL为1小时

### SearchResult构建示例
```go
result := model.SearchResult{
    UniqueID: fmt.Sprintf("zhizhen-%s", itemID),
    Title:    title,
    Content:  strings.Join(contentParts, "\n"),
    Links:    detailLinks,
    Tags:     tags,
    Images:   images,
    Channel:  "", // 插件搜索结果Channel为空
    Datetime: time.Time{}, // 使用零值
}
```

### 并发控制
- **最大并发数**: 20 (MaxConcurrency)
- **搜索超时**: 8秒 (DefaultTimeout)
- **详情页超时**: 6秒 (DetailTimeout)
- **缓存TTL**: 1小时 (cacheTTL)

## 与其他插件的差异

| 特性 | zhizhen | muou | 说明 |
|------|---------|------|------|
| **域名** | `xiaomi666.fun` | `666.666291.xyz` | 不同域名 |
| **数据格式** | HTML | HTML | 都是HTML格式 |
| **HTML结构** | 相同 | 相同 | 使用相同的CSS选择器 |
| **并发数** | 20 | 20 | 相同 |
| **缓存TTL** | 1小时 | 1小时 | 相同 |

## 注意事项
1. **HTML解析**: 使用 goquery 库进行HTML解析
2. **异步获取详情**: 搜索结果只包含基本信息，需要异步请求详情页获取下载链接
3. **并发控制**: 使用信号量限制并发数为20
4. **缓存管理**: 使用 sync.Map 缓存详情页结果，避免重复请求
5. **链接验证**: 过滤掉无效链接（如包含`javascript:`、`#`等）
6. **密码提取**: 从URL中提取 `?pwd=` 参数作为密码
7. **去重处理**: 避免在详情页中重复添加相同的链接

## 开发建议
- **参考muou插件**: zhizhen的HTML结构与muou完全相同，可以直接参考muou的实现
- **关键差异**: 仅需修改域名和插件名称
- **测试覆盖**: 重点测试多种网盘类型的链接解析和缓存功能
- **性能优化**: 使用并发请求详情页，提高搜索速度