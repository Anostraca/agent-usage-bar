package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKeychainService(t *testing.T) {
	home := "/Users/alice"
	// printf %s /Users/alice/.claude-work | shasum -a 256 → be865d75…
	cases := map[string]string{
		"":                           "Claude Code-credentials",
		"/Users/alice/.claude":       "Claude Code-credentials",
		"/Users/alice/.claude/":      "Claude Code-credentials",
		"/Users/alice/.claude-work":  "Claude Code-credentials-be865d75",
		"/Users/alice/.claude-work/": "Claude Code-credentials-be865d75",
	}
	for dir, want := range cases {
		if got := keychainService(dir, home); got != want {
			t.Errorf("keychainService(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestGlobalConfigPath(t *testing.T) {
	home := "/h"
	if got := globalConfigPath("/h/.claude", home); got != "/h/.claude.json" {
		t.Errorf("default dir: %q", got)
	}
	if got := globalConfigPath("", home); got != "/h/.claude.json" {
		t.Errorf("empty dir: %q", got)
	}
	if got := globalConfigPath("/h/.claude-max", home); got != "/h/.claude-max/.claude.json" {
		t.Errorf("other dir: %q", got)
	}
}

func TestLoadAccounts(t *testing.T) {
	home := t.TempDir()
	fallback := []Account{{Name: "claude", ConfigDir: filepath.Join(home, ".claude")}}
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg := configPath(home)
	if cfg != filepath.Join(home, ".config", "claude-usage-bar", "config.json") {
		t.Errorf("config path = %s", cfg)
	}
	if got := loadAccounts(cfg, home); !reflect.DeepEqual(got, fallback) {
		t.Errorf("missing config: %v", got)
	}

	writeFile(t, cfg, `{"accounts":[]}`)
	if got := loadAccounts(cfg, home); !reflect.DeepEqual(got, fallback) {
		t.Errorf("no accounts: %v", got)
	}

	writeFile(t, cfg, `{"command":"claude","accounts":[
		{"name":"team","config_dir":"~/.claude"},
		{"name":"personal","config_dir":"~/.claude-personal/"},
		{"name":"","config_dir":"~/x"},
		{"name":"rel","config_dir":"relative/dir"},
		{"name":"abs","config_dir":"/opt/c"}]}`)
	want := []Account{
		{Name: "team", ConfigDir: filepath.Join(home, ".claude")},
		{Name: "personal", ConfigDir: filepath.Join(home, ".claude-personal")},
		{Name: "abs", ConfigDir: "/opt/c"},
	}
	if got := loadAccounts(cfg, home); !reflect.DeepEqual(got, want) {
		t.Errorf("accounts = %v, want %v", got, want)
	}

	writeFile(t, cfg, `not json`)
	if got := loadAccounts(cfg, home); !reflect.DeepEqual(got, fallback) {
		t.Errorf("bad config: %v", got)
	}
}

func TestReadProfile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@example.com","organizationName":"Org"},"projects":{}}`)
	writeFile(t, filepath.Join(home, ".claude-max", ".claude.json"), `{"oauthAccount":{"emailAddress":"b@example.com"}}`)
	if p := readProfile(filepath.Join(home, ".claude"), home); p.Email != "a@example.com" || p.Org != "Org" {
		t.Errorf("default profile = %+v", p)
	}
	if p := readProfile(filepath.Join(home, ".claude-max"), home); p.Email != "b@example.com" || p.Org != "" {
		t.Errorf("max profile = %+v", p)
	}
	writeFile(t, filepath.Join(home, ".claude-old", ".config.json"), `{"oauthAccount":{"emailAddress":"c@example.com"}}`)
	if p := readProfile(filepath.Join(home, ".claude-old"), home); p.Email != "c@example.com" {
		t.Errorf("legacy profile = %+v", p)
	}
	if p := readProfile(filepath.Join(home, ".none"), home); p != (profile{}) {
		t.Errorf("missing profile = %+v", p)
	}
}

func TestReadCredsFallback(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude-x")
	failing := func(string) ([]byte, error) { return nil, errors.New("nope") }
	if _, err := readCreds(dir, home, failing); err != errNoCreds {
		t.Errorf("no creds: err = %v", err)
	}
	writeFile(t, filepath.Join(dir, ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"file-tok","refreshToken":"r","expiresAt":5,"subscriptionType":"pro"}}`)
	c, err := readCreds(dir, home, failing)
	if err != nil || c.AccessToken != "file-tok" || c.ExpiresAt != 5 || c.SubscriptionType != "pro" {
		t.Errorf("file creds = %+v, %v", c, err)
	}

	var asked string
	kc := func(s string) ([]byte, error) {
		asked = s
		return []byte(`{"claudeAiOauth":{"accessToken":"kc-tok"}}`), nil
	}
	if c, _ := readCreds(dir, home, kc); c.AccessToken != "kc-tok" || asked != keychainService(dir, home) {
		t.Errorf("keychain creds = %+v (service %q)", c, asked)
	}
}

func TestPlanName(t *testing.T) {
	cases := []struct{ sub, tier, want string }{
		{"max", "default_claude_max_20x", "max 20x"},
		{"max", "default_claude_max_5x", "max 5x"},
		{"team", "default_claude_max_5x", "team 5x"},
		{"pro", "default_claude_ai", "pro"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := planName(c.sub, c.tier); got != c.want {
			t.Errorf("planName(%q,%q) = %q, want %q", c.sub, c.tier, got, c.want)
		}
	}
}

func TestConfigPathXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got := configPath("/h"); got != "/x/cfg/claude-usage-bar/config.json" {
		t.Errorf("config path = %s", got)
	}
}
