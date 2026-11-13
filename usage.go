package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	fetchInterval = 3 * time.Minute  // min time between API calls per account
	staleAfter    = 10 * time.Minute // cached data older than this is stale
	tokenMargin   = 60 * time.Second // treat tokens expiring this soon as expired
	httpTimeout   = 8 * time.Second
	usagePath     = "/api/oauth/usage"
	defaultAPI    = "https://api.anthropic.com"
)

const errTokenExpired = "token expired — open claude on this account to refresh"

// Window is one rate-limit window: percent used and when it resets.
// Idle means it hasn't started; it starts with the next message.
type Window struct {
	Label    string    `json:"label,omitempty"`
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
	Idle     bool      `json:"idle,omitempty"`
}

// Share is one part of the weekly usage, e.g. by surface.
type Share struct {
	Name    string  `json:"name"`
	Percent float64 `json:"percent"`
}

// Usage is what the API tells us.
type Usage struct {
	Session  *Window  `json:"session"` // nil = the plan has no 5h limit
	Week     *Window  `json:"week"`
	Scoped   []Window `json:"scoped,omitempty"`   // per-model weekly limits
	Surfaces []Share  `json:"surfaces,omitempty"` // weekly usage by surface, used ones only
	Credits  string   `json:"credits,omitempty"`  // credits spent, when credits are on
	Balance  string   `json:"balance,omitempty"`  // credits left, or "unlimited"
}

// Cache is what we keep per account between runs. Never holds tokens.
type Cache struct {
	Plan  string `json:"plan,omitempty"`
	Email string `json:"email,omitempty"` // from the API, for providers without a local profile
	Usage
	FetchedAt time.Time `json:"fetched_at,omitzero"` // last good fetch
	AttemptAt time.Time `json:"attempt_at,omitzero"` // last API call
	Error     string    `json:"error,omitempty"`
}

// State is one account as shown in the menu.
type State struct {
	Name     string    `json:"name"`
	Provider string    `json:"provider,omitempty"` // "" = Claude
	Bar      bool      `json:"bar"`                // shown in the menu bar
	Email    string    `json:"email,omitempty"`
	Org      string    `json:"org,omitempty"`
	HasData  bool      `json:"has_data"`
	Stale    bool      `json:"stale"`
	Sessions []Session `json:"sessions,omitempty"`
	Stats    *Stats    `json:"stats,omitempty"`
	Cache
}

type app struct {
	home       string
	configPath string
	cacheDir   string
	baseURL    string
	codexURL   string
	now        func() time.Time
	keychain   func(service string) ([]byte, error)
	client     *http.Client
	agyFind    func() ([]agyProc, error)
	agyHTTP    *http.Client // loopback only, never for remote APIs
}

func newApp() (*app, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &app{
		home:       home,
		configPath: configPath(home),
		cacheDir:   filepath.Join(home, "Library", "Caches", "agent-usage-bar"),
		baseURL:    defaultAPI,
		codexURL:   defaultCodexAPI,
		now:        time.Now,
		keychain:   securityKeychain,
		client: &http.Client{
			Timeout: httpTimeout,
			// Never send the token on to a redirect target.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		agyFind: findAntigravity,
		agyHTTP: agyClient(),
	}, nil
}

// collect refreshes every account (concurrently) and returns their states.
func (a *app) collect(force bool) []State {
	accts := loadAccounts(a.configPath, a.home)
	out := make([]State, len(accts))
	var wg sync.WaitGroup
	for i, acc := range accts {
		wg.Go(func() { out[i] = a.refresh(acc, force) })
	}
	wg.Wait()
	return out
}

// refresh updates one account's cache when due and returns its state.
func (a *app) refresh(acc Account, force bool) State {
	p, _ := providerOf(acc.Provider)
	now := a.now()
	c := a.loadCache(acc.key())
	due := force || c.AttemptAt.IsZero() || now.Sub(c.AttemptAt) >= fetchInterval || retryError(c.Error)
	if due {
		before := c
		p.update(a, &c, acc, now)
		// A concurrent run (SwiftBar vs --force) may have saved newer data.
		if disk := a.loadCache(acc.key()); disk.FetchedAt.After(c.FetchedAt) {
			c = disk
		} else if !reflect.DeepEqual(c, before) {
			a.saveCache(acc.key(), c)
		}
	}
	pr := p.profile(acc.ConfigDir, a.home)
	st := State{Name: acc.Name, Provider: acc.Provider, Bar: acc.Bar, Email: cmp.Or(pr.Email, c.Email), Org: pr.Org, Cache: c}
	st.HasData = !c.FetchedAt.IsZero()
	// A closed app can't use its quota: its last data stays current.
	st.Stale = st.HasData && c.Error != errAgyNotRunning && (loginError(c.Error) || now.Sub(c.FetchedAt) > staleAfter)
	if p.lowestModel {
		st.Week = lowestWindow(st.Scoped, now)
	}
	if p.sessions != nil {
		st.Sessions = p.sessions(acc.ConfigDir)
	}
	if p.stats != nil {
		st.Stats = p.stats(a, acc, now, force)
	}
	return st
}

// update reads the login and calls the API. Local failures (no login,
// expired token) do not count as an API attempt.
func (a *app) update(c *Cache, acc Account, now time.Time) {
	cr, err := readCreds(acc.ConfigDir, a.home, a.keychain)
	if err != nil {
		c.Error = err.Error()
		return
	}
	if p := planName(cr.SubscriptionType, cr.RateLimitTier); p != "" {
		c.Plan = p
	}
	if cr.ExpiresAt > 0 && time.UnixMilli(cr.ExpiresAt).Before(now.Add(tokenMargin)) {
		c.Error = errTokenExpired
		return
	}
	c.AttemptAt = now
	u, err := a.fetch(cr.AccessToken)
	if err != nil {
		c.Error = err.Error()
		return
	}
	c.Usage = u
	c.FetchedAt = now
	c.Error = ""
}

type apiWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type apiLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
		Surface any `json:"surface"`
	} `json:"scope"`
}

type apiUsage struct {
	FiveHour       *apiWindow `json:"five_hour"`
	SevenDay       *apiWindow `json:"seven_day"`
	SevenDayOpus   *apiWindow `json:"seven_day_opus"`
	SevenDaySonnet *apiWindow `json:"seven_day_sonnet"`
	Limits         []apiLimit `json:"limits"`
	Breakdown      *struct {
		Rows []struct {
			DisplayName string  `json:"display_name"`
			Percent     float64 `json:"percent"`
		} `json:"rows"`
	} `json:"seven_day_breakdown"`
	Spend *struct {
		Enabled bool `json:"enabled"`
		Used    *struct {
			AmountMinor int64  `json:"amount_minor"`
			Currency    string `json:"currency"`
			Exponent    int    `json:"exponent"`
		} `json:"used"`
	} `json:"spend"`
}

// fetch calls the usage endpoint. Errors are short and never contain the token.
func (a *app) fetch(token string) (Usage, error) {
	data, err := a.get(strings.TrimRight(a.baseURL, "/")+usagePath, map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
	})
	if err != nil {
		return Usage{}, err
	}
	return parseUsage(data)
}

// get fetches url with the given headers, up to 1 MiB of a 200 response.
// Redirects are not followed. Errors are short and never contain header
// values.
func (a *app) get(url string, header map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("bad API URL")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "agent-usage-bar")
	resp, err := a.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return nil, errors.New("timeout")
		}
		return nil, errors.New("network error")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("network error")
	}
	return data, nil
}

// parseUsage prefers the "limits" list and falls back to the older
// five_hour / seven_day fields.
func parseUsage(data []byte) (Usage, error) {
	var r apiUsage
	if json.Unmarshal(data, &r) != nil {
		return Usage{}, errors.New("bad API response")
	}
	var u Usage
	if len(r.Limits) > 0 {
		sawSession := false
		for _, l := range r.Limits {
			w := l.window()
			switch l.Kind {
			case "session":
				sawSession = true
				if w == nil {
					w = &Window{Idle: true}
				}
				u.Session = w
			case "weekly_all":
				u.Week = w
			case "weekly_scoped":
				if w != nil {
					w.Label = l.label()
					u.Scoped = append(u.Scoped, *w)
				}
			}
		}
		if !sawSession {
			u.Session = r.FiveHour.window()
		}
		if u.Week == nil {
			u.Week = r.SevenDay.window()
		}
	} else {
		u.Session = r.FiveHour.window()
		u.Week = r.SevenDay.window()
		for _, x := range []struct {
			label string
			w     *apiWindow
		}{{"Opus", r.SevenDayOpus}, {"Sonnet", r.SevenDaySonnet}} {
			if w := x.w.window(); w != nil {
				w.Label = x.label
				u.Scoped = append(u.Scoped, *w)
			}
		}
	}
	if r.Breakdown != nil {
		for _, row := range r.Breakdown.Rows {
			if row.Percent > 0 && row.DisplayName != "" {
				u.Surfaces = append(u.Surfaces, Share{row.DisplayName, row.Percent})
			}
		}
	}
	if r.Spend != nil && r.Spend.Enabled && r.Spend.Used != nil {
		u.Credits = money(r.Spend.Used.AmountMinor, r.Spend.Used.Exponent, r.Spend.Used.Currency)
	}
	return u, nil
}

func (l apiLimit) window() *Window {
	if l.Percent == nil {
		return nil
	}
	w := &Window{Percent: clampPct(*l.Percent)}
	if t, err := time.Parse(time.RFC3339Nano, l.ResetsAt); err == nil {
		w.ResetsAt = t
	}
	// The API's is_active marks the binding limit, not a started window.
	w.Idle = w.ResetsAt.IsZero()
	return w
}

func (l apiLimit) label() string {
	if l.Scope != nil {
		if l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
			return l.Scope.Model.DisplayName
		}
		if s, ok := l.Scope.Surface.(string); ok && s != "" {
			return s
		}
	}
	return "scoped"
}

func (w *apiWindow) window() *Window {
	if w == nil || w.Utilization == nil {
		return nil
	}
	out := &Window{Percent: clampPct(*w.Utilization)}
	if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
		out.ResetsAt = t
	}
	out.Idle = out.ResetsAt.IsZero()
	return out
}

func money(minor int64, exp int, cur string) string {
	v := float64(minor) / math.Pow10(min(max(exp, 0), 6))
	switch cur {
	case "USD":
		return fmt.Sprintf("$%.2f", v)
	case "EUR":
		return fmt.Sprintf("€%.2f", v)
	case "GBP":
		return fmt.Sprintf("£%.2f", v)
	}
	return strings.TrimSpace(fmt.Sprintf("%.2f %s", v, cur))
}

// idle reports a window that hasn't started or whose reset has passed.
func idle(w *Window, now time.Time) bool {
	return w == nil || w.Idle || !w.ResetsAt.IsZero() && !w.ResetsAt.After(now)
}

// effective is the window's percent now; an idle window is 0.
func effective(w *Window, now time.Time) float64 {
	if idle(w, now) {
		return 0
	}
	return w.Percent
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// cacheFile is <kind>-<safe name>-<hash>.json; the hash keeps names that
// sanitize or case-fold to the same string apart.
func (a *app) cacheFile(kind, name string) string {
	return filepath.Join(a.cacheDir, kind+"-"+unsafeName.ReplaceAllString(name, "_")+"-"+hashHex(name, 4)+".json")
}

func (a *app) cachePath(name string) string { return a.cacheFile("usage", name) }

func (a *app) loadCache(name string) Cache {
	var c Cache
	readJSON(a.cachePath(name), &c)
	return c
}

func (a *app) saveCache(name string, c Cache) { writeJSON(a.cacheDir, a.cachePath(name), c) }
