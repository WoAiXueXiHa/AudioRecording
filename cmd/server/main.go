package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"audiorecording/internal/database"
	"audiorecording/internal/httpapi"
	"audiorecording/internal/recording"
)

func main() {
	// run 返回后资源清理已完成；不要在持有连接的函数中直接 log.Fatal 跳过 defer。
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func workerConcurrency() (int, error) {
	raw, ok := os.LookupEnv("WORKER_CONCURRENCY")
	if !ok {
		return 3, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("WORKER_CONCURRENCY must be a positive integer")
	}
	return n, nil
}

func run() error {
	concurrency, err := workerConcurrency()
	if err != nil {
		return err
	}
	// 读取配置，决定在哪个地址接收请求；没有设置使用默认值，显式空地址拒绝启动。
	addr, configured := os.LookupEnv("HTTP_ADDR")
	if !configured {
		addr = "127.0.0.1:8080"
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("HTTP_ADDR cannot be empty")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("invalid HTTP_ADDR %q: %w", addr, err)
	}
	// 在接收请求前检查数据库和存储目录；迁移仍由 SQL 文件显式执行。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := database.Open(ctx, os.Getenv("MYSQL_DSN"))
	cancel()
	if err != nil {
		return err
	}
	pool, err := db.DB()
	if err != nil {
		return err
	}
	defer pool.Close()
	uploadDir, configured := os.LookupEnv("UPLOAD_DIR")
	if !configured {
		uploadDir = "./uploads"
	}
	uploads, err := recording.NewService(db, uploadDir)
	if err != nil {
		return err
	}
	// 缺少真实 LLM 密钥明确失败，不静默降级为 Mock。
	llm, err := recording.NewDeepSeekClient(os.Getenv("DEEPSEEK_API_KEY"), os.Getenv("DEEPSEEK_BASE_URL"), os.Getenv("DEEPSEEK_MODEL"))
	if err != nil {
		return err
	}
	// 创建 HTTP 服务，请求交给 Gin
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewRouter(uploads),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// 先确认端口可监听，再处理遗留任务，避免同端口误启动第二实例时改动任务。
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	defer listener.Close()
	if err := uploads.InterruptTasks(context.Background()); err != nil {
		return fmt.Errorf("mark interrupted tasks: %w", err)
	}
	// 服务级 context 控制后台生命周期，独立于任何一个 HTTP 请求。
	serviceCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner, err := recording.NewRunner(uploads, recording.NewMockTranscriber(), concurrency, llm)
	if err != nil {
		return err
	}
	log.Printf("worker concurrency=%d", concurrency)
	runnerDone := make(chan struct{})
	go func() { runner.Run(serviceCtx); runner.Wait(); close(runnerDone) }()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	log.Printf("HTTP server ready on %s", listener.Addr())
	var serveErr error
	select {
	case <-serviceCtx.Done():
	case serveErr = <-serveDone:
	}
	stop() // 停止领取并取消在途外部调用，失败落库仍使用其独立短 context。
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	shutdownCancel()
	if shutdownErr != nil {
		_ = server.Close()
	}
	// Run 已停止后才 Wait，避免 WaitGroup.Add 与 Wait 的启动竞争。
	select {
	case <-runnerDone:
	case <-time.After(10 * time.Second):
		log.Print("background shutdown timed out; unfinished tasks will be marked failed on next startup")
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	if shutdownErr != nil {
		return fmt.Errorf("HTTP shutdown: %w", shutdownErr)
	}
	log.Print("HTTP server stopped")
	return nil
}
