package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptster-ai/promptster-teams-cli/internal/normalize"
)

// TestPollSkipsIdleTranscriptButReadsNewHistory pins the idle skip: a matched
// transcript already read to EOF is not opened again until it grows, while one
// the watcher has never tailed (a fresh install's backfill) is read whole in the
// same poll.
func TestPollSkipsIdleTranscriptButReadsNewHistory(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	now := time.Now().UTC()
	line := func(content string, ts time.Time) string {
		return fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"%s"}}`+"\n",
			resolvePath(workspace), ts.Format(time.RFC3339), content)
	}
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Idle: matched and read to EOF on an earlier run.
	idle := filepath.Join(dir, "idle-sess.jsonl")
	first := line("already read", now.Add(-time.Hour))
	if err := os.WriteFile(idle, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	// Never tailed: in-window history a new install must backfill.
	fresh := filepath.Join(dir, "fresh-sess.jsonl")
	if err := os.WriteFile(fresh, []byte(line("one", now.Add(-3*24*time.Hour))+line("two", now.Add(-3*24*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}

	idleKey := claudeProgressKey(idle)
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{idleKey: int64(len(first))},
		Match:   map[string]string{idleKey: "yes"},
		V:       claudeProgressSchemaV,
	})

	session := Session{DeviceID: "sess-idle", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: now}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{}
	cutoff := transcriptHistoryCutoff(now)

	parsed, _ := pollClaudeTranscripts(session, resolvePath(workspace), cutoff, processors, true, false)
	if parsed != 2 {
		t.Fatalf("never-tailed history parsed %d events, want 2 (a new install must read it all)", parsed)
	}
	if _, built := processors[idleKey]; built {
		t.Fatal("an idle transcript at its offset must be skipped before a processor is built")
	}
	if got := loadClaudeWatchProgress().Offsets[idleKey]; got != int64(len(first)) {
		t.Fatalf("idle offset moved to %d, want %d", got, len(first))
	}

	// Once it grows, it is read again: only the new line.
	f, err := os.OpenFile(idle, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	next := line("new prompt", now)
	if _, err := f.WriteString(next); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	parsed, _ = pollClaudeTranscripts(session, resolvePath(workspace), cutoff, processors, true, false)
	if parsed != 1 {
		t.Fatalf("grown transcript parsed %d events, want 1", parsed)
	}
	if got := loadClaudeWatchProgress().Offsets[idleKey]; got != int64(len(first)+len(next)) {
		t.Fatalf("offset after growth = %d, want %d", got, len(first)+len(next))
	}
}

// TestPollStillVisitsIdleTranscriptHoldingBufferedState pins the exception: a
// transcript at its offset whose processor holds an unfinished message is not
// skipped, so its context window is refreshed before the stale flush mints it.
func TestPollStillVisitsIdleTranscriptHoldingBufferedState(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "3f1d2c4b-0000-4000-8000-000000000001"
	path := filepath.Join(dir, sessionID+".jsonl")
	toolUse := `{"type":"assistant","requestId":"req-1","message":{"id":"msg-1","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}],"usage":{"input_tokens":1,"output_tokens":1}},"timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(path, []byte(toolUse), 0o600); err != nil {
		t.Fatal(err)
	}

	key := claudeProgressKey(path)
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{key: int64(len(toolUse))},
		Match:   map[string]string{key: "yes"},
		RootsFP: captureRootsFingerprint(workspaceMatchRoots(resolvePath(workspace))),
		V:       claudeProgressSchemaV,
	})
	proc := newClaudeProcessorForPath(path)
	proc.Process([]byte(toolUse))
	if !proc.HasBufferedState() {
		t.Fatal("setup: the tool call should leave the processor holding state")
	}
	processors := map[string]*normalize.ClaudeTranscriptProcessor{key: proc}

	if err := writeClaudeContextSpool(sessionID, claudeContextSpool{ContextWindowTokens: 200_000, ObservedAt: 10}); err != nil {
		t.Fatal(err)
	}

	session := Session{DeviceID: "sess-buf", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: time.Now()}
	pollClaudeTranscripts(session, resolvePath(workspace), session.StartedAt.Add(-time.Hour), processors, true, false)
	if proc.ContextWindow.Tokens != 200_000 {
		t.Fatalf("context window = %d; a processor holding state must still be refreshed while idle", proc.ContextWindow.Tokens)
	}
}

// TestPollReadsGrowthThroughSymlinkedTranscript: the walk's Lstat reports a
// symlink's own length, so it must never be taken as the transcript's size.
func TestPollReadsGrowthThroughSymlinkedTranscript(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_OUTBOX_PATH", filepath.Join(stateDir, "outbox.jsonl"))

	workspace := t.TempDir()
	now := time.Now().UTC()
	line := func(content string) string {
		return fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"%s"}}`+"\n",
			resolvePath(workspace), now.Format(time.RFC3339), content)
	}
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real.jsonl")
	// Longer than the link's own path, so an Lstat size would sit below the offset.
	first := line(strings.Repeat("x", 400))
	if err := os.WriteFile(target, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked-sess.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	key := claudeProgressKey(link)
	saveClaudeWatchProgress(claudeWatchProgress{
		Offsets: map[string]int64{key: int64(len(first))},
		Match:   map[string]string{key: "yes"},
		V:       claudeProgressSchemaV,
	})

	f, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line("after the link")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	session := Session{DeviceID: "sess-link", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: now}
	parsed, _ := pollClaudeTranscripts(session, resolvePath(workspace), transcriptHistoryCutoff(now), map[string]*normalize.ClaudeTranscriptProcessor{}, true, false)
	if parsed != 1 {
		t.Fatalf("symlinked transcript parsed %d events after growing, want 1", parsed)
	}
}
