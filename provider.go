package main

import "time"

// provider is what differs between kinds of accounts. Menu and icon code
// only see State, so a new provider needs an entry here and nothing there.
type provider struct {
	title    string // in the usage page links when several providers are shown
	dir      string // default config dir, relative to HOME
	usageURL string
	update   func(a *app, c *Cache, acc Account, now time.Time)
	profile  func(dir, home string) profile
	sessions func(dir string) []Session // nil: no live-session list
	stats    func(a *app, acc Account, now time.Time, force bool) *Stats
}

// providerOf looks up an Account.Provider; "" is Claude Code.
func providerOf(name string) (provider, bool) {
	switch name {
	case "":
		return provider{
			title:    "Claude",
			dir:      ".claude",
			usageURL: usageURL,
			update:   (*app).update,
			profile:  readProfile,
			sessions: liveSessions,
			stats:    (*app).stats,
		}, true
	case "codex":
		return provider{
			title:    "Codex",
			dir:      ".codex",
			usageURL: codexUsageURL,
			update:   (*app).updateCodex,
			profile:  codexProfile,
			stats:    (*app).codexStats,
		}, true
	}
	return provider{}, false
}

// loginError reports a local login problem (no login, expired token) of
// any provider: those are retried every run and make old data stale.
func loginError(msg string) bool {
	switch msg {
	case errTokenExpired, errNoCreds.Error(), errCodexExpired, errCodexNoLogin.Error():
		return true
	}
	return false
}
