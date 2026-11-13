// Command agent-usage-bar is a SwiftBar plugin that shows Claude, Codex
// and Antigravity subscription usage for every account as battery icons.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
)

var version = "dev"

const usageText = `agent-usage-bar - Claude, Codex and Antigravity usage battery icons for SwiftBar

Usage: agent-usage-bar [flag]

Without a flag it prints SwiftBar output.

Flags:
  --force       fetch all accounts now (ignore the 3 min interval), print nothing
  --png FILE    write the current menu bar image to FILE
  --json        print the parsed state (no tokens)
  --toggle-bar ID
                show or hide an account in the menu bar (ID from the dropdown);
                rewrites the config file
  --install-plugin
                write the SwiftBar wrapper for this binary into SwiftBar's plugin folder
  --help        show this help
  --version     show the version
`

// versionString prefers the ldflags version, then the module version from
// go install.
func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var force, asJSON, install, toggle bool
	var pngFile, toggleID string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force":
			force = true
		case "--json":
			asJSON = true
		case "--install-plugin":
			install = true
		case "--png":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "--png needs a file")
				return 2
			}
			i++
			pngFile = args[i]
		case "--toggle-bar":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "--toggle-bar needs an account id")
				return 2
			}
			i++
			toggle, toggleID = true, args[i]
		case "--help", "-h":
			fmt.Fprint(stdout, usageText)
			return 0
		case "--version":
			fmt.Fprintln(stdout, versionString())
			return 0
		default:
			fmt.Fprintf(stderr, "unknown argument %q\n%s", args[i], usageText)
			return 2
		}
	}

	a, err := newApp()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if toggle {
		if err := toggleBar(a.configPath, a.home, toggleID); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if install {
		if err := runInstallPlugin(a.home, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	return a.main(force, asJSON, pngFile, stdout, stderr)
}

func (a *app) main(force, asJSON bool, pngFile string, stdout, stderr io.Writer) int {
	states := a.collect(force)
	now := a.now()
	dark := os.Getenv("OS_APPEARANCE") == "Dark"
	switch {
	case pngFile != "":
		p, err := encodePNG(renderIcon(barGauges(states, now), dark, true))
		if err == nil {
			err = os.WriteFile(pngFile, p, 0o644)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case asJSON:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(states); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case force:
		// Caches are updated; SwiftBar reruns the plugin for display.
	default:
		if err := writeMenu(stdout, states, now, dark, selfPath(), a.home); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}

func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "agent-usage-bar"
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		return p
	}
	return exe
}
