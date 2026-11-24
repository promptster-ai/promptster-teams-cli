package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptster-ai/promptster-teams-cli/internal/event"
)

func msTs(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano) }

// aiCommandIn records one AI Bash command that ran in ws from startTs to endMs,
// through the same funnel the watchers use.
func aiCommandIn(t *testing.T, ws, startTs string, endMs int64) {
	t.Helper()
	ev := event.NewEvent("command", "bash-sess")
	ev.Ts = msTs(endMs)
	ev.StartTs = startTs
	ev.Provenance = event.AIProvenance()
	ev.Data = map[string]interface{}{"command": "cd " + ws + " && git apply ../p.patch && make check"}
	recordAiBashWindow(&ev, ws, false)
}

func TestBashWindowStartMs(t *testing.T) {
	end := time.Date(2026, 11, 24, 22, 37, 53, 0, time.UTC).UnixMilli()
	cases := []struct {
		name  string
		start string
		want  int64
	}{
		{"run of 3 minutes", msTs(end - 181_000), end - 181_000},
		{"no start (hook, legacy)", "", end},
		{"unparseable start", "yesterday", end},
		{"start after end (skew)", msTs(end + 5_000), end},
		{"run at the cap", msTs(end - bashWindowMaxSpanMs), end - bashWindowMaxSpanMs},
		{"run past the cap", msTs(end - bashWindowMaxSpanMs - 1), end},
	}
	for _, c := range cases {
		if got := bashWindowStartMs(c.start, end); got != c.want {
			t.Errorf("%s: start = %d, want %d", c.name, got, c.want)
		}
	}
}

// A file written at the START of a long command (apply, then check, then
// commit) is that command's: its mtime is minutes before the command's end, and
// the end point alone read it as a miss.
func TestBashRecoveryFileWrittenAtCommandStart(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "gen.go", "package main\n\nvar Gen = 1\n")
	git("add", "-A")
	git("commit", "-m", "written by an AI bash command")
	sha := gitOut("rev-parse", "HEAD")
	base := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(ws, "gen.go"), base, base); err != nil {
		t.Fatal(err)
	}
	m := statMtimeMs(t, filepath.Join(ws, "gen.go"))

	// The previous command ended 6s before the write; the writing one ran 3 minutes.
	aiCommandIn(t, ws, msTs(m-7_000), m-6_000)
	aiCommandIn(t, ws, msTs(m-200), m+181_000)

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha, siblingLineage{})
	if !ok {
		t.Fatal("expected an emittable event")
	}
	if ev.SessionID != "bash-sess" {
		t.Fatalf("sessionId = %q, want the command that wrote the file", ev.SessionID)
	}
	for _, r := range filesByPath(t, ev)["gen.go"]["lineRanges"].([]interface{}) {
		if r.(map[string]interface{})["attribution"] != attributionLikelyAI {
			t.Errorf("file written inside the command's run must be likely_ai, got %+v", r)
		}
	}
	if _, has := ev.Data.(map[string]interface{})["unattributedReason"]; has {
		t.Error("an attributed commit must carry no unattributedReason")
	}
}

// A human edit well before the command started stays unknown, and the miss is
// still reported against the window's start, as bash_window_miss_early.
func TestBashRecoveryEditBeforeCommandStartStaysUnknown(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)
	writeCommitFile(t, ws, "notes.md", "by hand\n")
	git("add", "-A")
	git("commit", "-m", "human edit")
	sha := gitOut("rev-parse", "HEAD")
	base := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(filepath.Join(ws, "notes.md"), base, base); err != nil {
		t.Fatal(err)
	}
	m := statMtimeMs(t, filepath.Join(ws, "notes.md"))

	aiCommandIn(t, ws, msTs(m+120_000), m+300_000)

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha, siblingLineage{})
	if !ok {
		t.Fatal("expected an emittable event")
	}
	if want := unattributedSessionID("dev-x"); ev.SessionID != want {
		t.Fatalf("sessionId = %q, want %q", ev.SessionID, want)
	}
	data := ev.Data.(map[string]interface{})
	if data["unattributedReason"] != unattributedBashMissEarly || data["bashWindowGapMs"] != int64(120_000) {
		t.Errorf("diag = %v / %v, want %s / 120000 (gap to the command's start)",
			data["unattributedReason"], data["bashWindowGapMs"], unattributedBashMissEarly)
	}
}

// Inside two sessions' windows at once, the narrower one wrote the file: a 2s
// `sed` beats another session's 5-minute test run that ends later.
func TestBashRecoveryPrefersTheNarrowerContainingWindow(t *testing.T) {
	dir := t.TempDir()
	writeCommitFile(t, dir, "x.go", "package x\n")
	m := statMtimeMs(t, filepath.Join(dir, "x.go"))
	here := []string{resolvePath(dir)}
	got, ok := recoverBashSession(dir, "x.go", []bashWindow{
		{SessionID: "long-tests", StartMs: m - 60_000, EndMs: m + 240_000, Roots: here},
		{SessionID: "sed", StartMs: m - 1_000, EndMs: m + 1_000, Roots: here},
	})
	if !ok || got != "sed" {
		t.Fatalf("credited %q (ok=%v), want the narrower window", got, ok)
	}
}

// An end-point window (Cursor, hooks, older ledgers) 40ms off its write still
// beats a long run that merely contains it.
func TestBashRecoveryEndPointBeatsAContainingLongRun(t *testing.T) {
	dir := t.TempDir()
	writeCommitFile(t, dir, "x.go", "package x\n")
	m := statMtimeMs(t, filepath.Join(dir, "x.go"))
	here := []string{resolvePath(dir)}
	got, ok := recoverBashSession(dir, "x.go", []bashWindow{
		{SessionID: "long-tests", StartMs: m - 60_000, EndMs: m + 240_000, Roots: here},
		{SessionID: "cursor-sed", StartMs: m + 40, EndMs: m + 40, Roots: here},
	})
	if !ok || got != "cursor-sed" {
		t.Fatalf("credited %q (ok=%v), want the end-point window at its write", got, ok)
	}
}
