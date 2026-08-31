# 4KDY 页面结构说明

## 地址

- 站点：`https://4kdy.vip`
- 搜索：`/4K-search/-------------.html?wd={关键词}`
- 详情：`/4K-detail/{ID}.html`

旧的 `4kfox.com`、`btnull.pro` 以及旧版 `/search/`、`/video/` 路径均不再使用。

## 搜索页

新版搜索结果位于首个 `div.space-y-4` 容器中。每个结果包含：

- 详情链接：`a[href*='/4K-detail/']`
- 播放链接和状态：`a[href^='/4K-vodplay/']`
- 标题：详情链接文本
- 图片：卡片内 `img` 的 `data-original` 或 `src`
- 元数据：卡片内首个 `div.gap-x-3`

解析时限定在搜索结果容器中，避免误抓页头、推荐区和页脚里的详情链接。

## 详情页

- 标题：`h1.title-mobile`
- 封面：`.info-card-mobile img[data-original]`
- 简介：`.info-card-mobile [class*='leading-relaxed']`
- 下载项：`.download-item a[href]`

下载链接直接位于 `href` 属性中，不能只读取页面文本。当前页面可包含磁力、夸克、百度、UC、阿里、天翼和迅雷等链接。

插件仍保留部分旧模板选择器作为兼容回退。
