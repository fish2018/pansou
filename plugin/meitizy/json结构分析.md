# Meitizy（美体资源）插件JSON API结构分析

## 网站概述
- **网站名称**: 美体资源
- **前端域名**: https://video.451024.xyz
- **API域名**: https://apis.451024.xyz
- **类型**: JSON API接口，提供影视资源网盘链接搜索
- **接口类型**: RESTful API

## API流程概述

### 搜索接口
- **请求URL**: `https://apis.451024.xyz/api/media/search`
- **请求方法**: POST
- **请求头**: 
  - `Content-Type: application/json`
  - `User-Agent`: 标准浏览器User-Agent
  - `Origin`: `https://video.451024.xyz`
  - `Referer`: `https://video.451024.xyz/`
- **请求体**: JSON格式
  ```json
  {
    "title": "遮天",
    "page": 1,
    "size": 10
  }
  ```
- **特点**: 前后端分离后由 `apis.451024.xyz` 提供 JSON 数据，需保留网页来源请求头，无需解析 HTML。

## 请求参数说明

### 请求体字段
| 字段 | 类型 | 必填 | 说明 | 示例 |
|------|------|------|------|------|
| `title` | string | 是 | 搜索关键词 | "遮天" |
| `page` | int | 是 | 页码，从1开始 | 1 |
| `size` | int | 是 | 每页返回数量，新版接口固定为 10 | 10 |

## 响应结构

### 响应格式
```json
{
  "data": [
    {
      "id": 458790,
      "title": "遮天 年番3 (2025) 4K 更新至137集",
      "content": "",
      "link": "https://www.alipan.com/s/6NB4Wop9fJc",
      "link_type": "alipan",
      "tags": "",
      "created_at": "2025-11-25T22:59:53.000Z",
      "updated_at": "2025-11-25T23:39:18.000Z"
    }
  ],
  "total": 441
}
```

### 响应字段说明

#### 根对象
| 字段 | 类型 | 说明 |
|------|------|------|
| `data` | array | 搜索结果数组 |
| `total` | int | 总结果数 |

#### data数组中的对象
| 字段 | 类型 | 说明 | 示例 |
|------|------|------|------|
| `id` | int | 资源唯一ID | 458790 |
| `title` | string | 资源标题 | "遮天 年番3 (2025) 4K 更新至137集" |
| `content` | string | 资源描述/简介 | "冰冷与黑暗并存的宇宙深处..." |
| `link` | string | 网盘链接URL | "https://www.alipan.com/s/6NB4Wop9fJc" |
| `link_type` | string | 网盘类型标识 | "alipan", "xunlei", "baidu", "quark" |
| `tags` | string | 标签（可能为空） | "" |
| `created_at` | string | 创建时间（ISO 8601格式） | "2025-11-25T22:59:53.000Z" |
| `updated_at` | string | 更新时间（ISO 8601格式） | "2025-11-25T23:39:18.000Z" |

## 网盘类型映射

### link_type 到系统网盘类型的映射
| API返回的link_type | 系统网盘类型 | 说明 |
|-------------------|------------|------|
| `alipan` | `aliyun` | 阿里云盘 |
| `xunlei` | `xunlei` | 迅雷网盘 |
| `baidu` | `baidu` | 百度网盘 |
| `quark` | `quark` | 夸克网盘 |
| 其他 | `others` | 其他类型 |

## 数据提取要点

### 1. 请求构建
- 使用POST方法
- 设置Content-Type为application/json
- 请求体为JSON格式
- page参数从1开始
- size使用 10；更大的值会返回 400。

### 2. 响应解析
- 解析JSON响应
- 提取data数组
- 遍历每个item，转换为标准SearchResult格式

### 3. 字段映射
- `id` → `UniqueID` (格式: `{插件名}-{id}`)
- `title` → `Title`
- `content` → `Content`
- `link` → `Links[0].URL`
- `link_type` → `Links[0].Type` (需要映射)
- `created_at` → `Datetime` (需要解析ISO 8601格式)
- `tags` → `Tags` (如果为空则忽略)

### 4. 时间解析
- 格式: ISO 8601格式 `2025-11-25T22:59:53.000Z`
- 优先使用 `created_at`，如果为空则使用 `updated_at`
- 使用 `time.Parse(time.RFC3339, timeStr)` 解析

### 5. 链接处理
- 直接使用API返回的link字段
- 根据link_type映射到系统网盘类型
- 检查链接有效性（非空且为有效URL）

## 特殊处理

### 错误处理
- HTTP状态码检查（200为成功）
- JSON解析错误处理
- 空结果处理
- 网络超时重试

### 性能优化
- 使用连接池（HTTP Transport配置）
- 预编译JSON解析器
- 批量处理结果
- 关键词过滤优化

### 注意事项
1. **API限频**: 避免请求过于频繁
2. **编码**: 处理中文关键词的JSON编码
3. **超时**: 设置合理的请求超时时间
4. **重试**: 实现重试机制提高稳定性
5. **缓存**: 合理使用缓存减少API请求

## 实现建议

### 1. 请求优化
- 使用优化的HTTP客户端（连接池）
- 设置合理的超时时间
- 实现重试机制

### 2. 响应处理
- 使用项目统一的JSON工具（`pansou/util/json`）
- 错误处理和降级策略
- 数据验证和清理

### 3. 性能优化
- 关键词过滤（标题匹配）
- 结果去重
- 合理的并发控制
