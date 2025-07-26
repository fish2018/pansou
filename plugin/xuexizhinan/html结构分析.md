# 4K指南 (xuexizhinan) 网站搜索结果HTML结构分析

## 搜索结果页面结构

搜索结果页面的主要内容位于`.content-wrap .content-layout .row`元素内，每个搜索结果项包含在`.url-card`元素中。

```html
<div class="row">
    <div class="url-card col-6 col-sm-4 col-md-3 col-lg-4 col-xl-5a">
        <div class="card-book list-item">
            <!-- 单个搜索结果 -->
        </div>
    </div>
    <!-- 更多搜索结果... -->
</div>
```

### 单个搜索结果结构

每个搜索结果包含以下主要元素：

#### 1. 电影封面

封面图片位于`.media-content`元素中：

```html
<div class="media media-5x7 p-0 rounded">
    <a class="media-content" href="https://xuexizhinan.com/book/17694.html" target="_blank" data-bg="url(https://img.alicdn.com/imgextra/i1/2355688876/O1CN017g1yTT2FRGYEB9T3H_!!2355688876.png)"></a>
</div>
```

封面图片URL可以从`data-bg`属性中提取。

#### 2. 详情页链接和ID

详情页链接和ID在多个位置出现，最明显的是在封面图片的链接中：

```html
<a class="media-content" href="https://xuexizhinan.com/book/17694.html" target="_blank"></a>
```

详情页链接格式为`https://xuexizhinan.com/book/{ID}.html`，其中`{ID}`是资源的唯一标识符（如`17694`）。

#### 3. 标题

标题在`.list-title`元素中：

```html
<a href="https://xuexizhinan.com/book/17694.html" target="_blank" class="list-title text-md overflowClip_1">
    变形金刚
</a>
```

#### 4. 资源类型/质量

资源类型或质量信息在`.list-subtitle`元素中：

```html
<div class="list-subtitle text-muted text-xs overflowClip_1">
    4K原盘
</div>
```

## 详情页面结构

详情页面包含更完整的资源信息，特别是网盘链接和磁力链接等下载信息。

### 1. 页面标题和元数据

页面标题包含了电影名称和资源类型：

```html
<title>变形金刚1 4K UHD蓝光原盘ISO夸克网盘下载 | 4K指南</title>
<meta name="keywords" content="4K蓝光原盘,1080P,高清电影下载,磁力链接,变形金刚1夸克网盘资源, 变形金刚1磁力链接" />
<meta name="description" content="提供修复资源+磁力链接。火种源激活的都市混战，点击获取4K资源！" />
```

### 2. 封面图片

封面图片位于`.book-cover`元素中：

```html
<div class="book-cover mb-3">
    <div class="text-center position-relative">
        <img class="rounded shadow" src="https://img.alicdn.com/imgextra/i1/2355688876/O1CN017g1yTT2FRGYEB9T3H_!!2355688876.png" alt="变形金刚" title="变形金刚" style="max-height: 350px;">
    </div>
</div>
```

### 3. 资源标题和分类

资源标题和分类标签位于`.book-header`元素中：

```html
<div class="book-header mt-3">
    <h1>变形金刚</h1>
    <div class="my-2">
        <span class="mr-2"><a href="https://xuexizhinan.com/books/dongzuo" rel="tag">动作</a><i class="iconfont icon-wailian text-ss"></i></span>
        <span class="mr-2"><a href="https://xuexizhinan.com/books/zuixin" rel="tag">影视</a><i class="iconfont icon-wailian text-ss"></i></span>
        <span class="mr-2"><a href="https://xuexizhinan.com/books/kehuan" rel="tag">科幻</a><i class="iconfont icon-wailian text-ss"></i></span>
        <!-- 更多标签 -->
    </div>
</div>
```

### 4. 下载链接区域

下载链接区域位于`.book-info`元素中，包含磁力链接和网盘链接：

```html
<div class="book-info mb-3">
    <div class="row main card">
        <div class="col-12 col-lg-9 left my-2">
            <p>4K原盘</p>
            <div>
                <ul>
                    <li class="my-2">
                        <span class="mr-3">磁力链接 :</span>magnet:?xt=urn:btih:17c69c04a26cf2f02a69fb722f973afdcdaf0db4&dn=Transformers.2007.2160p.BluRay.HEVC.TrueHD.7.1.Atmos-TASTED&tr=http%3A%2F%2Ftracker.trackerfix.com%3A80%2Fannounce&tr=udp%3A%2F%2F9.rarbg.me%3A2780&tr=udp%3A%2F%2F9.rarbg.to%3A2880
                    </li>
                </ul>
                <div class="site-go mt-3">
                    <a target="_blank" href="https://pan.quark.cn/s/54afc86c2ced" class="btn btn-arrow rounded-lg" title="夸克网盘">
                        <span class="b-name">夸克网盘</span>
                        <i class="iconfont icon-arrow-r"></i>
                    </a>
                </div>
            </div>
        </div>
    </div>
</div>
```

#### 4.1 磁力链接

磁力链接直接出现在`<li>`标签中，格式为`magnet:?xt=urn:btih:...`。

#### 4.2 网盘链接

网盘链接位于`.site-go`元素中，使用`<a>`标签：

```html
<a target="_blank" href="https://pan.quark.cn/s/54afc86c2ced" class="btn btn-arrow rounded-lg" title="夸克网盘">
    <span class="b-name">夸克网盘</span>
    <i class="iconfont icon-arrow-r"></i>
</a>
```

网盘链接的URL在`href`属性中，类型可以从`title`属性或`.b-name`元素中获取。

### 5. 资源详情

资源详细介绍位于`.panel-body.single`元素中：

```html
<div class="panel-body single mb-4 ">
    <p>&#8220;导演: 迈克尔·贝<br />
    编剧: 罗伯托·奥奇 / 艾里克斯·库兹曼 / 约翰·罗杰斯<br />
    主演: 希亚·拉博夫 / 梅根·福克斯 / 乔什·杜哈明 / 泰瑞斯·吉布森 / 瑞切尔·泰勒 / 更多...<br />
    类型: 动作 / 科幻<br />
    制片国家/地区: 美国<br />
    语言: 英语 / 西班牙语<br />
    上映日期: 2007-07-11(中国大陆) / 2007-07-03(美国)<br />
    片长: 144分钟<br />
    又名: 变形金刚电影版<br />
    IMDb: tt0418279&#8221;</p>
</div>
```

这部分通常包含了电影的详细信息，如导演、演员、类型、上映日期等。

## 提取逻辑

### 搜索结果页面提取逻辑

1. 定位所有的`.url-card`元素
2. 对于每个元素：
   - 提取详情页链接 (`href`属性)
   - 从链接中提取资源ID
   - 提取标题文本
   - 提取资源类型/质量
   - 记录封面图片URL

### 详情页面提取逻辑

1. 获取资源基本信息：
   - 标题（`h1`标签）
   - 分类标签（`.book-header .my-2 a`标签）
   - 封面图片（`.book-cover img`标签的`src`属性）

2. 提取下载链接：
   - 磁力链接：从`<li>`标签中提取以`magnet:`开头的文本
   - 网盘链接：从`.site-go a`标签的`href`属性提取
   - 网盘类型：从`.site-go a`标签的`title`属性或`.b-name`元素文本提取

3. 提取资源详情：
   - 从`.panel-body.single`元素中提取文本内容，包括导演、演员、类型等信息

## 注意事项

1. 搜索URL格式：`https://xuexizhinan.com/?post_type=book&s={关键词}`
2. 详情页URL格式：`https://xuexizhinan.com/book/{ID}.html`
3. 网站专注于4K超清资源，通常使用夸克网盘
4. 夸克网盘链接格式：`https://pan.quark.cn/s/{提取码}`，没有单独的密码
5. 有些资源同时提供磁力链接和网盘链接
6. 详情页中可能没有明确的发布日期，但可以从电影信息中提取上映日期 