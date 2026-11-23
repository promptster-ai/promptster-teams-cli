package capture

import (
	"fmt"
	"os"
	"path/filepath"
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
