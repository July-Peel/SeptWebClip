// SeptWebClip：按口令划分的共享便签墙，用来在手机和电脑之间互传文字和图片。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"septwebclip/internal/config"
	"septwebclip/internal/server"
	"septwebclip/internal/storage"
	"septwebclip/internal/store"
)

//go:embed web
var webFS embed.FS

func main() {
	health := flag.Bool("healthcheck", false, "检查本机服务是否正常（给 Docker 健康检查用）")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误：", err)
		os.Exit(1)
	}
	if *health {
		os.Exit(healthcheck(cfg.ListenAddr))
	}
	if err := run(cfg); err != nil {
		slog.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	if err := cfg.EnsureSecret(); err != nil {
		return fmt.Errorf("初始化密钥：%w", err)
	}

	db, err := store.Open(filepath.Join(cfg.DataDir, "septwebclip.db"))
	if err != nil {
		return fmt.Errorf("打开数据库：%w", err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var files storage.Storage
	switch cfg.ImageStorage {
	case "webdav":
		dav, err := storage.NewWebDAV(cfg.WebDAVURL, cfg.WebDAVUser, cfg.WebDAVPassword)
		if err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := dav.Check(checkCtx); err != nil {
			slog.Warn("WebDAV 暂时不可用，上传图片会失败", "err", err)
		}
		cancel()
		files = dav
	default:
		local, err := storage.NewLocal(cfg.ImageDir)
		if err != nil {
			return fmt.Errorf("创建图片目录：%w", err)
		}
		files = local
	}

	web, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	srv, err := server.New(cfg, db, files, web)
	if err != nil {
		return err
	}
	go srv.RunCleaner(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 请求上下文跟随进程退出，SSE 长连接才能及时结束。
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("SeptWebClip 已启动", "addr", cfg.ListenAddr, "storage", files.Name(),
		"ttl", cfg.ClipTTL.String(), "trustProxy", cfg.TrustProxy)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func healthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(strings.Trim(host, "[]"), port) + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
