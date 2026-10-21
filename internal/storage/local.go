package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

type Local struct{ dir string }

func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Local{dir: dir}, nil
}

func (l *Local) Name() string { return "local" }

func (l *Local) path(key string) string { return filepath.Join(l.dir, filepath.FromSlash(key)) }

func (l *Local) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	dst := l.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// 先写临时文件再改名，避免读到写了一半的图片。
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (l *Local) Serve(w http.ResponseWriter, r *http.Request, key string) error {
	f, err := os.Open(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}
