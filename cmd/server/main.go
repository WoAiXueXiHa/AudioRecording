package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"audiorecording/internal/database"
	"audiorecording/internal/httpapi"
	"audiorecording/internal/recording"
)

func main() {
	// 读取配置
	addr, configured := os.LookupEnv("HTTP_ADDR")
	if !configured {
		addr = "127.0.0.1:8080"
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		log.Fatal("HTTP_ADDR cannot be empty")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		log.Fatalf("invalid HTTP_ADDR %q: %v", addr, err)
	}

	// 在接收请求前检查数据库和存储目录；迁移仍由 SQL 文件显式执行。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := database.Open(ctx, os.Getenv("MYSQL_DSN"))
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	uploadDir, configured := os.LookupEnv("UPLOAD_DIR")
	if !configured {
		uploadDir = "./uploads"
	}
	uploads, err := recording.NewService(db, uploadDir)
	if err != nil {
		log.Fatal(err)
	}

	// 创建 HTTP 服务，请求交给 Gin
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewRouter(uploads),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 监听端口
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen failed: %v", err)
	}
	log.Printf("HTTP server ready on %s", listener.Addr())
	// server 接收请求
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
