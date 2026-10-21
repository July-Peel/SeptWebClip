package storage

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// WebDAV 把图片存到 WebDAV 服务（Nextcloud、坚果云、Alist、群晖等）。
type WebDAV struct {
	base       string // 以 / 结尾
	user, pass string
	client     *http.Client
	dirs       sync.Map // 已确认存在的目录
}

func NewWebDAV(rawURL, user, pass string) (*WebDAV, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return &WebDAV{
		base: u.String(),
		user: user,
		pass: pass,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
		}},
	}, nil
}

func (w *WebDAV) Name() string { return "webdav" }

// Check 确认根目录可访问，不存在则尝试创建。
func (w *WebDAV) Check(ctx context.Context) error {
	resp, err := w.do(ctx, "PROPFIND", w.base, nil, -1, http.Header{"Depth": {"0"}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return w.mkcol(ctx, w.base)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("WebDAV 认证失败：%s", resp.Status)
	case resp.StatusCode >= 300:
		return fmt.Errorf("WebDAV 根目录不可用：%s", resp.Status)
	}
	return nil
}

func (w *WebDAV) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := w.ensureDirs(ctx, path.Dir(key)); err != nil {
		return err
	}
	resp, err := w.do(ctx, http.MethodPut, w.base+key, r, size,
		http.Header{"Content-Type": {contentType}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("WebDAV 上传 %s 失败：%s", key, resp.Status)
	}
	return nil
}

func (w *WebDAV) Serve(rw http.ResponseWriter, r *http.Request, key string) error {
	hdr := http.Header{}
	for _, k := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(k); v != "" {
			hdr.Set(k, v)
		}
	}
	resp, err := w.do(r.Context(), http.MethodGet, w.base+key, nil, -1, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified:
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("WebDAV 读取 %s 失败：%s", key, resp.Status)
	}
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			rw.Header().Set(k, v)
		}
	}
	rw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(rw, resp.Body)
	return nil
}

func (w *WebDAV) ensureDirs(ctx context.Context, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	acc := ""
	for _, seg := range strings.Split(dir, "/") {
		acc += seg + "/"
		if _, ok := w.dirs.Load(acc); ok {
			continue
		}
		if err := w.mkcol(ctx, w.base+acc); err != nil {
			return err
		}
		w.dirs.Store(acc, struct{}{})
	}
	return nil
}

func (w *WebDAV) mkcol(ctx context.Context, u string) error {
	resp, err := w.do(ctx, "MKCOL", u, nil, -1, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	// 201 新建成功；405 表示目录已存在。
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed ||
		(resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("WebDAV 创建目录 %s 失败：%s", u, resp.Status)
}

func (w *WebDAV) do(ctx context.Context, method, u string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil && size >= 0 {
		req.ContentLength = size
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	// 关掉透明 gzip，否则 Content-Length / Range 会对不上。
	req.Header.Set("Accept-Encoding", "identity")
	if w.user != "" || w.pass != "" {
		req.SetBasicAuth(w.user, w.pass)
	}
	return w.client.Do(req)
}
