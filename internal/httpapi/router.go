package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// errorResponse 约定所有 HTTP 错误使用相同结构。
// Code 便于客户端判断错误类型，Message 用于说明；内部错误细节只记日志。
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewRouter() *gin.Engine {
	router := gin.New()
	// 使用直接连接的地址判断客户端 IP
	if err := router.SetTrustedProxies(nil); err != nil {
		panic(err)
	}
	// 注册日志
	router.Use(gin.Logger(), gin.CustomRecovery(func(c *gin.Context, recovered any) {
		writeError(c, http.StatusInternalServerError, "internal_error", "internal server error")
	}))

	// 注册 GET /health
	router.GET("/health", health)
	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "not_found", "route not found")
	})
	return router
}

func health(c *gin.Context) {
	// c 是当前请求的 Gin Context，用于响应
	// 只检查 HTTP 服务响应，检查不了数据库和 LLM 健康程度
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func writeError(c *gin.Context, status int, code, message string) {
	// Abort 阻止后续 handler 执行，但不会像 return 一样退出当前 Go 函数
	c.AbortWithStatusJSON(status, errorResponse{
		Error: errorDetail{Code: code, Message: message},
	})
}
