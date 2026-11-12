package main

import "time"

// provider is what differs between kinds of accounts. Menu and icon code
// only see State, so a new provider needs an entry here and nothing there.
type provider struct {
	title       string // in the usage page links when several providers are shown
	dir         string // default config dir, relative to HOME; "" = none
	usageURL    string // "" = no link
	noLimitNote string // head note when there is no session window
	padLabels   bool   // limit labels are long: pad them to one column
	lowestModel bool   // Week is the Scoped model with the least left, picked at display time
	update      func(a *app, c *Cache, acc Account, now time.Time)
	profile     func(dir, home string) profile
	sessions    func(dir string) []Session                                  // nil: no live-session list
	stats       func(a *app, acc Account, now time.Time, force bool) *Stats // nil: none
}

// providerOf looks up an Account.Provider; "" is Claude Code.
func providerOf(name string) (provider, bool) {
	switch name {
	case "":
		return provider{
			title:       "Claude",
			dir:         ".claude",
			usageURL:    usageURL,
			noLimitNote: "no 5h limit",
			update:      (*app).update,
			profile:     readProfile,
			sessions:    liveSessions,
			stats:       (*app).stats,
		}, true
	case "codex":
		return provider{
			title:       "Codex",
			dir:         ".codex",
			usageURL:    codexUsageURL,
			noLimitNote: "no 5h limit",
			update:      (*app).updateCodex,
			profile:     codexProfile,
			stats:       (*app).codexStats,
		}, true
	case "antigravity":
		return provider{
			title:       "Antigravity",
			padLabels:   true,
			lowestModel: true,
			update:      (*app).updateAntigravity,
			profile:     func(string, string) profile { return profile{} }, // email comes with the usage
		}, true
	}
	return provider{}, false
}

// retryError reports a local failure that is retried on every run and is
// not an API attempt.
func retryError(msg string) bool { return loginError(msg) || msg == errAgyNotRunning }

// loginError reports a local login problem (no login, expired token) of
// any provider: those are retried every run and make old data stale.
func loginError(msg string) bool {
	switch msg {
	case errTokenExpired, errNoCreds.Error(), errCodexExpired, errCodexNoLogin.Error():
		return true
	}
	return false
}
