package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"pansou/config"
	"pansou/util"
)

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

		// 对于搜索API，尝试解码关键词以便更好地显示
		displayURI := reqURI
		if strings.Contains(reqURI, "/api/search") && strings.Contains(reqURI, "kw=") {
			if parsedURL, err := url.Parse(reqURI); err == nil {
				if keyword := parsedURL.Query().Get("kw"); keyword != "" {
					if decodedKeyword, err := url.QueryUnescape(keyword); err == nil {
						// 替换原始URI中的编码关键词为解码后的关键词
						displayURI = strings.Replace(reqURI, "kw="+keyword, "kw="+decodedKeyword, 1)
					}
				}
			}
		}

		// 状态码
		statusCode := c.Writer.Status()

		// 请求IP
		clientIP := c.ClientIP()

		// 日志格式
		gin.DefaultWriter.Write([]byte(
			fmt.Sprintf("| %s | %s | %s | %d | %s\n",
				clientIP, reqMethod, displayURI, statusCode, latencyTime.String())))
	}
}

// AuthMiddleware JWT认证中间件
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果未启用认证，直接放行
		if !config.AppConfig.AuthEnabled {
			c.Next()
			return
		}

		// 定义公开接口（不需要认证）
		publicPaths := []string{
			"/api/auth/login",
			"/api/auth/logout",
			"/api/health", // 健康检查接口可选择是否需要认证
		}

		// 检查当前路径是否是公开接口
		path := c.Request.URL.Path
		for _, p := range publicPaths {
			if strings.HasPrefix(path, p) {
				c.Next()
				return
			}
		}

		// 获取Authorization头
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(401, gin.H{
				"error": "未授权：缺少认证令牌",
				"code":  "AUTH_TOKEN_MISSING",
			})
			c.Abort()
			return
		}

		// 解析Bearer token
		const bearerPrefix = "Bearer "
		if !strings.HasPrefix(authHeader, bearerPrefix) {
			c.JSON(401, gin.H{
				"error": "未授权：令牌格式错误",
				"code":  "AUTH_TOKEN_INVALID_FORMAT",
			})
			c.Abort()
			return
		}

		tokenString := strings.TrimPrefix(authHeader, bearerPrefix)

		// 验证token
		claims, err := util.ValidateToken(tokenString, config.AppConfig.AuthJWTSecret)
		if err != nil {
			c.JSON(401, gin.H{
				"error": "未授权：令牌无效或已过期",
				"code":  "AUTH_TOKEN_INVALID",
			})
			c.Abort()
			return
		}

		// 将用户信息存入上下文，供后续处理使用
		c.Set("username", claims.Username)
		c.Next()
	}
}

func McpAuthMiddleware(h mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
		// mcp token为空则，直接返回有效
		if config.AppConfig.AuthMcpToken == "" {
			return h(ctx, method, req)
		}
		extra := req.GetExtra()
		if extra == nil || extra.Header.Get("Authorization") != config.AppConfig.AuthMcpToken {
			return nil, errors.New("未授权：确认McpToken")
		}

		return h(ctx, method, req)
	}
}
