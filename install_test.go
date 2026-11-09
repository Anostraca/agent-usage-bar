package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPlugin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins")
	p, err := installPlugin(dir, "/opt/it's here/claude-usage-bar")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, "claude-usage-bar.1m.sh") {
		t.Errorf("path = %s", p)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("stat: %v %v", fi, err)
	}
	data, _ := os.ReadFile(p)
	for _, want := range []string{
		"#!/bin/bash\n",
		"<xbar.title>Claude Usage</xbar.title>",
		"<swiftbar.hideAbout>true</swiftbar.hideAbout>",
		"<swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>",
		"<swiftbar.hideLastUpdated>true</swiftbar.hideLastUpdated>",
		`exec '/opt/it'\''s here/claude-usage-bar' "$@"`,
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
	if _, err := installPlugin(dir, "/bin/cub"); err != nil {
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
	if _, err := installPlugin(dir2, "/bin/cub"); err == nil {
		t.Error("want error when the plugin path is a directory")
	}
}
