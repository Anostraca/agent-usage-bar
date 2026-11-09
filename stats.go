package main

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	statsInterval = 2 * time.Minute
	statsWindow   = 7 * 24 * time.Hour
	topProjects   = 5
)

// Session is a running Claude Code process of an account.
type Session struct {
	PID    int    `json:"pid"`
	Name   string `json:"name,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
	Status string `json:"status,omitempty"`
}

// liveSessions reads <dir>/sessions/<pid>.json and keeps running pids.
// Only the .json files are opened, never the .key files next to them.
func liveSessions(dir string) []Session {
	paths, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	var out []Session
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Name      string `json:"name"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(data, &s) != nil || s.PID <= 0 || !alive(s.PID) {
			continue
		}
		if s.Name == "" {
			s.Name = s.SessionID[:min(8, len(s.SessionID))]
		}
		out = append(out, Session{PID: s.PID, Name: s.Name, Cwd: s.Cwd, Status: s.Status})
	}
	slices.SortFunc(out, func(a, b Session) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Totals are token sums over assistant messages.
type Totals struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
	Msgs       int64 `json:"msgs"`
}

func (t Totals) All() int64 { return t.Input + t.Output + t.CacheWrite + t.CacheRead }

// CachePct is the share of input-side tokens read from the prompt cache.
func (t Totals) CachePct() float64 {
	in := t.Input + t.CacheWrite + t.CacheRead
	if in == 0 {
		return 0
	}
	return float64(t.CacheRead) * 100 / float64(in)
}

func (t *Totals) add(r rec) {
	t.Input += r[rIn]
	t.Output += r[rOut]
	t.CacheWrite += r[rCW]
	t.CacheRead += r[rCR]
	t.Msgs++
}

type Count struct {
	Name   string `json:"name"`
	Tokens int64  `json:"tokens"`
}

// Stats is local token usage from the account's transcripts.
type Stats struct {
	At       time.Time `json:"at"`
	Today    Totals    `json:"today"`
	Week     Totals    `json:"week"`
	Hourly   []int64   `json:"hourly"`             // output tokens per hour today
	Projects []Count   `json:"projects,omitempty"` // top projects today
	Models   []Count   `json:"models,omitempty"`   // last 7 days
}

// rec is one assistant message: message key hash, unix time, model and
// project index, then token counts.
type rec [8]int64

const (
	rKey = iota
	rTime
	rModel
	rProject
	rIn
	rOut
	rCW
	rCR
)

// fileAgg caches the messages of one transcript; Off is how far it was read.
type fileAgg struct {
	Ino      uint64   `json:"ino"`
	Size     int64    `json:"size"`
	Mtime    int64    `json:"mtime"`
	Off      int64    `json:"off"`
	Models   []string `json:"models"`
	Projects []string `json:"projects"` // cwd base names
	Recs     []rec    `json:"recs"`
}

// stats returns the account's local stats, recomputed at most every
// statsInterval. Changed transcripts are read from where they were left.
func (a *app) stats(acc Account, now time.Time, force bool) *Stats {
	root := filepath.Join(acc.ConfigDir, "projects")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil
	}
	statsPath := a.cacheFile("stats", acc.Name)
	var cached Stats
	if readJSON(statsPath, &cached) && !force && now.Sub(cached.At) < statsInterval && now.Sub(cached.At) >= 0 &&
		sameDay(cached.At, now) {
		return &cached
	}

	indexPath := a.cacheFile("files", acc.Name)
	index := map[string]*fileAgg{}
	readJSON(indexPath, &index)
	cut := now.Add(-statsWindow)
	for k, fa := range index {
		if fa == nil {
			delete(index, k)
			continue
		}
		fa.Recs = slices.DeleteFunc(fa.Recs, func(r rec) bool { return r[rTime] < cut.Unix() })
	}
	seen := map[string]bool{}
	deadline := time.Now().Add(statsBudget)
	complete := true
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(cut) {
			return nil
		}
		key := hashHex(p, 8)
		seen[key] = true
		ino := inode(fi)
		fa := index[key]
		if fa != nil && fa.Ino == ino && fa.Size == fi.Size() && fa.Mtime == fi.ModTime().UnixNano() {
			return nil
		}
		if !complete {
			return nil // out of time: keep the old numbers, finish next run
		}
		if fa == nil || fa.Ino != ino || fi.Size() < fa.Off {
			fa = &fileAgg{Ino: ino}
		}
		done, err := parseTranscript(p, fa, cut.Unix(), deadline)
		if err == nil {
			if done {
				fa.Size, fa.Mtime = fi.Size(), fi.ModTime().UnixNano()
			} else {
				complete = false
			}
			index[key] = fa
		}
		return nil
	})
	for k := range index {
		if !seen[k] {
			delete(index, k)
		}
	}
	writeJSON(a.cacheDir, indexPath, index)

	st := aggregate(index, now)
	if complete {
		writeJSON(a.cacheDir, statsPath, st)
	}
	return st
}

var (
	assistantMark = []byte(`"type":"assistant"`)
	statsBudget   = 3 * time.Second // per stats run; the rest waits for the next run
)

const maxLine = 4 << 20

func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// readLine returns the next complete line and the bytes it took. Lines over
// maxLine come back empty. At EOF a partial line is not consumed.
func readLine(r *bufio.Reader) ([]byte, int64, error) {
	var buf []byte
	var n int64
	skip := false
	for {
		chunk, err := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !skip {
			if len(buf)+len(chunk) > maxLine {
				skip, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err != nil:
			return nil, 0, err
		}
		return buf, n, nil
	}
}

// parseTranscript reads complete lines from fa.Off on and appends assistant
// messages with usage newer than cut (unix s). Only ids, time, model, cwd
// base name and token counts are kept. done is false when the deadline
// stopped it early.
func parseTranscript(path string, fa *fileAgg, cut int64, deadline time.Time) (done bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Seek(fa.Off, io.SeekStart); err != nil {
		return false, err
	}
	have := map[int64]bool{}
	for _, r := range fa.Recs {
		have[r[rKey]] = true
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for i := 0; ; i++ {
		if i%1000 == 0 && time.Now().After(deadline) {
			return false, nil
		}
		line, n, err := readLine(r)
		if err != nil {
			if err == io.EOF {
				return true, nil // a partial last line is read next time
			}
			return false, err
		}
		fa.Off += n
		if !bytes.Contains(line, assistantMark) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			RequestID string `json:"requestId"`
			Timestamp string `json:"timestamp"`
			Cwd       string `json:"cwd"`
			Message   struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage *struct {
					In  int64 `json:"input_tokens"`
					Out int64 `json:"output_tokens"`
					CW  int64 `json:"cache_creation_input_tokens"`
					CR  int64 `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.Usage == nil ||
			e.Message.Model == "" || e.Message.Model == "<synthetic>" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil || t.Unix() < cut {
			continue
		}
		var key int64
		if e.Message.ID != "" || e.RequestID != "" {
			h := fnv.New64a()
			h.Write([]byte(e.Message.ID + "\x00" + e.RequestID))
			key = int64(h.Sum64())
			if have[key] {
				continue
			}
			have[key] = true
		}
		project := "?"
		if e.Cwd != "" {
			project = filepath.Base(e.Cwd)
		}
		u := e.Message.Usage
		fa.Recs = append(fa.Recs, rec{key, t.Unix(), indexOf(&fa.Models, e.Message.Model),
			indexOf(&fa.Projects, project), u.In, u.Out, u.CW, u.CR})
	}
}

func indexOf(list *[]string, s string) int64 {
	if i := slices.Index(*list, s); i >= 0 {
		return int64(i)
	}
	*list = append(*list, s)
	return int64(len(*list) - 1)
}

// aggregate sums all cached messages, each message once across files.
func aggregate(index map[string]*fileAgg, now time.Time) *Stats {
	loc := now.Location()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	cut := now.Add(-statsWindow)
	st := &Stats{At: now, Hourly: make([]int64, now.Hour()+1)}
	seen := map[int64]bool{}
	models, projects := map[string]int64{}, map[string]int64{}
	keys := make([]string, 0, len(index))
	for k := range index {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fa := index[k]
		for _, r := range fa.Recs {
			if r[rKey] != 0 {
				if seen[r[rKey]] {
					continue
				}
				seen[r[rKey]] = true
			}
			t := time.Unix(r[rTime], 0).In(loc)
			if t.Before(cut) || t.After(now) {
				continue
			}
			st.Week.add(r)
			all := r[rIn] + r[rOut] + r[rCW] + r[rCR]
			if m := r[rModel]; m >= 0 && m < int64(len(fa.Models)) {
				models[fa.Models[m]] += all
			}
			if t.Before(dayStart) {
				continue
			}
			st.Today.add(r)
			st.Hourly[t.Hour()] += r[rOut]
			if p := r[rProject]; p >= 0 && p < int64(len(fa.Projects)) {
				projects[fa.Projects[p]] += all
			}
		}
	}
	st.Models = top(models, 0)
	st.Projects = top(projects, topProjects)
	return st
}

func top(m map[string]int64, n int) []Count {
	var out []Count
	for k, v := range m {
		if v > 0 {
			out = append(out, Count{k, v})
		}
	}
	slices.SortFunc(out, func(a, b Count) int {
		return cmp.Or(cmp.Compare(b.Tokens, a.Tokens), cmp.Compare(a.Name, b.Name))
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func sameDay(a, b time.Time) bool {
	a = a.In(b.Location())
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

func hashHex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:n])
}

func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, v) == nil
}

// writeJSON writes atomically (temp file + rename). Failures are ignored:
// caches are rebuilt on the next run.
func writeJSON(dir, path string, v any) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := f.Write(append(data, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(f.Name(), path) != nil {
		os.Remove(f.Name())
	}
}
