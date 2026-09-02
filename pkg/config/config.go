package config

import (
	"bufio"
	"encoding/json"
	"indonesia-stock/pkg/auth"
	"os"
	"path/filepath"
	"strings"
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

func Load() Config {
	redisURL := env("REDIS_URL", "redis://localhost:6379/0")
	token := env("STOCKBIT_TOKEN", "")
	if token == "" {
		// coba redis dulu (source of truth saat serve jalan)
		// pakai lazy import via interface biar tidak circular - fallback ke file jika redis tidak ada
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
	}
	_ = redisURL

	return Config{
		Port:            env("PORT", "8080"),
		StockbitBaseURL: env("STOCKBIT_BASE_URL", "https://api.stockbit.com"),
		StockbitToken:   token,
		RTIBaseURL:      env("RTI_BASE_URL", "https://analytics2.rti.co.id"),
		YahooBaseURL:    env("YAHOO_BASE_URL", "https://query1.finance.yahoo.com"),
		IDXBaseURL:      env("IDX_BASE_URL", "https://www.idx.co.id"),
		LogLevel:        env("LOG_LEVEL", "info"),
		RedisURL:        env("REDIS_URL", "redis://localhost:6379/0"),
		DatabaseURL:     env("DATABASE_URL", "postgres://indostock:indostock@localhost:5432/indostock?sslmode=disable"),
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
