// Package server 提供 HTTP API、SSE 推送和前端静态页面。
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"septwebclip/internal/config"
	"septwebclip/internal/storage"
	"septwebclip/internal/store"
)

type Server struct {
	cfg    *config.Config
	db     *store.DB
	files  storage.Storage
	hub    *hub
	web    fs.FS
	tmpDir string

	index   []byte // 渲染好的 index.html
	version string // 静态资源内容哈希，用作缓存版本号
	csp     string

	enterLimit *rateLimiter
}

func New(cfg *config.Config, db *store.DB, files storage.Storage, web fs.FS) (*Server, error) {
	s := &Server{
		cfg:        cfg,
		db:         db,
		files:      files,
		hub:        newHub(),
		web:        web,
		tmpDir:     filepath.Join(cfg.DataDir, "tmp"),
		enterLimit: newRateLimiter(20, 10*time.Minute),
	}
	// 上传时先落到这个目录，启动时清掉上次异常退出留下的残片。
	_ = os.RemoveAll(s.tmpDir)
	if err := os.MkdirAll(s.tmpDir, 0o755); err != nil {
		return nil, err
	}
	if err := s.prepareStatic(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/session", s.handleEnter)
	mux.HandleFunc("DELETE /api/session", s.handleLeave)

	mux.HandleFunc("GET /api/events", s.withRoom(s.handleEvents))
	mux.HandleFunc("GET /api/clips", s.withRoom(s.handleList))
	mux.HandleFunc("POST /api/clips", s.withRoom(s.handleCreateText))
	mux.HandleFunc("POST /api/clips/image", s.withRoom(s.handleCreateImage))
	mux.HandleFunc("POST /api/clips/file", s.withRoom(s.handleCreateFile))
	mux.HandleFunc("POST /api/views", s.withRoom(s.handleViews))
	mux.HandleFunc("GET /api/clips/{id}/image", s.withRoom(s.handleImage))
	mux.HandleFunc("GET /api/clips/{id}/file", s.withRoom(s.handleFile))
	mux.HandleFunc("GET /api/clips/{id}/logs", s.withRoom(s.handleLogs))
	mux.HandleFunc("POST /api/clips/{id}/copy", s.withRoom(s.handleCopy))
	mux.HandleFunc("POST /api/clips/{id}/pin", s.withRoom(s.handlePin))
	mux.HandleFunc("DELETE /api/clips/{id}", s.withRoom(s.handleDelete))

	mux.HandleFunc("GET /", s.handleStatic)
	return s.secureHeaders(mux)
}

// RunCleaner 定期删除过期且未钉住的便签记录，并通知在线的设备。
func (s *Server) RunCleaner(ctx context.Context) {
	t := time.NewTicker(s.cfg.CleanupInterval)
	defer t.Stop()
	for {
		removed, err := s.db.DeleteExpired(ctx, nowMs())
		if err != nil && ctx.Err() == nil {
			slog.Error("清理过期便签失败", "err", err)
		}
		for _, r := range removed {
			s.hub.publish(r.Room, event{Type: "delete", ID: r.ID})
		}
		if len(removed) > 0 {
			slog.Info("已清理过期便签", "count", len(removed))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", s.csp)
		next.ServeHTTP(w, r)
	})
}

// prepareStatic 计算静态资源版本号，并把字体地址和版本号填进 index.html。
func (s *Server) prepareStatic() error {
	sum := sha256.New()
	err := fs.WalkDir(s.web, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(s.web, p)
		if err != nil {
			return err
		}
		sum.Write([]byte(p))
		sum.Write(b)
		return nil
	})
	if err != nil {
		return err
	}
	s.version = hex.EncodeToString(sum.Sum(nil))[:10]

	idx, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		return err
	}
	fontLink, fontOrigin := "", ""
	if s.cfg.FontCSSURL != "" {
		fontLink = `<link rel="stylesheet" href="` + html.EscapeString(s.cfg.FontCSSURL) + `">`
		if u, err := url.Parse(s.cfg.FontCSSURL); err == nil {
			fontOrigin = " " + u.Scheme + "://" + u.Host
		}
	}
	idx = bytes.ReplaceAll(idx, []byte("__V__"), []byte(s.version))
	idx = bytes.ReplaceAll(idx, []byte("<!--FONT-->"), []byte(fontLink))
	s.index = idx

	s.csp = "default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; connect-src 'self'; " +
		"script-src 'self'; style-src 'self'" + fontOrigin + "; font-src 'self'" + fontOrigin +
		"; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	return nil
}

var staticTypes = map[string]string{
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
	".webp": "image/webp",
	".json": "application/json",
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	if p == "/" || p == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(s.index)
		return
	}
	name := strings.TrimPrefix(p, "/")
	st, err := fs.Stat(s.web, name)
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if ct, ok := staticTypes[path.Ext(name)]; ok {
		w.Header().Set("Content-Type", ct)
	}
	if r.URL.Query().Get("v") == s.version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.web, name)
}
