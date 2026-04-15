package app

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORSMiddleware 跨域资源共享中间件
// 通过环境变量 CCLOAD_CORS_ORIGINS 配置允许的来源，多个用逗号分隔
// 不设置则默认 *（允许所有来源），方便 Cloudflare Tunnel / 外网部署场景
func CORSMiddleware() gin.HandlerFunc {
	allowOrigin := strings.TrimSpace(os.Getenv("CCLOAD_CORS_ORIGINS"))
	if allowOrigin == "" {
		allowOrigin = "*"
	}

	// 预先拆好白名单，启动后不再反复 Split
	var allowedOrigins []string
	if allowOrigin != "*" {
		for _, o := range strings.Split(allowOrigin, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				allowedOrigins = append(allowedOrigins, trimmed)
			}
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			// 不是跨域请求，跳过
			c.Next()
			return
		}

		// 匹配 origin
		if allowOrigin == "*" {
			c.Header("Access-Control-Allow-Origin", "*")
		} else {
			for _, allowed := range allowedOrigins {
				if allowed == origin {
					c.Header("Access-Control-Allow-Origin", origin)
					c.Header("Vary", "Origin")
					break
				}
			}
		}

		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, x-goog-api-key")
		c.Header("Access-Control-Max-Age", "86400")

		// OPTIONS 预检直接 204，不走后续 auth 和 handler
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
