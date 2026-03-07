package api

import (
	"net/http"
	"pansou/config"
	"pansou/plugin"
	"pansou/service"
	"pansou/util"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SetupRouter 设置路由
func SetupRouter(searchService *service.SearchService) *gin.Engine {
	// 设置搜索服务
	SetSearchService(searchService)

	// 设置为生产模式
	gin.SetMode(gin.ReleaseMode)

	// 创建默认路由
	r := gin.Default()

	// 创建MCP服务，注册到gin路由
	mcpServer := SetupMcpTool()
	// 注册MCP路由
	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{})
	// 不走gin的http中间件
	r.Any("/mcp", gin.WrapH(handler))

	// 添加中间件
	r.Use(CORSMiddleware())
	r.Use(LoggerMiddleware())
	r.Use(util.GzipMiddleware()) // 添加压缩中间件
	r.Use(AuthMiddleware())      // 添加认证中间件

	// 定义API路由组
	api := r.Group("/api")
	{
		// 认证接口（不需要认证，由中间件公开路径处理）
		auth := api.Group("/auth")
		{
			auth.POST("/login", LoginHandler)
			auth.POST("/verify", VerifyHandler)
			auth.POST("/logout", LogoutHandler)
		}

		// 搜索接口 - 支持POST和GET两种方式
		api.POST("/search", SearchHandler)
		api.GET("/search", SearchHandler) // 添加GET方式支持

		// 健康检查接口
		api.GET("/health", func(c *gin.Context) {
			c.JSON(200, Health())
		})
	}

	// 注册插件的Web路由（如果插件实现了PluginWithWebHandler接口）
	// 只有当插件功能启用且插件在启用列表中时才注册路由
	if config.AppConfig.AsyncPluginEnabled && searchService != nil && searchService.GetPluginManager() != nil {
		enabledPlugins := searchService.GetPluginManager().GetPlugins()
		for _, p := range enabledPlugins {
			if webPlugin, ok := p.(plugin.PluginWithWebHandler); ok {
				webPlugin.RegisterWebRoutes(r.Group(""))
			}
		}
	}

	return r
}
