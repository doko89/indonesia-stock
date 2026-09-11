package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
	defaultPaths = []string{
		"/apps/indostock/token.json",
		"/etc/indostock/token.json",
	}
	secondaryStore Store
	secondaryMu    sync.RWMutex
	locker         Locker
	lockerMu       sync.RWMutex
)

// Store is a secondary persistence for tokens (e.g. Redis).
type Store interface {
	Save(*StoredToken) error
	Load() (*StoredToken, error)
}

// SetSecondaryStore sets the secondary store used by Refresh (best-effort).
func SetSecondaryStore(s Store) {
	secondaryMu.Lock()
	defer secondaryMu.Unlock()
	secondaryStore = s
}

func getSecondaryStore() Store {
	secondaryMu.RLock()
	defer secondaryMu.RUnlock()
	return secondaryStore
}

// Locker provides distributed lock (e.g. Redis SET NX PX).
type Locker interface {
	TryLock(id string, ttl time.Duration) bool
	Unlock(id string)
}

// SetLocker sets the distributed locker.
func SetLocker(l Locker) {
	lockerMu.Lock()
	defer lockerMu.Unlock()
	locker = l
}

func getLocker() Locker {
	lockerMu.RLock()
	defer lockerMu.RUnlock()
	return locker
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fallback to hex of time
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	// format as hex
	return hex.EncodeToString(b)
}

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
	// also save to secondary store if set (best-effort)
	if s := getSecondaryStore(); s != nil {
		if err := s.Save(nt); err != nil {
			fmt.Fprintf(os.Stderr, "secondary store save failed: %v\n", err)
		}
	}
	return nt, nil
}

// ForceRefresh reloads current token and refreshes even if not expired, guarded by same lock.
func ForceRefresh() (string, error) {
	t, err := loadBestToken()
	if err != nil {
		return "", err
	}
	if t.RefreshToken == "" {
		return "", fmt.Errorf("no refresh_token stored, need browser login once to capture refresh_token")
	}
	locked, release := acquireRefreshLock()
	if !locked {
		// wait polling for lock
		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			locked, release = acquireRefreshLock()
			if locked {
				break
			}
		}
		if !locked {
			return "", fmt.Errorf("could not acquire refresh lock after waiting")
		}
	}
	defer release()
	// reload after acquiring lock to get freshest token
	if nt, err := loadBestToken(); err == nil && nt.RefreshToken != "" {
		t = nt
	}
	nt, err := Refresh(t)
	if err != nil {
		return "", err
	}
	return nt.AccessToken, nil
}

func loadBestToken() (*StoredToken, error) {
	if s := getSecondaryStore(); s != nil {
		if t, err := s.Load(); err == nil && t != nil && t.AccessToken != "" {
			return t, nil
		}
	}
	return Load()
}

// acquireRefreshLock tries Redis locker first, falls back to file lock.
func acquireRefreshLock() (bool, func()) {
	if l := getLocker(); l != nil {
		id := newUUID()
		if l.TryLock(id, 15*time.Second) {
			return true, func() { l.Unlock(id) }
		}
		return false, func() {}
	}
	// file fallback
	return acquireFileLock()
}

func acquireFileLock() (bool, func()) {
	lockPath := "/tmp/indostock_refresh.lock"
	// stale detection: if lockfile older than 30s, remove
	if fi, err := os.Stat(lockPath); err == nil {
		if time.Since(fi.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath)
		}
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, func() {}
	}
	return true, func() {
		f.Close()
		os.Remove(lockPath)
	}
}

func GetValidToken() (string, error) {
	t, err := Load()
	if err != nil {
		return "", err
	}
	if IsExpired(t, 5*time.Minute) {
		if t.RefreshToken != "" {
			locked, release := acquireRefreshLock()
			if locked {
				defer release()
				// reload after acquiring: a peer may have rotated just before us
				src := t
				if cur, err := loadBestToken(); err == nil && cur != nil && cur.RefreshToken != "" {
					if !IsExpired(cur, 5*time.Minute) {
						return cur.AccessToken, nil
					}
					src = cur
				}
				nt, err := Refresh(src)
				if err != nil {
					return "", fmt.Errorf("auto-refresh failed: %w", err)
				}
				return nt.AccessToken, nil
			}
			// lock not acquired: wait up to ~5s polling Load() (secondary first, then file) every 500ms
			for i := 0; i < 10; i++ {
				time.Sleep(500 * time.Millisecond)
				// try secondary store first
				if s := getSecondaryStore(); s != nil {
					if nt, err := s.Load(); err == nil && nt != nil && !IsExpired(nt, 5*time.Minute) {
						return nt.AccessToken, nil
					}
				}
				if nt, err := Load(); err == nil && !IsExpired(nt, 5*time.Minute) {
					return nt.AccessToken, nil
				}
			}
			// after polling, try to acquire lock again and refresh with freshly loaded token
			locked2, release2 := acquireRefreshLock()
			if locked2 {
				defer release2()
				// reload latest token before refreshing (Fix 1: use nt not t)
				if nt, err := loadBestToken(); err == nil && nt != nil && nt.RefreshToken != "" {
					// if nt is still expired, refresh nt
					if IsExpired(nt, 5*time.Minute) {
						refreshed, err := Refresh(nt)
						if err == nil {
							return refreshed.AccessToken, nil
						}
						return "", fmt.Errorf("auto-refresh failed: %w", err)
					}
					return nt.AccessToken, nil
				}
				// never Refresh(t) with the pre-wait token: its refresh token may
				// have been retired by the peer's rotation. Without a freshly
				// loaded refreshable token there is nothing safe to rotate with.
				return "", fmt.Errorf("auto-refresh failed: could not reload a refreshable token")
			}
			// still no lock: try one final load
			if s := getSecondaryStore(); s != nil {
				if nt, err := s.Load(); err == nil && nt != nil && !IsExpired(nt, 5*time.Minute) {
					return nt.AccessToken, nil
				}
			}
			if nt, err := Load(); err == nil && !IsExpired(nt, 5*time.Minute) {
				return nt.AccessToken, nil
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

func SaveToRedis(cacheClient interface {
	Get(string) (string, error)
	SetEx(string, string, time.Duration) error
}, t *StoredToken) error {
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

func GetValidTokenRedis(cacheClient interface {
	Get(string) (string, error)
	SetEx(string, string, time.Duration) error
}) (string, error) {
	if cacheClient != nil {
		if t, err := LoadFromRedis(cacheClient); err == nil && t.AccessToken != "" {
			if !IsExpired(t, 5*time.Minute) {
				return t.AccessToken, nil
			}
			if t.RefreshToken != "" {
				if locked, release := acquireRefreshLock(); locked {
					// reload freshest under lock: a peer may have rotated
					// already, and t's refresh token may be retired.
					src := t
					if cur, err := LoadFromRedis(cacheClient); err == nil && cur.RefreshToken != "" {
						src = cur
					} else if cur, err := Load(); err == nil && cur.RefreshToken != "" {
						src = cur
					}
					nt, err := Refresh(src)
					release()
					if err == nil {
						_ = SaveToRedis(cacheClient, nt)
						_ = Save(nt, "")
						return nt.AccessToken, nil
					}
				} else {
					// peer is refreshing: wait briefly, then use a freshly
					// loaded token — never rotate with the stale t.
					time.Sleep(1200 * time.Millisecond)
					if nt, err := LoadFromRedis(cacheClient); err == nil && !IsExpired(nt, 5*time.Minute) {
						return nt.AccessToken, nil
					}
					if nt, err := Load(); err == nil && !IsExpired(nt, 5*time.Minute) {
						return nt.AccessToken, nil
					}
				}
			}
			if !IsExpired(t, 0) {
				return t.AccessToken, nil
			}
		}
	}
	return GetValidToken()
}
