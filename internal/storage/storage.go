// Package storage 保存图片文件，支持本地目录和 WebDAV。
package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
)

var ErrNotFound = errors.New("file not found")

// Storage 以 key（形如 202610/abc.png）存取图片和文件。只写不删。
type Storage interface {
	Name() string
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Serve 把文件写给客户端，支持 Range。调用方需事先设置好 Content-Type 等响应头。
	// 文件不存在时返回 ErrNotFound，且不会写任何响应。
	Serve(w http.ResponseWriter, r *http.Request, key string) error
}
