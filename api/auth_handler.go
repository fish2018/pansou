package api

import (
	"time"

	"github.com/gin-gonic/gin"
	"pansou/config"
	"pansou/util"
)

// LoginRequest 登录请求结构
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginResponse 登录响应结构
type LoginResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Username  string `json:"username"`
}

// LoginHandler 处理用户登录
func LoginHandler(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "参数错误：用户名和密码不能为空"})
		return
	}

	// 验证认证系统是否启用
	if !config.AppConfig.AuthEnabled {
		c.JSON(403, gin.H{"error": "认证功能未启用"})
		return
	}

	// 验证用户配置是否存在
	if config.AppConfig.AuthUsers == nil || len(config.AppConfig.AuthUsers) == 0 {
		c.JSON(500, gin.H{"error": "认证系统未正确配置"})
		return
	}

	// 验证用户名和密码
	storedPassword, exists := config.AppConfig.AuthUsers[req.Username]
	if !exists || storedPassword != req.Password {
		c.JSON(401, gin.H{"error": "用户名或密码错误"})
		return
	}

	// 生成JWT token
	token, err := util.GenerateToken(
		req.Username,
		config.AppConfig.AuthJWTSecret,
		config.AppConfig.AuthTokenExpiry,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": "生成令牌失败"})
		return
	}

	// 返回token和过期时间
	expiresAt := time.Now().Add(config.AppConfig.AuthTokenExpiry).Unix()
	c.JSON(200, LoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		Username:  req.Username,
	})
}

// VerifyHandler 验证token有效性
func VerifyHandler(c *gin.Context) {
	// 如果未启用认证，直接返回有效
	if !config.AppConfig.AuthEnabled {
		c.JSON(200, gin.H{
			"valid":   true,
			"message": "认证功能未启用",
		})
		return
	}

	// 如果能到达这里，说明中间件已经验证通过
	username, exists := c.Get("username")
	if !exists {
		c.JSON(401, gin.H{"error": "未授权"})
		return
	}

	c.JSON(200, gin.H{
		"valid":    true,
		"username": username,
	})
}

// LogoutHandler 退出登录（客户端删除token即可）
func LogoutHandler(c *gin.Context) {
	// JWT是无状态的，服务端不需要处理注销
	// 客户端删除存储的token即可
	c.JSON(200, gin.H{"message": "退出成功"})
}
