package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const agyCSRF = "11111111-2222-3333-4444-FAKECSRFTOKEN"

const agySample = `{"userStatus":{
  "name":"Me","email":"agy@example.com",
  "planStatus":{"planInfo":{"teamsTier":"TEAMS_TIER_PRO","planName":"Pro"},"availablePromptCredits":500,"availableFlowCredits":100},
  "cascadeModelConfigData":{"clientModelConfigs":[
    {"label":"Gemini 3.8 Flash (High)","modelOrAlias":{"model":"MODEL_PLACEHOLDER_M318"},"quotaInfo":{"remainingFraction":0.995996,"resetTime":"2026-11-19T23:12:25Z"}},
    {"label":"Gemini 3.8 Flash (Medium)", "quotaInfo":{"remainingFraction":0.995996,"resetTime":"2026-11-19T23:12:25Z"}},
    {"label":"Gemini 3.8 Flash (Low)",    "quotaInfo":{"remainingFraction":0.995996,"resetTime":"2026-11-19T23:12:25Z"}},
    {"label":"Claude Sonnet 4.6 (Thinking)","quotaInfo":{"remainingFraction":1,"resetTime":"2026-11-19T23:13:27Z"}},
    {"label":"Claude Opus 4.6 (Thinking)",  "quotaInfo":{"remainingFraction":1,"resetTime":"2026-11-19T23:13:27Z"}},
    {"label":"GPT-OSS 120B (Medium)",       "quotaInfo":{"remainingFraction":1,"resetTime":"2026-11-19T23:13:27Z"}},
    {"label":"Gemini 4 Carbon","quotaInfo":{}},
    {"label":"Gemini 4 Argon","quotaInfo":{}},
    {"label":"Gemini 4 Hydrogen","quotaInfo":{}}]},
  "userTier":{"id":"g1-plus-tier","name":"Antigravity Starter Quota"},
  "profilePictureUrl":"data:image/png;base64,AAAA"}}`

func TestParseAgy(t *testing.T) {
	u, plan, email, err := parseAgy([]byte(agySample))
	if err != nil || plan != "Starter Quota" || email != "agy@example.com" {
		t.Fatalf("plan %q email %q err %v", plan, email, err)
	}
	var labels []string
	for _, w := range u.Scoped {
		labels = append(labels, w.Label)
	}
	if got := strings.Join(labels, "|"); got != "Gemini 3.8 Flash|Claude Sonnet 4.6|Claude Opus 4.6|GPT-OSS 120B" {
		t.Errorf("labels = %s", got)
	}
	g := u.Scoped[0]
	if g.Percent < 0.39 || g.Percent > 0.41 || g.Idle || g.ResetsAt.Format(time.RFC3339) != "2026-11-19T23:12:25Z" {
		t.Errorf("gemini = %+v", g)
	}
	if s := u.Scoped[1]; s.Percent != 0 || !s.Idle {
		t.Errorf("unused model = %+v", s)
	}
	if u.Session != nil || u.Week != nil {
		t.Errorf("session %+v week %+v", u.Session, u.Week)
	}
	if low := lowestWindow(u.Scoped, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)); low == nil || low.Label != "lowest" ||
		low.Percent != g.Percent || low.Idle || u.Scoped[0].Label != "Gemini 3.8 Flash" {
		t.Errorf("lowest = %+v", low)
	}
	if u.Credits != "" || u.Balance != "" {
		t.Errorf("credits %q balance %q", u.Credits, u.Balance)
	}

	// Variants merge to the most used; a reset without a fraction is used
	// up; bad entries are skipped; the plan falls back to planName.
	u, plan, _, err = parseAgy([]byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"}},
		"cascadeModelConfigData":{"clientModelConfigs":[
		{"label":"M (Low)","quotaInfo":{"remainingFraction":0.9,"resetTime":"2026-11-19T23:12:25Z"}},
		{"label":"M (High)","quotaInfo":{"remainingFraction":0.25,"resetTime":"2026-11-19T20:00:00Z"}},
		{"label":"M (Medium)","quotaInfo":{"remainingFraction":0.5}},
		{"label":"Gone","quotaInfo":{"resetTime":"2026-11-14T10:00:00Z"}},
		{"label":"Odd","quotaInfo":"x"}, 7, {"label":"NoQuota"},
		{"label":"Over (x)","quotaInfo":{"remainingFraction":-0.5,"resetTime":"bad"}}]}}}`))
	want := []Window{
		{Label: "M", Percent: 75, ResetsAt: time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)},
		{Label: "Gone", Percent: 100, ResetsAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)},
		{Label: "Over", Percent: 100},
	}
	if err != nil || plan != "Pro" || fmt.Sprint(u.Scoped) != fmt.Sprint(want) {
		t.Errorf("merged: plan %q err %v\n%+v", plan, err, u.Scoped)
	}
	if low := lowestWindow(u.Scoped, want[1].ResetsAt.Add(-time.Hour)); low == nil || low.Percent != 100 || !low.ResetsAt.Equal(want[1].ResetsAt) {
		t.Errorf("lowest = %+v", low)
	}

	for bad, msg := range map[string]string{
		`<html>`:              "bad API response",
		`{}`:                  "bad API response",
		`{"userStatus":null}`: "bad API response",
		`{"userStatus":{"cascadeModelConfigData":{"clientModelConfigs":[]}}}`:                                "no model quotas",
		`{"userStatus":{"cascadeModelConfigData":{"clientModelConfigs":[{"label":"A","quotaInfo":{}},{}]}}}`: "no model quotas",
	} {
		if _, _, _, err := parseAgy([]byte(bad)); err == nil || err.Error() != msg {
			t.Errorf("parseAgy(%s): %v, want %s", bad, err, msg)
		}
	}
	if lowestWindow(nil, time.Now()) != nil {
		t.Error("lowest of nothing")
	}
}

func TestParsePS(t *testing.T) {
	out := `  123 /Applications/Antigravity.app/Contents/Resources/bin/language_server --standalone --override_ide_name antigravity --https_server_port 0 --csrf_token ` + agyCSRF + ` --app_data_dir antigravity --host_bridge_token=BRIDGE-SECRET
  124 /Users/me/My Apps/Antigravity.app/Contents/Resources/bin/language_server --csrf_token=tok-2 --app_data_dir=antigravity
  125 /Applications/Windsurf.app/Contents/Resources/bin/language_server --csrf_token tok-3 --app_data_dir windsurf
  126 /Applications/Antigravity.app/Contents/Resources/bin/language_server --csrf_token tok-4 --app_data_dir other
  127 /Applications/Antigravity.app/Contents/Resources/bin/language_server --app_data_dir antigravity
  128 /Applications/Antigravity.app/Contents/Resources/bin/language_server --csrf_token bad|tok --app_data_dir antigravity
  129 /Applications/Antigravity.app/Contents/Resources/bin/language_server_x --csrf_token tok-5 --app_data_dir antigravity
  130 grep language_server --app_data_dir antigravity
  131 /usr/bin/grep -e /Applications/Antigravity.app/Contents/Resources/bin/language_server --csrf_token tok-7 --app_data_dir antigravity
  132 grep /Applications/Antigravity.app/Contents/Resources/bin/language_server --csrf_token tok-8 --app_data_dir antigravity
  abc /Applications/Antigravity.app/Contents/Resources/bin/language_server --csrf_token tok-6 --app_data_dir antigravity
`
	got := parsePS([]byte(out))
	// A server without a usable token is still found, so it is not "not running".
	want := []agyProc{{pid: 123, csrf: agyCSRF}, {pid: 124, csrf: "tok-2"}, {pid: 127}, {pid: 128}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("parsePS = %v", got)
	}
}

func TestParseLsof(t *testing.T) {
	out := "p49240\nf7\nn127.0.0.1:53273\nf8\nn127.0.0.1:53274\nf9\nn*:5000\nf10\nn[::1]:6000\nf11\nn10.0.0.2:7000\nf12\nn127.0.0.1:99999\nnjunk\n"
	if got := fmt.Sprint(parseLsof([]byte(out))); got != "[53273 53274 5000]" {
		t.Errorf("parseLsof = %s", got)
	}
}

func TestAgyClientLoopbackOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	c := agyClient()
	if resp, err := c.Get("http://127.0.0.1:" + port); err != nil {
		t.Errorf("loopback: %v", err)
	} else {
		resp.Body.Close()
	}
	if _, err := c.Get("http://localhost:" + port); err == nil {
		t.Error("dialled a host other than 127.0.0.1")
	}
}

type agyEnv struct {
	*app
	clock time.Time
	finds atomic.Int32
	hits  atomic.Int32
	ports []int
	csrf  atomic.Int32 // requests with the right csrf header
	body  string       // served instead of agySample; set before the server starts

	findErr  error // ps failed
	noPorts  bool  // a server runs but has no listening port
	badToken bool  // a server runs but its token didn't parse
}

func portOf(t *testing.T, u string) int {
	t.Helper()
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(pu.Port())
	return p
}

// newAgyEnv sets up one "agy" account; ports are what discovery reports.
func newAgyEnv(t *testing.T) *agyEnv {
	t.Helper()
	home := t.TempDir()
	e := &agyEnv{clock: time.Date(2026, 11, 13, 0, 13, 54, 0, time.UTC)}
	cfg := filepath.Join(home, "config.json")
	writeFile(t, cfg, `{"accounts":[{"name":"agy","provider":"antigravity","config_dir":"relative/ignored"}]}`)
	e.app = &app{
		home:       home,
		configPath: cfg,
		cacheDir:   filepath.Join(home, "cache"),
		now:        func() time.Time { return e.clock },
		agyHTTP:    agyClient(),
	}
	e.agyFind = func() ([]agyProc, error) {
		e.finds.Add(1)
		switch {
		case e.findErr != nil:
			return nil, e.findErr
		case len(e.ports) == 0 && !e.noPorts:
			return nil, nil
		case e.badToken:
			return []agyProc{{pid: 1, ports: e.ports}}, nil
		}
		return []agyProc{{pid: 1, csrf: agyCSRF, ports: e.ports}}, nil
	}
	return e
}

// handler serves the sample to a well-formed request.
func (e *agyEnv) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		if r.Header.Get("X-Codeium-Csrf-Token") == agyCSRF {
			e.csrf.Add(1)
		}
		if r.Method != http.MethodPost || r.URL.Path != agyStatusPath || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Connect-Protocol-Version") != "1" || r.Header.Get("X-Codeium-Csrf-Token") != agyCSRF {
			w.WriteHeader(403)
			return
		}
		fmt.Fprint(w, cmp.Or(e.body, agySample))
	})
}

func (e *agyEnv) one(t *testing.T, force bool) State {
	t.Helper()
	st := e.collect(force)
	if len(st) != 1 {
		t.Fatalf("got %d states", len(st))
	}
	return st[0]
}

func TestAgyRefresh(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  func(http.Handler) *httptest.Server
	}{{"tls", httptest.NewTLSServer}, {"plain", httptest.NewServer}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAgyEnv(t)
			unauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(401)
				fmt.Fprint(w, "unauthorized")
			}))
			defer unauth.Close()
			good := tc.srv(e.handler())
			defer good.Close()
			e.ports = []int{portOf(t, unauth.URL), portOf(t, good.URL)}
			s := e.one(t, false)
			if !s.HasData || s.Stale || s.Error != "" || s.Provider != "antigravity" || s.Plan != "Starter Quota" ||
				s.Email != "agy@example.com" || s.Week == nil || s.Week.Label != "lowest" || len(s.Scoped) != 4 || s.Stats != nil {
				t.Fatalf("state = %+v", s)
			}
			if e.csrf.Load() != 1 || e.hits.Load() != 1 {
				t.Errorf("good server: hits %d with csrf %d", e.hits.Load(), e.csrf.Load())
			}
			// A cached fetch within the interval: no new call.
			e.clock = e.clock.Add(time.Minute)
			hits := e.hits.Load()
			e.one(t, false)
			if e.hits.Load() != hits {
				t.Error("fetched within interval")
			}
		})
	}
}

func TestAgyOnlyUnauthorized(t *testing.T) {
	e := newAgyEnv(t)
	unauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer unauth.Close()
	e.ports = []int{portOf(t, unauth.URL)}
	s := e.one(t, false)
	if s.HasData || s.Error != "HTTP 401" || s.AttemptAt.IsZero() {
		t.Errorf("state = %+v", s)
	}
	e.ports = []int{1} // nothing listens: network errors only
	if s = e.one(t, true); s.Error != errAgyNoServer {
		t.Errorf("no answer: %+v", s)
	}
}

func TestAgyNotRunning(t *testing.T) {
	// Never ran: the no-data battery, retried every run, no attempt.
	e := newAgyEnv(t)
	s := e.one(t, false)
	e.one(t, false)
	if s.HasData || s.Stale || s.Error != errAgyNotRunning || !s.AttemptAt.IsZero() || e.finds.Load() != 2 {
		t.Errorf("never ran: %+v finds %d", s, e.finds.Load())
	}

	// Closed after a fetch: the data stays current, not dimmed.
	good := httptest.NewTLSServer(e.handler())
	defer good.Close()
	e.ports = []int{portOf(t, good.URL)}
	e.one(t, false)
	e.ports = nil
	e.clock = e.clock.Add(fetchInterval + staleAfter)
	s = e.one(t, false)
	if !s.HasData || s.Stale || s.Error != errAgyNotRunning || s.Week == nil {
		t.Fatalf("closed: %+v", s)
	}
	var b bytes.Buffer
	if err := writeMenu(&b, []State{s}, e.clock, false, "/bin/cub", "/h"); err != nil {
		t.Fatal(err)
	}
	updated := s.FetchedAt.Local().Format("15:04")
	if !strings.Contains(b.String(), "\n--"+errAgyNotRunning+" · updated "+updated+" | color=") ||
		!strings.Contains(b.String(), "\nagy   lowest 100% | image=") {
		t.Errorf("menu:\n%s", b.String())
	}
	if g := toGauge(s, e.clock); g.Stale || g.NoData || !g.NoSession {
		t.Errorf("gauge = %+v", g)
	}

	// Running again: fetched at once, not stale.
	e.ports = []int{portOf(t, good.URL)}
	e.clock = e.clock.Add(time.Minute)
	if s = e.one(t, false); s.Error != "" || !s.FetchedAt.Equal(e.clock) {
		t.Errorf("reopened: %+v", s)
	}
}

func TestWriteMenuAntigravity(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 13, 54, 0, time.Local)
	u, plan, email, err := parseAgy([]byte(agySample))
	if err != nil {
		t.Fatal(err)
	}
	u.Week = lowestWindow(u.Scoped, now)
	agy := State{Name: "agy", Provider: "antigravity", Bar: true, Email: email, HasData: true,
		Cache: Cache{Plan: plan, Usage: u, FetchedAt: now}}
	var b bytes.Buffer
	if err := writeMenu(&b, []State{agy}, now, false, "/bin/cub", "/h"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"\nagy   lowest 100% | image=",
		"\n--agy · Starter Quota · agy@example.com\n",
		"\n--lowest            █████████████ 100% left   resets ",
		"\n--Gemini 3.8 Flash  █████████████ 100% left   resets ",
		"\n--Claude Sonnet 4.6 █████████████ 100% left   idle · starts with next message |",
		"\n--GPT-OSS 120B      █████████████ 100% left   idle",
		"\n-----\n--Show in menu bar | bash=/bin/cub param1=--toggle-bar param2=" + hashHex(accountKey("antigravity", "agy"), 4) +
			" terminal=false refresh=true checked=true\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "no 5h limit") || strings.Contains(strings.ToLower(out), "usage page") || strings.Contains(out, "credits") {
		t.Errorf("unexpected lines:\n%s", out)
	}
	if i, j := strings.Index(out, "\n--lowest"), strings.Index(out, "\n--Gemini"); i < 0 || i > j {
		t.Error("lowest row not above the model rows")
	}

	// With Claude, only Claude's link, as before.
	b.Reset()
	claude := State{Name: "work", Bar: true, HasData: true, Cache: Cache{Usage: Usage{Week: &Window{Percent: 1}}, FetchedAt: now}}
	if err := writeMenu(&b, []State{claude, agy}, now, false, "/bin/cub", "/h"); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.HasSuffix(out, "\nUsage page | href="+usageURL+"\n") || strings.Contains(out, "Claude usage page") {
		t.Errorf("links:\n%s", out)
	}
}

func TestAgyTokenNeverLeaks(t *testing.T) {
	e := newAgyEnv(t)
	good := httptest.NewTLSServer(e.handler())
	defer good.Close()
	e.ports = []int{portOf(t, good.URL)}
	var out, errOut bytes.Buffer
	for _, json := range []bool{false, true} {
		if code := e.main(false, json, "", &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
	}
	e.ports = []int{1}
	e.main(true, false, "", &out, &errOut)
	e.main(false, true, "", &out, &errOut)
	all := out.String() + errOut.String()
	filepath.Walk(e.cacheDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			all += string(b)
		}
		return nil
	})
	if strings.Contains(all, agyCSRF) || strings.Contains(all, "FAKECSRF") || strings.Contains(all, "data:image") {
		t.Fatal("output or cache contains the csrf token or the picture")
	}
	if !strings.Contains(all, `"provider": "antigravity"`) || !strings.Contains(all, errAgyNoServer) {
		t.Errorf("expected JSON and error:\n%s", all)
	}
}

func TestAgyDiscoveryFailures(t *testing.T) {
	good := func(e *agyEnv) *httptest.Server { return httptest.NewTLSServer(e.handler()) }
	for _, tc := range []struct {
		name string
		set  func(e *agyEnv)
	}{
		{"ps failed", func(e *agyEnv) { e.findErr = errors.New("ps failed") }},
		{"no ports", func(e *agyEnv) { e.ports, e.noPorts = nil, true }},
		{"bad token", func(e *agyEnv) { e.badToken = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAgyEnv(t)
			srv := good(e)
			defer srv.Close()
			e.ports = []int{portOf(t, srv.URL)}
			e.one(t, false)
			tc.set(e)
			e.clock = e.clock.Add(fetchInterval)
			s := e.one(t, false)
			if s.Error != errAgyNoServer || !s.AttemptAt.Equal(e.clock) || s.Stale || !s.HasData {
				t.Errorf("fresh failure: %+v", s)
			}
			// A normal failed fetch: not retried within the interval, and
			// stale once the data is old.
			finds := e.finds.Load()
			e.clock = e.clock.Add(time.Minute)
			e.one(t, false)
			if e.finds.Load() != finds {
				t.Error("retried within the interval")
			}
			e.clock = e.clock.Add(staleAfter)
			if s = e.one(t, false); s.Error != errAgyNoServer || !s.Stale {
				t.Errorf("old failure: %+v", s)
			}
		})
	}
}

func TestAgyNoQuotasKeepsCache(t *testing.T) {
	e := newAgyEnv(t)
	e.body = `{"userStatus":{"email":"x@example.com","cascadeModelConfigData":{"clientModelConfigs":[{"label":"A","quotaInfo":{}}]}}}`
	srv := httptest.NewServer(e.handler())
	defer srv.Close()
	e.ports = []int{portOf(t, srv.URL)}
	if s := e.one(t, false); s.HasData || s.Error != "no model quotas" {
		t.Errorf("state = %+v", s)
	}
}

func TestAgyLowestAtDisplayTime(t *testing.T) {
	e := newAgyEnv(t)
	resetA := e.clock.Add(time.Hour)
	e.body = fmt.Sprintf(`{"userStatus":{"cascadeModelConfigData":{"clientModelConfigs":[
		{"label":"A","quotaInfo":{"remainingFraction":0,"resetTime":%q}},
		{"label":"B","quotaInfo":{"remainingFraction":0.4,"resetTime":%q}}]}}}`,
		resetA.Format(time.RFC3339), e.clock.Add(5*24*time.Hour).Format(time.RFC3339))
	srv := httptest.NewServer(e.handler())
	defer srv.Close()
	e.ports = []int{portOf(t, srv.URL)}
	if s := e.one(t, false); s.Week == nil || s.Week.Label != "lowest" || s.Week.Percent != 100 || toGauge(s, e.clock).Week != 100 {
		t.Fatalf("now: week %+v", s.Week)
	}
	e.ports = nil // app closed: the cached rows stay
	e.clock = e.clock.Add(2 * time.Hour)
	s := e.one(t, false)
	if s.Week == nil || s.Week.Percent < 59.9 || s.Week.Percent > 60.1 || toGauge(s, e.clock).Week != s.Week.Percent {
		t.Errorf("after A's reset: week %+v", s.Week)
	}
	if text, color := accountLine(s, mustProvider(t, "antigravity"), e.clock); text != "lowest 40%" || color != "" {
		t.Errorf("line = %q %q", text, color)
	}
}

func mustProvider(t *testing.T, name string) provider {
	t.Helper()
	p, ok := providerOf(name)
	if !ok {
		t.Fatalf("no provider %q", name)
	}
	return p
}
