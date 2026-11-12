package main

import (
	"bytes"
	"encoding/base64"
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

const (
	codexSecret  = "FAKE-CODEX-SECRET-do-not-leak"
	codexRefresh = "FAKE-CODEX-REFRESH-do-not-leak"
	codexAcct    = "acct-1234"
)

// fakeJWT builds an unsigned token around claims; never a real one.
func fakeJWT(claims string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + "." + codexSecret
}

// codexAuthJSON is an auth.json with a ChatGPT login expiring at exp.
func codexAuthJSON(exp time.Time) string {
	at := fakeJWT(fmt.Sprintf(`{"exp":%d,"jti":%q,"https://api.openai.com/profile":{"email":"cx@example.com"},`+
		`"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`, exp.Unix(), codexSecret))
	return fmt.Sprintf(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":%q,"access_token":%q,`+
		`"refresh_token":%q,"account_id":%q},"last_refresh":"2026-11-09T10:00:00Z"}`,
		fakeJWT(`{"email":"id@example.com"}`), at, codexRefresh, codexAcct)
}

const codexSample = `{"plan_type":"team",
 "rate_limit":{"allowed":true,"limit_reached":false,
   "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000,"reset_at":1794520294},
   "secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_after_seconds":489547,"reset_at":1794991840}},
 "code_review_rate_limit":null,
 "additional_rate_limits":null,
 "credits":{"has_credits":false,"unlimited":false,"overage_limit_reached":false,"balance":null}}`

func TestParseCodexUsage(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 16, 0, 0, time.UTC)
	u, plan, err := parseCodexUsage([]byte(codexSample), now)
	if err != nil || plan != "team" {
		t.Fatalf("plan %q err %v", plan, err)
	}
	if s := u.Session; s == nil || s.Label != "5h" || s.Percent != 0 || s.ResetsAt.Unix() != 1794520294 || !s.Idle {
		t.Errorf("session = %+v", s)
	}
	if w := u.Week; w == nil || w.Label != "week" || w.Percent != 1 || w.ResetsAt.Unix() != 1794991840 || w.Idle {
		t.Errorf("week = %+v", w)
	}
	if len(u.Scoped) != 0 || u.Balance != "" || u.Credits != "" {
		t.Errorf("scoped %+v balance %q credits %q", u.Scoped, u.Balance, u.Credits)
	}

	// Only the weekly window, odd optional parts.
	u, plan, err = parseCodexUsage([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":null,
		"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1794991840}},
		"code_review_rate_limit":7,"additional_rate_limits":"garbage","credits":"x"}`), now)
	if err != nil || plan != "plus" || u.Session != nil || u.Week == nil || u.Week.Percent != 40 || len(u.Scoped) != 0 {
		t.Errorf("weekly only: %+v %+v %v", u.Session, u.Week, err)
	}

	// A single short window is the session; extra limits become rows.
	u, _, err = parseCodexUsage([]byte(`{"rate_limit":{"primary_window":{"used_percent":12.5,"limit_window_seconds":18000,"reset_after_seconds":100,"reset_at":1794520294}},
		"code_review_rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_at":1794991840},"secondary_window":null},
		"additional_rate_limits":[{"limit_name":"Spark","metered_feature":"x","rate_limit":{"primary_window":{"used_percent":150,"limit_window_seconds":259200,"reset_at":0}}},
			5, {"metered_feature":"f","rate_limit":"bad"},
			{"metered_feature":"imgs","rate_limit":{"secondary_window":{"used_percent":2,"limit_window_seconds":43200,"reset_at":1794991840}}},
			{"limit_name":"rel","rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_after_seconds":600}}},
			{"limit_name":"unused","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000}}}]}`), now)
	if err != nil || u.Session == nil || u.Session.Percent != 12.5 || u.Session.Idle || u.Week != nil {
		t.Fatalf("short only: %+v %+v %v", u.Session, u.Week, err)
	}
	want := []Window{
		{Label: "review week", Percent: 3, ResetsAt: time.Unix(1794991840, 0)},
		{Label: "Spark 3d", Percent: 100}, // used, reset unknown: not idle
		{Label: "imgs 12h", Percent: 2, ResetsAt: time.Unix(1794991840, 0)},
		{Label: "rel 5h", Percent: 80, ResetsAt: now.Add(10 * time.Minute)},
		{Label: "unused 5h", Idle: true},
	}
	if fmt.Sprint(u.Scoped) != fmt.Sprint(want) {
		t.Errorf("scoped =\n%+v\nwant\n%+v", u.Scoped, want)
	}

	if _, _, err := parseCodexUsage([]byte(`<html>`), now); err == nil || err.Error() != "bad API response" {
		t.Errorf("bad JSON: %v", err)
	}
}

func TestCodexBalance(t *testing.T) {
	cases := map[string]string{
		`{"has_credits":false,"unlimited":false,"balance":null}`:   "",
		`{"has_credits":false,"unlimited":true,"balance":null}`:    "unlimited",
		`{"has_credits":true,"unlimited":false,"balance":"12.50"}`: "12.50",
		`{"has_credits":true,"balance":7.25}`:                      "7.25",
		`{"has_credits":true,"balance":null}`:                      "",
		`{"has_credits":false,"balance":5}`:                        "",
		`{"has_credits":true,"balance":{"x":1}}`:                   "",
		`null`:                                                     "",
		`"x"`:                                                      "",
	}
	for in, want := range cases {
		if got := codexBalance([]byte(in)); got != want {
			t.Errorf("codexBalance(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestWindowLabel(t *testing.T) {
	cases := map[int64]string{18000: "5h", 604800: "week", 259200: "3d", 43200: "12h", 1800: "30m", 90: "", 0: "", -5: ""}
	for secs, want := range cases {
		if got := windowLabel(secs); got != want {
			t.Errorf("windowLabel(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestReadCodexAuth(t *testing.T) {
	exp := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, auth   string
		err          error
		email, plan  string
		exp          time.Time
		id, hasToken bool
	}{
		{name: "valid", auth: codexAuthJSON(exp), email: "cx@example.com", plan: "plus", exp: exp, id: true, hasToken: true},
		{name: "id token email, no exp", auth: fmt.Sprintf(`{"tokens":{"id_token":%q,"access_token":%q}}`,
			fakeJWT(`{"email":"id@example.com"}`), fakeJWT(`{"jti":"x"}`)), email: "id@example.com", hasToken: true},
		{name: "opaque token", auth: `{"tokens":{"access_token":"opaque-` + codexSecret + `"}}`, hasToken: true},
		{name: "api key", auth: `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-` + codexSecret + `","tokens":null}`, err: errCodexNoLogin},
		{name: "empty token", auth: `{"tokens":{"access_token":"","refresh_token":"` + codexRefresh + `"}}`, err: errCodexNoLogin},
		{name: "bad json", auth: `{"tokens":` + codexSecret, err: errCodexNoLogin},
		{name: "missing"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if c.auth != "" {
			writeFile(t, filepath.Join(dir, "auth.json"), c.auth)
		} else {
			c.err = errCodexNoLogin
		}
		cr, err := readCodexAuth(dir)
		if err != c.err {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.err)
		}
		if err != nil && (strings.Contains(err.Error(), codexSecret) || strings.Contains(err.Error(), codexRefresh)) {
			t.Errorf("%s: error leaks the token", c.name)
		}
		if cr.Email != c.email || cr.Plan != c.plan || !cr.Expires.Equal(c.exp) ||
			(cr.AccountID == codexAcct) != c.id || (cr.AccessToken != "") != c.hasToken {
			t.Errorf("%s: creds email %q plan %q exp %v id %v token %v", c.name, cr.Email, cr.Plan, cr.Expires,
				cr.AccountID == codexAcct, cr.AccessToken != "")
		}
	}
}

type codexEnv struct {
	*app
	clock time.Time
	hits  atomic.Int32
	dir   string
}

// newCodexEnv sets up one "cx" Codex account and an httptest wham/usage API
// that wants the right headers.
func newCodexEnv(t *testing.T) *codexEnv {
	t.Helper()
	home := t.TempDir()
	e := &codexEnv{clock: time.Date(2026, 11, 9, 15, 16, 0, 0, time.UTC), dir: filepath.Join(home, ".codex")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		ok := r.URL.Path == codexUsagePath && r.Header.Get("ChatGPT-Account-Id") == codexAcct &&
			strings.HasPrefix(r.Header.Get("Authorization"), "Bearer e30.") &&
			strings.HasSuffix(r.Header.Get("Authorization"), "."+codexSecret) &&
			r.Header.Get("User-Agent") == "claude-usage-bar"
		if !ok {
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, codexSample)
	}))
	t.Cleanup(srv.Close)
	cfg := filepath.Join(home, ".config", "claude-usage-bar", "config.json")
	writeFile(t, cfg, `{"accounts":[{"name":"cx","provider":"codex"}]}`)
	writeFile(t, filepath.Join(e.dir, "auth.json"), codexAuthJSON(e.clock.Add(time.Hour)))
	e.app = &app{
		home:       home,
		configPath: cfg,
		cacheDir:   filepath.Join(home, "cache"),
		codexURL:   srv.URL,
		now:        func() time.Time { return e.clock },
		client:     srv.Client(),
	}
	return e
}

func (e *codexEnv) one(t *testing.T, force bool) State {
	t.Helper()
	st := e.collect(force)
	if len(st) != 1 {
		t.Fatalf("got %d states", len(st))
	}
	return st[0]
}

func TestCodexRefresh(t *testing.T) {
	e := newCodexEnv(t)
	s := e.one(t, false)
	if e.hits.Load() != 1 || !s.HasData || s.Stale || s.Error != "" {
		t.Fatalf("state = %+v hits = %d", s, e.hits.Load())
	}
	if s.Provider != "codex" || !s.Bar || s.Plan != "team" || s.Email != "cx@example.com" || s.Org != "" ||
		s.Session == nil || s.Session.Label != "5h" || s.Week == nil || s.Week.Percent != 1 || s.Sessions != nil {
		t.Errorf("state = %+v", s)
	}

	// Expired token: no call, stale data kept, retried every run.
	writeFile(t, filepath.Join(e.dir, "auth.json"), codexAuthJSON(e.clock.Add(fetchInterval+30*time.Second)))
	e.clock = e.clock.Add(fetchInterval)
	s = e.one(t, true)
	if e.hits.Load() != 1 || s.Error != errCodexExpired || !s.Stale || s.Week == nil || s.Week.Percent != 1 {
		t.Errorf("expired: state = %+v hits = %d", s, e.hits.Load())
	}
	writeFile(t, filepath.Join(e.dir, "auth.json"), codexAuthJSON(e.clock.Add(time.Hour)))
	e.clock = e.clock.Add(time.Minute)
	if s = e.one(t, false); e.hits.Load() != 2 || s.Error != "" || s.Stale {
		t.Errorf("after refresh: state = %+v hits = %d", s, e.hits.Load())
	}

	// Logged out: no call.
	os.Remove(filepath.Join(e.dir, "auth.json"))
	if s = e.one(t, true); e.hits.Load() != 2 || s.Error != errCodexNoLogin.Error() || !s.Stale {
		t.Errorf("logged out: state = %+v", s)
	}
}

func TestCodexTokenNeverLeaks(t *testing.T) {
	e := newCodexEnv(t)
	var out, errOut bytes.Buffer
	for _, json := range []bool{false, true} {
		if code := e.main(true, false, "", &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		if code := e.main(false, json, "", &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
	}
	all := out.String() + errOut.String()
	filepath.Walk(e.cacheDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			all += string(b)
		}
		return nil
	})
	for _, secret := range []string{codexSecret, codexRefresh, "e30."} {
		if strings.Contains(all, secret) {
			t.Fatalf("output or cache contains %q", secret)
		}
	}
	if !strings.Contains(all, `"provider": "codex"`) || !strings.Contains(out.String(), "Usage page | href="+codexUsageURL) {
		t.Errorf("expected codex JSON and menu:\n%s", out.String())
	}
}

func rolloutLine(ts time.Time, typ, payload string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":%q,"payload":%s}`+"\n", ts.UTC().Format(time.RFC3339Nano), typ, payload)
}

func sessionMeta(ts time.Time, id, cwd string) string {
	return rolloutLine(ts, "session_meta", fmt.Sprintf(`{"id":%q,"cwd":%q,"base_instructions":{"text":%q}}`, id, cwd, secretText))
}

func turnContext(ts time.Time, model, cwd string) string {
	return rolloutLine(ts, "turn_context", fmt.Sprintf(`{"model":%q,"cwd":%q}`, model, cwd))
}

// tokenCount has the cumulative total and the last request's usage.
func tokenCount(ts time.Time, total, in, cached, out int64) string {
	return rolloutLine(ts, "event_msg", fmt.Sprintf(`{"type":"token_count","info":{"total_token_usage":{"total_tokens":%d},`+
		`"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"output_tokens":%d,`+
		`"reasoning_output_tokens":0,"total_tokens":%d}},"rate_limits":{}}`, total, in, cached, out, in+out))
}

func TestCodexStats(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	a := &app{home: home, cacheDir: filepath.Join(home, "cache")}
	acc := Account{Name: "cx", ConfigDir: dir, Provider: "codex"}
	now := time.Date(2026, 11, 9, 15, 30, 0, 0, time.UTC)
	day := filepath.Join(dir, "sessions", "2026", "11", "09")
	fa := filepath.Join(day, "rollout-2026-11-09T14-00-00-aaaa.jsonl")
	t0 := now.Add(-time.Hour)

	appendFile(t, fa, sessionMeta(t0, "sess-a", "/src/api")+
		turnContext(t0, "gpt-a", "/src/api")+
		rolloutLine(t0, "response_item", `{"type":"message","content":[{"text":"`+secretText+` \"token_count\""}]}`)+
		tokenCount(t0, 100, 80, 60, 20)+
		rolloutLine(t0, "event_msg", `{"type":"token_count","info":null,"rate_limits":{}}`)+
		tokenCount(t0.Add(time.Second), 100, 80, 60, 20)) // rate-limit-only repeat
	// A resumed copy of the same session counts once; another session with
	// the same total counts again.
	appendFile(t, filepath.Join(day, "rollout-2026-11-09T14-10-00-bbbb.jsonl"),
		sessionMeta(t0, "sess-a", "/src/api")+turnContext(t0, "gpt-a", "/src/api")+tokenCount(t0, 100, 80, 60, 20))
	appendFile(t, filepath.Join(day, "rollout-2026-11-09T14-20-00-cccc.jsonl"),
		sessionMeta(t0, "sess-c", "/src/cli")+tokenCount(t0, 100, 100, 0, 0))
	appendFile(t, filepath.Join(day, "other.jsonl"), tokenCount(t0, 999, 999, 0, 0))
	appendFile(t, filepath.Join(dir, "projects", "p", "s.jsonl"), asstLine("m1", "r1", t0, "claude-x", "/p", 1, 1, 1, 1))

	st := a.codexStats(acc, now, false)
	if st == nil {
		t.Fatal("no stats")
	}
	if want := (Totals{Input: 120, Output: 20, CacheRead: 60, Msgs: 2}); st.Today != want || st.Today.All() != 200 {
		t.Errorf("today = %+v, want %+v", st.Today, want)
	}
	if fmt.Sprint(st.Projects) != "[{api 100} {cli 100}]" || fmt.Sprint(st.Models) != "[{? 100} {gpt-a 100}]" {
		t.Errorf("projects %v models %v", st.Projects, st.Models)
	}

	// Appends are read from where the file was left, with its state.
	t1 := now.Add(-30 * time.Minute)
	appendFile(t, fa, turnContext(t1, "gpt-b", "/src/web")+tokenCount(t1, 250, 140, 100, 10))
	st = a.codexStats(acc, now, true)
	if st.Today.Msgs != 3 || st.Today.All() != 350 {
		t.Errorf("after append: %+v", st.Today)
	}
	appendFile(t, fa, tokenCount(t1, 250, 140, 100, 10)+tokenCount(t1.Add(time.Minute), 300, 40, 0, 10)+
		`{"timestamp":"x","type":"event_msg","payload":{"type":"token_co`) // unfinished line
	st = a.codexStats(acc, now, true)
	if st.Today.Msgs != 4 || st.Today.All() != 400 || st.Today.Output != 40 {
		t.Errorf("after second append: %+v", st.Today)
	}
	if fmt.Sprint(st.Models) != "[{gpt-b 200} {? 100} {gpt-a 100}]" || fmt.Sprint(st.Projects) != "[{web 200} {api 100} {cli 100}]" {
		t.Errorf("models %v projects %v", st.Models, st.Projects)
	}

	// Caches hold numbers and names only.
	entries, _ := os.ReadDir(a.cacheDir)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(a.cacheDir, e.Name()))
		for _, bad := range []string{secretText, "sess-a", "/src/api", "rollout-"} {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s contains %q", e.Name(), bad)
			}
		}
	}
}
