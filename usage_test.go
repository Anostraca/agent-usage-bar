package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeToken = "sk-ant-oat01-FAKE-SECRET-TOKEN-do-not-leak"

const sampleUsage = `{"five_hour":{"utilization":93.0,"resets_at":"2026-11-09T15:39:59.945210+00:00"},
"seven_day":{"utilization":35.0,"resets_at":"2026-11-14T19:59:59.945233+00:00"},
"seven_day_opus":null,"seven_day_sonnet":{"utilization":12.5,"resets_at":"2026-11-14T19:59:59+00:00"},
"seven_day_oauth_apps":null,"extra_usage":null}`

type testEnv struct {
	*app
	clock  time.Time
	hits   atomic.Int32
	status atomic.Int32
	body   atomic.Value // string
	expiry int64        // token expiresAt, ms
}

// newTestEnv sets up one "max" account with a fake keychain and an
// httptest usage API.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	home := t.TempDir()
	e := &testEnv{clock: time.Date(2026, 11, 9, 15, 16, 0, 0, time.UTC)}
	e.status.Store(200)
	e.body.Store(sampleUsage)
	e.expiry = e.clock.Add(time.Hour).UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		if r.URL.Path != usagePath || r.Header.Get("Authorization") != "Bearer "+fakeToken ||
			r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(int(e.status.Load()))
		fmt.Fprint(w, e.body.Load().(string))
	}))
	t.Cleanup(srv.Close)
	e0 := filepath.Join(home, ".config", "agent-usage-bar", "config.json")
	writeFile(t, e0,
		`{"accounts":[{"name":"max","config_dir":"~/.claude-max"}]}`)
	writeFile(t, filepath.Join(home, ".claude-max", ".claude.json"),
		`{"oauthAccount":{"emailAddress":"me@example.com"}}`)
	e.app = &app{
		home:       home,
		configPath: e0,
		cacheDir:   filepath.Join(home, "cache"),
		baseURL:    srv.URL,
		now:        func() time.Time { return e.clock },
		client:     srv.Client(),
	}
	e.keychain = func(service string) ([]byte, error) {
		if service != keychainService(filepath.Join(home, ".claude-max"), home) {
			return nil, fmt.Errorf("unexpected service %q", service)
		}
		return fmt.Appendf(nil, `{"claudeAiOauth":{"accessToken":%q,"refreshToken":"refresh-secret","expiresAt":%d,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`,
			fakeToken, e.expiry), nil
	}
	return e
}

func (e *testEnv) one(t *testing.T, force bool) State {
	t.Helper()
	st := e.collect(force)
	if len(st) != 1 {
		t.Fatalf("got %d states", len(st))
	}
	return st[0]
}

func TestParseUsage(t *testing.T) {
	u, err := parseUsage([]byte(sampleUsage))
	if err != nil {
		t.Fatal(err)
	}
	if u.Session == nil || u.Session.Percent != 93 || u.Session.ResetsAt.Minute() != 39 {
		t.Errorf("session = %+v", u.Session)
	}
	if u.Week == nil || u.Week.Percent != 35 {
		t.Errorf("week = %+v", u.Week)
	}
	if len(u.Scoped) != 1 || u.Scoped[0].Label != "Sonnet" || u.Scoped[0].Percent != 12.5 {
		t.Errorf("scoped = %+v", u.Scoped)
	}

	// Without limits, a null five_hour means the plan has no 5h limit.
	u, err = parseUsage([]byte(`{"five_hour":null,"seven_day":{"utilization":0,"resets_at":null}}`))
	if err != nil || u.Session != nil || u.Week == nil || u.Week.Percent != 0 || !u.Week.Idle {
		t.Errorf("null session: %+v %+v %v", u.Session, u.Week, err)
	}
	u, _ = parseUsage([]byte(`{"five_hour":{"utilization":0.0,"resets_at":null}}`))
	if u.Session == nil || !u.Session.Idle {
		t.Errorf("idle five_hour = %+v", u.Session)
	}
	if _, err := parseUsage([]byte(`<html>`)); err == nil {
		t.Error("want error for bad JSON")
	}
}

const teamLimits = `{"five_hour":{"utilization":0.0,"resets_at":null},
"seven_day":{"utilization":35.0,"resets_at":"2026-11-14T20:00:00.164822+00:00"},
"limits":[{"kind":"session","group":"session","percent":0,"severity":"normal","resets_at":null,"is_active":false,"scope":null},
{"kind":"weekly_all","group":"weekly","percent":35,"severity":"normal","resets_at":"2026-11-14T20:00:00.164822+00:00","is_active":true,"scope":null},
{"kind":"weekly_scoped","group":"weekly","percent":4,"resets_at":"2026-11-14T20:00:00+00:00","is_active":false,"scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}],
"spend":{"used":{"amount_minor":1234,"currency":"USD","exponent":2},"limit":null,"percent":0,"enabled":true}}`

const maxLimits = `{"five_hour":null,"seven_day":{"utilization":2.0,"resets_at":"2026-11-13T18:00:00.742184+00:00"},
"limits":[{"kind":"weekly_all","group":"weekly","percent":2,"resets_at":"2026-11-13T18:00:00.742184+00:00","is_active":true,"scope":null},
{"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-11-13T18:00:00+00:00","is_active":false,"scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}],
"seven_day_breakdown":{"as_of":"2026-11-09T15:43:53Z","window_started_at":"2026-11-06T18:00:00Z","rows":[
{"key":"claude_code","display_name":"Claude Code","percent":100},{"key":"chat","display_name":"Chats","percent":0}]},
"spend":{"used":{"amount_minor":0,"currency":"USD","exponent":2},"enabled":false}}`

func TestParseLimits(t *testing.T) {
	u, err := parseUsage([]byte(teamLimits))
	if err != nil {
		t.Fatal(err)
	}
	if u.Session == nil || !u.Session.Idle || u.Session.Percent != 0 {
		t.Errorf("team session = %+v", u.Session)
	}
	if u.Week == nil || u.Week.Percent != 35 || u.Week.Idle || u.Week.ResetsAt.Hour() != 20 {
		t.Errorf("team week = %+v", u.Week)
	}
	if len(u.Scoped) != 1 || u.Scoped[0].Label != "Fable" || u.Scoped[0].Percent != 4 || u.Scoped[0].Idle {
		t.Errorf("team scoped = %+v", u.Scoped)
	}
	if u.Credits != "$12.34" {
		t.Errorf("credits = %q", u.Credits)
	}

	// is_active marks the binding limit, not a started window: a running
	// session below the weekly limit has is_active false.
	u, err = parseUsage([]byte(`{"limits":[
{"kind":"session","percent":40,"resets_at":"2026-11-13T13:30:00.728925+00:00","is_active":false},
{"kind":"weekly_all","percent":50,"resets_at":"2026-11-14T20:00:00.728947+00:00","is_active":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if u.Session == nil || u.Session.Percent != 40 || u.Session.Idle {
		t.Errorf("running session = %+v", u.Session)
	}

	u, err = parseUsage([]byte(maxLimits))
	if err != nil {
		t.Fatal(err)
	}
	if u.Session != nil {
		t.Errorf("max has no 5h limit, got %+v", u.Session)
	}
	if u.Week == nil || u.Week.Percent != 2 || len(u.Scoped) != 1 {
		t.Errorf("max week = %+v scoped = %+v", u.Week, u.Scoped)
	}
	if len(u.Surfaces) != 1 || u.Surfaces[0] != (Share{"Claude Code", 100}) || u.Credits != "" {
		t.Errorf("surfaces = %+v credits = %q", u.Surfaces, u.Credits)
	}
}

func TestRefreshFetchesAndCaches(t *testing.T) {
	e := newTestEnv(t)
	s := e.one(t, false)
	if e.hits.Load() != 1 || !s.HasData || s.Stale || s.Error != "" {
		t.Fatalf("state = %+v hits = %d", s, e.hits.Load())
	}
	if s.Plan != "max 20x" || s.Email != "me@example.com" || s.Week.Percent != 35 {
		t.Errorf("state = %+v", s)
	}
	fi, err := os.Stat(e.cacheDir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("cache dir: %v %v", fi, err)
	}
	if _, err := os.Stat(e.cachePath("max")); err != nil {
		t.Error(err)
	}
}

func TestFetchInterval(t *testing.T) {
	e := newTestEnv(t)
	e.one(t, false)
	e.clock = e.clock.Add(time.Minute)
	e.one(t, false)
	e.clock = e.clock.Add(time.Minute)
	e.one(t, false)
	if n := e.hits.Load(); n != 1 {
		t.Fatalf("hits within interval = %d, want 1", n)
	}
	e.one(t, true)
	if n := e.hits.Load(); n != 2 {
		t.Fatalf("hits after --force = %d, want 2", n)
	}
	e.clock = e.clock.Add(fetchInterval)
	e.one(t, false)
	if n := e.hits.Load(); n != 3 {
		t.Fatalf("hits after interval = %d, want 3", n)
	}
}

func TestExpiredTokenNoCall(t *testing.T) {
	e := newTestEnv(t)
	e.one(t, false)
	e.clock = e.clock.Add(fetchInterval)
	e.expiry = e.clock.Add(30 * time.Second).UnixMilli()
	s := e.one(t, true)
	if n := e.hits.Load(); n != 1 {
		t.Errorf("API called with expired token: hits = %d", n)
	}
	if !s.Stale || s.Error != errTokenExpired || s.Week.Percent != 35 {
		t.Errorf("state = %+v", s)
	}

	// Token refreshed by claude: next run fetches again.
	e.expiry = e.clock.Add(time.Hour).UnixMilli()
	e.clock = e.clock.Add(time.Minute)
	s = e.one(t, false)
	if e.hits.Load() != 2 || s.Stale || s.Error != "" {
		t.Errorf("after refresh: hits = %d state = %+v", e.hits.Load(), s)
	}
}

func TestErrorKeepsCache(t *testing.T) {
	e := newTestEnv(t)
	e.one(t, false)
	e.status.Store(429)
	e.body.Store(`{"error":"rate limited"}`)
	e.clock = e.clock.Add(fetchInterval)
	s := e.one(t, false)
	if s.Error != "HTTP 429" || s.Stale || s.Week == nil || s.Week.Percent != 35 {
		t.Errorf("fresh 429 state = %+v", s)
	}
	e.clock = e.clock.Add(staleAfter)
	s = e.one(t, false)
	if s.Error != "HTTP 429" || !s.Stale || s.Week.Percent != 35 || e.hits.Load() != 3 {
		t.Errorf("old 429 state = %+v hits = %d", s, e.hits.Load())
	}
}

func TestNoCredsNoData(t *testing.T) {
	e := newTestEnv(t)
	e.keychain = func(string) ([]byte, error) { return nil, fmt.Errorf("x") }
	s := e.one(t, false)
	if s.HasData || s.Error != errNoCreds.Error() || e.hits.Load() != 0 {
		t.Errorf("state = %+v", s)
	}
}

func TestLostCredsIsStale(t *testing.T) {
	e := newTestEnv(t)
	e.one(t, false)
	e.keychain = func(string) ([]byte, error) { return nil, fmt.Errorf("timeout") }
	e.clock = e.clock.Add(fetchInterval)
	if s := e.one(t, false); !s.Stale || s.Error != errNoCreds.Error() || s.Week.Percent != 35 {
		t.Errorf("state = %+v", s)
	}
}

func TestCachePathsDistinct(t *testing.T) {
	a := &app{cacheDir: "/c"}
	seen := map[string]string{}
	for _, n := range []string{"Jiří", "Ji__", "max", "Max", "a/b", "a_b"} {
		p := strings.ToLower(a.cachePath(n))
		if prev, ok := seen[p]; ok {
			t.Errorf("%q and %q share cache %s", prev, n, p)
		}
		seen[p] = n
	}
}

func TestNewerCacheOnDiskWins(t *testing.T) {
	e := newTestEnv(t)
	e.one(t, false)
	// Another run saves fresher data while this one's fetch fails.
	newer := e.loadCache("max")
	newer.FetchedAt = e.clock.Add(5 * time.Minute)
	newer.Week = &Window{Percent: 50}
	e.status.Store(500)
	e.keychain = func(service string) ([]byte, error) {
		e.saveCache("max", newer)
		return fmt.Appendf(nil, `{"claudeAiOauth":{"accessToken":%q}}`, fakeToken), nil
	}
	e.clock = e.clock.Add(fetchInterval)
	s := e.one(t, false)
	if s.Week.Percent != 50 || e.loadCache("max").Week.Percent != 50 {
		t.Errorf("older result overwrote newer cache: %+v", s)
	}
}

func TestNoRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+usagePath, http.StatusFound)
	}))
	defer redir.Close()
	a, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	a.baseURL = redir.URL
	if _, err := a.fetch(fakeToken); err == nil || err.Error() != "HTTP 302" {
		t.Errorf("err = %v, want HTTP 302", err)
	}
	if leaked.Load() {
		t.Error("token sent to redirect target")
	}
}

func TestEffectivePastReset(t *testing.T) {
	now := time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)
	if p := effective(&Window{Percent: 93, ResetsAt: now.Add(-time.Second)}, now); p != 0 {
		t.Errorf("past reset = %v", p)
	}
	if p := effective(&Window{Percent: 93, ResetsAt: now.Add(time.Minute)}, now); p != 93 {
		t.Errorf("future reset = %v", p)
	}
	if p := effective(&Window{Percent: 40}, now); p != 40 {
		t.Errorf("no reset time = %v", p)
	}
	if p := effective(&Window{Percent: 40, Idle: true}, now); p != 0 {
		t.Errorf("idle = %v", p)
	}
	if p := effective(nil, now); p != 0 {
		t.Errorf("nil = %v", p)
	}
}

func TestTokenNeverLeaks(t *testing.T) {
	e := newTestEnv(t)
	var out, errOut bytes.Buffer
	e.one(t, true)
	if code := e.main(false, false, "", &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if code := e.main(false, true, "", &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	e.status.Store(500)
	e.one(t, true)
	e.expiry = 1
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
	for _, secret := range []string{fakeToken, "refresh-secret"} {
		if strings.Contains(all, secret) {
			t.Fatalf("output or cache contains %q", secret)
		}
	}
	if !strings.Contains(all, `"week"`) || !strings.Contains(out.String(), "| image=") {
		t.Error("expected menu and JSON output")
	}
}

func TestParseLimitsSessionFallback(t *testing.T) {
	// No session entry, but five_hour is there: use it.
	u, _ := parseUsage([]byte(`{"five_hour":{"utilization":40,"resets_at":"2026-11-09T18:00:00Z"},
		"limits":[{"kind":"weekly_all","percent":2,"resets_at":"2026-11-13T18:00:00Z","is_active":true}]}`))
	if u.Session == nil || u.Session.Percent != 40 || u.Session.Idle {
		t.Errorf("fallback session = %+v", u.Session)
	}
	// A session entry with a null percent is idle, not "no limit".
	u, _ = parseUsage([]byte(`{"five_hour":null,"limits":[{"kind":"session","percent":null,"resets_at":null},
		{"kind":"weekly_all","percent":2,"resets_at":"2026-11-13T18:00:00Z"}]}`))
	if u.Session == nil || !u.Session.Idle {
		t.Errorf("null-percent session = %+v", u.Session)
	}
}

func TestMoneyExponentClamp(t *testing.T) {
	if got := money(150, -3, "USD"); got != "$150.00" {
		t.Errorf("negative exponent: %q", got)
	}
	if got := money(1234, 1<<30, "USD"); got != "$0.00" {
		t.Errorf("huge exponent: %q", got)
	}
	if got := money(1234, 2, "CZK"); got != "12.34 CZK" {
		t.Errorf("other currency: %q", got)
	}
}
