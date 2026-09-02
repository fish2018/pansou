# DIDUAN（低端影视）插件HTML结构分析

## 网站概述
- **网站名称**: 低端影视
- **域名**: https://ddys.io/
- **类型**: 影视资源网站，提供在线播放和网盘下载链接

## API流程概述

### 搜索页面
- **请求URL**: `https://ddys.io/?s={keyword}&post_type=post`
- **方法**: GET
- **Headers**: 标准浏览器请求头
- **特点**: WordPress网站，使用标准搜索功能

## 搜索结果结构

### 搜索结果页面HTML结构
```html
<main id="main" class="site-main col-md-8" role="main">
    <article id="post-1404" class="post-1404 post type-post status-publish ...">
        <div class="row">
            <div class="post-content col-md-12">
                <header class="entry-header">
                    <h2 class="post-title">
                        <a href="https://ddys.io/deadpool/" rel="bookmark">死侍 1-3</a>
                    </h2>
                </header>
                
                <div class="entry-content">
                    <p>注：本片不适合公共场合观看</p>
                </div>
                
                <footer class="entry-footer">
                    <div class="metadata">
                        <ul>
                            <li class="meta_date">
                                <time class="entry-date published" datetime="2018-08-08T01:41:40+08:00">
                                    2018年8月8日
                                </time>
                            </li>
                            <li class="meta_categories">
                                <span class="cat-links">
                                    <a href="..." rel="category tag">欧美电影</a>
                                </span>
                            </li>
                        </ul>
                    </div>
                </footer>
            </div>
        </div>
    </article>
</main>
```

### 详情页面HTML结构
```html
<main id="main" class="site-main" role="main">
    <article id="post-19840" class="...">
        <div class="post-content">
            <h1 class="post-title">变形金刚 超能勇士崛起</h1>
            
            <div class="metadata">
                <ul>
                    <li class="meta_date">
                        <time class="entry-date published updated" 
                              datetime="2023-07-13T14:27:08+08:00">
                            2023年7月13日
                        </time>
                    </li>
                    <li class="meta_categories">
                        <span class="cat-links">
                            <a href="..." rel="category tag">欧美电影</a>
                        </span>
                    </li>
                    <li class="meta_tags">
                        <span class="tags-links">
                            标签：<a href="..." rel="tag">动作</a>
                        </span>
                    </li>
                </ul>
            </div>
            
            <div class="entry">
                <!-- 播放器相关内容 -->
                
                <!-- 网盘下载链接 -->
                <p>视频下载 (夸克网盘)： 
                    <a href="https://pan.quark.cn/s/a372a91a0296" 
                       rel="noopener nofollow" target="_blank">
                        https://pan.quark.cn/s/a372a91a0296
                    </a>
                </p>
                
                <!-- 豆瓣信息区块 -->
                <div class="doulist-item">
                    <div class="mod">
                        <div class="v-overflowHidden doulist-subject">
                            <div class="post">
                                <img src="douban_cache/xxx.jpg">
                            </div>
                            <div class="title">
                                <a href="https://movie.douban.com/subject/..." 
                                   class="cute" target="_blank">
                                    影片名称 英文名
                                </a>
                            </div>
                            <div class="rating">
                                <span class="rating_nums">5.8</span>
                            </div>
                            <div class="abstract">
                                <!-- 详细信息：又名、导演、演员、类型等 -->
                            </div>
                        </div>
                    </div>
                </div>
            </div>
        </div>
    </article>
</main>
```

## 数据提取要点

### 搜索结果页面
1. **结果列表**: `article[class^="post-"]` - 每个搜索结果
2. **文章ID**: 从article的class或id属性提取，如 `post-1404`
3. **标题**: `.post-title > a` - 获取文本和href属性
4. **链接**: `.post-title > a[href]` - 详情页链接
5. **发布时间**: `.meta_date > time.entry-date[datetime]` - ISO格式时间
6. **分类**: `.meta_categories > .cat-links > a` - 分类信息
7. **简介**: `.entry-content` - 内容简介（可能为空）

### 详情页面  
1. **标题**: `h1.post-title` - 影片标题
2. **发布时间**: `.meta_date > time.entry-date[datetime]` - 发布时间
3. **分类标签**: `.meta_categories`和`.meta_tags`中的链接
4. **网盘链接提取**: 
   - 模式1: `(网盘名)：<a href="链接">链接文本</a>`
   - 模式2: `(网盘名) <a href="链接">链接文本</a>`
   - 常见网盘: 夸克网盘、百度网盘、阿里云盘、天翼云盘等
5. **豆瓣信息**: `.doulist-item`区块（可选）

## 网盘链接识别规则

### 支持的网盘类型
- **夸克网盘**: `pan.quark.cn`
- **百度网盘**: `pan.baidu.com`  
- **阿里云盘**: `aliyundrive.com` / `alipan.com`
- **天翼云盘**: `cloud.189.cn`
- **迅雷网盘**: `pan.xunlei.com`
- **115网盘**: `115.com`
- **蓝奏云**: `lanzou`相关域名

### 链接提取策略
1. 在详情页的`.entry`内容区域搜索
2. 使用正则表达式匹配网盘链接模式
3. 提取网盘类型、链接和可能的提取码
4. 链接去重和验证

## 特殊处理

### 时间解析
- 格式: ISO 8601格式 `2023-07-13T14:27:08+08:00`
- 显示: `2023年7月13日`

### 内容清理
- 移除HTML标签
- 处理特殊字符和编码
- 清理多余空格和换行

### 错误处理
- 网络超时重试
- 解析失败的降级处理
- 空结果的处理

## 注意事项

1. **反爬虫**: 网站可能有基础的反爬虫措施，需要设置合理的请求头
2. **限频**: 避免请求过于频繁
3. **编码**: 处理中文关键词的URL编码
4. **更新**: 网站结构可能会变化，需要定期维护选择器
