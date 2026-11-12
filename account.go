package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Account is a named login; ConfigDir is its CLAUDE_CONFIG_DIR (or the
// provider's equivalent). Provider "" is Claude Code.
type Account struct {
	Name      string
	ConfigDir string
	Provider  string
	Bar       bool // shown in the menu bar, not only in the dropdown
}

// key names the account in cache files and the toggle id. Claude accounts
// use the plain name, so their cache files keep their names.
func (a Account) key() string { return accountKey(a.Provider, a.Name) }

func accountKey(provider, name string) string {
	if provider == "" {
		return name
	}
	return provider + "\x00" + name
}

// accountConf is one entry of the config file.
type accountConf struct {
	Name      string `json:"name"`
	ConfigDir string `json:"config_dir"`
	Provider  string `json:"provider"`
	Bar       *bool  `json:"bar"`
}

// account validates an entry: it needs a name, a known provider and an
// absolute (or ~) config dir.
func (c accountConf) account(home string) (Account, bool) {
	prov := c.Provider
	if prov == "claude" {
		prov = ""
	}
	p, ok := providerOf(prov)
	if !ok || strings.TrimSpace(c.Name) == "" {
		return Account{}, false
	}
	dir := filepath.Join(home, p.dir)
	if c.ConfigDir != "" {
		if dir, ok = expandDir(c.ConfigDir, home); !ok {
			return Account{}, false
		}
	}
	return Account{Name: c.Name, ConfigDir: dir, Provider: prov, Bar: c.Bar == nil || *c.Bar}, true
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
// returns the fallback accounts.
func loadAccounts(path, home string) []Account {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallbackAccounts(home)
	}
	var cfg struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return fallbackAccounts(home)
	}
	var out []Account
	for _, raw := range cfg.Accounts {
		if a, ok := parseAccount(raw, home); ok {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return fallbackAccounts(home)
	}
	return out
}

// parseAccount decodes and validates one config entry; a bad entry is
// skipped on its own.
func parseAccount(raw json.RawMessage, home string) (Account, bool) {
	var c accountConf
	if json.Unmarshal(raw, &c) != nil {
		return Account{}, false
	}
	return c.account(home)
}

// fallbackAccounts is the default ~/.claude login, plus ~/.codex when Codex
// has a ChatGPT login there.
func fallbackAccounts(home string) []Account {
	out := []Account{{Name: "claude", ConfigDir: filepath.Join(home, ".claude"), Bar: true}}
	if dir := filepath.Join(home, ".codex"); codexLoggedIn(dir) {
		out = append(out, Account{Name: "codex", ConfigDir: dir, Provider: "codex", Bar: true})
	}
	return out
}

func codexLoggedIn(dir string) bool {
	_, err := readCodexAuth(dir)
	return err == nil
}

// toggleBar flips "bar" for the account whose hashHex(key, 4) is id and
// rewrites the config file, keeping keys it doesn't know. Without a config
// file it writes the fallback accounts. A file that isn't valid JSON, or a
// symlink that can't be resolved, is left alone.
func toggleBar(path, home, id string) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		p, err := filepath.EvalSymlinks(path)
		if err != nil {
			return errors.New("config file is a broken symlink, not changed")
		}
		path = p // keep a symlinked config a symlink
	}
	top := map[string]json.RawMessage{}
	var entries []json.RawMessage
	mode := os.FileMode(0o644)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(data, &top) != nil || top == nil {
			return errors.New("config file is not valid JSON, not changed")
		}
		if raw, ok := top["accounts"]; ok && json.Unmarshal(raw, &entries) != nil {
			return errors.New("config accounts are not a list, not changed")
		}
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	match, valid := -1, false
	for i, raw := range entries {
		if a, ok := parseAccount(raw, home); ok {
			valid = true
			if match < 0 && hashHex(a.key(), 4) == id {
				match = i
			}
		}
	}
	if !valid {
		// The fallback accounts are in effect: write them out.
		for _, a := range fallbackAccounts(home) {
			e := map[string]string{"name": a.Name}
			if a.Provider != "" {
				e["provider"] = a.Provider
			}
			if match < 0 && hashHex(a.key(), 4) == id {
				match = len(entries)
			}
			entries = append(entries, jsonValue(e))
		}
	}
	if match < 0 {
		return errors.New("no account with that id")
	}
	var e map[string]json.RawMessage
	if json.Unmarshal(entries[match], &e) != nil {
		return errors.New("config entry is not an object, not changed")
	}
	var bar *bool
	if json.Unmarshal(e["bar"], &bar) != nil || bar == nil || *bar {
		e["bar"] = jsonValue(false)
	} else {
		delete(e, "bar")
	}
	entries[match] = jsonValue(e)
	top["accounts"] = jsonValue(entries)
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(out, '\n'), mode)
}

func jsonValue(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// writeFileAtomic writes via a temp file in the same dir and a rename.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := cmp.Or(werr, cerr, os.Chmod(f.Name(), mode)); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// expandDir resolves "~" and "" (= ~/.claude) to a clean absolute path.
// Account dirs resolve "" to their provider's default before this.
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
