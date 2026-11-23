package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptster-ai/promptster-teams-cli/internal/event"
	"github.com/promptster-ai/promptster-teams-cli/internal/normalize"
	"github.com/promptster-ai/promptster-teams-cli/internal/redact"
)

// aiWrite writes content to home/rel and observes it as the Claude and Codex
// funnels do: content recorded, then the dedupe that feeds the path ledger.
func aiWrite(t *testing.T, home, rel, content, sessionID string, replay bool) {
	t.Helper()
	writeCommitFile(t, home, rel, content)
	aiObserve(home, rel, sessionID, content, time.Now(), replay)
}

// aiObserve reports a Write of `wrote` to home/rel, whatever is on disk.
func aiObserve(home, rel, sessionID, wrote string, at time.Time, replay bool) {
	aiObservePath(home, filepath.Join(home, rel), sessionID, map[string]interface{}{"content": wrote}, at, replay)
}

// aiObservePath is the funnel's order: content from the path as the tool
// reported it, then relativize, then the dedupe that feeds the path ledger.
func aiObservePath(home, reported, sessionID string, data map[string]interface{}, at time.Time, replay bool) {
	data["path"] = reported
	ev := event.Event{
		Kind:       "file_diff",
		SessionID:  sessionID,
		Ts:         at.UTC().Format(time.RFC3339Nano),
		Data:       data,
		Provenance: &event.Provenance{Attribution: "likely_ai"},
	}
	recordAiContentFromEvent(&ev, replay)
	normalize.RelativizeEventPaths(&ev, home)
	dedupeFileDiff(home, &ev, replay)
}

// agentWorktreeThenSquash reproduces the Codex flow that read `unknown`: the agent
// writes in its own worktree and commits there, the worktree is removed, and the
// branch lands on the main checkout as a squash, a SHA no checkout held. The
// human edit (if any) lands in the main checkout before the squash is committed.
func agentWorktreeThenSquash(t *testing.T, aiBytes, humanBytes string) (home, primary, sha string) {
	t.Helper()
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home = t.TempDir()
	primary = filepath.Join(home, "repo")
	git, gitOut := gitRepoAt(t, primary)
	git("commit", "--allow-empty", "-m", "base")
	wt := filepath.Join(home, "repo", ".worktrees", "ticket")
	git("worktree", "add", "-q", "-b", "ticket", wt)

	aiWrite(t, home, "repo/.worktrees/ticket/src/a.go", aiBytes, "sess-codex", false)
	wtGit, _ := gitIn(t, wt)
	wtGit("add", "-A")
	wtGit("commit", "-m", "agent work")
	git("worktree", "remove", wt)

	git("merge", "--squash", "ticket")
	if humanBytes != "" {
		writeCommitFile(t, primary, "src/a.go", humanBytes)
		git("add", "-A")
	}
	git("commit", "-m", "squash")
	return home, primary, gitOut("rev-parse", "HEAD")
}

func TestCommitAttributionMatchesAgentBytesFromARemovedWorktree(t *testing.T) {
	home, primary, sha := agentWorktreeThenSquash(t, "package a\n\nvar X = 1\n", "")
	got, sess := attributionOf(t, primary, home, sha)
	if got["src/a.go"] != attributionLikelyAI || sess != "sess-codex" {
		t.Fatalf("src/a.go = %q, session %q; want likely_ai from sess-codex", got["src/a.go"], sess)
	}
	// The content ledger is what carried it: without it the path ledger alone
	// reads the removed worktree's write as nothing.
	if err := os.Remove(aiContentLedgerPath()); err != nil {
		t.Fatal(err)
	}
	if got, _ := attributionOf(t, primary, home, sha); got["src/a.go"] != attributionUnknown {
		t.Fatalf("without the content ledger src/a.go = %q, want unknown — the test no longer isolates the content pass", got["src/a.go"])
	}
}

// The line the checkout gate holds: different bytes at the same path are not
// the agent's, so a human's change on top stays unknown.
func TestCommitAttributionDoesNotMatchBytesTheAgentNeverWrote(t *testing.T) {
	home, primary, sha := agentWorktreeThenSquash(t, "package a\n\nvar X = 1\n", "package a\n\nvar X = 2\n")
	got, sess := attributionOf(t, primary, home, sha)
	if got["src/a.go"] != attributionUnknown || sess != "" {
		t.Fatalf("src/a.go = %q, session %q; want unknown with no session", got["src/a.go"], sess)
	}
}

// The same bytes at the same path in ANOTHER repository are not evidence here.
func TestCommitAttributionContentMatchStaysInsideOneRepository(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	other := filepath.Join(home, "other")
	otherGit, _ := gitRepoAt(t, other)
	otherGit("commit", "--allow-empty", "-m", "base")
	aiWrite(t, home, "other/src/a.go", "same\n", "sess-other", false)

	primary := filepath.Join(home, "repo")
	git, gitOut := gitRepoAt(t, primary)
	writeCommitFile(t, primary, "src/a.go", "same\n")
	git("add", "-A")
	git("commit", "-m", "human")
	got, sess := attributionOf(t, primary, home, gitOut("rev-parse", "HEAD"))
	if got["src/a.go"] != attributionUnknown || sess != "" {
		t.Fatalf("src/a.go = %q, session %q; want unknown — another repo's write", got["src/a.go"], sess)
	}
}

// A replayed write describes bytes from the past; the file on disk now is not
// them, so nothing is recorded.
func TestReplayedWriteRecordsNoContent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	git, _ := gitRepoAt(t, filepath.Join(home, "repo"))
	git("commit", "--allow-empty", "-m", "base")
	aiWrite(t, home, "repo/a.go", "x\n", "sess", true)
	if n := len(readAiContentLedger().Entries); n != 0 {
		t.Fatalf("replay recorded %d content entries, want 0", n)
	}
}

// A file saved again after the edit event (a person, before the watcher caught
// up) carries bytes the agent may not have written: nothing is recorded.
func TestWriteChangedSinceTheEventRecordsNoContent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	git, _ := gitRepoAt(t, filepath.Join(home, "repo"))
	git("commit", "--allow-empty", "-m", "base")
	writeCommitFile(t, home, "repo/a.go", "human\n")
	aiObserve(home, "repo/a.go", "sess", "human\n", time.Now().Add(-10*time.Second), false)
	if n := len(readAiContentLedger().Entries); n != 0 {
		t.Fatalf("recorded %d content entries for a file changed after its event, want 0", n)
	}
}

// A failed or denied edit reports a write that never happened: the file on disk
// is a person's earlier save, so nothing is recorded.
func TestEditThatDidNotWriteRecordsNoContent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	git, _ := gitRepoAt(t, filepath.Join(home, "repo"))
	git("commit", "--allow-empty", "-m", "base")
	writeCommitFile(t, home, "repo/a.go", "human\n")
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(home, "repo/a.go"), old, old); err != nil {
		t.Fatal(err)
	}
	aiObserve(home, "repo/a.go", "sess", "human\n", time.Now(), false)
	if n := len(readAiContentLedger().Entries); n != 0 {
		t.Fatalf("recorded %d content entries for an edit that wrote nothing, want 0", n)
	}
}

// A path relative to a working directory the event doesn't carry is not guessed.
func TestRelativeReportedPathRecordsNoContent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	git, _ := gitRepoAt(t, filepath.Join(home, "repo"))
	git("commit", "--allow-empty", "-m", "base")
	writeCommitFile(t, home, "repo/a.go", "x\n")
	aiObservePath(home, "repo/a.go", "sess", map[string]interface{}{"content": "x\n"}, time.Now(), false)
	if n := len(readAiContentLedger().Entries); n != 0 {
		t.Fatalf("recorded %d content entries for a relative path, want 0", n)
	}
}

// The reviewer's case: a person saved their own version just before the agent's
// edit was denied or failed. The file is fresh, but the edit's lines are not in
// it, so it is not the agent's.
func TestEditWhoseLinesAreMissingRecordsNoContent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	git, _ := gitRepoAt(t, filepath.Join(home, "repo"))
	git("commit", "--allow-empty", "-m", "base")
	writeCommitFile(t, home, "repo/a.go", "package a\n\nvar Human = 1\n")
	aiObservePath(home, filepath.Join(home, "repo/a.go"), "sess",
		map[string]interface{}{"diff": "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+var Agent = 2\n"}, time.Now(), false)
	if n := len(readAiContentLedger().Entries); n != 0 {
		t.Fatalf("recorded %d content entries for an edit whose lines are absent, want 0", n)
	}
}

// Greptile's case: a failed replacement whose new line (`return nil`) already
// appears elsewhere in a file a person just saved. The lines are present, but
// the tool said it failed, so nothing is recorded.
func TestFailedEditRecordsNoContentEvenWhenItsLinesArePresent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	git, _ := gitRepoAt(t, filepath.Join(home, "repo"))
	git("commit", "--allow-empty", "-m", "base")
	writeCommitFile(t, home, "repo/a.go", "package a\n\nfunc f() error {\n\treturn nil\n}\n")
	data := map[string]interface{}{"diff": "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+\treturn nil\n"}
	aiObservePath(home, filepath.Join(home, "repo/a.go"), "sess", data, time.Now(), false)
	if n := len(readAiContentLedger().Entries); n != 1 {
		t.Fatalf("setup: a successful edit with these lines recorded %d entries, want 1", n)
	}
	if err := os.Remove(aiContentLedgerPath()); err != nil {
		t.Fatal(err)
	}
	data = map[string]interface{}{"diff": data["diff"], normalize.ToolFailedKey: true}
	aiObservePath(home, filepath.Join(home, "repo/a.go"), "sess", data, time.Now(), false)
	if n := len(readAiContentLedger().Entries); n != 0 {
		t.Fatalf("a failed edit recorded %d content entries, want 0", n)
	}
}

func TestAiEditLanded(t *testing.T) {
	file := []byte("package a\r\n\nvar Agent = 2\n")
	diff := func(lines ...string) map[string]interface{} {
		return map[string]interface{}{"diff": "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n" + strings.Join(lines, "\n")}
	}
	cases := []struct {
		name string
		data map[string]interface{}
		want bool
	}{
		{"write equal", map[string]interface{}{"content": string(file)}, true},
		{"write differs", map[string]interface{}{"content": "other"}, false},
		{"every added line present (CRLF tolerated)", diff("-old", "+package a", "+var Agent = 2"), true},
		{"an added line missing", diff("+var Agent = 2", "+var Gone = 3"), false},
		{"deletion only proves nothing", diff("-old"), false},
		{"no diff at all", map[string]interface{}{}, false},
	}
	for _, c := range cases {
		if got := aiEditLanded(c.data, file); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A no-fast-forward merge of the agent's branch: the branch commit is credited
// by content, and the merge commit, whose first-parent diff carries the same
// bytes, is not credited a second time.
func TestMergeCommitIsNotCreditedTwiceByContent(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	primary := filepath.Join(home, "repo")
	git, gitOut := gitRepoAt(t, primary)
	git("commit", "--allow-empty", "-m", "base")
	wt := filepath.Join(primary, ".worktrees", "ticket")
	git("worktree", "add", "-q", "-b", "ticket", wt)
	aiWrite(t, home, "repo/.worktrees/ticket/src/a.go", "package a\n", "sess-codex", false)
	wtGit, wtOut := gitIn(t, wt)
	wtGit("add", "-A")
	wtGit("commit", "-m", "agent work")
	branchSha := wtOut("rev-parse", "HEAD")
	git("worktree", "remove", wt)
	writeCommitFile(t, primary, "other.go", "package other\n")
	git("add", "other.go")
	git("commit", "-m", "main moves on")
	git("merge", "--no-ff", "-m", "merge", "ticket")

	if got, sess := attributionOf(t, primary, home, branchSha); got["src/a.go"] != attributionLikelyAI || sess != "sess-codex" {
		t.Fatalf("branch commit: src/a.go = %q, session %q; want likely_ai from sess-codex", got["src/a.go"], sess)
	}
	if got, sess := attributionOf(t, primary, home, gitOut("rev-parse", "HEAD")); got["src/a.go"] != attributionUnknown || sess != "" {
		t.Fatalf("merge commit: src/a.go = %q, session %q; want unknown — its lines were credited on the branch commit", got["src/a.go"], sess)
	}
}

func TestAiContentLedgerPrunesByAgeThenSlotThenCap(t *testing.T) {
	now := time.Now().UnixMilli()
	l := aiContentLedger{V: aiContentLedgerVersion, Entries: map[string]aiContentEntry{
		"stale": {SessionID: "s", TsMs: now - aiPathsTTL.Milliseconds() - 1, Slot: "x"},
	}}
	// One path saved past the per-path limit: only its newest versions stay.
	versions := aiContentVersionsKept + 3
	for i := 0; i < versions; i++ {
		l.Entries[fmt.Sprint("busy", i)] = aiContentEntry{SessionID: "s", TsMs: now - int64(i), Slot: "busy"}
	}
	pruneAiContent(&l, now)
	if _, ok := l.Entries["stale"]; ok {
		t.Fatal("an entry past the TTL survived")
	}
	for i := 0; i < versions; i++ {
		if _, ok := l.Entries[fmt.Sprint("busy", i)]; ok != (i < aiContentVersionsKept) {
			t.Fatalf("version %d kept = %v, want %v", i, ok, i < aiContentVersionsKept)
		}
	}
	// Distinct paths beyond the overall cap: the oldest goes.
	for i := 0; i <= aiContentMaxEntries; i++ {
		l.Entries[fmt.Sprint(i)] = aiContentEntry{SessionID: "s", TsMs: now - 10 - int64(i), Slot: fmt.Sprint(i)}
	}
	pruneAiContent(&l, now)
	if len(l.Entries) != aiContentMaxEntries {
		t.Fatalf("kept %d entries, want %d", len(l.Entries), aiContentMaxEntries)
	}
	if _, ok := l.Entries[fmt.Sprint(aiContentMaxEntries)]; ok {
		t.Fatal("the oldest entry survived the cap")
	}
}

func TestGitBlobOIDMatchesGit(t *testing.T) {
	// `git hash-object` of "hello\n".
	if got := gitBlobOID([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("gitBlobOID = %s", got)
	}
}

// The failure mark is local: projection drops it before anything is sent.
func TestToolFailedMarkIsNeverSent(t *testing.T) {
	ev := event.Event{Kind: "file_diff", SessionID: "s", Data: map[string]interface{}{
		"path": "a.go", "linesAdded": 1, normalize.ToolFailedKey: true,
	}}
	redact.ProjectEvent(&ev, false)
	d, _ := ev.Data.(map[string]interface{})
	if _, ok := d[normalize.ToolFailedKey]; ok {
		t.Fatalf("%s survived projection: %v", normalize.ToolFailedKey, d)
	}
	if d["linesAdded"] == nil {
		t.Fatalf("projection dropped an allowlisted field too, so this test checks nothing: %v", d)
	}
}
