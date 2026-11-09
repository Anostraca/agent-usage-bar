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

const pluginName = "claude-usage-bar.1m.sh"

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
# <xbar.title>Claude Usage</xbar.title>
# <xbar.version>` + version + `</xbar.version>
# <xbar.author>Jiří Grmolec</xbar.author>
# <xbar.author.github>Anostraca</xbar.author.github>
# <xbar.desc>Claude session and weekly usage as battery icons</xbar.desc>
# <xbar.dependencies>claude-usage-bar,claude-code</xbar.dependencies>
# <xbar.abouturl>https://github.com/Anostraca/claude-usage-bar</xbar.abouturl>
# <swiftbar.hideAbout>true</swiftbar.hideAbout>
# <swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>
# <swiftbar.hideLastUpdated>true</swiftbar.hideLastUpdated>
exec '` + strings.ReplaceAll(exe, `'`, `'\''`) + `' "$@"
`
}

// installPlugin writes the wrapper into dir and returns its path.
func installPlugin(dir, exe, version string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, pluginName)
	if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	// Temp file + rename replaces a symlink instead of writing through it.
	f, err := os.CreateTemp(dir, ".claude-usage-bar-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(pluginScript(exe, version))
	cerr := f.Close()
	if err := cmp.Or(werr, cerr, os.Chmod(f.Name(), 0o755)); err != nil {
		return "", err
	}
	return p, os.Rename(f.Name(), p)
}

func runInstallPlugin(home string, w io.Writer) error {
	p, err := installPlugin(pluginDir(home), selfPath(), versionString())
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "wrote", p)
	return nil
}
