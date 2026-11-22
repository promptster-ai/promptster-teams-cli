package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptster-ai/promptster-teams-cli/internal/redact"
)

// classifyUnattributed: every evidence state maps to its own reason, and a bash
// miss reports which side of the nearest window the file's mtime fell on.
func TestClassifyUnattributed(t *testing.T) {
	root := t.TempDir()
	writeCommitFile(t, root, "a.go", "package a\n")
	mtime := time.UnixMilli(1_800_000_000_000)
	if err := os.Chtimes(filepath.Join(root, "a.go"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	m := mtime.UnixMilli()
	here := resolvePath(root)
	win := func(start, end int64, roots ...string) bashWindow {
		return bashWindow{SessionID: "s", StartMs: start, EndMs: end, Roots: roots}
	}
	// The explicit-repo scope (root == workspace) and the daemon's (workspace is
	// HOME, this repo at "repo/", a sibling worktree of it at /sib).
	own := ledgerScope{root: here}
	daemon := ledgerScope{prefix: "repo", root: here, alts: []ledgerScope{{prefix: "sib", root: "/sib"}}}
	cases := []struct {
		name    string
		scope   ledgerScope
		paths   []string
		aiPaths map[string]string
		windows []bashWindow
		want    unattributedDiag
	}{
		{"nothing recorded", own, []string{"a.go"}, nil, nil,
			unattributedDiag{Reason: unattributedNoAIEvidence}},
		{"AI edited other files of this repo", own, []string{"a.go"}, map[string]string{"b.go": "s"}, nil,
			unattributedDiag{Reason: unattributedPathsNotLedger}},
		{"daemon: evidence only from other repos is no evidence", daemon, []string{"a.go"},
			map[string]string{"other/b.go": "s"}, []bashWindow{win(m, m, "/other"), win(m, m, bashNoCheckout)},
			unattributedDiag{Reason: unattributedNoAIEvidence}},
		{"daemon: AI edited another file of this repo", daemon, []string{"a.go"}, map[string]string{"repo/b.go": "s"}, nil,
			unattributedDiag{Reason: unattributedPathsNotLedger}},
		{"daemon: windows only from a sibling worktree", daemon, []string{"a.go"}, nil, []bashWindow{win(m, m, "/sib")},
			unattributedDiag{Reason: unattributedOtherCheckout}},
		{"committed file gone from the tree", own, []string{"gone.go"}, nil, []bashWindow{win(m, m, here)},
			unattributedDiag{Reason: unattributedFileMissing}},
		{"window ended 10s before the write", own, []string{"a.go"}, nil, []bashWindow{win(m-12_000, m-10_000, here), win(m-60_000, m-60_000)},
			unattributedDiag{Reason: unattributedBashMissLate, GapMs: 10_000, HasGap: true}},
		{"window began 5s after the write", own, []string{"a.go"}, nil, []bashWindow{win(m+5_000, m+5_000, here)},
			unattributedDiag{Reason: unattributedBashMissEarly, GapMs: 5_000, HasGap: true}},
		{"a window within tolerance is not a miss", own, []string{"a.go"}, nil, []bashWindow{win(m+1_000, m+1_000, here)},
			unattributedDiag{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyUnattributed(root, c.scope, c.paths, c.aiPaths, c.windows); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// End to end through the real builder and the redaction projector: an
// all-unknown commit ships its reason; an attributed one ships none.
func TestCommitAttributionCarriesUnattributedReason(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	ws, git, gitOut := gitRepo(t)

	writeCommitFile(t, ws, "hand.go", "package main\n\nvar x = 1\n")
	git("add", "-A")
	git("commit", "-m", "no AI evidence")
	sha := gitOut("rev-parse", "HEAD")

	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha, siblingLineage{})
	if !ok {
		t.Fatal("expected an emittable event")
	}
	redact.ProjectEvent(&ev, false)
	data := ev.Data.(map[string]interface{})
	if data["unattributedReason"] != unattributedNoAIEvidence {
		t.Errorf("unattributedReason = %v, want %q", data["unattributedReason"], unattributedNoAIEvidence)
	}
	if _, has := data["bashWindowGapMs"]; has {
		t.Errorf("bashWindowGapMs present without a bash miss: %v", data["bashWindowGapMs"])
	}

	// The same shape with AI evidence for the file is attributed: no reason.
	writeCommitFile(t, ws, "ai.go", "package main\n\nvar y = 2\n")
	recordAiTouchedPath("sess-ai", gitWatchRootKey(ws), "ai.go")
	git("add", "-A")
	git("commit", "-m", "AI edit")
	sha2 := gitOut("rev-parse", "HEAD")
	ev2, ok := buildCommitAttributionEvent(Session{DeviceID: "dev-x", TaskRoot: ws}, ws, sha2, siblingLineage{})
	if !ok {
		t.Fatal("expected an emittable event")
	}
	if _, has := ev2.Data.(map[string]interface{})["unattributedReason"]; has {
		t.Error("an attributed commit must not carry unattributedReason")
	}
}
