package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	agyServer        = "/Antigravity.app/Contents/Resources/bin/language_server"
	agyStatusPath    = "/exa.language_server_pb.LanguageServerService/GetUserStatus"
	agyHost          = "127.0.0.1" // never taken from the process or lsof
	agyTryTimeout    = 2 * time.Second
	procTimeout      = 3 * time.Second
	errAgyNotRunning = "Antigravity not running — open it to refresh"
	errAgyNoServer   = "Antigravity server not reachable"
)

// agyProc is one running Antigravity language server. csrf is a secret:
// it is only sent to the server's own loopback port. "" means the token
// was missing or malformed; such a process can't be asked.
type agyProc struct {
	pid   int
	csrf  string
	ports []int
}

var csrfToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// parsePS finds Antigravity language servers in `ps -o pid=,args=` output.
// Other language_server processes (no --app_data_dir antigravity) are
// skipped, and --host_bridge_token is never read.
func parsePS(out []byte) []agyProc {
	var procs []agyProc
	for _, line := range strings.Split(string(out), "\n") {
		pidStr, cmd, ok := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(pidStr)
		if !ok || err != nil || pid <= 0 {
			continue
		}
		// The executable path may hold spaces; the args follow it.
		i := strings.Index(cmd, agyServer)
		if i < 0 || !strings.HasPrefix(cmd, "/") || strings.Contains(cmd[:i], " -") {
			continue
		}
		rest := cmd[i+len(agyServer):]
		if rest != "" && rest[0] != ' ' {
			continue
		}
		var app, csrf string
		args := strings.Fields(rest)
		for j := 0; j < len(args); j++ {
			name, val, eq := strings.Cut(args[j], "=")
			if name != "--app_data_dir" && name != "--csrf_token" {
				continue
			}
			if !eq && j+1 < len(args) {
				j++
				val = args[j]
			}
			if name == "--app_data_dir" {
				app = val
			} else {
				csrf = val
			}
		}
		if app != "antigravity" {
			continue
		}
		if !csrfToken.MatchString(csrf) {
			csrf = ""
		}
		procs = append(procs, agyProc{pid: pid, csrf: csrf})
	}
	return procs
}

// parseLsof reads the listening ports from `lsof -Fn` output. Only ports
// bound to 127.0.0.1 or all addresses are kept.
func parseLsof(out []byte) []int {
	var ports []int
	for _, line := range strings.Split(string(out), "\n") {
		addr, ok := strings.CutPrefix(strings.TrimSpace(line), "n")
		if !ok {
			continue
		}
		host, p, err := net.SplitHostPort(addr)
		if err != nil || host != agyHost && host != "*" {
			continue
		}
		port, err := strconv.Atoi(p)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		ports = append(ports, port)
	}
	return ports
}

// runTool runs a system tool by absolute path; a hung tool hits the
// timeout instead of hanging the plugin.
func runTool(path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), procTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// findAntigravity lists the running Antigravity language servers with
// their listening ports. A server whose ports can't be read is still
// listed, without ports; an error means ps itself failed.
func findAntigravity() ([]agyProc, error) {
	out, err := runTool("/bin/ps", "-axww", "-o", "pid=,args=")
	if err != nil {
		return nil, errors.New("ps failed")
	}
	procs := parsePS(out)
	clear(out)
	for i := range procs {
		if lo, err := runTool("/usr/sbin/lsof", "-w", "-nP", "-a", "-p", strconv.Itoa(procs[i].pid), "-iTCP", "-sTCP:LISTEN", "-Fn"); err == nil {
			procs[i].ports = parseLsof(lo)
		}
	}
	return procs, nil
}

// agyClient talks to the local language server only. Its certificate is
// self-signed, so verification is off; in exchange the transport dials
// nothing but 127.0.0.1, uses no proxy and follows no redirects. Never use
// it for anything else.
func agyClient() *http.Client {
	d := &net.Dialer{Timeout: agyTryTimeout}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if host, _, err := net.SplitHostPort(addr); err != nil || host != agyHost {
					return nil, errors.New("only loopback")
				}
				return d.DialContext(ctx, "tcp4", addr)
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// updateAntigravity asks the running app's language server. A closed app
// is a local failure: no API attempt, and the cached data stays as is.
// Failed discovery of a running app is a failed fetch.
func (a *app) updateAntigravity(c *Cache, acc Account, now time.Time) {
	procs, err := a.agyFind()
	if err == nil && len(procs) == 0 {
		c.Error = errAgyNotRunning
		return
	}
	c.AttemptAt = now
	if err != nil {
		c.Error = errAgyNoServer
		return
	}
	u, plan, email, err := a.agyFetch(procs)
	if err != nil {
		c.Error = err.Error()
		return
	}
	c.Plan, c.Email = plan, email
	c.Usage = u
	c.FetchedAt = now
	c.Error = ""
}

// agyFetch tries each port, https then http, within httpTimeout. The first
// parseable 200 wins. Errors are constants.
func (a *app) agyFetch(procs []agyProc) (Usage, string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	best := errors.New(errAgyNoServer)
	for _, p := range procs {
		if p.csrf == "" {
			continue
		}
		for _, port := range p.ports {
			for _, scheme := range []string{"https", "http"} {
				if ctx.Err() != nil {
					return Usage{}, "", "", best
				}
				data, err := a.agyPost(ctx, scheme, port, p.csrf)
				if err == nil {
					u, plan, email, perr := parseAgy(data)
					if perr == nil {
						return u, plan, email, nil
					}
					err = perr
				}
				var se statusError
				if errors.As(err, &se) && se == http.StatusBadRequest {
					continue // wrong scheme
				}
				if err.Error() != "network error" {
					best = err
				}
			}
		}
	}
	return Usage{}, "", "", best
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func (a *app) agyPost(ctx context.Context, scheme string, port int, csrf string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, agyTryTimeout)
	defer cancel()
	url := fmt.Sprintf("%s://%s:%d%s", scheme, agyHost, port, agyStatusPath)
	body := `{"metadata":{"ideName":"antigravity","extensionName":"antigravity","locale":"en"}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, errors.New("network error")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Codeium-Csrf-Token", csrf)
	req.Header.Set("User-Agent", "agent-usage-bar")
	resp, err := a.agyHTTP.Do(req)
	if err != nil {
		return nil, errors.New("network error")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("network error")
	}
	return data, nil
}

var variantSuffix = regexp.MustCompile(`\s*\([^()]*\)$`)

// parseAgy maps GetUserStatus onto Usage: a row per model (variants such
// as "(High)" merged, the most used kept). Week is picked at display time
// (lowestWindow). Returns plan and email too.
func parseAgy(data []byte) (Usage, string, string, error) {
	var r struct {
		UserStatus *struct {
			Email      string `json:"email"`
			PlanStatus struct {
				PlanInfo struct {
					PlanName string `json:"planName"`
				} `json:"planInfo"`
			} `json:"planStatus"`
			Models struct {
				Configs []json.RawMessage `json:"clientModelConfigs"`
			} `json:"cascadeModelConfigData"`
			UserTier struct {
				Name string `json:"name"`
			} `json:"userTier"`
		} `json:"userStatus"`
	}
	if json.Unmarshal(data, &r) != nil || r.UserStatus == nil {
		return Usage{}, "", "", errors.New("bad API response")
	}
	us := r.UserStatus
	var u Usage
	for _, raw := range us.Models.Configs {
		var m struct {
			Label     string `json:"label"`
			QuotaInfo *struct {
				Remaining *float64 `json:"remainingFraction"`
				ResetTime string   `json:"resetTime"`
			} `json:"quotaInfo"`
		}
		if json.Unmarshal(raw, &m) != nil || m.QuotaInfo == nil {
			continue
		}
		q := m.QuotaInfo
		left := 0.0 // a reset without a fraction: used up
		switch {
		case q.Remaining != nil:
			left = *q.Remaining
		case q.ResetTime == "":
			continue
		}
		w := Window{Label: variantSuffix.ReplaceAllString(m.Label, ""), Percent: clampPct((1 - left) * 100), Idle: left >= 1}
		if w.Label == "" {
			w.Label = "?"
		}
		if t, err := time.Parse(time.RFC3339Nano, q.ResetTime); err == nil {
			w.ResetsAt = t
		}
		merged := false
		for i := range u.Scoped {
			if u.Scoped[i].Label == w.Label {
				if w.Percent > u.Scoped[i].Percent {
					u.Scoped[i] = w
				}
				merged = true
			}
		}
		if !merged {
			u.Scoped = append(u.Scoped, w)
		}
	}
	if len(u.Scoped) == 0 {
		return Usage{}, "", "", errors.New("no model quotas")
	}
	plan := strings.TrimPrefix(us.UserTier.Name, "Antigravity ")
	if plan == "" {
		plan = us.PlanStatus.PlanInfo.PlanName
	}
	return u, plan, us.Email, nil
}

// lowestWindow is the model window with the least left now, labelled
// "lowest". A model whose reset has passed counts as unused.
func lowestWindow(ws []Window, now time.Time) *Window {
	var low *Window
	for i := range ws {
		if low == nil || effective(&ws[i], now) > effective(low, now) {
			low = &ws[i]
		}
	}
	if low == nil {
		return nil
	}
	w := *low
	w.Label = "lowest"
	return &w
}
