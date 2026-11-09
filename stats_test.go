package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secretText = "PRIVATE-PROMPT-TEXT"

func asstLine(id, req string, ts time.Time, model, cwd string, in, out, cw, cr int) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"timestamp":%q,"cwd":%q,"message":{"id":%q,"model":%q,"role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`+"\n",
		req, ts.UTC().Format(time.RFC3339Nano), cwd, id, model, secretText, in, out, cw, cr)
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func TestStats(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	a := &app{home: home, cacheDir: filepath.Join(home, "cache")}
	acc := Account{Name: "work", ConfigDir: dir}
	now := time.Date(2026, 11, 9, 15, 30, 0, 0, time.UTC)

	s1 := filepath.Join(dir, "projects", "-src-api", "s1.jsonl")
	sub := filepath.Join(dir, "projects", "-src-api", "s1", "subagents", "agent-1.jsonl")
	other := filepath.Join(dir, "projects", "-src-web", "s2.jsonl")
	appendFile(t, s1, `{"type":"user","message":{"content":"`+secretText+` \"type\":\"assistant\""}}`+"\n"+
		asstLine("m1", "r1", now.Add(-time.Hour), "claude-opus-5-5", "/src/api", 10, 100, 1000, 9000)+
		asstLine("m1", "r1", now.Add(-time.Hour), "claude-opus-5-5", "/src/api", 10, 100, 1000, 9000)+ // streamed twice
		asstLine("m2", "r2", now.Add(-26*time.Hour), "claude-sonnet-5", "/src/api", 5, 50, 0, 0)+ // yesterday
		asstLine("m3", "r3", now.Add(-8*24*time.Hour), "claude-opus-5-5", "/src/api", 1, 1, 1, 1)+ // too old
		asstLine("m4", "r4", now.Add(-time.Minute), "<synthetic>", "/src/api", 1, 1, 1, 1))
	appendFile(t, sub, asstLine("m5", "r5", now.Add(-2*time.Hour), "claude-opus-5-5", "/src/api", 1, 20, 0, 100))
	appendFile(t, other, asstLine("m1", "r1", now.Add(-time.Hour), "claude-opus-5-5", "/src/api", 10, 100, 1000, 9000)+ // copied from s1
		asstLine("m6", "r6", now.Add(-10*time.Minute), "claude-opus-5-5", "/src/web", 0, 30, 0, 0)+
		`{"type":"assistant","message":{"id":"partial"`) // unfinished line

	st := a.stats(acc, now, false)
	if st == nil {
		t.Fatal("no stats")
	}
	want := Totals{Input: 11, Output: 150, CacheWrite: 1000, CacheRead: 9100, Msgs: 3}
	if st.Today != want {
		t.Errorf("today = %+v, want %+v", st.Today, want)
	}
	if st.Week.Msgs != 4 || st.Week.Output != 200 {
		t.Errorf("week = %+v", st.Week)
	}
	if len(st.Hourly) != 16 || st.Hourly[14] != 100 || st.Hourly[13] != 20 || st.Hourly[15] != 30 {
		t.Errorf("hourly = %v", st.Hourly)
	}
	if len(st.Projects) != 2 || st.Projects[0].Name != "api" || st.Projects[0].Tokens != 10231 || st.Projects[1] != (Count{"web", 30}) {
		t.Errorf("projects = %+v", st.Projects)
	}
	if len(st.Models) != 2 || st.Models[0].Name != "claude-opus-5-5" || st.Models[1] != (Count{"claude-sonnet-5", 55}) {
		t.Errorf("models = %+v", st.Models)
	}
	if p := st.Today.CachePct(); p < 89 || p > 91 {
		t.Errorf("cache pct = %v", p)
	}

	// New lines within the interval are not counted yet...
	appendFile(t, other, `,"model":"claude-opus-5-5"}}`+"\n"+
		asstLine("m7", "r7", now.Add(-5*time.Minute), "claude-opus-5-5", "/src/web", 0, 7, 0, 0))
	if st := a.stats(acc, now.Add(time.Minute), false); st.Today.Msgs != 3 {
		t.Errorf("recomputed within interval: %+v", st.Today)
	}
	// ...but are after it, read from where the file was left.
	st = a.stats(acc, now.Add(statsInterval), false)
	if st.Today.Msgs != 4 || st.Today.Output != 157 {
		t.Errorf("after append: %+v", st.Today)
	}

	// Caches hold numbers and names only.
	entries, _ := os.ReadDir(a.cacheDir)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(a.cacheDir, e.Name()))
		for _, bad := range []string{secretText, "-src-api", "s1.jsonl", "/src/api"} {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s contains %q", e.Name(), bad)
			}
		}
	}
}

func TestStatsNoProjects(t *testing.T) {
	home := t.TempDir()
	a := &app{home: home, cacheDir: filepath.Join(home, "cache")}
	if st := a.stats(Account{Name: "x", ConfigDir: filepath.Join(home, ".claude")}, time.Now(), false); st != nil {
		t.Errorf("stats = %+v", st)
	}
}

func TestLiveSessions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", os.Getpid())),
		fmt.Sprintf(`{"pid":%d,"sessionId":"abcdef123456","cwd":"/src/api","name":"api","status":"busy","updatedAt":1}`, os.Getpid()))
	writeFile(t, filepath.Join(dir, "sessions", "1.json"), `{"pid":1,"sessionId":"x","status":"idle"}`)
	writeFile(t, filepath.Join(dir, "sessions", "99999999.json"), `{"pid":99999999,"sessionId":"dead","status":"idle"}`)
	writeFile(t, filepath.Join(dir, "sessions", "12.abc.key"), "secret-key")
	got := liveSessions(dir)
	if len(got) != 2 {
		t.Fatalf("sessions = %+v", got)
	}
	if got[0] != (Session{PID: os.Getpid(), Name: "api", Cwd: "/src/api", Status: "busy"}) || got[1].Name != "x" {
		t.Errorf("sessions = %+v", got)
	}
}

func TestAggregateCorruptIndex(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	index := map[string]*fileAgg{"a": {Models: []string{"m"}, Projects: []string{"p"}, Recs: []rec{
		{1, now.Add(-time.Hour).Unix(), -1, -5, 1, 1, 1, 1},
		{2, now.Add(-time.Hour).Unix(), 9, 9, 1, 1, 1, 1},
	}}}
	st := aggregate(index, now)
	if st.Today.Msgs != 2 || len(st.Models) != 0 || len(st.Projects) != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestStatsIndexMaintenance(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	a := &app{home: home, cacheDir: filepath.Join(home, "cache")}
	acc := Account{Name: "w", ConfigDir: dir}
	now := time.Date(2026, 11, 9, 15, 30, 0, 0, time.UTC)
	path := filepath.Join(dir, "projects", "p", "s.jsonl")

	// Null entries and old records in the index are dropped, not fatal.
	indexPath := a.cacheFile("files", acc.Name)
	writeFile(t, indexPath, fmt.Sprintf(`{"dead":null,"old":{"recs":[[7,%d,0,0,1,1,1,1]],"models":["m"],"projects":["p"]}}`,
		now.Add(-30*24*time.Hour).Unix()))

	// An over-long line is skipped; the next line still counts.
	long := `{"type":"assistant","pad":"` + strings.Repeat("x", maxLine+10) + `"}` + "\n"
	appendFile(t, path, long+asstLine("m1", "r1", now.Add(-time.Hour), "opus", "/p", 0, 10, 0, 0))
	st := a.stats(acc, now, true)
	if st.Today.Msgs != 1 || st.Today.Output != 10 {
		t.Fatalf("today = %+v", st.Today)
	}
	var index map[string]*fileAgg
	readJSON(indexPath, &index)
	if len(index) != 1 || index["old"] != nil || index["dead"] != nil {
		t.Errorf("index keys = %v", index)
	}

	// The file is replaced by a new one (new inode, bigger): read from 0.
	tmp := path + ".new"
	appendFile(t, tmp, asstLine("m2", "r2", now.Add(-time.Hour), "opus", "/p", 0, 20, 0, 0)+
		asstLine("m3", "r3", now.Add(-time.Hour), "opus", "/p", 0, 30, 0, 0)+long)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	st = a.stats(acc, now, true)
	if st.Today.Msgs != 2 || st.Today.Output != 50 {
		t.Errorf("after replace: %+v", st.Today)
	}

	// Out of time budget: nothing new is read and the stats aren't saved.
	appendFile(t, path, asstLine("m4", "r4", now.Add(-time.Hour), "opus", "/p", 0, 40, 0, 0))
	os.Remove(a.cacheFile("stats", acc.Name))
	old := statsBudget
	statsBudget = -time.Second
	st = a.stats(acc, now, true)
	statsBudget = old
	if st.Today.Output != 50 {
		t.Errorf("over budget: %+v", st.Today)
	}
	if _, err := os.Stat(a.cacheFile("stats", acc.Name)); err == nil {
		t.Error("partial stats were saved")
	}
	if st = a.stats(acc, now, false); st.Today.Output != 90 {
		t.Errorf("next run: %+v", st.Today)
	}
}
