package cli

import (
	"encoding/json"
	"fmt"
	"indonesia-stock/internal/infrastructure/cache"
	"indonesia-stock/pkg/auth"
	"indonesia-stock/pkg/config"
	"io"
	"os"
	"strings"
	"time"
)

func runAuth(rest []string) int {
	if len(rest) == 0 {
		printAuthHelp()
		return 0
	}
	sub := strings.ToLower(rest[0])
	args := rest[1:]
	switch sub {
	case "status", "check", "show":
		return runAuthStatus(args)
	case "refresh":
		return runAuthRefresh(args)
	case "save", "login", "set":
		return runAuthSave(args)
	case "rescue":
		return runAuthRescue(args)
	case "help", "--help", "-h":
		printAuthHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "auth: unknown subcommand %q\n", sub)
		printAuthHelp()
		return 1
	}
}

func printAuthHelp() {
	fmt.Print(`indostock auth - Stockbit token management (auto-rotate)

Usage:
  indostock auth status [--json]              Show token expiry & refresh status
  indostock auth refresh [--json]             Force refresh via POST /login/refresh (needs refresh_token)
  indostock auth save --token <JWT> [--refresh <JWT>] [--file <path>] [--json]
  indostock auth rescue [--compact]          Recover newest refresh journal into live store
     Save token from browser. One-time step, then auto-rotate daily.
  indostock auth save --stdin                 Read JSON from stdin: {"access_token": "...", "refresh_token": "..."}

Browser is source of truth (ADR-0009):
  1) Login sekali di Brave/Firefox -> stockbit.com (akun krez.tk@gmail.com)
  2) DevTools (F12) -> Application -> Local Storage -> https://stockbit.com -> credentialStorage / token
     atau Network -> exodus.stockbit.com/login/v6/username -> Response -> data.login.token_data.access.token + refresh.token
  3) Copy lalu: indostock auth save --token <access> --refresh <refresh>
     Token tersimpan di /apps/indostock/token.json (server) dan ~/.config/indostock/token.json (local)
  4) Server auto-refresh 5 menit sebelum expiry via: indostock auth refresh (systemd timer 04:00 WIB)

Files checked (in order): /apps/indostock/token.json, /etc/indostock/token.json, ~/.config/indostock/token.json, ~/.indostock/token.json, $STOCKBIT_TOKEN env
`)
}

// flagCompact is the shared output-format flag across all auth subcommands.
const flagCompact = "--compact"

func runAuthStatus(args []string) int {
	compact := false
	jsonOut := true
	for _, a := range args {
		if a == flagCompact {
			compact = true
		}
		if a == "--json=false" || a == "--no-json" {
			jsonOut = false
		}
	}
	st := auth.Status()
	// tambahkan status redis
	{
		cfg := config.Load()
		rc := cache.New(cfg.RedisURL)
		if rc.Available() {
			if t, err := auth.LoadFromRedis(rc); err == nil {
				st["redis"] = map[string]any{"ok": !auth.IsExpired(t, 0), "expires_at": t.ExpiresAt.Format(time.RFC3339), "has_refresh": t.RefreshToken != "", "key": auth.RedisKey}
			} else {
				st["redis"] = map[string]any{"ok": false, "error": err.Error(), "key": auth.RedisKey}
			}
		} else {
			st["redis"] = map[string]any{"ok": false, "error": "redis not available", "key": auth.RedisKey}
		}
	}
	if jsonOut {
		outputJSON(st, compact)
	} else {
		fmt.Printf("ok=%v expires_at=%v has_refresh=%v file=%v\n", st["ok"], st["expires_at"], st["has_refresh"], st["file"])
	}
	if ok, _ := st["ok"].(bool); ok {
		return 0
	}
	return 1
}

func runAuthRefresh(args []string) int {
	compact := false
	for _, a := range args {
		if a == flagCompact {
			compact = true
		}
	}
	t, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load token: %v\n", err)
		outputJSON(map[string]any{"ok": false, "error": err.Error()}, compact)
		return 1
	}
	if t.RefreshToken == "" {
		fmt.Fprintln(os.Stderr, "no refresh_token, need: indostock auth save --token <access> --refresh <refresh>")
		outputJSON(map[string]any{"ok": false, "error": "no refresh_token", "hint": "indostock auth save --token ... --refresh ..."}, compact)
		return 1
	}
	fmt.Fprintf(os.Stderr, "refreshing via POST https://exodus.stockbit.com/login/refresh ...\n")
	nt, err := auth.Refresh(t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "refresh failed: %v\n", err)
		outputJSON(map[string]any{"ok": false, "error": err.Error()}, compact)
		return 1
	}
	// sync to redis
	{
		cfg2 := config.Load()
		rc := cache.New(cfg2.RedisURL)
		if rc.Available() {
			_ = auth.SaveToRedis(rc, nt)
		}
	}
	outputJSON(map[string]any{
		"ok":          true,
		"expires_at":  nt.ExpiresAt.Format(time.RFC3339),
		"expires_in":  time.Until(nt.ExpiresAt).String(),
		"file":        auth.FindTokenFile(),
		"redis_key":   auth.RedisKey,
		"token_len":   len(nt.AccessToken),
		"refresh_len": len(nt.RefreshToken),
		"message":     "token refreshed and saved to file+redis, cron hourly will auto-rotate",
	}, compact)
	return 0
}

// parseArgsFlag walks args for "--name value" and "--name=value" pairs.
// Returns (values map, rest flags seen as bare words).
func parseArgsFlag(args []string, names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, n := range names {
			if a == "--"+n && i+1 < len(args) {
				i++
				out[n] = args[i]
			} else if strings.HasPrefix(a, "--"+n+"=") {
				out[n] = strings.TrimPrefix(a, "--"+n+"=")
			}
		}
	}
	return out
}

// extractStdinTokens digs access/refresh tokens out of arbitrary pasted JSON
// (full login response, token_data wrapper, or plain token.json shape).
func extractStdinTokens(m map[string]any) (token, refresh string) {
	if v, ok := m["access_token"].(string); ok {
		token = v
	}
	if v, ok := m["refresh_token"].(string); ok {
		refresh = v
	}
	if v, ok := m["data"].(map[string]any); ok {
		if l, ok := v["login"].(map[string]any); ok {
			if td, ok := l["token_data"].(map[string]any); ok {
				token, refresh = digTokenData(td, token, refresh)
			}
		}
		if td, ok := v["token_data"].(map[string]any); ok {
			token, refresh = digTokenData(td, token, refresh)
		}
	}
	return token, refresh
}

func digTokenData(td map[string]any, token, refresh string) (string, string) {
	if ac, ok := td["access"].(map[string]any); ok {
		if tok, ok := ac["token"].(string); ok && tok != "" {
			token = tok
		}
	}
	if rf, ok := td["refresh"].(map[string]any); ok {
		if tok, ok := rf["token"].(string); ok && tok != "" {
			refresh = tok
		}
	}
	return token, refresh
}

func runAuthSave(args []string) int {
	vals := parseArgsFlag(args, "token", "refresh", "file")
	token, refresh, file := vals["token"], vals["refresh"], vals["file"]
	stdin := sliceContains(args, "--stdin")
	compact := sliceContains(args, flagCompact)
	if stdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
			return 1
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err == nil {
			token, refresh = extractStdinTokens(m)
		} else {
			s := strings.TrimSpace(string(b))
			if len(s) > 100 && strings.Count(s, ".") == 2 {
				token = s
			}
		}
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "auth save: need --token <JWT> or --stdin")
		fmt.Fprintln(os.Stderr, "example: indostock auth save --token eyJ... --refresh eyJ... ")
		fmt.Fprintln(os.Stderr, "or: cat token.json | indostock auth save --stdin")
		return 1
	}
	t := &auth.StoredToken{AccessToken: strings.TrimSpace(token), RefreshToken: strings.TrimSpace(refresh)}
	// try parse expiry from jwt
	if err := auth.Save(t, file); err != nil {
		fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
		return 1
	}
	// also push to redis if available
	{
		cfg2 := config.Load()
		rc := cache.New(cfg2.RedisURL)
		if rc.Available() {
			_ = auth.SaveToRedis(rc, t)
			fmt.Fprintf(os.Stderr, "redis: saved %s TTL=%v\n", auth.RedisKey, time.Until(t.ExpiresAt).Round(time.Second))
		}
	}
	// reload to get parsed expiry
	saved, _ := auth.Load()
	expStr := ""
	if saved != nil && !saved.ExpiresAt.IsZero() {
		expStr = saved.ExpiresAt.Format(time.RFC3339)
	}
	outputJSON(map[string]any{
		"ok":          true,
		"file":        auth.FindTokenFile(),
		"expires_at":  expStr,
		"has_refresh": refresh != "",
		"message":     "saved. test: indostock auth status --json && indostock orderbook BBCA --depth 2 --json",
	}, compact)
	return 0
}

// runAuthRescue recovers the newest journaled refresh response into the live
// token store. Escape hatch when a rotation succeeded server-side but the
// parser failed client-side (the 2026-09-12 incident).
func runAuthRescue(args []string) int {
	compact := sliceContains(args, flagCompact)
	nt, journal, err := auth.Rescue()
	out := map[string]any{"journal": journal}
	if err != nil {
		out["ok"] = false
		out["error"] = err.Error()
		if nt != nil && nt.AccessToken != "" {
			out["recovered_access"] = true
		}
		outputJSON(out, compact)
		return 1
	}
	// sync to redis
	cfg2 := config.Load()
	rc := cache.New(cfg2.RedisURL)
	if rc.Available() {
		_ = auth.SaveToRedis(rc, nt)
	}
	out["ok"] = true
	out["expires_at"] = nt.ExpiresAt.Format(time.RFC3339)
	out["has_refresh"] = nt.RefreshToken != ""
	out["message"] = "rescued from journal. test: indostock auth status --json && indostock orderbook BBCA --depth 2 --json"
	outputJSON(out, compact)
	return 0
}

func sliceContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
