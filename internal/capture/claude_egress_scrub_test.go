package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptster-ai/promptster-teams-cli/internal/normalize"
	"github.com/promptster-ai/promptster-teams-cli/internal/redact"
)

// The raw pre-parse pass skips Titus (redact.RedactRawBytes); the egress scrub
// at the ledger choke point does not. A secret only Titus recognizes must
// therefore still be gone from everything that can leave the machine: the
// signed ledger and the send queue.
func TestTitusOnlySecretIsScrubbedAtEgress(t *testing.T) {
	root := claudeProjectsRoot(t)
	stateDir := t.TempDir()
	buffer := filepath.Join(stateDir, "buffer.jsonl")
	outboxPath := filepath.Join(stateDir, "outbox.jsonl")
	t.Setenv("PROMPTSTER_STATE_DIR", stateDir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", buffer)
	t.Setenv("PROMPTSTER_OUTBOX_PATH", outboxPath)

	// A Shopify access token: no stage-1 or generic rule knows the shape.
	secret := "shpat_" + "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	if string(redact.RedactBytes([]byte(secret))) == secret {
		t.Skip("titus scanner unavailable")
	}
	workspace := t.TempDir()
	now := time.Now().UTC()
	line := fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"%s","uuid":"u-1","message":{"role":"user","content":"send it with %s please"}}`+"\n",
		resolvePath(workspace), now.Format(time.RFC3339), secret)
	dir := filepath.Join(root, "-Users-me-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "egress-sess.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	session := Session{DeviceID: "dev-egress", SessionToken: "PSE-TEST", TaskRoot: workspace, StartedAt: now}
	parsed, _ := pollClaudeTranscripts(session, resolvePath(workspace), transcriptHistoryCutoff(now), map[string]*normalize.ClaudeTranscriptProcessor{}, false, false)
	if parsed != 1 {
		t.Fatalf("parsed %d events, want 1", parsed)
	}

	ledger, err := os.ReadFile(buffer)
	if err != nil || len(ledger) == 0 {
		t.Fatalf("ledger not written: %v", err)
	}
	if strings.Contains(string(ledger), secret) {
		t.Fatalf("Titus-only secret reached the signed ledger: %s", ledger)
	}
	if !strings.Contains(string(ledger), "[REDACTED:") {
		t.Fatalf("egress scrub left no Titus marker: %s", ledger)
	}
	queued, _ := os.ReadFile(outboxPath)
	if strings.Contains(string(queued), secret) {
		t.Fatalf("Titus-only secret reached the send queue: %s", queued)
	}
}
