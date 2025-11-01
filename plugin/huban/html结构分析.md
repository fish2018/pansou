# Huban HTML 数据结构分析

## 基本信息
- **数据源类型**: HTML 网页
- **搜索URL格式**: `http://xsayang.fun:12512/index.php/vod/search/wd/{关键词}.html`
- **详情URL格式**: `http://xsayang.fun:12512/index.php/vod/detail/id/{资源ID}.html`
- **数据特点**: 视频点播(VOD)系统网页，提供HTML格式的影视资源数据
- **特殊说明**: 使用HTML解析替代JSON API，与erxiao/zhizhen/muou插件使用相同的HTML结构

## HTML 页面结构

### 搜索结果页面 (`.module-search-item`)
搜索结果页面包含多个搜索项，每个搜索项的HTML结构如下：

```html
<div class="module-search-item">
  <div class="module-item-pic">
    <img data-src="https://..." />
  </div>
  <div class="module-item-text">
    <div class="video-info-header">
      <h3><a href="/index.php/vod/detail/id/12345.html">电影标题</a></h3>
      <span class="video-info-remarks">HD</span>
    </div>
    <div class="video-info-items">
      <div class="video-info-item">
        <span class="video-info-itemtitle">分类：</span>
        <span class="video-info-item">动作</span>
      </div>
      <div class="video-info-item">
        <span class="video-info-itemtitle">导演：</span>
        <span class="video-info-item">导演名字</span>
      </div>
      <div class="video-info-item">
        <span class="video-info-itemtitle">主演：</span>
        <span class="video-info-item">演员1,演员2</span>
      </div>
      <div class="video-info-item">
        <span class="video-info-itemtitle">年份：</span>
        <span class="video-info-item">2024</span>
      </div>
      <div class="video-info-item">
        <span class="video-info-itemtitle">剧情：</span>
        <span class="video-info-item">这是一部精彩的电影...</span>
      </div>
    </div>
  </div>
</div>
```

### 详情页面 (`.mobile-play` 和 `#download-list`)
详情页面包含海报图片和下载链接：

```html
<div class="mobile-play">
  <img class="lazyload" data-src="https://poster-url.jpg" />
</div>

<div id="download-list">
  <div class="module-row-one">
    <div class="module-row-text">
      <span data-clipboard-text="https://pan.quark.cn/s/xxxxx">夸克网盘</span>
    </div>
  </div>
  <div class="module-row-one">
    <div class="module-row-text">
      <span data-clipboard-text="https://pan.baidu.com/s/xxxxx?pwd=xxxx">百度网盘</span>
    </div>
  </div>
</div>
```

## CSS 选择器参考

### 搜索结果提取
- **搜索结果容器**: `.module-search-item`
- **标题**: `.video-info-header h3 a` (文本内容)
- **详情页链接**: `.video-info-header h3 a` (href属性)
- **封面图片**: `.module-item-pic > img` (data-src属性)
- **质量/状态**: `.video-info-header .video-info-remarks` (文本内容)

### 详情页下载链接提取
- **海报图片**: `.mobile-play .lazyload` (data-src属性)
- **下载链接容器**: `#download-list .module-row-one`
- **下载链接**: `[data-clipboard-text]` (data-clipboard-text属性)

## 支持的网盘类型
- **Quark网盘**: `https://pan.quark.cn/s/{分享码}`
- **百度网盘**: `https://pan.baidu.com/s/{分享码}?pwd={密码}`
- **阿里云盘**: `https://www.aliyundrive.com/s/{分享码}`
- **迅雷网盘**: `https://pan.xunlei.com/s/{分享码}`
- **天翼云盘**: `https://cloud.189.cn/t/{分享码}`
- **UC网盘**: `https://drive.uc.cn/s/{分享码}`
- **115网盘**: `https://115.com/s/{分享码}`
- **123网盘**: `https://123pan.com/s/{分享码}`
- **PikPak**: `https://mypikpak.com/s/{分享码}`
- **移动云盘**: `https://caiyun.feixin.10086.cn/{分享码}`
- **磁力链接**: `magnet:?xt=urn:btih:{hash}`
- **ED2K链接**: `ed2k://|file|...`

## 数据流程

### 搜索流程
1. **构建搜索URL**: `http://xsayang.fun:12512/index.php/vod/search/wd/{keyword}.html`
2. **发送HTTP请求**: 获取搜索结果页面
3. **解析HTML**: 使用goquery解析页面
4. **提取搜索项**: 遍历`.module-search-item`元素
5. **异步获取详情**: 并发请求详情页面获取下载链接
6. **缓存管理**: 使用sync.Map缓存详情页结果，TTL为1小时
7. **关键词过滤**: 过滤不相关的结果

## 并发控制
- **最大并发数**: 20 (MaxConcurrency)
- **搜索超时**: 8秒 (DefaultTimeout)
- **详情页超时**: 6秒 (DetailTimeout)
- **缓存TTL**: 1小时 (cacheTTL)

## 性能统计
- **搜索请求数**: 总搜索请求数
- **平均搜索时间**: 单次搜索平均耗时(毫秒)
- **详情页请求数**: 总详情页请求数
- **平均详情页时间**: 单次详情页请求平均耗时(毫秒)
- **缓存命中数**: 详情页缓存命中次数
- **缓存未命中数**: 详情页缓存未命中次数

## 注意事项
1. **HTML解析**: 使用goquery库进行HTML解析
2. **异步获取详情**: 搜索结果只包含基本信息，需要异步请求详情页获取下载链接
3. **并发控制**: 使用信号量限制并发数为20
4. **缓存管理**: 使用sync.Map缓存详情页结果，避免重复请求
5. **链接验证**: 过滤掉无效链接（如包含`javascript:`、`#`等）
6. **密码提取**: 从URL中提取`?pwd=`参数作为密码

