# PanSou API层设计详解

## 1. API层概述

API层是PanSou系统的外部接口层，负责处理来自客户端的HTTP请求，并返回适当的响应。该层采用Gin框架实现，主要包含路由定义、请求处理和中间件三个核心部分。

## 2. 目录结构

```
pansou/api/
├── handler.go    # 请求处理器
├── middleware.go # 中间件
└── router.go     # 路由定义
```

## 3. 路由设计

### 3.1 路由定义（router.go）

路由模块负责定义API端点和路由规则，将请求映射到相应的处理函数。

```go
// SetupRouter 设置路由
func SetupRouter(searchService *service.SearchService) *gin.Engine {
    // 设置搜索服务
    SetSearchService(searchService)
    
    // 设置为生产模式
    gin.SetMode(gin.ReleaseMode)
    
    // 创建默认路由
    r := gin.Default()
    
    // 添加中间件
    r.Use(CORSMiddleware())
    r.Use(LoggerMiddleware())
    r.Use(util.GzipMiddleware()) // 添加压缩中间件
    
    // 定义API路由组
    api := r.Group("/api")
    {
        // 搜索接口 - 支持POST和GET两种方式
        api.POST("/search", SearchHandler)
        api.GET("/search", SearchHandler) // 添加GET方式支持
        
        // 健康检查接口
        api.GET("/health", func(c *gin.Context) {
            pluginCount := 0
            if searchService != nil && searchService.GetPluginManager() != nil {
                pluginCount = len(searchService.GetPluginManager().GetPlugins())
            }
            
            c.JSON(200, gin.H{
                "status": "ok",
                "plugins_enabled": true,
                "plugin_count": pluginCount,
            })
        })
    }
    
    return r
}
```

### 3.2 路由设计思想

1. **RESTful API设计**：采用RESTful风格设计API，使用适当的HTTP方法和路径
2. **路由分组**：使用路由组对API进行分类管理
3. **灵活的请求方式**：搜索接口同时支持GET和POST请求，满足不同场景需求
4. **健康检查**：提供健康检查接口，便于监控系统状态

## 4. 请求处理器

### 4.1 处理器实现（handler.go）

处理器模块负责处理具体的业务逻辑，包括参数解析、验证、调用服务层和返回响应。

```go
// SearchHandler 搜索处理函数
func SearchHandler(c *gin.Context) {
    var req model.SearchRequest
    var err error

    // 根据请求方法不同处理参数
    if c.Request.Method == http.MethodGet {
        // GET方式：从URL参数获取
        // 获取keyword，必填参数
        keyword := c.Query("kw")
        
        // 处理channels参数，支持逗号分隔
        channelsStr := c.Query("channels")
        var channels []string
        // 只有当参数非空时才处理
        if channelsStr != "" && channelsStr != " " {
            parts := strings.Split(channelsStr, ",")
            for _, part := range parts {
                trimmed := strings.TrimSpace(part)
                if trimmed != "" {
                    channels = append(channels, trimmed)
                }
            }
        }
        
        // 处理并发数
        concurrency := 0
        concStr := c.Query("conc")
        if concStr != "" && concStr != " " {
            concurrency = util.StringToInt(concStr)
        }
        
        // 处理强制刷新
        forceRefresh := false
        refreshStr := c.Query("refresh")
        if refreshStr != "" && refreshStr != " " && refreshStr == "true" {
            forceRefresh = true
        }
        
        // 处理结果类型和来源类型
        resultType := c.Query("res")
        if resultType == "" || resultType == " " {
            resultType = "" // 使用默认值
        }
        
        sourceType := c.Query("src")
        if sourceType == "" || sourceType == " " {
            sourceType = "" // 使用默认值
        }
        
        // 处理plugins参数，支持逗号分隔
        pluginsStr := c.Query("plugins")
        var plugins []string
        // 只有当参数非空时才处理
        if pluginsStr != "" && pluginsStr != " " {
            parts := strings.Split(pluginsStr, ",")
            for _, part := range parts {
                trimmed := strings.TrimSpace(part)
                if trimmed != "" {
                    plugins = append(plugins, trimmed)
                }
            }
        }

        // 处理ext参数，JSON格式
        var ext map[string]interface{}
        extStr := c.Query("ext")
        if extStr != "" && extStr != " " {
            // 处理特殊情况：ext={}
            if extStr == "{}" {
                ext = make(map[string]interface{})
            } else {
                if err := jsonutil.Unmarshal([]byte(extStr), &ext); err != nil {
                    c.JSON(http.StatusBadRequest, model.NewErrorResponse(400, "无效的ext参数格式: "+err.Error()))
                    return
                }
            }
        }
        // 确保ext不为nil
        if ext == nil {
            ext = make(map[string]interface{})
        }

        req = model.SearchRequest{
            Keyword:      keyword,
            Channels:     channels,
            Concurrency:  concurrency,
            ForceRefresh: forceRefresh,
            ResultType:   resultType,
            SourceType:   sourceType,
            Plugins:      plugins,
            Ext:          ext,
        }
    } else {
        // POST方式：从请求体获取
        data, err := c.GetRawData()
        if err != nil {
            c.JSON(http.StatusBadRequest, model.NewErrorResponse(400, "读取请求数据失败: "+err.Error()))
            return
        }

        if err := jsonutil.Unmarshal(data, &req); err != nil {
            c.JSON(http.StatusBadRequest, model.NewErrorResponse(400, "无效的请求参数: "+err.Error()))
            return
        }
    }
    
    // 检查并设置默认值
    if len(req.Channels) == 0 {
        req.Channels = config.AppConfig.DefaultChannels
    }
    
    // 如果未指定结果类型，默认返回merge
    if req.ResultType == "" {
        req.ResultType = "merge"
    } else if req.ResultType == "merge" {
        // 将merge转换为merged_by_type，以兼容内部处理
        req.ResultType = "merged_by_type"
    }
    
    // 如果未指定数据来源类型，默认为全部
    if req.SourceType == "" {
        req.SourceType = "all"
    }
    
    // 参数互斥逻辑：当src=tg时忽略plugins参数，当src=plugin时忽略channels参数
    if req.SourceType == "tg" {
        req.Plugins = nil // 忽略plugins参数
    } else if req.SourceType == "plugin" {
        req.Channels = nil // 忽略channels参数
    }
    
    // 执行搜索
    result, err := searchService.Search(req.Keyword, req.Channels, req.Concurrency, req.ForceRefresh, req.ResultType, req.SourceType, req.Plugins, req.Ext)
    
    if err != nil {
        response := model.NewErrorResponse(500, "搜索失败: "+err.Error())
        jsonData, _ := jsonutil.Marshal(response)
        c.Data(http.StatusInternalServerError, "application/json", jsonData)
        return
    }

    // 返回结果
    response := model.NewSuccessResponse(result)
    jsonData, _ := jsonutil.Marshal(response)
    c.Data(http.StatusOK, "application/json", jsonData)
}
```

### 4.2 处理器设计思想

1. **多种请求方式支持**：同时支持GET和POST请求，并针对不同请求方式采用不同的参数解析策略
2. **参数规范化**：对输入参数进行清理和规范化处理，确保不同形式但语义相同的参数能够生成一致的缓存键
3. **默认值处理**：为未提供的参数设置合理的默认值
4. **参数互斥逻辑**：实现参数间的互斥关系，避免冲突
5. **统一响应格式**：使用标准化的响应格式，包括成功和错误响应
6. **高性能JSON处理**：使用优化的JSON库处理请求和响应
7. **缓存一致性支持**：通过参数处理确保相同语义的查询能够命中相同的缓存

## 5. 中间件设计

### 5.1 中间件实现（middleware.go）

中间件模块提供了跨域处理、日志记录等功能，用于处理请求前后的通用逻辑。

```go
// CORSMiddleware 跨域中间件
func CORSMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
        c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        c.Writer.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
        
        if c.Request.Method == "OPTIONS" {
            c.AbortWithStatus(204)
            return
        }
        
        c.Next()
    }
}

// LoggerMiddleware 日志中间件
func LoggerMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        // 开始时间
        startTime := time.Now()
        
        // 处理请求
        c.Next()
        
        // 结束时间
        endTime := time.Now()
        
        // 执行时间
        latencyTime := endTime.Sub(startTime)
        
        // 请求方式
        reqMethod := c.Request.Method
        
        // 请求路由
        reqURI := c.Request.RequestURI
        
        // 状态码
        statusCode := c.Writer.Status()
        
        // 请求IP
        clientIP := c.ClientIP()
        
        // 日志格式
        gin.DefaultWriter.Write([]byte(
            fmt.Sprintf("| %s | %s | %s | %d | %s\n", 
                clientIP, reqMethod, reqURI, statusCode, latencyTime.String())))
    }
}
```

### 5.2 中间件设计思想

1. **关注点分离**：将通用功能抽象为中间件，与业务逻辑分离
2. **链式处理**：中间件可以按顺序组合，形成处理管道
3. **前置/后置处理**：支持在请求处理前后执行逻辑
4. **性能监控**：通过日志中间件记录请求处理时间，便于性能分析

## 6. API接口规范

### 6.1 搜索API

**接口地址**：`/api/search`  
**请求方法**：`POST` 或 `GET`  
**Content-Type**：`application/json`（POST方法）

#### POST请求参数

| 参数名 | 类型 | 必填 | 描述 |
|--------|------|------|------|
| kw | string | 是 | 搜索关键词 |
| channels | string[] | 否 | 搜索的频道列表，不提供则使用默认配置 |
| conc | number | 否 | 并发搜索数量，不提供则自动设置为频道数+插件数+10 |
| refresh | boolean | 否 | 强制刷新，不使用缓存，便于调试和获取最新数据 |
| res | string | 否 | 结果类型：all(返回所有结果)、results(仅返回results)、merge(仅返回merged_by_type)，默认为merge |
| src | string | 否 | 数据来源类型：all(默认，全部来源)、tg(仅Telegram)、plugin(仅插件) |
| plugins | string[] | 否 | 指定搜索的插件列表，不指定则搜索全部插件 |
| ext | object | 否 | 扩展参数，用于传递给插件的自定义参数，如{"title_en":"English Title", "is_all":true} |

#### GET请求参数

| 参数名 | 类型 | 必填 | 描述 |
|--------|------|------|------|
| kw | string | 是 | 搜索关键词 |
| channels | string | 否 | 搜索的频道列表，使用英文逗号分隔多个频道，不提供则使用默认配置 |
| conc | number | 否 | 并发搜索数量，不提供则自动设置为频道数+插件数+10 |
| refresh | boolean | 否 | 强制刷新，设置为"true"表示不使用缓存 |
| res | string | 否 | 结果类型：all(返回所有结果)、results(仅返回results)、merge(仅返回merged_by_type)，默认为merge |
| src | string | 否 | 数据来源类型：all(默认，全部来源)、tg(仅Telegram)、plugin(仅插件) |
| plugins | string | 否 | 指定搜索的插件列表，使用英文逗号分隔多个插件名，不指定则搜索全部插件 |
| ext | string | 否 | JSON格式的扩展参数，用于传递给插件的自定义参数，如{"title_en":"English Title", "is_all":true} |

#### 成功响应

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "total": 15,
    "results": [
      {
        "message_id": "12345",
        "unique_id": "channel-12345",
        "channel": "tgsearchers2",
        "datetime": "2023-06-10T14:23:45Z",
        "title": "速度与激情全集1-10",
        "content": "速度与激情系列全集，1080P高清...",
        "links": [
          {
            "type": "baidu",
            "url": "https://pan.baidu.com/s/1abcdef",
            "password": "1234"
          }
        ],
        "tags": ["电影", "合集"]
      },
      // 更多结果...
    ],
    "merged_by_type": {
      "baidu": [
        {
          "url": "https://pan.baidu.com/s/1abcdef",
          "password": "1234",
          "note": "速度与激情全集1-10",
          "datetime": "2023-06-10T14:23:45Z"
        },
        // 更多百度网盘链接...
      ],
      "aliyun": [
        // 阿里云盘链接...
      ]
      // 更多网盘类型...
    }
  }
}
```

#### 错误响应

```json
{
  "code": 400,
  "message": "关键词不能为空"
}
```

### 6.2 健康检查API

**接口地址**：`/api/health`  
**请求方法**：`GET`

#### 成功响应

```json
{
  "status": "ok",
  "plugins_enabled": true,
  "plugin_count": 6,
  "plugins": ["pansearch", "panta", "qupansou", "hunhepan", "jikepan", "pan666"],
  "channels": ["tgsearchers2", "SharePanBaidu"]
}
```

## 7. 参数处理优化

### 7.1 GET请求参数处理

```go
// 处理plugins参数，支持逗号分隔
var plugins []string
// 检查请求中是否存在plugins参数
if c.Request.URL.Query().Has("plugins") {
    pluginsStr := c.Query("plugins")
    // 判断参数是否非空
    if pluginsStr != "" && pluginsStr != " " {
        parts := strings.Split(pluginsStr, ",")
        for _, part := range parts {
            trimmed := strings.TrimSpace(part)
            if trimmed != "" {
                plugins = append(plugins, trimmed)
            }
        }
    }
} else {
    // 如果请求中不存在plugins参数，设置为nil
    plugins = nil
}
```

### 7.2 扩展参数处理

```go
// 处理ext参数，JSON格式
var ext map[string]interface{}
extStr := c.Query("ext")
if extStr != "" && extStr != " " {
    // 处理特殊情况：ext={}
    if extStr == "{}" {
        ext = make(map[string]interface{})
    } else {
        if err := jsonutil.Unmarshal([]byte(extStr), &ext); err != nil {
            c.JSON(http.StatusBadRequest, model.NewErrorResponse(400, "无效的ext参数格式: "+err.Error()))
            return
        }
    }
}
// 确保ext不为nil
if ext == nil {
    ext = make(map[string]interface{})
}
```

### 7.3 参数互斥与规范化处理

```go
// 参数互斥逻辑：当src=tg时忽略plugins参数，当src=plugin时忽略channels参数
if req.SourceType == "tg" {
    req.Plugins = nil // 忽略plugins参数
} else if req.SourceType == "plugin" {
    req.Channels = nil // 忽略channels参数
} else if req.SourceType == "all" {
    // 对于all类型，如果plugins为空或不存在，统一设为nil
    if req.Plugins == nil || len(req.Plugins) == 0 {
        req.Plugins = nil
    }
}
```

## 8. 性能优化措施

1. **高效参数处理**：对GET请求参数进行高效处理，避免不必要的字符串操作
2. **高性能JSON库**：使用sonic高性能JSON库处理请求和响应
3. **响应压缩**：通过GzipMiddleware实现响应压缩，减少传输数据量
4. **避免内存分配**：合理使用预分配和对象池，减少内存分配和GC压力
5. **直接写入响应体**：使用`c.Data`直接写入响应体，避免中间转换
6. **精确参数检查**：使用`c.Request.URL.Query().Has()`检查参数是否存在，避免不必要的处理
7. **参数统一处理**：对相同语义的不同形式参数进行统一处理，确保缓存一致性
