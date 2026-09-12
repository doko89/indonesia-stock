package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"indonesia-stock/internal/infrastructure/cache"
	"indonesia-stock/pkg/auth"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Port            string
	StockbitBaseURL string
	StockbitToken   string
	RTIBaseURL      string
	YahooBaseURL    string
	IDXBaseURL      string
	LogLevel        string
	RedisURL        string
	DatabaseURL     string
}

var authWiredOnce sync.Once

// wiredCache is the redis client used for auth wiring, or nil when redis is
// unavailable (then the auth package stays in file-lock-only mode).
var wiredCache *cache.Client

// ensureAuthWiring publishes auth hooks only when redis is reachable.
// Every read of wiredCache follows an Once.Do call, which provides the
// happens-before edge for the write inside Do.
func ensureAuthWiring(redisURL string) *cache.Client {
	authWiredOnce.Do(func() {
		c := cache.New(redisURL)
		if !c.Available() {
			return
		}
		wiredCache = c
		auth.SetSecondaryStore(&redisStore{client: c})
		auth.SetLocker(&redisLocker{addr: redisAddr(redisURL)})
	})
	return wiredCache
}

func redisAddr(redisURL string) string {
	addr := "localhost:6379"
	if strings.HasPrefix(redisURL, "redis://") {
		h := strings.TrimPrefix(redisURL, "redis://")
		if idx := strings.Index(h, "/"); idx != -1 {
			h = h[:idx]
		}
		if h != "" {
			addr = h
		}
		if !strings.Contains(addr, ":") {
			addr += ":6379"
		}
	} else if redisURL != "" && strings.Contains(redisURL, ":") {
		addr = redisURL
	}
	return addr
}

type redisStore struct {
	client *cache.Client
}

func (s *redisStore) Save(t *auth.StoredToken) error {
	return auth.SaveToRedis(s.client, t)
}

func (s *redisStore) Load() (*auth.StoredToken, error) {
	return auth.LoadFromRedis(s.client)
}

type redisLocker struct {
	addr string
}

func (l *redisLocker) TryLock(id string, ttl time.Duration) bool {
	conn, err := net.DialTimeout("tcp", l.addr, 800*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	key := "indostock:auth:lock"
	px := fmt.Sprintf("%d", ttl.Milliseconds())
	args := []string{"SET", key, id, "NX", "PX", px}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("*%d\r\n", len(args)))
	for _, a := range args {
		b.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(a), a))
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return false
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSpace(line)
	if line == "+OK" {
		return true
	}
	return false
}

func (l *redisLocker) Unlock(id string) {
	conn, err := net.DialTimeout("tcp", l.addr, 800*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	key := "indostock:auth:lock"
	getCmd := fmt.Sprintf("*2\r\n$3\r\nGET\r\n$%d\r\n%s\r\n", len(key), key)
	if _, err := conn.Write([]byte(getCmd)); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimSpace(line)
	if line == "$-1" {
		return
	}
	if !strings.HasPrefix(line, "$") {
		return
	}
	var n int
	if _, err := fmt.Sscan(line[1:], &n); err != nil {
		return
	}
	buf := make([]byte, n+2)
	if _, err := io.ReadFull(br, buf); err != nil {
		return
	}
	val := string(buf[:n])
	if val != id {
		return
	}
	_ = conn.Close()
	conn2, err := net.DialTimeout("tcp", l.addr, 800*time.Millisecond)
	if err != nil {
		return
	}
	defer conn2.Close()
	_ = conn2.SetDeadline(time.Now().Add(2 * time.Second))
	delCmd := fmt.Sprintf("*2\r\n$3\r\nDEL\r\n$%d\r\n%s\r\n", len(key), key)
	_, _ = conn2.Write([]byte(delCmd))
	br2 := bufio.NewReader(conn2)
	_, _ = br2.ReadString('\n')
}

func Load() Config {
	redisURL := env("REDIS_URL", "redis://localhost:6379/0")
	_ = ensureAuthWiring(redisURL) // wires locker + redis store into pkg/auth

	// NOTE: env STOCKBIT_TOKEN is deliberately IGNORED here too.
	// token.json is the single source of truth; env shadowing burned the
	// live token twice (2026-09-02, 2026-09-12). Read path: file, with
	// auto-rotate via GetValidToken (lock-serialized inside Refresh).
	var token string
	if t, err := auth.Load(); err == nil && t.AccessToken != "" {
		if v, err := auth.GetValidToken(); err == nil {
			token = v
		} else {
			token = t.AccessToken
		}
	}
	if token == "" {
		token = tokenFromFile()
	}

	return Config{
		Port:            env("PORT", "8080"),
		StockbitBaseURL: env("STOCKBIT_BASE_URL", "https://api.stockbit.com"),
		StockbitToken:   token,
		RTIBaseURL:      env("RTI_BASE_URL", "https://analytics2.rti.co.id"),
		YahooBaseURL:    env("YAHOO_BASE_URL", "https://query1.finance.yahoo.com"),
		IDXBaseURL:      env("IDX_BASE_URL", "https://www.idx.co.id"),
		LogLevel:        env("LOG_LEVEL", "info"),
		RedisURL:        env("REDIS_URL", "redis://localhost:6379/0"),
		// no default DSN with a password: require env (Sonar: hardcoded credential)
		DatabaseURL: env("DATABASE_URL", ""),
	}
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func tokenFromFile() string {
	for _, p := range auth.TokenFileCandidates() {
		if b, err := os.ReadFile(p); err == nil {
			s := string(b)
			if len(s) > 20 {
				var j map[string]any
				if json.Unmarshal(b, &j) == nil {
					if v, ok := j["access_token"].(string); ok && len(v) > 50 {
						return v
					}
				}
			}
		}
	}
	candidates := []string{
		"/apps/indostock/.env",
		"/etc/indostock/token",
		"/etc/profile.d/indostock.sh",
	}
	if h := os.Getenv("HOME"); h != "" {
		candidates = append(candidates, filepath.Join(h, ".config", "indostock", "token"), filepath.Join(h, ".indostock_token"))
	}
	for _, p := range candidates {
		if t := readTokenFile(p); t != "" {
			return t
		}
	}
	return ""
}

func readTokenFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "STOCKBIT_TOKEN="); idx != -1 {
			v := strings.TrimSpace(line[idx+len("STOCKBIT_TOKEN="):])
			v = strings.Trim(v, "\"'")
			v = strings.TrimPrefix(v, "export ")
			v = strings.Trim(v, "\"'")
			if strings.HasPrefix(v, "export ") {
				v = strings.TrimSpace(strings.TrimPrefix(v, "export "))
			}
			v = strings.Trim(v, "\"'")
			if v != "" {
				return v
			}
		} else if !strings.Contains(line, "=") && len(line) > 100 {
			return strings.Trim(line, "\"'")
		}
	}
	return ""
}
