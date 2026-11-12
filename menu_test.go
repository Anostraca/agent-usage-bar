package main

import (
	"bytes"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRow(t *testing.T) {
	got := row("5h", 93, "resets 16:40 (in 23m)")
	want := "5h    █░░░░░░░░░░░░   7% left   resets 16:40 (in 23m)"
	if got != want {
		t.Errorf("row =\n%q\nwant\n%q", got, want)
	}
	if got := row("week", 35, ""); got != "week  ████████░░░░░  65% left" {
		t.Errorf("row = %q", got)
	}
}

func TestHostileTextNoParamInjection(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 16, 0, 0, time.Local)
	evil := `x" bash=/usr/bin/open param1=-a terminal=false y="`
	states := []State{{
		Name: `n'|a href=http://e`, Email: "e\"@example.com\nbash=/bin/sh | color=red", Org: evil, HasData: true, Bar: true,
		Cache: Cache{Plan: `p" href='h'`, Week: &Window{Percent: 5}, FetchedAt: now, Error: `boom" bash=x`},
	}}
	var b bytes.Buffer
	if err := writeMenu(&b, states, now, false, "/bin/cub", "/h"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if n := strings.Count(out, "bash="); n != 2 { // refresh + bar toggle
		t.Errorf("bash= appears %d times\n%s", n, out)
	}
	if n := strings.Count(out, "href="); n != 1 {
		t.Errorf("href= appears %d times\n%s", n, out)
	}
	head := strings.Split(out, "\n")[2]
	_, params, _ := strings.Cut(head, " | ")
	// Only image= and one quoted tooltip= without inner quotes.
	if !regexp.MustCompile(`^image=[A-Za-z0-9+/=]+ tooltip="[^"']*"$`).MatchString(params) {
		t.Errorf("header params injected: %q", params)
	}
	if strings.Count(head, "|") != 1 {
		t.Errorf("header has extra separators: %q", head)
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		23 * time.Minute:                    "23m",
		2*time.Hour + 5*time.Minute:         "2h 5m",
		(5*24+4)*time.Hour + 10*time.Minute: "5d 4h",
		10 * time.Second:                    "<1m",
	}
	for d, want := range cases {
		if got := duration(d); got != want {
			t.Errorf("duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestWriteMenu(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 16, 0, 0, time.Local)
	states := []State{
		{Name: "work", Email: "a@example.com", HasData: true, Bar: true, Cache: Cache{
			Plan:      "team 5x",
			Session:   &Window{Percent: 93, ResetsAt: now.Add(23 * time.Minute)},
			Week:      &Window{Percent: 35, ResetsAt: now.Add(5*24*time.Hour + 4*time.Hour)},
			Scoped:    []Window{{Label: "Fable", Percent: 80, ResetsAt: now.Add(time.Hour)}},
			FetchedAt: now,
		}, Sessions: []Session{
			{PID: 1, Name: "api", Cwd: "/h/src/api", Status: "busy"},
			{PID: 2, Name: "docs-site", Cwd: "/srv/docs", Status: "idle"},
			{PID: 3, Name: "web", Cwd: "/h/src/web", Status: "busy"},
		}, Stats: &Stats{
			Today:    Totals{Input: 1000, Output: 700_000, CacheWrite: 2_300_000, CacheRead: 82_000_000, Msgs: 637},
			Week:     Totals{Input: 1, Output: 13_000_000, CacheRead: 2_800_000_000, Msgs: 12_700},
			Hourly:   []int64{0, 0, 10, 50, 100},
			Projects: []Count{{"api", 60_000_000}, {"web", 25_000_000}},
			Models:   []Count{{"claude-opus-5-5", 2_520_000_000}, {"claude-sonnet-5", 293_000_000}},
		}},
		{Name: "personal", HasData: true, Stale: true, Bar: true, Cache: Cache{
			Usage: Usage{
				Week:     &Window{Percent: 10, ResetsAt: now.Add(-time.Minute)},
				Surfaces: []Share{{"Claude Code", 100}},
				Credits:  "$1.50",
			},
			FetchedAt: now.Add(-20 * time.Minute),
			Error:     "HTTP 429",
		}},
		{Name: "idle", HasData: true, Bar: true, Cache: Cache{Usage: Usage{
			Session: &Window{Idle: true},
			Week:    &Window{Percent: 1, ResetsAt: now.Add(time.Hour)},
		}, FetchedAt: now}},
		{Name: "new", Bar: true, Cache: Cache{Error: errNoCreds.Error()}},
	}
	var b bytes.Buffer
	if err := writeMenu(&b, states, now, false, "/opt/my bin/cub", "/h"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"| image=",
		"work · team 5x · a@example.com | image=",
		"5h    █░░░░░░░░░░░░   7% left   resets 15:39 (in 23m) | font=Menlo size=12 color=#E5534B",
		"week  ████████░░░░░  65% left   resets ",
		"(in 5d 4h) | font=Menlo size=12\n",
		"Fable ███░░░░░░░░░░  20% left   resets 16:16 (in 1h 0m) | font=Menlo size=12 color=#E0A030",
		"● 3 sessions · 2 busy, 1 idle\n",
		"--api        ~/src/api  busy | font=Menlo size=12\n",
		"--docs-site  /srv/docs  idle |",
		"Today   85M tok · 700k out · 637 msgs · cache 97% | font=Menlo size=12\n",
		"7 days  2.8B tok · 13M out · 12.7k msgs | font=Menlo size=12\n",
		"today ▁▁▂▅█ | font=Menlo size=12 color=#8E8E93\n",
		"Top projects today\n--api     60M tok |",
		"Models · 7 days\n--claude-opus-5-5   90% |",
		"personal · no 5h limit | image=",
		"week  █████████████ 100% left   idle · starts with next message |",
		"week by surface: Claude Code 100% | color=#8E8E93 size=11",
		"credits used $1.50 | color=#8E8E93 size=11",
		"5h    █████████████ 100% left   idle · starts with next message | font=Menlo size=12\n",
		"stale · HTTP 429 · updated 14:56 | color=#8E8E93 size=11",
		"no data · " + errNoCreds.Error(),
		"Updated 15:16 |",
		`Refresh now | bash="/opt/my bin/cub" param1=--force terminal=false refresh=true`,
		"\nUsage page | href=https://claude.ai/settings/usage\n",
		`Show in menu bar | bash="/opt/my bin/cub" param1=--toggle-bar param2=` + hashHex("work", 4) + " terminal=false refresh=true checked=true\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing %q\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "| image=") {
		t.Errorf("first line: %q", strings.SplitN(out, "\n", 2)[0])
	}
	if strings.Count(out, "5h    ") != 2 {
		t.Errorf("want 5h rows only for accounts with a 5h limit\n%s", out)
	}
	if strings.Count(out, "Show in menu bar |") != 4 || strings.Contains(out, "Claude usage page") ||
		strings.Contains(out, "credits unlimited") || strings.Contains(out, "credits balance") {
		t.Errorf("claude-only extras:\n%s", out)
	}
}

func TestWriteMenuBarAndProviders(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 16, 0, 0, time.Local)
	states := []State{
		{Name: "team", Bar: true, HasData: true, Cache: Cache{Usage: Usage{Week: &Window{Percent: 10}}, FetchedAt: now}},
		{Name: "max", HasData: true, Cache: Cache{Usage: Usage{Week: &Window{Percent: 50}}, FetchedAt: now}},
		{Name: "cx", Provider: "codex", Bar: true, Email: "cx@example.com", HasData: true, Cache: Cache{Plan: "team", Usage: Usage{
			Session: &Window{Label: "5h", Percent: 20, ResetsAt: now.Add(time.Hour)},
			Week:    &Window{Label: "3d", Percent: 80, ResetsAt: now.Add(48 * time.Hour)},
			Scoped:  []Window{{Label: "review week", Percent: 3, Idle: true}},
			Balance: "unlimited",
		}, FetchedAt: now}},
	}
	var b bytes.Buffer
	if err := writeMenu(&b, states, now, false, "/bin/cub", "/h"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	title, err := iconB64([]gauge{toGauge(states[0], now), toGauge(states[2], now)}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if first := strings.SplitN(out, "\n", 2)[0]; first != "| image="+title {
		t.Errorf("title is not the bar accounts' icon: %.60s", first)
	}
	for _, want := range []string{
		"max · no 5h limit | image=",
		"cx · team · cx@example.com | image=",
		"5h    ██████████░░░  80% left   resets 16:16 (in 1h 0m) |",
		"3d    ███░░░░░░░░░░  20% left   resets ",
		"review week█████████████ 100% left   idle · starts with next message |",
		"credits unlimited | color=#8E8E93 size=11\n",
		"Show in menu bar | bash=/bin/cub param1=--toggle-bar param2=" + hashHex("team", 4) + " terminal=false refresh=true checked=true\n",
		"Show in menu bar | bash=/bin/cub param1=--toggle-bar param2=" + hashHex("max", 4) + " terminal=false refresh=true\n",
		"Show in menu bar | bash=/bin/cub param1=--toggle-bar param2=" + hashHex(accountKey("codex", "cx"), 4) + " terminal=false refresh=true checked=true\n",
		"\nClaude usage page | href=https://claude.ai/settings/usage\nCodex usage page | href=https://chatgpt.com/codex/settings/usage\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nUsage page") || strings.Count(out, "week  ") != 2 { // team and max only
		t.Errorf("unexpected lines\n%s", out)
	}

	// Nothing in the bar: a text title keeps the menu clickable.
	for i := range states {
		states[i].Bar = false
	}
	b.Reset()
	if err := writeMenu(&b, states, now, false, "/bin/cub", "/h"); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.HasPrefix(out, "AI\n---\n") || strings.Count(out, "image=") != 3 ||
		strings.Contains(out, "checked=true") {
		t.Errorf("all hidden:\n%s", out)
	}
}

func TestPNGBarAccountsOnly(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.json")
	writeFile(t, cfg, `{"accounts":[{"name":"a"},{"name":"b","bar":false},{"name":"c","provider":"codex"}]}`)
	a := &app{home: home, configPath: cfg, cacheDir: filepath.Join(home, "cache"),
		now: time.Now, keychain: func(string) ([]byte, error) { return nil, errors.New("none") }}
	file := filepath.Join(home, "bar.png")
	var out, errOut bytes.Buffer
	if code := a.main(false, false, file, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	data, _ := os.ReadFile(file)
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if w := img.Bounds().Dx(); w != int((2*gaugeWidth(true)+accountGap)*pxPerPt) {
		t.Errorf("width %d, want two gauges", w)
	}
}

func TestHuman(t *testing.T) {
	cases := map[int64]string{0: "0", 950: "950", 1000: "1k", 12_700: "12.7k", 123_400: "123k", 2_500: "2.5k",
		85_000_000: "85M", 700_000: "700k", 2_800_000_000: "2.8B"}
	for n, want := range cases {
		if got := human(n); got != want {
			t.Errorf("human(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestShortPath(t *testing.T) {
	cases := map[string]string{
		"/h":         "~",
		"/h/src/api": "~/src/api",
		"/srv/x":     "/srv/x",
		"/h/a/very/long/path/that/goes/on/and/on/proj": "~/…/on/proj",
		"": "?",
	}
	for p, want := range cases {
		if got := shortPath(p, "/h"); got != want {
			t.Errorf("shortPath(%q) = %q, want %q", p, got, want)
		}
	}
}
