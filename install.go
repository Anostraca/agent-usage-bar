package main

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const pluginName = "agent-usage-bar.1m.sh"

// pluginDir is SwiftBar's plugin folder, or ~/SwiftBar.
func pluginDir(home string) string {
	out, err := exec.Command("defaults", "read", "com.ameba.SwiftBar", "PluginDirectory").Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		return filepath.Join(home, "SwiftBar")
	}
	if d, ok := expandDir(dir, home); ok {
		return d
	}
	return filepath.Join(home, "SwiftBar")
}

// pluginScript is the SwiftBar wrapper that runs exe every minute.
func pluginScript(exe, version string) string {
	if version != "dev" && !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return `#!/bin/bash
# <xbar.title>Agent Usage</xbar.title>
# <xbar.version>` + version + `</xbar.version>
# <xbar.author>Jiří Grmolec</xbar.author>
# <xbar.author.github>Anostraca</xbar.author.github>
# <xbar.desc>Claude, Codex and Antigravity usage as battery icons</xbar.desc>
# <xbar.dependencies>agent-usage-bar,claude-code</xbar.dependencies>
# <xbar.abouturl>https://github.com/Anostraca/agent-usage-bar</xbar.abouturl>
# <swiftbar.hideAbout>true</swiftbar.hideAbout>
# <swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>
# <swiftbar.hideLastUpdated>true</swiftbar.hideLastUpdated>
exec '` + strings.ReplaceAll(exe, `'`, `'\''`) + `' "$@"
`
}

// installPlugin writes the wrapper into dir and returns its path. It also
// removes a legacy claude-usage-bar wrapper (file or symlink) and returns
// its path in removed.
func installPlugin(dir, exe, version string) (p, removed string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	p = filepath.Join(dir, pluginName)
	if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
		return "", "", fmt.Errorf("%s is a directory", p)
	}
	// Temp file + rename replaces a symlink instead of writing through it.
	f, err := os.CreateTemp(dir, ".agent-usage-bar-*")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(pluginScript(exe, version))
	cerr := f.Close()
	if err := cmp.Or(werr, cerr, os.Chmod(f.Name(), 0o755)); err != nil {
		return "", "", err
	}
	if err := os.Rename(f.Name(), p); err != nil {
		return "", "", err
	}
	old := filepath.Join(dir, legacyName+".1m.sh")
	if fi, err := os.Lstat(old); err == nil && !fi.IsDir() {
		if err := os.Remove(old); err == nil {
			removed = old
		} else if !os.IsNotExist(err) {
			return p, "", err
		}
	}
	return p, removed, nil
}

func runInstallPlugin(home string, w io.Writer) error {
	p, removed, err := installPlugin(pluginDir(home), selfPath(), versionString())
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "wrote", p)
	if removed != "" {
		fmt.Fprintln(w, "removed", removed)
	}
	return nil
}
