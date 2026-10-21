// Package config 从环境变量读取运行配置。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultFontCSS 是默认的手写字体（霞鹜文楷屏幕阅读版，按 unicode-range 切片加载）。
const DefaultFontCSS = "https://cdn.jsdelivr.net/npm/lxgw-wenkai-screen-webfont@1.7.0/lxgwwenkaigbscreen.css"

type Config struct {
	ListenAddr string
	DataDir    string

	ImageStorage   string // local | webdav
	ImageDir       string
	WebDAVURL      string
	WebDAVUser     string
	WebDAVPassword string

	ClipTTL         time.Duration // 未钉住的便签多久后自动删除
	CleanupInterval time.Duration
	ViewDedup       time.Duration // 同一设备在此时间内重复查看只记一次

	MaxImageBytes int64
	MaxFileBytes  int64
	MaxTextBytes  int64
	MinCodeLen    int

	TrustProxy bool // 是否信任反向代理传来的 X-Real-IP / X-Forwarded-*
	Secret     string
	FontCSSURL string
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:     env("LISTEN_ADDR", ":8080"),
		DataDir:        env("DATA_DIR", "data"),
		ImageStorage:   strings.ToLower(env("IMAGE_STORAGE", "local")),
		WebDAVURL:      env("WEBDAV_URL", ""),
		WebDAVUser:     env("WEBDAV_USER", ""),
		WebDAVPassword: env("WEBDAV_PASSWORD", ""),
		Secret:         env("SECRET", ""),
		FontCSSURL:     env("FONT_CSS_URL", DefaultFontCSS),
	}
	c.ImageDir = env("IMAGE_DIR", filepath.Join(c.DataDir, "images"))

	var errs []string
	dur := func(key, def string) time.Duration {
		v := env(key, def)
		d, err := ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("%s=%q 不是有效的时长（例：3d、72h、30m）", key, v))
		}
		return d
	}
	num := func(key string, def int) int {
		v := env(key, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("%s=%q 必须是正整数", key, v))
		}
		return n
	}

	c.ClipTTL = dur("CLIP_TTL", "3d")
	c.CleanupInterval = dur("CLEANUP_INTERVAL", "10m")
	c.ViewDedup = dur("VIEW_DEDUP", "30m")
	c.MaxImageBytes = int64(num("MAX_IMAGE_MB", 20)) << 20
	c.MaxFileBytes = int64(num("MAX_FILE_MB", 50)) << 20
	c.MaxTextBytes = int64(num("MAX_TEXT_KB", 1024)) << 10
	c.MinCodeLen = num("MIN_CODE_LEN", 4)

	switch strings.ToLower(env("TRUST_PROXY", "false")) {
	case "1", "true", "yes", "on":
		c.TrustProxy = true
	case "0", "false", "no", "off":
	default:
		errs = append(errs, "TRUST_PROXY 只能是 true 或 false")
	}

	switch c.ImageStorage {
	case "local":
	case "webdav":
		if !isHTTPURL(c.WebDAVURL) {
			errs = append(errs, "IMAGE_STORAGE=webdav 时必须设置有效的 WEBDAV_URL（http:// 或 https://）")
		}
	default:
		errs = append(errs, "IMAGE_STORAGE 只能是 local 或 webdav")
	}
	if c.FontCSSURL != "" && !isHTTPURL(c.FontCSSURL) {
		errs = append(errs, "FONT_CSS_URL 必须是 http(s) 地址，留空则使用系统字体")
	}

	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "；"))
	}
	return c, nil
}

// EnsureSecret 在未配置 SECRET 时，从数据目录读取或生成一个随机密钥。
// 口令通过它换算成墙的 ID，更换密钥会让所有旧墙无法再进入。
func (c *Config) EnsureSecret() error {
	if c.Secret != "" {
		return nil
	}
	p := filepath.Join(c.DataDir, "secret.key")
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 32 {
			c.Secret = s
			return nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	c.Secret = hex.EncodeToString(buf)
	return os.WriteFile(p, []byte(c.Secret+"\n"), 0o600)
}

// ParseDuration 在 time.ParseDuration 的基础上支持 "3d" 这种按天的写法。
func ParseDuration(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, ok := strings.CutSuffix(s, "d"); ok {
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(f * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
