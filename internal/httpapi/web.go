package httpapi

import (
	"embed"
	"github.com/gin-gonic/gin"
	"net/http"
)

//go:embed web/*
var webFiles embed.FS

// 静态页面随二进制发布，不依赖工作目录或额外前端服务。
func registerWeb(router *gin.Engine) {
	for _, asset := range []struct{ route, file, contentType string }{
		{"/", "web/index.html", "text/html; charset=utf-8"},
		{"/assets/style.css", "web/style.css", "text/css; charset=utf-8"},
		{"/assets/app.js", "web/app.js", "text/javascript; charset=utf-8"},
	} {
		body, err := webFiles.ReadFile(asset.file)
		if err != nil {
			panic(err)
		}
		router.GET(asset.route, func(c *gin.Context) {
			c.Header("Cache-Control", "no-cache")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Data(http.StatusOK, asset.contentType, body)
		})
	}
}
