package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPlugin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins")
	p, removed, err := installPlugin(dir, "/opt/it's here/agent-usage-bar", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, "agent-usage-bar.1m.sh") || removed != "" {
		t.Errorf("path = %s, removed = %q", p, removed)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("stat: %v %v", fi, err)
	}
	data, _ := os.ReadFile(p)
	for _, want := range []string{
		"#!/bin/bash\n",
		"<xbar.title>Agent Usage</xbar.title>",
		"<xbar.version>v0.2.0</xbar.version>",
		"<xbar.author>Jiří Grmolec</xbar.author>",
		"<xbar.author.github>Anostraca</xbar.author.github>",
		"<xbar.abouturl>https://github.com/Anostraca/agent-usage-bar</xbar.abouturl>",
		"<swiftbar.hideAbout>true</swiftbar.hideAbout>",
		"<swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>",
		"<swiftbar.hideLastUpdated>true</swiftbar.hideLastUpdated>",
		`exec '/opt/it'\''s here/agent-usage-bar' "$@"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("wrapper missing %q:\n%s", want, data)
		}
	}
}

func TestInstallPluginReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.txt")
	writeFile(t, target, "keep me")
	p := filepath.Join(dir, pluginName)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installPlugin(dir, "/bin/cub", "dev"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "keep me" {
		t.Errorf("symlink target overwritten: %q", data)
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
		t.Errorf("plugin: %v %v", fi, err)
	}

	dir2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir2, pluginName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installPlugin(dir2, "/bin/cub", "dev"); err == nil {
		t.Error("want error when the plugin path is a directory")
	}
}

func TestInstallPluginRemovesLegacy(t *testing.T) {
	legacy := legacyName + ".1m.sh"
	cases := []struct {
		name     string
		setup    func(t *testing.T, old string)
		wantGone bool
	}{
		{"file", func(t *testing.T, old string) { writeFile(t, old, "#!/bin/bash\n") }, true},
		{"symlink", func(t *testing.T, old string) {
			target := filepath.Join(filepath.Dir(old), "elsewhere.txt")
			writeFile(t, target, "keep me")
			if err := os.Symlink(target, old); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"directory", func(t *testing.T, old string) {
			if err := os.Mkdir(old, 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			old := filepath.Join(dir, legacy)
			c.setup(t, old)
			p, removed, err := installPlugin(dir, "/bin/cub", "dev")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(p); err != nil {
				t.Errorf("new plugin: %v", err)
			}
			_, lerr := os.Lstat(old)
			if gone := os.IsNotExist(lerr); gone != c.wantGone {
				t.Errorf("legacy gone = %v (%v), want %v", gone, lerr, c.wantGone)
			}
			if want := map[bool]string{true: old}[c.wantGone]; removed != want {
				t.Errorf("removed = %q, want %q", removed, want)
			}
			if c.name == "symlink" {
				if data, _ := os.ReadFile(filepath.Join(dir, "elsewhere.txt")); string(data) != "keep me" {
					t.Errorf("symlink target touched: %q", data)
				}
			}
		})
	}
}
