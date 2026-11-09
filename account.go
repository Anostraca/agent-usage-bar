package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Account is a named Claude Code login; ConfigDir is its CLAUDE_CONFIG_DIR.
type Account struct {
	Name      string `json:"name"`
	ConfigDir string `json:"config_dir"`
}

// configPath is $XDG_CONFIG_HOME/claude-usage-bar/config.json, by default
// under ~/.config.
func configPath(home string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "claude-usage-bar", "config.json")
}

// loadAccounts reads the account list from the config file. Without one it
// returns the default ~/.claude login.
func loadAccounts(path, home string) []Account {
	fallback := []Account{{Name: "claude", ConfigDir: filepath.Join(home, ".claude")}}
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	var cfg struct {
		Accounts []Account `json:"accounts"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return fallback
	}
	var out []Account
	for _, a := range cfg.Accounts {
		dir, ok := expandDir(a.ConfigDir, home)
		if strings.TrimSpace(a.Name) == "" || !ok {
			continue
		}
		a.ConfigDir = dir
		out = append(out, a)
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// expandDir resolves "~" and "" (= ~/.claude) to a clean absolute path.
// Relative paths are rejected: Claude Code resolves them against the folder
// it starts in, so their keychain item can't be known here.
func expandDir(dir, home string) (string, bool) {
	switch {
	case dir == "":
		return filepath.Join(home, ".claude"), true
	case dir == "~":
		return home, true
	case strings.HasPrefix(dir, "~/"):
		return filepath.Join(home, dir[2:]), true
	case !filepath.IsAbs(dir):
		return "", false
	}
	return filepath.Clean(dir), true
}

func isDefaultDir(dir, home string) bool {
	return dir == "" || filepath.Clean(dir) == filepath.Join(home, ".claude")
}

// keychainService is the macOS keychain item Claude Code stores the login in.
func keychainService(dir, home string) string {
	const base = "Claude Code-credentials"
	if isDefaultDir(dir, home) {
		return base
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// globalConfigPath is Claude Code's .claude.json for the account: in HOME for
// the default dir, inside the config dir otherwise (or the legacy
// .config.json when only that exists).
func globalConfigPath(dir, home string) string {
	if isDefaultDir(dir, home) {
		return filepath.Join(home, ".claude.json")
	}
	p := filepath.Join(dir, ".claude.json")
	if !fileExists(p) {
		if legacy := filepath.Join(dir, ".config.json"); fileExists(legacy) {
			return legacy
		}
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

type profile struct {
	Email string `json:"emailAddress"`
	Org   string `json:"organizationName"`
}

func readProfile(dir, home string) profile {
	data, err := os.ReadFile(globalConfigPath(dir, home))
	if err != nil {
		return profile{}
	}
	var cfg struct {
		OAuth profile `json:"oauthAccount"`
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg.OAuth
}

// creds holds what we need from the stored login. The refresh token is never
// decoded.
type creds struct {
	AccessToken      string `json:"accessToken"`
	ExpiresAt        int64  `json:"expiresAt"` // ms since epoch
	SubscriptionType string `json:"subscriptionType"`
	RateLimitTier    string `json:"rateLimitTier"`
}

var errNoCreds = errors.New("no login found — run claude on this account")

const keychainTimeout = 5 * time.Second

// securityKeychain reads a generic password with the macOS security tool.
// Read only; errors never carry the item's contents. A locked keychain or
// an access prompt hits the timeout instead of hanging the plugin.
func securityKeychain(service string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-w")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New("keychain item not readable")
	}
	return out, nil
}

// readCreds tries the keychain, then <dir>/.credentials.json.
func readCreds(dir, home string, keychain func(string) ([]byte, error)) (creds, error) {
	if keychain != nil {
		if data, err := keychain(keychainService(dir, home)); err == nil {
			c, ok := parseCreds(data)
			clear(data)
			if ok {
				return c, nil
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return creds{}, errNoCreds
	}
	c, ok := parseCreds(data)
	clear(data)
	if !ok {
		return creds{}, errNoCreds
	}
	return c, nil
}

func parseCreds(data []byte) (creds, bool) {
	var v struct {
		OAuth creds `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &v) != nil || v.OAuth.AccessToken == "" {
		return creds{}, false
	}
	return v.OAuth, true
}

var tierMult = regexp.MustCompile(`_(\d+x)$`)

// planName turns subscriptionType + rateLimitTier into e.g. "max 20x".
func planName(sub, tier string) string {
	m := tierMult.FindStringSubmatch(tier)
	switch {
	case sub == "" && m == nil:
		return ""
	case sub == "":
		return m[1]
	case m == nil:
		return sub
	}
	return sub + " " + m[1]
}
