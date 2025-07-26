# SuSu网站搜索结果HTML结构分析

## 页面整体结构

搜索结果页面的主要内容位于`.post-1.post-list.post-item-1`元素内，每个搜索结果项包含在`.post-list-item.item-post-style-1`元素中。

```html
<div class="post-1 post-list post-item-1" id="post-list">
    <div class="hidden-line">
        <ul class="b2_gap">
            <li class="post-list-item item-post-style-1" id="item-18892">
                <!-- 单个搜索结果 -->
            </li>
            <li class="post-list-item item-post-style-1" id="item-13859">
                <!-- 单个搜索结果 -->
            </li>
        </ul>
    </div>
</div>
```

## 单个搜索结果结构

### 1. 帖子ID

帖子ID可以从以下位置提取：
- 详情页链接：`href="https://susuifa.com/18892.html"`，其中`18892`为帖子ID

### 2. 标题

标题位于`.post-info h2 a`元素中：

```html
<div class="post-info">
    <h2><a href="https://susuifa.com/18892.html">瑞克和莫蒂：日漫版 Rick and Morty: The Anime (2024)</a></h2>
</div>
```

### 3. 内容描述

内容描述位于`.post-excerpt`元素中：

```html
<div class="post-excerpt">
    　　Adult Swim推出《瑞克和莫蒂》衍生剧：日本动画风的《Rick and Morty: The Anime》，佐野隆史（《神之塔》）执导，描述为一个关于"这个很棒的家庭"的新故事，独立于主线之外，但会包括《瑞克和莫蒂》的主题和事件。　　佐野隆史此前打造过两部《瑞克和莫蒂》的衍生短片：《瑞克和莫蒂对抗种族灭绝者》和《瑞克和莫蒂外传姐姐遇见上帝》。 打开手机迅雷或者迅雷PC客户端，在搜索框输入&hellip;
</div>
```

### 4. 日期时间

日期时间信息位于`.list-footer time.b2timeago`元素中：

```html
<div class="list-footer">
    <span><time class="b2timeago" datetime="2024-08-16 20:25:28" itemprop="datePublished">24年8月16日</time></span>
</div>
```

### 5. 分类标签

分类标签位于`.post-list-cat-item`元素中：

```html
<div class="post-list-meta-box">
    <div class="post-list-cat b2-radius">
        <a class="post-list-cat-item b2-radius" href="https://susuifa.com/rhjj" style="color:#607d8b">日韩剧集</a>
    </div>
</div>
```