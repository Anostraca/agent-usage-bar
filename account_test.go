package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	fallback := []Account{{Name: "claude", ConfigDir: filepath.Join(home, ".claude"), Bar: true}}
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg := configPath(home)
	if cfg != filepath.Join(home, ".config", "agent-usage-bar", "config.json") {
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
		{Name: "team", ConfigDir: filepath.Join(home, ".claude"), Bar: true},
		{Name: "personal", ConfigDir: filepath.Join(home, ".claude-personal"), Bar: true},
		{Name: "abs", ConfigDir: "/opt/c", Bar: true},
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
	if got := configPath("/h"); got != "/x/cfg/agent-usage-bar/config.json" {
		t.Errorf("config path = %s", got)
	}
}

func TestConfigPathLegacy(t *testing.T) {
	cases := []struct {
		name          string
		newer, legacy bool
		want          string
	}{
		{"both", true, true, "agent-usage-bar"},
		{"legacy only", false, true, legacyName},
		{"neither", false, false, "agent-usage-bar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", base)
			if c.newer {
				writeFile(t, filepath.Join(base, "agent-usage-bar", "config.json"), "{}")
			}
			if c.legacy {
				writeFile(t, filepath.Join(base, legacyName, "config.json"), "{}")
			}
			if got, want := configPath("/h"), filepath.Join(base, c.want, "config.json"); got != want {
				t.Errorf("config path = %s, want %s", got, want)
			}
		})
	}
}

func TestLoadAccountsProviders(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.json")
	writeFile(t, cfg, `{"accounts":[
		{"name":"team","config_dir":"~/.claude"},
		{"name":"max","config_dir":"~/.claude-max","bar":false},
		{"name":"cx","provider":"codex"},
		{"name":"cl","provider":"claude","bar":true},
		{"name":"gem","provider":"gemini"},
		{"name":"cx2","provider":"codex","config_dir":"/opt/cx"},
		{"name":"cx3","provider":"codex","config_dir":"rel"}]}`)
	want := []Account{
		{Name: "team", ConfigDir: filepath.Join(home, ".claude"), Bar: true},
		{Name: "max", ConfigDir: filepath.Join(home, ".claude-max")},
		{Name: "cx", ConfigDir: filepath.Join(home, ".codex"), Provider: "codex", Bar: true},
		{Name: "cl", ConfigDir: filepath.Join(home, ".claude"), Bar: true},
		{Name: "cx2", ConfigDir: "/opt/cx", Provider: "codex", Bar: true},
	}
	if got := loadAccounts(cfg, home); !reflect.DeepEqual(got, want) {
		t.Errorf("accounts =\n%v\nwant\n%v", got, want)
	}
}

func TestFallbackAccounts(t *testing.T) {
	home := t.TempDir()
	claude := Account{Name: "claude", ConfigDir: filepath.Join(home, ".claude"), Bar: true}
	missing := filepath.Join(home, "none.json")
	if got := loadAccounts(missing, home); !reflect.DeepEqual(got, []Account{claude}) {
		t.Errorf("without codex: %v", got)
	}
	// An API key login has no usage to show.
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x","tokens":null}`)
	if got := loadAccounts(missing, home); !reflect.DeepEqual(got, []Account{claude}) {
		t.Errorf("codex api key: %v", got)
	}
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), codexAuthJSON(time.Now().Add(time.Hour)))
	codex := Account{Name: "codex", ConfigDir: filepath.Join(home, ".codex"), Provider: "codex", Bar: true}
	if got := loadAccounts(missing, home); !reflect.DeepEqual(got, []Account{claude, codex}) {
		t.Errorf("with codex: %v", got)
	}
}

func TestToggleBar(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "cfg", "config.json")
	writeFile(t, cfg, `{"command":"x","accounts":[{"name":"team","config_dir":"~/.claude","color":"red"},{"name":"cx","provider":"codex"}]}`)
	team := hashHex("team", 4)
	bars := func() []bool {
		var out []bool
		for _, a := range loadAccounts(cfg, home) {
			out = append(out, a.Bar)
		}
		return out
	}

	if err := toggleBar(cfg, home, team); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	want := `{
  "accounts": [
    {
      "bar": false,
      "color": "red",
      "config_dir": "~/.claude",
      "name": "team"
    },
    {
      "name": "cx",
      "provider": "codex"
    }
  ],
  "command": "x"
}
`
	if string(data) != want {
		t.Errorf("hidden:\n%s", data)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if got := bars(); !reflect.DeepEqual(got, []bool{false, true}) {
		t.Errorf("bars = %v", got)
	}

	if err := toggleBar(cfg, home, team); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(cfg)
	if strings.Contains(string(data), `"bar"`) || !strings.Contains(string(data), `"color": "red"`) {
		t.Errorf("shown again:\n%s", data)
	}
	if got := bars(); !reflect.DeepEqual(got, []bool{true, true}) {
		t.Errorf("bars = %v", got)
	}

	if err := toggleBar(cfg, home, "00000000"); err == nil {
		t.Error("want error for an unknown id")
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(data) {
		t.Error("unknown id changed the file")
	}
	entries, _ := os.ReadDir(filepath.Dir(cfg))
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}

	writeFile(t, cfg, `{"accounts":[`)
	if err := toggleBar(cfg, home, team); err == nil {
		t.Error("want error for invalid JSON")
	}
	if after, _ := os.ReadFile(cfg); string(after) != `{"accounts":[` {
		t.Errorf("invalid JSON overwritten: %q", after)
	}
}

func TestToggleBarNoConfig(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), codexAuthJSON(time.Now().Add(time.Hour)))
	cfg := filepath.Join(home, "new", "dir", "config.json")
	if err := toggleBar(cfg, home, hashHex(accountKey("codex", "codex"), 4)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	want := `{
  "accounts": [
    {
      "name": "claude"
    },
    {
      "bar": false,
      "name": "codex",
      "provider": "codex"
    }
  ]
}
`
	if string(data) != want {
		t.Errorf("config =\n%s", data)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode())
	}
	got := loadAccounts(cfg, home)
	if len(got) != 2 || !got[0].Bar || got[1].Bar || got[1].Provider != "codex" {
		t.Errorf("accounts = %v", got)
	}
}

func TestToggleBarFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	var out, errOut strings.Builder
	if code := run([]string{"--toggle-bar", "00000000"}, &out, &errOut); code != 1 || out.Len() != 0 || errOut.Len() == 0 {
		t.Errorf("unknown id: exit %d out %q err %q", code, out.String(), errOut.String())
	}
	if code := run([]string{"--toggle-bar", hashHex("claude", 4)}, &out, &errOut); code != 0 || out.Len() != 0 {
		t.Errorf("toggle: exit %d out %q err %q", code, out.String(), errOut.String())
	}
	if got := loadAccounts(configPath(home), home); len(got) != 1 || got[0].Bar {
		t.Errorf("accounts = %v", got)
	}
	if code := run([]string{"--toggle-bar"}, &out, &errOut); code != 2 {
		t.Errorf("missing id: exit %d", code)
	}
}

func TestLoadAccountsBadEntry(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.json")
	writeFile(t, cfg, `{"accounts":[{"name":"a"},{"name":"b","bar":"no"},7,{"name":"c","provider":"codex","bar":false}]}`)
	want := []Account{
		{Name: "a", ConfigDir: filepath.Join(home, ".claude"), Bar: true},
		{Name: "c", ConfigDir: filepath.Join(home, ".codex"), Provider: "codex"},
	}
	if got := loadAccounts(cfg, home); !reflect.DeepEqual(got, want) {
		t.Errorf("accounts = %v", got)
	}
	// The toggle sees the same accounts and keeps the bad entries.
	if err := toggleBar(cfg, home, hashHex(accountKey("codex", "c"), 4)); err != nil {
		t.Fatal(err)
	}
	if got := loadAccounts(cfg, home); len(got) != 2 || !got[1].Bar {
		t.Errorf("after toggle: %v", got)
	}
	if data, _ := os.ReadFile(cfg); !strings.Contains(string(data), `"bar": "no"`) || !strings.Contains(string(data), "    7,") {
		t.Errorf("bad entries dropped:\n%s", data)
	}
}

func TestSameNameDifferentProvider(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.json")
	writeFile(t, cfg, `{"accounts":[{"name":"work"},{"name":"work","provider":"codex"}]}`)
	accts := loadAccounts(cfg, home)
	if len(accts) != 2 {
		t.Fatalf("accounts = %v", accts)
	}
	a := &app{cacheDir: "/c"}
	cl, cx := accts[0].key(), accts[1].key()
	for _, kind := range []string{"usage", "stats", "files"} {
		if a.cacheFile(kind, cl) == a.cacheFile(kind, cx) {
			t.Errorf("%s cache shared: %s", kind, a.cacheFile(kind, cl))
		}
	}
	if a.cachePath(cl) != "/c/usage-work-"+hashHex("work", 4)+".json" {
		t.Errorf("claude cache path changed: %s", a.cachePath(cl))
	}
	if err := toggleBar(cfg, home, hashHex(cx, 4)); err != nil {
		t.Fatal(err)
	}
	if got := loadAccounts(cfg, home); !got[0].Bar || got[1].Bar {
		t.Errorf("codex toggle: %v", got)
	}
	if err := toggleBar(cfg, home, hashHex(cl, 4)); err != nil {
		t.Fatal(err)
	}
	if got := loadAccounts(cfg, home); got[0].Bar || got[1].Bar {
		t.Errorf("claude toggle: %v", got)
	}
}

func TestToggleBarDanglingSymlink(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.json")
	target := filepath.Join(home, "dotfiles", "config.json")
	if err := os.Symlink(target, cfg); err != nil {
		t.Fatal(err)
	}
	if err := toggleBar(cfg, home, hashHex("claude", 4)); err == nil {
		t.Error("want error for a broken symlink")
	}
	if fi, err := os.Lstat(cfg); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink replaced: %v %v", fi, err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("target written")
	}

	// A working symlink stays a symlink; the target is written.
	writeFile(t, target, `{"accounts":[{"name":"claude"}]}`)
	if err := toggleBar(cfg, home, hashHex("claude", 4)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(cfg); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink replaced")
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), `"bar": false`) {
		t.Errorf("target = %s", data)
	}
}
