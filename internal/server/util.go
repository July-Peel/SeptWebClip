package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const roomCookie = "swc_room"

func nowMs() int64 { return time.Now().UnixMilli() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

const idAlphabet = "0123456789abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"

func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// roomID 把口令换算成墙的 ID。不保存口令本身。
func (s *Server) roomID(code string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.Secret))
	m.Write([]byte("room:" + code))
	return hex.EncodeToString(m.Sum(nil))[:40]
}

func validRoomID(id string) bool {
	if len(id) != 40 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

type roomHandler func(w http.ResponseWriter, r *http.Request, room string)

func (s *Server) withRoom(h roomHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(roomCookie)
		if err != nil || !validRoomID(c.Value) {
			writeErr(w, http.StatusUnauthorized, "请先输入口令进入便签墙")
			return
		}
		h(w, r, c.Value)
	}
}

// clientIP 取访问者 IP。经 nginx 反代时使用它设置的 X-Real-IP。
func (s *Server) clientIP(r *http.Request) string {
	ip := ""
	if s.cfg.TrustProxy {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
			ip = v
		} else if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 最后一个是最近一层代理追加的，前面的可能被客户端伪造。
			parts := strings.Split(xff, ",")
			ip = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if ip == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip = host
	}
	if len(ip) > 64 {
		ip = ip[:64]
	}
	return ip
}

func userAgent(r *http.Request) string {
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	return ua
}

func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

// rateLimiter 是按 IP 计数的固定窗口限流器，用于防止暴力猜口令。
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*rlEntry
}

type rlEntry struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: map[string]*rlEntry{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > 10000 {
		for k, e := range l.hits {
			if now.After(e.reset) {
				delete(l.hits, k)
			}
		}
	}
	e := l.hits[key]
	if e == nil || now.After(e.reset) {
		e = &rlEntry{reset: now.Add(l.window)}
		l.hits[key] = e
	}
	e.count++
	return e.count <= l.limit
}
