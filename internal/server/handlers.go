package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"septwebclip/internal/storage"
	"septwebclip/internal/store"
)

const maxThumbBytes = 2 << 20

// ---- 进入 / 离开一面墙 ----

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	entered := false
	if c, err := r.Cookie(roomCookie); err == nil && validRoomID(c.Value) {
		entered = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entered":       entered,
		"ttlMs":         s.cfg.ClipTTL.Milliseconds(),
		"maxImageBytes": s.cfg.MaxImageBytes,
		"maxFileBytes":  s.cfg.MaxFileBytes,
		"maxTextBytes":  s.cfg.MaxTextBytes,
		"minCodeLen":    s.cfg.MinCodeLen,
	})
}

func (s *Server) handleEnter(w http.ResponseWriter, r *http.Request) {
	if !s.enterLimit.allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "尝试太频繁了，请过几分钟再试")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	code := strings.TrimSpace(req.Code)
	n := utf8.RuneCountInString(code)
	if n < s.cfg.MinCodeLen || n > 64 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("口令长度需要在 %d 到 64 个字符之间", s.cfg.MinCodeLen))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     roomCookie,
		Value:    s.roomID(code),
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		Secure:   s.isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: roomCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.isHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- 便签 ----

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, room string) {
	clips, err := s.db.ListClips(r.Context(), room, nowMs())
	if err != nil {
		s.internalErr(w, "读取便签失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clips": clips, "now": nowMs()})
}

func (s *Server) newClip(r *http.Request, room, kind string, pinned bool) *store.Clip {
	now := nowMs()
	return &store.Clip{
		ID:        newID(10),
		Room:      room,
		Kind:      kind,
		Pinned:    pinned,
		CreatedAt: now,
		ExpiresAt: now + s.cfg.ClipTTL.Milliseconds(),
		CreatorIP: s.clientIP(r),
	}
}

func (s *Server) handleCreateText(w http.ResponseWriter, r *http.Request, room string) {
	var req struct {
		Content string `json:"content"`
		Pinned  bool   `json:"pinned"`
	}
	// JSON 转义会让正文变长，这里放宽一些，后面再按真实长度校验。
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxTextBytes*6+4096)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, s.textTooLong())
			return
		}
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	switch {
	case strings.TrimSpace(req.Content) == "":
		writeErr(w, http.StatusBadRequest, "内容是空的")
		return
	case int64(len(req.Content)) > s.cfg.MaxTextBytes:
		writeErr(w, http.StatusRequestEntityTooLarge, s.textTooLong())
		return
	case !utf8.ValidString(req.Content):
		writeErr(w, http.StatusBadRequest, "文字编码不正确")
		return
	}

	c := s.newClip(r, room, "text", req.Pinned)
	c.Content = req.Content
	if err := s.db.CreateClip(r.Context(), c, userAgent(r)); err != nil {
		s.internalErr(w, "保存便签失败", err)
		return
	}
	s.hub.publish(room, event{Type: "create", Clip: c})
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) textTooLong() string {
	return fmt.Sprintf("文字太长了，上限是 %d KB", s.cfg.MaxTextBytes>>10)
}

// spooled 是落到临时文件里的一个上传分段。
type spooled struct {
	f       *os.File
	size    int64
	sniffed string
	name    string
	claimed string
}

func (sp *spooled) cleanup() {
	if sp != nil {
		sp.f.Close()
		os.Remove(sp.f.Name())
	}
}

var errTooLarge = errors.New("too large")

func (s *Server) spool(r io.Reader, limit int64) (*spooled, error) {
	f, err := os.CreateTemp(s.tmpDir, "up-*")
	if err != nil {
		return nil, err
	}
	sp := &spooled{f: f}
	head := &bytes.Buffer{}
	n, err := io.Copy(io.MultiWriter(f, &limitedBuffer{buf: head, n: 512}), io.LimitReader(r, limit+1))
	if err == nil && n > limit {
		err = errTooLarge
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		sp.cleanup()
		return nil, err
	}
	sp.size = n
	sp.sniffed = http.DetectContentType(head.Bytes())
	return sp, nil
}

// limitedBuffer 只保留前 n 个字节，用于判断文件类型。
type limitedBuffer struct {
	buf *bytes.Buffer
	n   int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

var imageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"image/bmp":  ".bmp",
	"image/avif": ".avif",
	"image/heic": ".heic",
	"image/heif": ".heif",
}

// imageType 以文件内容为准判断图片类型；HEIC/AVIF 无法嗅探，才参考浏览器声明的类型。
// 不接受 SVG，避免脚本注入。
func imageType(sniffed, claimed string) (string, bool) {
	switch sniffed {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "image/avif":
		return sniffed, true
	case "application/octet-stream":
		switch claimed {
		case "image/heic", "image/heif", "image/avif":
			return claimed, true
		}
	}
	return "", false
}

// uploadForm 是解析好的上传表单：主文件、可选的缩略图、是否钉住。
type uploadForm struct {
	main, thumb *spooled
	pinned      bool
}

func (u *uploadForm) cleanup() { u.main.cleanup(); u.thumb.cleanup() }

func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.Is(err, errTooLarge) || errors.As(err, &mbe)
}

// readUpload 以流的方式把 multipart 表单落到临时文件，不占用大块内存。
// 出错时已经写好响应，返回 false。
func (s *Server) readUpload(w http.ResponseWriter, r *http.Request, maxMain int64, tooLargeMsg string) (*uploadForm, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMain+maxThumbBytes+1<<16)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return nil, false
	}

	u := &uploadForm{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			u.cleanup()
			if isTooLarge(err) {
				writeErr(w, http.StatusRequestEntityTooLarge, tooLargeMsg)
			} else {
				writeErr(w, http.StatusBadRequest, "上传中断了，请重试")
			}
			return nil, false
		}
		switch part.FormName() {
		case "pinned":
			b, _ := io.ReadAll(io.LimitReader(part, 8))
			u.pinned = string(b) == "1" || string(b) == "true"
		case "file":
			if u.main == nil {
				if u.main, err = s.spool(part, maxMain); err == nil {
					u.main.name, u.main.claimed = part.FileName(), part.Header.Get("Content-Type")
				}
			}
		case "thumb":
			if u.thumb == nil {
				u.thumb, err = s.spool(part, maxThumbBytes)
			}
		}
		part.Close()
		if err != nil {
			u.cleanup()
			if isTooLarge(err) {
				writeErr(w, http.StatusRequestEntityTooLarge, tooLargeMsg)
			} else {
				s.internalErr(w, "接收文件失败", err)
			}
			return nil, false
		}
	}

	if u.main == nil || u.main.size == 0 {
		u.cleanup()
		writeErr(w, http.StatusBadRequest, "没有收到文件，或者文件是空的")
		return nil, false
	}
	return u, true
}

// datePrefix 按年月分文件夹，例如 202610/，图片和文件放在一起。
func datePrefix(ms int64) string { return time.UnixMilli(ms).Format("200601") + "/" }

func (s *Server) handleCreateImage(w http.ResponseWriter, r *http.Request, room string) {
	u, ok := s.readUpload(w, r, s.cfg.MaxImageBytes, fmt.Sprintf("图片超过 %d MB 上限", s.cfg.MaxImageBytes>>20))
	if !ok {
		return
	}
	defer u.cleanup()

	mimeType, ok := imageType(u.main.sniffed, u.main.claimed)
	if !ok {
		writeErr(w, http.StatusUnsupportedMediaType, "不支持这种图片格式（支持 PNG、JPG、GIF、WebP、BMP、HEIC、AVIF）")
		return
	}

	c := s.newClip(r, room, "image", u.pinned)
	dir := datePrefix(c.CreatedAt)
	c.FileKey = dir + c.ID + imageExt[mimeType]
	c.Mime = mimeType
	c.Size = u.main.size
	c.FileName = cleanFileName(u.main.name)

	if err := s.files.Put(r.Context(), c.FileKey, u.main.f, u.main.size, mimeType); err != nil {
		s.internalErr(w, "保存图片失败", err)
		return
	}
	// 缩略图由浏览器生成，失败不影响主图。
	if thumb := u.thumb; thumb != nil {
		if ext, ok := thumbExt[thumb.sniffed]; ok {
			key := dir + c.ID + "_t" + ext
			if err := s.files.Put(r.Context(), key, thumb.f, thumb.size, thumb.sniffed); err == nil {
				c.ThumbKey = key
			} else {
				slog.Warn("保存缩略图失败", "err", err)
			}
		}
	}

	if err := s.db.CreateClip(r.Context(), c, userAgent(r)); err != nil {
		s.internalErr(w, "保存便签失败", err)
		return
	}
	s.hub.publish(room, event{Type: "create", Clip: c})
	writeJSON(w, http.StatusCreated, c)
}

var thumbExt = map[string]string{"image/jpeg": ".jpg", "image/webp": ".webp", "image/png": ".png"}

// handleCreateFile 接收任意类型的文件。文件只作为附件下载，从不在页面里直接打开。
func (s *Server) handleCreateFile(w http.ResponseWriter, r *http.Request, room string) {
	u, ok := s.readUpload(w, r, s.cfg.MaxFileBytes, fmt.Sprintf("文件超过 %d MB 上限", s.cfg.MaxFileBytes>>20))
	if !ok {
		return
	}
	defer u.cleanup()

	c := s.newClip(r, room, "file", u.pinned)
	c.FileName = cleanFileName(u.main.name)
	if c.FileName == "" {
		c.FileName = "未命名文件"
	}
	c.FileKey = datePrefix(c.CreatedAt) + c.ID + fileExt(c.FileName)
	c.Mime = fileMime(u.main.claimed)
	c.Size = u.main.size

	if err := s.files.Put(r.Context(), c.FileKey, u.main.f, u.main.size, "application/octet-stream"); err != nil {
		s.internalErr(w, "保存文件失败", err)
		return
	}
	if err := s.db.CreateClip(r.Context(), c, userAgent(r)); err != nil {
		s.internalErr(w, "保存便签失败", err)
		return
	}
	s.hub.publish(room, event{Type: "create", Clip: c})
	writeJSON(w, http.StatusCreated, c)
}

// fileExt 取文件名里安全的扩展名用于存储，取不到就用 .bin。
func fileExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if len(ext) < 2 || len(ext) > 12 {
		return ".bin"
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ".bin"
		}
	}
	return ext
}

// fileMime 只记录浏览器声明的类型用于展示，下载时一律按二进制流返回。
func fileMime(claimed string) string {
	mt, _, err := mime.ParseMediaType(claimed)
	if err != nil || mt == "" || len(mt) > 100 {
		return "application/octet-stream"
	}
	return mt
}

func cleanFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if utf8.RuneCountInString(name) > 120 {
		name = string([]rune(name)[:120])
	}
	return name
}

// clipInRoom 读取便签并确认它属于当前这面墙，否则直接返回 404。
func (s *Server) clipInRoom(w http.ResponseWriter, r *http.Request, room string) (*store.Clip, bool) {
	c, err := s.db.GetClip(r.Context(), r.PathValue("id"), nowMs())
	if err == nil && c.Room == room {
		return c, true
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalErr(w, "读取便签失败", err)
		return nil, false
	}
	writeErr(w, http.StatusNotFound, "这张便签不存在或已经过期")
	return nil, false
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request, room string) {
	c, ok := s.clipInRoom(w, r, room)
	if !ok {
		return
	}
	if c.Kind != "image" {
		writeErr(w, http.StatusNotFound, "这张便签没有图片")
		return
	}
	q := r.URL.Query()
	key, ctype := c.FileKey, c.Mime
	if q.Has("thumb") && c.ThumbKey != "" {
		key, ctype = c.ThumbKey, "image/jpeg"
		for t, ext := range thumbExt {
			if ext == path.Ext(key) {
				ctype = t
			}
		}
	}

	s.sendStored(w, r, room, c, key, ctype, q.Has("dl"))
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request, room string) {
	c, ok := s.clipInRoom(w, r, room)
	if !ok {
		return
	}
	if c.Kind != "file" {
		writeErr(w, http.StatusNotFound, "这张便签没有文件")
		return
	}
	// 任意文件都按二进制附件返回，避免 HTML 之类的文件在本站点里被浏览器执行。
	s.sendStored(w, r, room, c, c.FileKey, "application/octet-stream", true)
}

// sendStored 把存储里的文件发给客户端；download 为 true 时作为附件下载并计数。
func (s *Server) sendStored(w http.ResponseWriter, r *http.Request, room string, c *store.Clip, key, ctype string, download bool) {
	if download {
		// 断点续传的后续分段不重复计数。
		if rg := r.Header.Get("Range"); rg == "" || strings.HasPrefix(rg, "bytes=0-") {
			s.record(r, room, c.ID, "download", 0)
		}
		disp := mime.FormatMediaType("attachment", map[string]string{"filename": downloadName(c)})
		if disp == "" {
			disp = "attachment"
		}
		w.Header().Set("Content-Disposition", disp)
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	w.Header().Set("Content-Type", ctype)

	if err := s.files.Serve(w, r, key); err != nil {
		w.Header().Del("Content-Disposition")
		if errors.Is(err, storage.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "文件不见了")
			return
		}
		slog.Error("读取文件失败", "key", key, "err", err)
		writeErr(w, http.StatusBadGateway, "读取文件失败")
	}
}

func downloadName(c *store.Clip) string {
	if c.Kind == "file" {
		return c.FileName
	}
	name := c.FileName
	if name == "" || strings.HasPrefix(strings.ToLower(name), "image.") {
		name = fmt.Sprintf("SeptWebClip-%03d%s", c.Seq, imageExt[c.Mime])
	}
	return name
}

// record 记一条日志，记录成功时把最新计数推送给整面墙。
func (s *Server) record(r *http.Request, room, id, action string, dedupe time.Duration) (*store.Clip, error) {
	c, recorded, err := s.db.Record(r.Context(), id, action, s.clientIP(r), userAgent(r), nowMs(), dedupe.Milliseconds())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("记录日志失败", "action", action, "err", err)
		}
		return nil, err
	}
	if recorded {
		s.hub.publish(room, event{Type: "update", Clip: c})
	}
	return c, nil
}

func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request, room string) {
	c, ok := s.clipInRoom(w, r, room)
	if !ok {
		return
	}
	if c, err := s.record(r, room, c.ID, "copy", 0); err == nil {
		writeJSON(w, http.StatusOK, c)
		return
	}
	writeErr(w, http.StatusNotFound, "这张便签不存在或已经过期")
}

func (s *Server) handleViews(w http.ResponseWriter, r *http.Request, room string) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if len(req.IDs) > 100 {
		req.IDs = req.IDs[:100]
	}
	now := nowMs()
	for _, id := range req.IDs {
		c, err := s.db.GetClip(r.Context(), id, now)
		if err != nil || c.Room != room {
			continue
		}
		s.record(r, room, id, "view", s.cfg.ViewDedup)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request, room string) {
	c, ok := s.clipInRoom(w, r, room)
	if !ok {
		return
	}
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	c, err := s.db.SetPinned(r.Context(), c.ID, req.Pinned, s.cfg.ClipTTL.Milliseconds(), nowMs(),
		s.clientIP(r), userAgent(r))
	if err != nil {
		s.internalErr(w, "修改失败", err)
		return
	}
	s.hub.publish(room, event{Type: "update", Clip: c})
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, room string) {
	c, ok := s.clipInRoom(w, r, room)
	if !ok {
		return
	}
	if err := s.db.DeleteClip(r.Context(), c.ID); err != nil {
		s.internalErr(w, "删除失败", err)
		return
	}
	slog.Info("便签被删除", "id", c.ID, "kind", c.Kind, "ip", s.clientIP(r))
	s.hub.publish(room, event{Type: "delete", ID: c.ID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, room string) {
	c, ok := s.clipInRoom(w, r, room)
	if !ok {
		return
	}
	logs, err := s.db.Logs(r.Context(), c.ID, 500)
	if err != nil {
		s.internalErr(w, "读取日志失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clip": c, "logs": logs})
}

func (s *Server) internalErr(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, "err", err)
	writeErr(w, http.StatusInternalServerError, msg)
}
