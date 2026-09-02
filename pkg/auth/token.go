package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type StoredToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	UserID       int64     `json:"user_id"`
}

var (
	mu           sync.RWMutex
	cached       *StoredToken
	defaultPaths = []string{
		"/apps/indostock/token.json",
		"/etc/indostock/token.json",
	}
)

func TokenFileCandidates() []string {
	paths := append([]string{}, defaultPaths...)
	if h := os.Getenv("HOME"); h != "" {
		paths = append(paths, filepath.Join(h, ".config", "indostock", "token.json"))
		paths = append(paths, filepath.Join(h, ".indostock", "token.json"))
		paths = append(paths, filepath.Join(h, ".config", "indostock", "token"))
	}
	paths = append(paths, "./token.json")
	return paths
}

func FindTokenFile() string {
	for _, p := range TokenFileCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func Load() (*StoredToken, error) {
	if v := os.Getenv("STOCKBIT_TOKEN"); v != "" {
		rf := os.Getenv("STOCKBIT_REFRESH_TOKEN")
		t := &StoredToken{AccessToken: strings.TrimSpace(v), RefreshToken: strings.TrimSpace(rf)}
		if t.AccessToken != "" {
			exp := jwtExpiry(t.AccessToken)
			if !exp.IsZero() {
				t.ExpiresAt = exp
			} else {
				t.ExpiresAt = time.Now().Add(24 * time.Hour)
			}
			return t, nil
		}
	}
	path := FindTokenFile()
	if path == "" {
		return nil, fmt.Errorf("token file not found, checked %v and env STOCKBIT_TOKEN", TokenFileCandidates())
	}
	return LoadFrom(path)
}

func LoadFrom(path string) (*StoredToken, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "{") {
		var t StoredToken
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("parse token json %s: %w", path, err)
		}
		if t.AccessToken == "" {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				if v, ok := m["data"].(map[string]any); ok {
					if tok, ok := v["access_token"].(string); ok {
						t.AccessToken = tok
					}
				}
			}
		}
		if t.ExpiresAt.IsZero() && t.AccessToken != "" {
			if exp := jwtExpiry(t.AccessToken); !exp.IsZero() {
				t.ExpiresAt = exp
			}
		}
		return &t, nil
	}
	// plain token or env style
	tok := strings.Trim(strings.TrimSpace(s), "\"'")
	if strings.Contains(tok, "STOCKBIT_TOKEN=") {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if idx := strings.Index(line, "STOCKBIT_TOKEN="); idx != -1 {
				v := strings.TrimSpace(line[idx+len("STOCKBIT_TOKEN="):])
				v = strings.Trim(v, "\"'")
				v = strings.TrimPrefix(v, "export ")
				v = strings.Trim(v, "\"' ")
				if v != "" {
					tok = v
					break
				}
			}
		}
	}
	if tok == "" {
		return nil, fmt.Errorf("empty token in %s", path)
	}
	t := &StoredToken{AccessToken: tok}
	if exp := jwtExpiry(tok); !exp.IsZero() {
		t.ExpiresAt = exp
	} else {
		t.ExpiresAt = time.Now().Add(24 * time.Hour)
	}
	return t, nil
}

func Save(t *StoredToken, path string) error {
	if t.ExpiresAt.IsZero() && t.AccessToken != "" {
		if exp := jwtExpiry(t.AccessToken); !exp.IsZero() {
			t.ExpiresAt = exp
		} else {
			t.ExpiresAt = time.Now().Add(24 * time.Hour)
		}
	}
	if path == "" {
		if found := FindTokenFile(); found != "" {
			path = found
		} else if h := os.Getenv("HOME"); h != "" {
			path = filepath.Join(h, ".config", "indostock", "token.json")
			if h == "/root" || h == "" {
				path = defaultPaths[0]
			}
		} else {
			path = defaultPaths[0]
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	b, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(b, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}

func IsExpired(t *StoredToken, buffer time.Duration) bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	if t.ExpiresAt.IsZero() {
		if exp := jwtExpiry(t.AccessToken); !exp.IsZero() {
			return time.Now().Add(buffer).After(exp)
		}
		return false
	}
	return time.Now().Add(buffer).After(t.ExpiresAt)
}

type refreshResp struct {
	Data struct {
		Refresh struct {
			Access struct {
				Token     string `json:"token"`
				ExpiredAt string `json:"expired_at"`
			} `json:"access"`
			Refresh struct {
				Token     string `json:"token"`
				ExpiredAt string `json:"expired_at"`
			} `json:"refresh"`
		} `json:"refresh"`
	} `json:"data"`
}

func Refresh(t *StoredToken) (*StoredToken, error) {
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh_token stored, need browser login once to capture refresh_token")
	}
	req, err := http.NewRequest("POST", "https://exodus.stockbit.com/login/refresh", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.RefreshToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124 Safari/537.36")
	req.Header.Set("Origin", "https://stockbit.com")
	req.Header.Set("Referer", "https://stockbit.com/")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		n := len(body)
		if n > 800 {
			n = 800
		}
		return nil, fmt.Errorf("refresh failed %d: %s", resp.StatusCode, string(body[:n]))
	}
	var rr refreshResp
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, fmt.Errorf("decode refresh: %w", err)
	}
	nt := &StoredToken{
		AccessToken:  rr.Data.Refresh.Access.Token,
		RefreshToken: rr.Data.Refresh.Refresh.Token,
		UserID:       t.UserID,
	}
	if rr.Data.Refresh.Access.ExpiredAt != "" {
		if ts, err := time.Parse(time.RFC3339, rr.Data.Refresh.Access.ExpiredAt); err == nil {
			nt.ExpiresAt = ts.UTC()
		}
	}
	if nt.ExpiresAt.IsZero() {
		if exp := jwtExpiry(nt.AccessToken); !exp.IsZero() {
			nt.ExpiresAt = exp
		} else {
			nt.ExpiresAt = time.Now().Add(24 * time.Hour)
		}
	}
	if nt.AccessToken == "" {
		return nil, fmt.Errorf("refresh returned empty access token")
	}
	// prefer existing file location
	path := FindTokenFile()
	if path == "" {
		path = TokenFileCandidates()[0]
	}
	_ = Save(nt, path)
	mu.Lock()
	cached = nt
	mu.Unlock()
	return nt, nil
}

func GetValidToken() (string, error) {
	t, err := Load()
	if err != nil {
		return "", err
	}
	if IsExpired(t, 5*time.Minute) {
		if t.RefreshToken != "" {
			lockPath := "/tmp/indostock_refresh.lock"
			if f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0600); err == nil {
				defer func() { f.Close(); os.Remove(lockPath) }()
				nt, err := Refresh(t)
				if err != nil {
					return "", fmt.Errorf("auto-refresh failed: %w", err)
				}
				return nt.AccessToken, nil
			} else {
				// another process refreshing, wait 1s and reload
				time.Sleep(1200 * time.Millisecond)
				if nt, err := Load(); err == nil && !IsExpired(nt, 5*time.Minute) {
					return nt.AccessToken, nil
				}
				if t.RefreshToken != "" {
					nt, err := Refresh(t)
					if err == nil {
						return nt.AccessToken, nil
					}
				}
			}
		}
		if IsExpired(t, 0) {
			return "", fmt.Errorf("token expired at %s and no refresh_token, re-login via browser required", t.ExpiresAt.Format(time.RFC3339))
		}
	}
	return t.AccessToken, nil
}

func Status() map[string]any {
	t, err := Load()
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "checked": TokenFileCandidates()}
	}
	exp := t.ExpiresAt
	remaining := time.Until(exp)
	valid := !IsExpired(t, 0)
	willRefresh := IsExpired(t, 5*time.Minute)
	jwtExp := jwtExpiry(t.AccessToken)
	return map[string]any{
		"ok":                valid,
		"has_refresh":       t.RefreshToken != "",
		"expires_at":        exp.Format(time.RFC3339),
		"expires_in":        remaining.String(),
		"will_auto_refresh": willRefresh,
		"jwt_exp":           jwtExp.Format(time.RFC3339),
		"user_id":           t.UserID,
		"file":              FindTokenFile(),
		"token_len":         len(t.AccessToken),
		"refresh_len":       len(t.RefreshToken),
	}
}

const RedisKey = "indostock:auth:stockbit"

func SaveToRedis(cacheClient interface{ Get(string) (string, error); SetEx(string, string, time.Duration) error }, t *StoredToken) error {
	if t.ExpiresAt.IsZero() && t.AccessToken != "" {
		if exp := jwtExpiry(t.AccessToken); !exp.IsZero() {
			t.ExpiresAt = exp
		}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	ttl := time.Until(t.ExpiresAt)
	if ttl < 0 {
		ttl = 0
	}
	if ttl > 0 && ttl < time.Hour {
		ttl = time.Hour
	}
	return cacheClient.SetEx(RedisKey, string(b), ttl)
}

func LoadFromRedis(cacheClient interface{ Get(string) (string, error) }) (*StoredToken, error) {
	s, err := cacheClient.Get(RedisKey)
	if err != nil {
		return nil, err
	}
	var t StoredToken
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return nil, err
	}
	if t.AccessToken == "" {
		return nil, fmt.Errorf("empty token in redis")
	}
	return &t, nil
}

func GetValidTokenRedis(cacheClient interface{ Get(string) (string, error); SetEx(string, string, time.Duration) error }) (string, error) {
	if cacheClient != nil {
		if t, err := LoadFromRedis(cacheClient); err == nil && t.AccessToken != "" {
			if !IsExpired(t, 5*time.Minute) {
				return t.AccessToken, nil
			}
			if t.RefreshToken != "" {
				nt, err := Refresh(t)
				if err == nil {
					_ = SaveToRedis(cacheClient, nt)
					_ = Save(nt, "")
					return nt.AccessToken, nil
				}
			}
			if !IsExpired(t, 0) {
				return t.AccessToken, nil
			}
		}
	}
	return GetValidToken()
}

