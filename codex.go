package main

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	defaultCodexAPI = "https://chatgpt.com"
	codexUsagePath  = "/backend-api/wham/usage"
	codexUsageURL   = "https://chatgpt.com/codex/settings/usage"
	errCodexExpired = "token expired — open codex to refresh"
)

var errCodexNoLogin = errors.New("no ChatGPT login found — run codex login")

// codexCreds is what we need from Codex's auth.json. The refresh token is
// never decoded.
type codexCreds struct {
	AccessToken string
	AccountID   string
	Expires     time.Time // zero: unknown, treated as valid
	Email       string
	Plan        string
}

// jwtClaims are the access / id token claims we read.
type jwtClaims struct {
	Exp     float64 `json:"exp"`
	Email   string  `json:"email"`
	Profile *struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
	Auth *struct {
		Plan string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

// parseJWT decodes a JWT's payload without checking the signature: the
// claims are only used for display and the expiry check.
func parseJWT(tok string) jwtClaims {
	var c jwtClaims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return c
	}
	if json.Unmarshal(data, &c) != nil {
		c = jwtClaims{}
	}
	clear(data)
	return c
}

// readCodexAuth reads <dir>/auth.json. Only a ChatGPT login has tokens; an
// API key login gives errCodexNoLogin.
func readCodexAuth(dir string) (codexCreds, error) {
	data, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return codexCreds{}, errCodexNoLogin
	}
	var v struct {
		Tokens *struct {
			IDToken     string `json:"id_token"`
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	err = json.Unmarshal(data, &v)
	clear(data)
	if err != nil || v.Tokens == nil || v.Tokens.AccessToken == "" {
		return codexCreds{}, errCodexNoLogin
	}
	c := codexCreds{AccessToken: v.Tokens.AccessToken, AccountID: v.Tokens.AccountID}
	at := parseJWT(v.Tokens.AccessToken)
	if at.Exp > 0 {
		c.Expires = time.Unix(int64(at.Exp), 0)
	}
	if at.Profile != nil {
		c.Email = at.Profile.Email
	}
	if c.Email == "" {
		c.Email = parseJWT(v.Tokens.IDToken).Email
	}
	if at.Auth != nil {
		c.Plan = at.Auth.Plan
	}
	return c, nil
}

// codexProfile is the email from the login; Codex has no org.
func codexProfile(dir, _ string) profile {
	c, _ := readCodexAuth(dir)
	return profile{Email: c.Email}
}

// updateCodex reads the login and calls the API. Local failures do not
// count as an API attempt.
func (a *app) updateCodex(c *Cache, acc Account, now time.Time) {
	cr, err := readCodexAuth(acc.ConfigDir)
	if err != nil {
		c.Error = err.Error()
		return
	}
	if cr.Plan != "" {
		c.Plan = cr.Plan
	}
	if !cr.Expires.IsZero() && cr.Expires.Before(now.Add(tokenMargin)) {
		c.Error = errCodexExpired
		return
	}
	c.AttemptAt = now
	hdr := map[string]string{"Authorization": "Bearer " + cr.AccessToken}
	if cr.AccountID != "" {
		hdr["ChatGPT-Account-Id"] = cr.AccountID
	}
	data, err := a.get(strings.TrimRight(a.codexURL, "/")+codexUsagePath, hdr)
	if err != nil {
		c.Error = err.Error()
		return
	}
	u, plan, err := parseCodexUsage(data, now)
	if err != nil {
		c.Error = err.Error()
		return
	}
	if plan != "" {
		c.Plan = plan
	}
	c.Usage = u
	c.FetchedAt = now
	c.Error = ""
}

type codexWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	Seconds     float64  `json:"limit_window_seconds"`
	ResetAt     float64  `json:"reset_at"`
	ResetAfter  float64  `json:"reset_after_seconds"`
}

type codexLimit struct {
	Primary   *codexWindow `json:"primary_window"`
	Secondary *codexWindow `json:"secondary_window"`
}

// parseCodexUsage maps wham/usage, fetched at now, onto Usage and returns
// the plan. Only rate_limit must parse; the optional parts are skipped when
// odd.
func parseCodexUsage(data []byte, now time.Time) (Usage, string, error) {
	var r struct {
		PlanType   string          `json:"plan_type"`
		RateLimit  *codexLimit     `json:"rate_limit"`
		CodeReview json.RawMessage `json:"code_review_rate_limit"`
		Additional json.RawMessage `json:"additional_rate_limits"`
		Credits    json.RawMessage `json:"credits"`
	}
	if json.Unmarshal(data, &r) != nil {
		return Usage{}, "", errors.New("bad API response")
	}
	var u Usage
	if r.RateLimit != nil {
		u.Session, u.Week = r.RateLimit.split(now)
	}
	var review codexLimit
	if json.Unmarshal(r.CodeReview, &review) == nil {
		u.Scoped = append(u.Scoped, review.scoped("review", now)...)
	}
	var extra []json.RawMessage
	if json.Unmarshal(r.Additional, &extra) == nil {
		for _, raw := range extra {
			var x struct {
				Name      string     `json:"limit_name"`
				Feature   string     `json:"metered_feature"`
				RateLimit codexLimit `json:"rate_limit"`
			}
			if json.Unmarshal(raw, &x) == nil {
				name := x.Name
				if name == "" {
					name = x.Feature
				}
				if name == "" {
					name = "limit"
				}
				u.Scoped = append(u.Scoped, x.RateLimit.scoped(name, now)...)
			}
		}
	}
	u.Balance = codexBalance(r.Credits)
	return u, r.PlanType, nil
}

// split puts the shorter window into Session and the longer into Week. A
// single window goes by its length: up to a day is a session.
func (l codexLimit) split(now time.Time) (session, week *Window) {
	var ws []*codexWindow
	for _, w := range []*codexWindow{l.Primary, l.Secondary} {
		if w.window(now) != nil {
			ws = append(ws, w)
		}
	}
	slices.SortStableFunc(ws, func(a, b *codexWindow) int { return cmp.Compare(a.Seconds, b.Seconds) })
	switch {
	case len(ws) == 2:
		return ws[0].window(now), ws[1].window(now)
	case len(ws) == 1 && ws[0].Seconds <= 24*3600:
		return ws[0].window(now), nil
	case len(ws) == 1:
		return nil, ws[0].window(now)
	}
	return nil, nil
}

// scoped is one row per window, labelled name plus the window length.
func (l codexLimit) scoped(name string, now time.Time) []Window {
	var out []Window
	for _, cw := range []*codexWindow{l.Primary, l.Secondary} {
		if w := cw.window(now); w != nil {
			w.Label = strings.TrimSpace(name + " " + w.Label)
			out = append(out, *w)
		}
	}
	return out
}

// window maps one API window. Without reset_at the reset is now plus
// reset_after_seconds; without either it is unknown. Only an unused window
// can be idle.
func (w *codexWindow) window(now time.Time) *Window {
	if w == nil || w.UsedPercent == nil {
		return nil
	}
	out := &Window{Label: windowLabel(int64(w.Seconds)), Percent: clampPct(*w.UsedPercent)}
	switch {
	case w.ResetAt > 0:
		out.ResetsAt = time.Unix(int64(w.ResetAt), 0)
	case w.ResetAfter > 0:
		out.ResetsAt = now.Add(time.Duration(w.ResetAfter * float64(time.Second))).Truncate(time.Second)
	}
	// Unused with no reset or a full window ahead: it starts with the next
	// request.
	out.Idle = out.Percent == 0 && (out.ResetsAt.IsZero() || w.Seconds > 0 && w.ResetAfter >= w.Seconds)
	return out
}

// windowLabel is "5h", "week", "3d", "12h" or "" when unknown.
func windowLabel(secs int64) string {
	switch {
	case secs <= 0:
		return ""
	case secs == 7*24*3600:
		return "week"
	case secs%(24*3600) == 0:
		return fmt.Sprintf("%dd", secs/(24*3600))
	case secs%3600 == 0:
		return fmt.Sprintf("%dh", secs/3600)
	case secs%60 == 0:
		return fmt.Sprintf("%dm", secs/60)
	}
	return ""
}

// codexBalance is "unlimited", the balance, or "" when there is nothing to
// show.
func codexBalance(raw json.RawMessage) string {
	var c struct {
		HasCredits bool            `json:"has_credits"`
		Unlimited  bool            `json:"unlimited"`
		Balance    json.RawMessage `json:"balance"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	if c.Unlimited {
		return "unlimited"
	}
	if !c.HasCredits {
		return ""
	}
	var s string
	if json.Unmarshal(c.Balance, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(c.Balance, &n) == nil {
		return n.String()
	}
	return ""
}

// codexStats is local token usage from the Codex session rollouts.
func (a *app) codexStats(acc Account, now time.Time, force bool) *Stats {
	return a.localStats(acc, logSource{root: filepath.Join(acc.ConfigDir, "sessions"), prefix: "rollout-", parse: parseRollout}, now, force)
}

var rolloutMarks = [][]byte{[]byte(`"token_count"`), []byte(`"turn_context"`), []byte(`"session_meta"`)}

type codexTokens struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Total      int64 `json:"total_tokens"`
}

// parseRollout reads a Codex rollout from fa.Off on. A token_count event
// counts when the cumulative total moved since the last counted one (the
// others only update rate limits). Model and project come from the latest
// turn_context; that state lives in fa so appends continue where they left.
func parseRollout(path string, fa *fileAgg, cut int64, deadline time.Time) (bool, error) {
	have := map[int64]bool{}
	for _, r := range fa.Recs {
		have[r[rKey]] = true
	}
	return readLog(path, fa, deadline, func(line []byte) {
		if !slices.ContainsFunc(rolloutMarks, func(m []byte) bool { return bytes.Contains(line, m) }) {
			return
		}
		var e struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Cwd   string `json:"cwd"`
				Model string `json:"model"`
				Info  *struct {
					Total *codexTokens `json:"total_token_usage"`
					Last  *codexTokens `json:"last_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil {
			return
		}
		p := e.Payload
		switch {
		case e.Type == "session_meta":
			if p.ID != "" && fa.Session == "" {
				fa.Session = hashHex(p.ID, 8)
			}
			if p.Cwd != "" && fa.Project == "" {
				fa.Project = filepath.Base(p.Cwd)
			}
		case e.Type == "turn_context":
			if p.Cwd != "" {
				fa.Project = filepath.Base(p.Cwd)
			}
			if p.Model != "" {
				fa.Model = p.Model
			}
		case e.Type == "event_msg" && p.Type == "token_count":
			if p.Info == nil || p.Info.Total == nil || p.Info.Last == nil || p.Info.Total.Total == fa.Total {
				return
			}
			fa.Total = p.Info.Total.Total
			t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
			if err != nil || t.Unix() < cut {
				return
			}
			h := fnv.New64a()
			fmt.Fprintf(h, "%s\x00%d", cmp.Or(fa.Session, hashHex(path, 8)), fa.Total)
			key := int64(h.Sum64())
			if have[key] {
				return
			}
			have[key] = true
			l := p.Info.Last
			model, project := fa.Model, fa.Project
			if model == "" {
				model = "?"
			}
			if project == "" {
				project = "?"
			}
			fa.Recs = append(fa.Recs, rec{key, t.Unix(), indexOf(&fa.Models, model), indexOf(&fa.Projects, project),
				max(l.Input-l.Cached-l.CacheWrite, 0), l.Output, l.CacheWrite, l.Cached})
		}
	})
}
