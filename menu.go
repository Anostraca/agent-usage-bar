package main

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	barLen    = 13
	gray      = "#8E8E93"
	amberHex  = "#E0A030"
	redHex    = "#E5534B"
	usageURL  = "https://claude.ai/settings/usage"
	rowParams = "font=Menlo size=12"
)

func toGauge(s State, now time.Time) gauge {
	g := gauge{Letter: initial(s.Name), Stale: s.Stale, NoData: !s.HasData, NoSession: s.HasData && s.Session == nil}
	if s.HasData {
		g.Week = effective(s.Week, now)
		g.Session = effective(s.Session, now)
	}
	return g
}

func initial(name string) rune {
	for _, r := range name {
		return unicode.ToUpper(r)
	}
	return '?'
}

func iconB64(gs []gauge, dark, letters bool) (string, error) {
	p, err := encodePNG(renderIcon(gs, dark, letters))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(p), nil
}

// barGauges are the gauges of the accounts shown in the menu bar.
func barGauges(states []State, now time.Time) []gauge {
	var gs []gauge
	for _, s := range states {
		if s.Bar {
			gs = append(gs, toGauge(s, now))
		}
	}
	return gs
}

// writeMenu prints the SwiftBar output: title image, then one line per
// account with its details in a submenu. Without any account in the menu
// bar the title is plain text, so the dropdown can still be opened.
func writeMenu(w io.Writer, states []State, now time.Time, dark bool, exe, home string) error {
	if gs := barGauges(states, now); len(gs) > 0 {
		title, err := iconB64(gs, dark, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "| image=%s\n", title)
	} else {
		fmt.Fprintln(w, "AI")
	}
	fmt.Fprintln(w, "---")

	names := make([]string, len(states))
	for i, s := range states {
		names[i] = s.Name
	}
	names = padded(names)
	var updated time.Time
	var provs []string
	for i, s := range states {
		p, _ := providerOf(s.Provider)
		if p.usageURL != "" && !slices.Contains(provs, s.Provider) {
			provs = append(provs, s.Provider)
		}
		img, err := iconB64([]gauge{toGauge(s, now)}, dark, false)
		if err != nil {
			return err
		}
		text, color := accountLine(s, p, now)
		params := "image=" + img + " " + rowParams
		if color != "" {
			params += " color=" + color
		}
		fmt.Fprintf(w, "%s | %s\n", clean(names[i]+"   "+text), params)
		writeAccount(w, sub, s, p, now, exe, home)
		if s.HasData && s.FetchedAt.After(updated) {
			updated = s.FetchedAt
		}
	}

	fmt.Fprintln(w, "---")
	if !updated.IsZero() {
		fmt.Fprintf(w, "Updated %s | color=%s size=11\n", updated.Local().Format("15:04"), gray)
	}
	fmt.Fprintf(w, "Refresh now | bash=%s param1=--force terminal=false refresh=true\n", pathParam(exe))
	if len(states) == 0 {
		provs = []string{""} // no accounts: Claude's page
	}
	switch len(provs) {
	case 0:
	case 1:
		p, _ := providerOf(provs[0])
		fmt.Fprintf(w, "Usage page | href=%s\n", p.usageURL)
	default:
		fmt.Fprintln(w, "Usage pages")
		for _, name := range provs {
			p, _ := providerOf(name)
			fmt.Fprintf(w, "%s%s usage page | href=%s\n", sub, p.title, p.usageURL)
		}
	}
	return nil
}

// sub is one SwiftBar submenu level; subSep is a separator inside one.
const (
	sub    = "--"
	subSep = "-----"
)

// accountLine is the account's top-level summary: what is left of each
// window, worst colour first. Errors with data stay in the submenu.
func accountLine(s State, p provider, now time.Time) (text, color string) {
	if !s.HasData {
		return "no data", ""
	}
	var parts []string
	worst := 0.0
	add := func(label string, win *Window) {
		if win == nil {
			return
		}
		e := effective(win, now)
		worst = max(worst, e)
		parts = append(parts, fmt.Sprintf("%s %.0f%%", label, 100-clampPct(e)))
	}
	if s.Session != nil && !p.lowestModel {
		add(cmp.Or(s.Session.Label, "5h"), s.Session)
	}
	if s.Week != nil {
		add(cmp.Or(s.Week.Label, "week"), s.Week)
	}
	if !p.lowestModel {
		for i := range s.Scoped {
			add(s.Scoped[i].Label, &s.Scoped[i])
		}
	}
	text = strings.Join(parts, " · ")
	if s.Stale {
		text += " · stale"
	}
	return text, levelHex(worst)
}

// writeAccount prints an account's details, each line led by prefix.
func writeAccount(w io.Writer, prefix string, s State, p provider, now time.Time, exe, home string) {
	head := []string{s.Name}
	if s.Plan != "" {
		head = append(head, s.Plan)
	}
	if s.Email != "" {
		head = append(head, s.Email)
	}
	if s.HasData && s.Session == nil && p.noLimitNote != "" {
		head = append(head, p.noLimitNote)
	}
	line := prefix + clean(strings.Join(head, " · "))
	if s.Org != "" {
		line += " | tooltip=" + textParam(s.Org)
	}
	fmt.Fprintln(w, line)

	if s.HasData {
		weekLabel := "week"
		if s.Week != nil {
			weekLabel = cmp.Or(s.Week.Label, weekLabel)
		}
		width := 0 // 0: row()'s own column
		if p.padLabels {
			width = len([]rune(weekLabel))
			for _, x := range s.Scoped {
				width = max(width, len([]rune(x.Label)))
			}
			width++
		}
		if s.Session != nil {
			writeRow(w, prefix, padTo(cmp.Or(s.Session.Label, "5h"), width), s.Session, now)
		}
		writeRow(w, prefix, padTo(weekLabel, width), s.Week, now)
		for i := range s.Scoped {
			writeRow(w, prefix, padTo(s.Scoped[i].Label, width), &s.Scoped[i], now)
		}
		if len(s.Surfaces) > 0 {
			parts := make([]string, len(s.Surfaces))
			for i, x := range s.Surfaces {
				parts[i] = fmt.Sprintf("%s %.0f%%", x.Name, x.Percent)
			}
			grayLine(w, prefix, "week by surface: "+strings.Join(parts, " · "))
		}
		if s.Credits != "" {
			grayLine(w, prefix, "credits used "+s.Credits)
		}
		switch s.Balance {
		case "":
		case "unlimited":
			grayLine(w, prefix, "credits unlimited")
		default:
			grayLine(w, prefix, "credits balance "+s.Balance)
		}
	}
	if line := statusLine(s); line != "" {
		grayLine(w, prefix, line)
	}
	writeLocal(w, prefix, s, home)
	fmt.Fprintln(w, subSep)
	toggle := fmt.Sprintf("%sShow in menu bar | bash=%s param1=--toggle-bar param2=%s terminal=false refresh=true",
		prefix, pathParam(exe), hashHex(accountKey(s.Provider, s.Name), 4))
	if s.Bar {
		toggle += " checked=true"
	}
	fmt.Fprintln(w, toggle)
}

func writeRow(w io.Writer, prefix, label string, win *Window, now time.Time) {
	if win == nil {
		fmt.Fprintf(w, "%s%s | %s\n", prefix, clean(row(label, -1, "no data")), rowParams)
		return
	}
	p := effective(win, now)
	var tail string
	switch {
	case idle(win, now):
		tail = "idle · starts with next message"
	case win.ResetsAt.IsZero():
	default:
		tail = "resets " + resetTime(win.ResetsAt, now) + " (in " + duration(win.ResetsAt.Sub(now)) + ")"
	}
	params := rowParams
	if c := levelHex(p); c != "" {
		params += " color=" + c
	}
	fmt.Fprintf(w, "%s%s | %s\n", prefix, clean(row(label, p, tail)), params)
}

// padTo right-pads s to n runes.
func padTo(s string, n int) string {
	return s + strings.Repeat(" ", max(n-len([]rune(s)), 0))
}

func grayLine(w io.Writer, prefix, s string) {
	fmt.Fprintf(w, "%s%s | color=%s size=11\n", prefix, clean(s), gray)
}

// writeLocal prints running sessions and token stats from the transcripts,
// each line led by prefix; per-item rows go one level deeper.
func writeLocal(w io.Writer, prefix string, s State, home string) {
	deeper := prefix + sub
	if n := len(s.Sessions); n > 0 {
		counts := map[string]int{}
		var order []string
		names := make([]string, n)
		for i, x := range s.Sessions {
			if counts[x.Status] == 0 {
				order = append(order, x.Status)
			}
			counts[x.Status]++
			names[i] = x.Name
		}
		names = padded(names)
		slices.Sort(order)
		parts := make([]string, 0, len(order))
		for _, st := range order {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], cmp.Or(st, "unknown")))
		}
		fmt.Fprintf(w, "%s● %s · %s\n", prefix, plural(n, "session"), clean(strings.Join(parts, ", ")))
		for i, x := range s.Sessions {
			fmt.Fprintf(w, "%s%s | %s\n", deeper, clean(names[i]+"  "+shortPath(x.Cwd, home)+"  "+x.Status), rowParams)
		}
	}

	st := s.Stats
	if st == nil || st.Week.Msgs == 0 {
		return
	}
	fmt.Fprintf(w, "%sToday   %s tok · %s out · %s msgs · cache %.0f%% | %s\n",
		prefix, human(st.Today.All()), human(st.Today.Output), human(st.Today.Msgs), st.Today.CachePct(), rowParams)
	fmt.Fprintf(w, "%s7 days  %s tok · %s out · %s msgs | %s\n",
		prefix, human(st.Week.All()), human(st.Week.Output), human(st.Week.Msgs), rowParams)
	if st.Today.Msgs > 0 {
		fmt.Fprintf(w, "%stoday %s | %s color=%s\n", prefix, spark(st.Hourly), rowParams, gray)
	}
	if len(st.Projects) > 0 {
		fmt.Fprintln(w, prefix+"Top projects today")
		names := padded(countNames(st.Projects))
		for i, p := range st.Projects {
			fmt.Fprintf(w, "%s%s  %6s tok | %s\n", deeper, clean(names[i]), human(p.Tokens), rowParams)
		}
	}
	if len(st.Models) > 0 && st.Week.All() > 0 {
		fmt.Fprintln(w, prefix+"Models · 7 days")
		names := padded(countNames(st.Models))
		for i, m := range st.Models {
			share := fmt.Sprintf("%3.0f%%", float64(m.Tokens)*100/float64(st.Week.All()))
			if m.Tokens*100 < st.Week.All() {
				share = " <1%"
			}
			fmt.Fprintf(w, "%s%s  %s | %s\n", deeper, clean(names[i]), share, rowParams)
		}
	}
}

func countNames(cs []Count) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

// padded right-pads names to the longest one, for Menlo columns.
func padded(names []string) []string {
	w := 0
	for _, n := range names {
		w = max(w, len([]rune(n)))
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + strings.Repeat(" ", w-len([]rune(n)))
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// human is 950, 12.7k, 2.8M, 130B.
func human(n int64) string {
	v := float64(n)
	unit := ""
	for _, u := range []struct {
		s string
		f float64
	}{{"B", 1e9}, {"M", 1e6}, {"k", 1e3}} {
		if v >= u.f {
			v, unit = v/u.f, u.s
			break
		}
	}
	if unit == "" {
		return fmt.Sprintf("%d", n)
	}
	if v < 100 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + unit
	}
	return fmt.Sprintf("%.0f", v) + unit
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// spark draws one character per value, scaled to the largest.
func spark(vals []int64) string {
	top := slices.Max(append([]int64{0}, vals...))
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if top > 0 && v > 0 {
			i = min(max(int(math.Ceil(float64(v)*7/float64(top))), 1), 7)
		}
		b.WriteRune(sparkRunes[i])
	}
	return b.String()
}

// shortPath uses ~ for HOME and keeps the last two parts of long paths.
func shortPath(p, home string) string {
	if p == "" {
		return "?"
	}
	prefix := ""
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		p, prefix = strings.TrimPrefix(p[len(home):], "/"), "~/"
		if p == "" {
			return "~"
		}
	}
	if len([]rune(prefix+p)) > 40 {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) > 2 {
			p = "…/" + strings.Join(parts[len(parts)-2:], "/")
		}
	}
	return prefix + p
}

// row shows what is left of a window; used < 0 means unknown.
func row(label string, used float64, tail string) string {
	var s string
	if used < 0 {
		s = fmt.Sprintf("%-6s%s %-8s", label, bar(100), "  ?")
	} else {
		s = fmt.Sprintf("%-6s%s %3.0f%% left", label, bar(used), 100-clampPct(used))
	}
	if tail != "" {
		s += "   " + tail
	}
	return s
}

// bar is full when nothing is used.
func bar(used float64) string {
	n := int(math.Round((100 - clampPct(used)) / 100 * barLen))
	return strings.Repeat("█", n) + strings.Repeat("░", barLen-n)
}

func levelHex(p float64) string {
	switch {
	case p >= 90:
		return redHex
	case p >= 75:
		return amberHex
	}
	return ""
}

func statusLine(s State) string {
	if s.Error == "" && !s.Stale {
		return ""
	}
	var parts []string
	switch {
	case !s.HasData:
		parts = append(parts, "no data")
	case s.Stale:
		parts = append(parts, "stale")
	}
	if s.Error != "" {
		parts = append(parts, s.Error)
	}
	if s.HasData {
		parts = append(parts, "updated "+s.FetchedAt.Local().Format("15:04"))
	}
	return strings.Join(parts, " · ")
}

// resetTime is "16:40" today, "Sat 20:59" within a week, else "Jan 2 15:04".
func resetTime(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case t.Sub(now) < 7*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2 15:04")
}

func duration(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	days, h, m := m/(24*60), m/60%24, m%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return "<1m"
}

// cleaner swaps characters SwiftBar treats as syntax for look-alikes, so
// text from the API, config or .claude.json can't end a line, start the
// param list, close a quoted value or add params. No backslash escapes:
// SwiftBar may not honour them.
var cleaner = strings.NewReplacer(
	"|", "¦", "\n", " ", "\r", " ", "\t", " ",
	`"`, "″", "'", "’", "=", "＝",
)

func clean(s string) string { return cleaner.Replace(s) }

// textParam is an untrusted param value, cleaned and quoted.
func textParam(s string) string { return `"` + clean(s) + `"` }

// pathParam quotes a local path (our own executable) if it has spaces.
func pathParam(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}
