package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// EnvTokenPrefix is the .env key prefix for the Stockbit token; exported so
// pkg/config reuses the same literal (Sonar: duplicated string literals).
const EnvTokenPrefix = envTokenPrefix

// Shared path/literal constants (Sonar: duplicated string literals).
const (
	tokenFileName   = "token.json"
	configDirName   = ".config"
	appDirName      = "indostock"
	envTokenPrefix  = "STOCKBIT_TOKEN="
	exportPrefix    = "export "
	journalGlob     = "refresh_journal_*.json"
	journalTimeFmt  = "20060102T150405"
	journalKeepLast = 10
)

var (
	defaultPaths = []string{
		"/apps/indostock/" + tokenFileName,
		"/etc/indostock/" + tokenFileName,
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
		paths = append(paths, filepath.Join(h, configDirName, appDirName, tokenFileName))
		paths = append(paths, filepath.Join(h, ".indostock", tokenFileName))
		paths = append(paths, filepath.Join(h, configDirName, appDirName, "token"))
	}
	paths = append(paths, "./"+tokenFileName)
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
	// NOTE: env STOCKBIT_TOKEN support was REMOVED on purpose.
	// An env var with a stale token shadowed the on-disk token twice
	// (2026-09-02 and 2026-09-12 incidents) and killed auto-refresh.
	// token.json (0600) is the single source of truth; Redis is a cache.
	path := FindTokenFile()
	if path == "" {
		return nil, fmt.Errorf("token file not found, checked %v — save with: indostock auth save --token <JWT> --refresh <JWT>", TokenFileCandidates())
	}
	return LoadFrom(path)
}

// parseStoredJSON parses token.json content: either our StoredToken shape or
// an exported login-response with data.access_token.
func parseStoredJSON(b []byte, path string) (*StoredToken, error) {
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

// tokenFromEnvStyle extracts STOCKBIT_TOKEN from a pasted .env-style file.
func tokenFromEnvStyle(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, envTokenPrefix); idx != -1 {
			v := strings.TrimSpace(line[idx+len(envTokenPrefix):])
			v = strings.Trim(v, "\"'")
			v = strings.TrimPrefix(v, exportPrefix)
			v = strings.Trim(v, "\"' ")
			if v != "" {
				return v
			}
		}
	}
	return ""
}

func LoadFrom(path string) (*StoredToken, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "{") {
		return parseStoredJSON(b, path)
	}
	// plain token or env style
	tok := strings.Trim(s, "\"'")
	if strings.Contains(tok, envTokenPrefix) {
		tok = tokenFromEnvStyle(s)
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

// resolveTokenPath picks where token.json lives: explicit path, existing
// file, or HOME/default candidates.
func resolveTokenPath(path string) string {
	if path != "" {
		return path
	}
	if found := FindTokenFile(); found != "" {
		return found
	}
	if h := os.Getenv("HOME"); h != "" {
		p := filepath.Join(h, configDirName, appDirName, tokenFileName)
		if h == "/root" || h == "" {
			return defaultPaths[0]
		}
		return p
	}
	return defaultPaths[0]
}

// ensureExpiry fills ExpiresAt from the AT JWT (or 24h fallback).
func ensureExpiry(t *StoredToken) {
	if !t.ExpiresAt.IsZero() || t.AccessToken == "" {
		return
	}
	if exp := jwtExpiry(t.AccessToken); !exp.IsZero() {
		t.ExpiresAt = exp
		return
	}
	t.ExpiresAt = time.Now().Add(24 * time.Hour)
}

func Save(t *StoredToken, path string) error {
	ensureExpiry(t)
	path = resolveTokenPath(path)
	dir := filepath.Dir(path)
	// Sonar S5443: the token dir holds 0600 secrets, so never create it
	// world-accessible. MkdirAll only sets perms on created dirs — existing
	// user setups keep their current mode, so this changes nothing for them.
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	var claims jwtClaims
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

// jwtClaims is the minimal JWT payload the auth package inspects.
type jwtClaims struct {
	Exp int64 `json:"exp"`
}

// tokenHalf is one side (access or refresh) of a token pair inside a
// refresh response.
type tokenHalf struct {
	Token     string `json:"token"`
	ExpiredAt string `json:"expired_at"`
}

// tokenPayload is the nested access/refresh pair inside a refresh response
// (named type per Sonar: anonymous struct extraction; also used by the
// L251/L255 findings — same shape reused three times).
type tokenPayload struct {
	Access  tokenHalf `json:"access"`
	Refresh tokenHalf `json:"refresh"`
}

// refreshData is the "data" object of a /login/refresh response body.
type refreshData struct {
	Refresh   tokenPayload `json:"refresh"`
	TokenData tokenPayload `json:"token_data"`
}

// refreshEnvelope wraps tokenPayload shapes seen from /login/refresh.
type refreshEnvelope struct {
	Data refreshData `json:"data"`
}

// extractTokens pulls the rotated access/refresh token pair out of a refresh
// response body, tolerating multiple API shapes:
//  1. {"data":{"refresh":{"access":{"token":...,"expired_at":...},"refresh":{"token":...}}}}
//  2. {"data":{"token_data":{"access":{"token":...,"expired_at":...},"refresh":{"token":...}}}}
//  3. {"data":{"access_token":...,"refresh_token":...}}
//  4. {"access_token":...,"refresh_token":...}
//
// Returns (access, refresh, accessExpiredAtRFC3339).
// tokensFromNested extracts shapes 1&2: data.refresh.token_data / data.token_data
func tokensFromNested(raw map[string]any) (string, string, string) {
	d, ok := raw["data"].(map[string]any)
	if !ok {
		return "", "", ""
	}
	for _, mid := range []string{"refresh", "token_data"} {
		td, ok := d[mid].(map[string]any)
		if !ok {
			continue
		}
		at, atExp := jwtFromObj(td, "access")
		rt, _ := jwtFromObj(td, "refresh")
		if at != "" {
			return at, rt, atExp
		}
	}
	return "", "", ""
}

// tokensFromExodus extracts shape 5: data.access.token + data.refresh.token
func tokensFromExodus(raw map[string]any) (string, string, string) {
	d, ok := raw["data"].(map[string]any)
	if !ok {
		return "", "", ""
	}
	at, atExp := jwtFromObj(d, "access")
	rt, _ := jwtFromObj(d, "refresh")
	if at != "" {
		return at, rt, atExp
	}
	return "", "", ""
}

// tokensFromFlat extracts shapes 3&4: data.access_token / flat access_token
func tokensFromFlat(raw map[string]any) (string, string, string) {
	if d, ok := raw["data"].(map[string]any); ok {
		at, _ := d["access_token"].(string)
		rt, _ := d["refresh_token"].(string)
		if at != "" {
			return at, rt, ""
		}
	}
	at, _ := raw["access_token"].(string)
	rt, _ := raw["refresh_token"].(string)
	if at != "" {
		return at, rt, ""
	}
	return "", "", ""
}

func extractTokens(body []byte) (string, string, string) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", "", ""
	}
	for _, fn := range []func(map[string]any) (string, string, string){
		tokensFromNested, tokensFromExodus, tokensFromFlat,
	} {
		if at, rt, exp := fn(raw); at != "" {
			return at, rt, exp
		}
	}
	return "", "", ""
}

func jwtFromObj(parent map[string]any, key string) (token, expiredAt string) {
	o, ok := parent[key].(map[string]any)
	if !ok {
		// sometimes the value is the raw token string itself
		if s, ok := parent[key].(string); ok && strings.Count(s, ".") == 2 {
			return s, ""
		}
		return "", ""
	}
	if t, ok := o["token"].(string); ok {
		e, _ := o["expired_at"].(string)
		if e == "" {
			e, _ = o["expires_at"].(string)
		}
		return t, e
	}
	return "", ""
}

// journalRefreshResponse persists the raw refresh response body BEFORE any
// parsing, into a timestamped journal. Stockbit rotates the refresh token on
// every successful /login/refresh call — once the endpoint returns 200 the
// old refresh token is dead server-side, so the response body is the ONLY
// place the new pair exists. Keep the last 10 journals.
func journalRefreshResponse(body []byte) string {
	// Sonar S5443: never journal rotated secrets into a publicly writable
	// dir — /tmp is shared across local users (symlink/squat attacks).
	// The journal dir is the token dir when known, else a 0700 user-owned
	// cache dir; filenames are unchanged.
	dir := tokenDir()
	if dir == "" {
		dir = lockDir()
	}
	name := filepath.Join(dir, "refresh_journal_"+time.Now().UTC().Format(journalTimeFmt)+".json")
	if err := os.WriteFile(name, body, 0600); err != nil {
		return ""
	}
	// keep last 10 journals
	if matches, _ := filepath.Glob(filepath.Join(dir, journalGlob)); len(matches) > journalKeepLast {
		sort.Strings(matches)
		for _, old := range matches[:len(matches)-journalKeepLast] {
			_ = os.Remove(old)
		}
	}
	return name
}

func tokenDir() string {
	if p := FindTokenFile(); p != "" {
		return filepath.Dir(p)
	}
	return ""
}

// loadRefreshJournal returns the newest refresh journal entry (raw body).
func loadRefreshJournal() ([]byte, string, error) {
	dir := tokenDir()
	if dir == "" {
		return nil, "", fmt.Errorf("no token file location known yet")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, journalGlob))
	if len(matches) == 0 {
		if last := filepath.Join(dir, "last_refresh_response.json"); fileExists(last) {
			matches = []string{last}
		} else {
			return nil, "", fmt.Errorf("no refresh journal found in %s", dir)
		}
	}
	sort.Strings(matches)
	latest := matches[len(matches)-1]
	b, err := os.ReadFile(latest)
	if err != nil {
		return nil, "", err
	}
	return b, latest, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// waitForPeerRotation polls while another process holds the refresh lock,
// adopting its result when it lands. Returns (token, releaseFn, ok).
func waitForPeerRotation(t *StoredToken) (*StoredToken, func(), bool) {
	for i := 0; i < 12; i++ {
		time.Sleep(500 * time.Millisecond)
		if cur, err := loadBestToken(); err == nil && cur != nil && cur.RefreshToken != "" && cur.RefreshToken != t.RefreshToken {
			// peer's rotation produced a new pair; adopt it
			return cur, func() {
				// no-op release: we never held the lock, the owning
				// peer releases it themselves
			}, true
		}
		if locked, r2 := acquireRefreshLock(); locked {
			return nil, r2, false // we got the lock; caller proceeds to rotate
		}
	}
	// caller never holds any lock here, so release must stay a no-op
	return nil, func() {
		// no-op release: lock was never acquired on this code path
	}, false
}

// buildRotatedToken maps the refresh response onto a StoredToken with expiry
// from the explicit field, the JWT, or a 24h fallback — in that order.
func buildRotatedToken(old *StoredToken, at, rt, atExp string) *StoredToken {
	nt := &StoredToken{AccessToken: at, RefreshToken: rt, UserID: old.UserID}
	if atExp != "" {
		if ts, err := time.Parse(time.RFC3339, atExp); err == nil {
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
	return nt
}

// validateRotation refuses bad rotation results (empty/same/expired rt).
func validateRotation(nt *StoredToken, oldRT, journal string) error {
	if nt.AccessToken == "" {
		return fmt.Errorf("refresh returned empty access token (raw response journaled at %s — recover with: indostock auth rescue)", journal)
	}
	if nt.RefreshToken == "" {
		return fmt.Errorf("refresh returned empty refresh token (raw response journaled at %s — recover with: indostock auth rescue)", journal)
	}
	if nt.RefreshToken == oldRT {
		return fmt.Errorf("refresh returned the SAME refresh token (rotation did not happen; refusing to overwrite — journal at %s)", journal)
	}
	if nExp := jwtExpiry(nt.RefreshToken); !nExp.IsZero() && nExp.Before(time.Now()) {
		return fmt.Errorf("refresh returned an already-expired refresh token (exp %s) — journal at %s", nExp.Format(time.RFC3339), journal)
	}
	return nil
}

// newRefreshRequest builds the exodus /login/refresh request for rt.
func newRefreshRequest(rt string) (*http.Request, error) {
	req, err := http.NewRequest("POST", "https://exodus.stockbit.com/login/refresh", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124 Safari/537.36")
	req.Header.Set("Origin", "https://stockbit.com")
	req.Header.Set("Referer", "https://stockbit.com/")
	return req, nil
}

// doRefreshCall performs the HTTP refresh call, returning the raw body.
func doRefreshCall(rt string) ([]byte, error) {
	req, err := newRefreshRequest(rt)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		n := len(body)
		if n > 800 {
			n = 800
		}
		return nil, fmt.Errorf("refresh failed %d: %s", resp.StatusCode, string(body[:n]))
	}
	return body, nil
}

// persistRotated saves the rotated pair to file + secondary store.
func persistRotated(nt *StoredToken) {
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
}

// Refresh rotates the token pair. It is the ONLY place the refresh call is
// made; the single-writer lock inside serializes all trigger paths.
func Refresh(t *StoredToken) (*StoredToken, error) {
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh_token stored, need browser login once to capture refresh_token")
	}
	// pre-flight: never spend the refresh call on a dead refresh token —
	// a failed call is harmless, but a successful one RETIRES the token.
	if rtExp := jwtExpiry(t.RefreshToken); !rtExp.IsZero() && rtExp.Before(time.Now()) {
		return nil, fmt.Errorf("refresh token EXPIRED at %s — server login required (browser is source of truth, ADR-0009)", rtExp.Format(time.RFC3339))
	}
	// single-writer lock INSIDE Refresh(): every trigger path (cron, serve
	// startup, CLI, on-demand 401 retry) is serialized here. Without this,
	// two concurrent refreshers can both get HTTP 200 and fork the token
	// lineage — one of the two new pairs is silently invalid server-side.
	nt, err := refreshLocked(t)
	if err != nil {
		return nil, err
	}
	persistRotated(nt)
	return nt, nil
}

// refreshLocked acquires the single-writer lock and performs the rotation.
// Peers mid-rotation are adopted instead of double-rotating.
func refreshLocked(t *StoredToken) (*StoredToken, error) {
	locked, release := acquireRefreshLock()
	if !locked {
		// someone else is rotating right now: poll for their result
		var adopted *StoredToken
		adopted, release, locked = waitForPeerRotation(t)
		if adopted != nil {
			return adopted, nil
		}
		if !locked {
			return nil, fmt.Errorf("could not acquire refresh lock — another rotation is stuck?")
		}
		// got the lock after waiting: reload freshest before rotating
		if cur, err := loadBestToken(); err == nil && cur != nil && cur.RefreshToken != "" {
			if cur.RefreshToken != t.RefreshToken {
				release()
				return cur, nil // peer rotated while we waited
			}
			t = cur
		}
	}
	defer release()

	body, err := doRefreshCall(t.RefreshToken)
	if err != nil {
		return nil, err
	}
	// journal raw response BEFORE parsing so a freshly-rotated token pair is
	// never lost to a parser mismatch (Stockbit rotates refresh tokens on
	// every use — after HTTP 200 the old pair is already dead server-side)
	journal := journalRefreshResponse(body)
	_ = os.WriteFile(filepath.Join(tokenDirOrTmp(), "last_refresh_response.json"), body, 0600)

	// accept multiple known shapes for the rotated token pair
	at, rt, atExp := extractTokens(body)
	nt := buildRotatedToken(t, at, rt, atExp)
	if err := validateRotation(nt, t.RefreshToken, journal); err != nil {
		return nil, err
	}
	return nt, nil
}

func tokenDirOrTmp() string {
	if d := tokenDir(); d != "" {
		return d
	}
	// Sonar S5443: fall back to the 0700 user-owned cache dir, never /tmp.
	return lockDir()
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
	// Refresh() acquires the single-writer lock internally now — do NOT
	// double-lock here (the Redis lock is not reentrant; nesting caused a
	// self-deadlock until the 15s lock TTL rescued us).
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
		// no-op unlock: lock not acquired, nothing to release
		return false, func() {
			// no-op: Redis lock belongs to the peer that holds it
		}
	}
	// file fallback
	return acquireFileLock()
}

// lockDir returns a per-user, non-world-writable directory for the refresh
// lock. /tmp is shared across users (Sonar hotspot: publicly writable dir) —
// another local user could pre-create/DoS the lock there. XDG cache dir is
// 0700 and only ours.
func lockDir() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		_ = os.MkdirAll(filepath.Join(v, "indostock"), 0o700)
		return filepath.Join(v, "indostock")
	}
	if base, err := os.UserCacheDir(); err == nil {
		_ = os.MkdirAll(filepath.Join(base, "indostock"), 0o700)
		return filepath.Join(base, "indostock")
	}
	// last resort: home-relative, never world-writable
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".cache", "indostock")
		_ = os.MkdirAll(p, 0o700)
		return p
	}
	return "."
}

func acquireFileLock() (bool, func()) {
	lockPath := filepath.Join(lockDir(), "refresh.lock")
	// stale detection: if lockfile older than 30s, remove
	if fi, err := os.Stat(lockPath); err == nil {
		if time.Since(fi.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath)
		}
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// no-op unlock: lock not acquired, nothing to release
		return false, func() {
			// no-op: another process owns the lockfile
		}
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
			// Refresh() acquires the single-writer lock internally and adopts
			// a peer's fresh pair if one appears while we wait — no outer
			// locking here (non-reentrant lock = self-deadlock).
			nt, err := Refresh(t)
			if err != nil {
				return "", fmt.Errorf("auto-refresh failed: %w", err)
			}
			return nt.AccessToken, nil
		}
		if IsExpired(t, 0) {
			return "", fmt.Errorf("token expired at %s and no refresh_token, re-login via browser required", t.ExpiresAt.Format(time.RFC3339))
		}
	}
	return t.AccessToken, nil
}

// Rescue recovers the newest journaled refresh response: parse it, validate
// the pair, and save to file + secondary store. This is the escape hatch if
// a future API shape change breaks extractTokens after a rotation.
// rescueParse validates the journaled body into a usable StoredToken.
func rescueParse(body []byte, journal string) (*StoredToken, error) {
	at, rt, atExp := extractTokens(body)
	if at == "" || rt == "" {
		return nil, fmt.Errorf("journal %s does not contain a parsable token pair (shape changed again? body len %d)", journal, len(body))
	}
	if exp := jwtExpiry(rt); !exp.IsZero() && exp.Before(time.Now()) {
		return nil, fmt.Errorf("journal %s refresh token already expired at %s — too late, browser login required", journal, exp.Format(time.RFC3339))
	}
	nt := &StoredToken{AccessToken: at, RefreshToken: rt}
	if atExp != "" {
		if ts, err := time.Parse(time.RFC3339, atExp); err == nil {
			nt.ExpiresAt = ts.UTC()
		}
	}
	if nt.ExpiresAt.IsZero() {
		if exp := jwtExpiry(at); !exp.IsZero() {
			nt.ExpiresAt = exp
		}
	}
	return nt, nil
}

// rescueNotNewer guards against downgrading to an already-retired pair.
func rescueNotNewer(nt *StoredToken) bool {
	cur, err := Load()
	return err == nil && cur != nil && cur.RefreshToken == nt.RefreshToken
}

func Rescue() (*StoredToken, string, error) {
	body, journal, err := loadRefreshJournal()
	if err != nil {
		return nil, "", err
	}
	nt, perr := rescueParse(body, journal)
	if perr != nil {
		return nil, journal, perr
	}
	// never downgrade: only save if journaled RT differs from current file RT
	if rescueNotNewer(nt) {
		return nt, journal, fmt.Errorf("journal is not newer than current token (same refresh token) — nothing to rescue")
	}
	path := FindTokenFile()
	if path == "" {
		path = TokenFileCandidates()[0]
	}
	if err := Save(nt, path); err != nil {
		return nt, journal, err
	}
	if s := getSecondaryStore(); s != nil {
		_ = s.Save(nt)
	}
	return nt, journal, nil
}

// RTBattery reports refresh-token health: expiry and remaining time.
func RTBattery(t *StoredToken) (time.Time, time.Duration) {
	exp := jwtExpiry(t.RefreshToken)
	if exp.IsZero() {
		return time.Time{}, 0
	}
	return exp, time.Until(exp)
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
	rtExp, rtLeft := RTBattery(t)
	// AT is considered "cache-healthy" only if some store still holds it
	st := map[string]any{
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
	// refresh-token battery: the REAL health indicator (access tokens are
	// disposable — the refresh token is the single source of truth)
	if !rtExp.IsZero() {
		st["rt_expires_at"] = rtExp.Format(time.RFC3339)
		st["rt_remaining"] = rtLeft.String()
		st["rt_days_left"] = math.Round(rtLeft.Hours()/24*10) / 10
		// warning threshold: < 48h
		st["rt_warning"] = rtLeft < 48*time.Hour
		st["rt_dead"] = rtLeft <= 0
	} else {
		st["rt_warning"] = true
		st["rt_dead"] = true
	}
	// last rotation info from journals
	if dir := tokenDir(); dir != "" {
		if matches, _ := filepath.Glob(filepath.Join(dir, journalGlob)); len(matches) > 0 {
			sort.Strings(matches)
			last := matches[len(matches)-1]
			if fi, err := os.Stat(last); err == nil {
				st["last_rotation_at"] = fi.ModTime().UTC().Format(time.RFC3339)
				st["last_rotation_ago"] = time.Since(fi.ModTime()).String()
			}
		}
	}
	return st
}

const RedisKey = "indostock:auth:stockbit"

func SaveToRedis(cacheClient redisTokenClient, t *StoredToken) error {
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

// redisTokenClient is the minimal cache surface the auth package needs
// (named interface per Sonar: anonymous struct/interface extraction).
type redisTokenClient interface {
	Get(string) (string, error)
	SetEx(string, string, time.Duration) error
}

// redisTokenGetter is the read-only half of redisTokenClient, for loads
// that must not require write access.
type redisTokenGetter interface {
	Get(string) (string, error)
}

func LoadFromRedis(cacheClient redisTokenGetter) (*StoredToken, error) {
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

// freshestStored picks the most current token across Redis then file.
func freshestStored(cacheClient redisTokenClient) *StoredToken {
	if cur, err := LoadFromRedis(cacheClient); err == nil && cur.RefreshToken != "" {
		return cur
	}
	if cur, err := Load(); err == nil && cur.RefreshToken != "" {
		return cur
	}
	return nil
}

// rotateUnderLock performs the rotation while holding the refresh lock.
func rotateUnderLock(cacheClient redisTokenClient, t *StoredToken) (string, error) {
	src := t
	if cur := freshestStored(cacheClient); cur != nil {
		src = cur
	}
	nt, err := Refresh(src)
	if err != nil {
		return "", err
	}
	_ = SaveToRedis(cacheClient, nt)
	_ = Save(nt, "")
	return nt.AccessToken, nil
}

// awaitPeerRotation handles the not-locked case: wait, then use whichever
// store now holds a fresh token — never rotate with the stale t.
func awaitPeerRotation(cacheClient redisTokenClient) (string, bool) {
	time.Sleep(1200 * time.Millisecond)
	if nt, err := LoadFromRedis(cacheClient); err == nil && !IsExpired(nt, 5*time.Minute) {
		return nt.AccessToken, true
	}
	if nt, err := Load(); err == nil && !IsExpired(nt, 5*time.Minute) {
		return nt.AccessToken, true
	}
	return "", false
}

// redisTokenUsable checks whether t has a refreshable pair; if so it rotates
// under the single-writer lock (or adopts a peer's in-flight rotation) and
// returns the fresh access token.
func redisTokenUsable(cacheClient redisTokenClient, t *StoredToken) (string, bool) {
	if t.RefreshToken == "" {
		return "", false
	}
	if locked, release := acquireRefreshLock(); locked {
		// reload freshest under lock: a peer may have rotated
		// already, and t's refresh token may be retired.
		at, rerr := rotateUnderLock(cacheClient, t)
		release()
		return at, rerr == nil
	}
	at, ok := awaitPeerRotation(cacheClient)
	return at, ok
}

func GetValidTokenRedis(cacheClient redisTokenClient) (string, error) {
	if cacheClient == nil {
		return GetValidToken()
	}
	t, err := LoadFromRedis(cacheClient)
	if err != nil || t.AccessToken == "" {
		return GetValidToken()
	}
	if !IsExpired(t, 5*time.Minute) {
		return t.AccessToken, nil
	}
	if at, ok := redisTokenUsable(cacheClient, t); ok {
		return at, nil
	}
	if !IsExpired(t, 0) {
		return t.AccessToken, nil
	}
	return GetValidToken()
}
