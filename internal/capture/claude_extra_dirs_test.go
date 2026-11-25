package capture

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// extraClaudeRoot registers a second config root with a projects dir and
// returns that projects dir.
func extraClaudeRoot(t *testing.T) string {
	t.Helper()
	extra := t.TempDir()
	projects := filepath.Join(extra, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	if changed, err := AddExtraClaudeConfigDir(extra); err != nil || !changed {
		t.Fatalf("add = %v, %v", changed, err)
	}
	return filepath.Join(resolvePath(extra), "projects")
}

func writeTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An extra root's transcripts are walked beside the primary's and keyed by the
// same slug-relative identity, while every primary key stays what it was (an
// upgrade must not re-read the primary root from byte zero).
func TestExtraClaudeRootTranscriptsAreCandidatesWithSlugRelativeKeys(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	primary := claudeProjectsRoot(t)
	extra := extraClaudeRoot(t)

	primaryPath := filepath.Join(primary, "-Users-me-repo", "aaa.jsonl")
	extraPath := filepath.Join(extra, "-private-var-T-pr-babysit-1", "bbb.jsonl")
	sidechain := filepath.Join(extra, "-private-var-T-pr-babysit-1", "bbb", "subagents", "agent-1.jsonl")
	for _, p := range []string{primaryPath, extraPath, sidechain} {
		writeTranscript(t, p)
	}

	got := candidateClaudeTranscripts(time.Now().Add(-time.Hour))
	for _, want := range []string{primaryPath, extraPath, sidechain} {
		if !slices.Contains(got, want) {
			t.Errorf("candidates %v missing %s", got, want)
		}
	}
	for path, want := range map[string]string{
		primaryPath: "aaa.jsonl",
		extraPath:   "bbb.jsonl",
		sidechain:   "bbb/subagents/agent-1.jsonl",
		"aaa.jsonl": "aaa.jsonl", // an already-migrated key maps to itself
	} {
		if k := claudeProgressKey(path); k != want {
			t.Errorf("claudeProgressKey(%s) = %q, want %q", path, k, want)
		}
	}

	// Removing the root stops the walk; the primary is untouched.
	if changed, err := RemoveExtraClaudeConfigDir(filepath.Dir(extra)); err != nil || !changed {
		t.Fatalf("remove = %v, %v", changed, err)
	}
	got = candidateClaudeTranscripts(time.Now().Add(-time.Hour))
	if slices.Contains(got, extraPath) || !slices.Contains(got, primaryPath) {
		t.Errorf("after remove, candidates = %v", got)
	}
}

func TestAddExtraClaudeConfigDirRefusals(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	primary := filepath.Dir(claudeProjectsRoot(t))

	// The primary is accepted (the daemon's primary may differ from this
	// process's) but never walked twice.
	if _, err := AddExtraClaudeConfigDir(primary); err != nil {
		t.Errorf("adding the primary config dir: %v", err)
	}
	if _, err := AddExtraClaudeConfigDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("adding a dir that does not exist must be refused")
	}
	extra := t.TempDir()
	if changed, _ := AddExtraClaudeConfigDir(extra); !changed {
		t.Error("first add should change the list")
	}
	if changed, err := AddExtraClaudeConfigDir(extra); changed || err != nil {
		t.Errorf("second add = %v, %v; want a no-op", changed, err)
	}
	if roots := ClaudeConfigRoots(); len(roots) != 2 || roots[0] != primary {
		t.Errorf("roots = %v, want the primary then one extra", roots)
	}
	if changed, err := RemoveExtraClaudeConfigDir(filepath.Join(t.TempDir(), "never-added")); changed || err != nil {
		t.Errorf("removing an unknown dir = %v, %v; want a no-op", changed, err)
	}

	// Removable after it is deleted, by the path it was added under (a symlink
	// on macOS: TempDir lives under /var -> /private/var).
	if err := os.RemoveAll(extra); err != nil {
		t.Fatal(err)
	}
	if changed, err := RemoveExtraClaudeConfigDir(extra); !changed || err != nil {
		t.Errorf("removing a deleted dir = %v, %v; want it removed", changed, err)
	}

	// A corrupt file is refused, not overwritten.
	if err := os.WriteFile(claudeExtraDirsPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddExtraClaudeConfigDir(t.TempDir()); err == nil {
		t.Error("an edit over an unreadable file must fail")
	}
}

// After a hook takeover only the primary root is fast-forwarded: hooks never
// ran for an extra root, so its lines from that window are still owed.
func TestFastForwardSkipsExtraClaudeRoots(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	primary := claudeProjectsRoot(t)
	extra := extraClaudeRoot(t)
	primaryPath := filepath.Join(primary, "-ws", "aaa.jsonl")
	extraPath := filepath.Join(extra, "-ws", "bbb.jsonl")
	writeTranscript(t, primaryPath)
	writeTranscript(t, extraPath)
	p := loadClaudeWatchProgress()
	p.Match["aaa.jsonl"], p.Match["bbb.jsonl"] = "yes", "yes"
	saveClaudeWatchProgress(p)

	fastForwardClaudeTranscripts(t.TempDir(), time.Now().Add(-time.Hour))

	got := loadClaudeWatchProgress().Offsets
	if got["aaa.jsonl"] != 3 {
		t.Errorf("primary offset = %d, want EOF (3)", got["aaa.jsonl"])
	}
	if off, ok := got["bbb.jsonl"]; ok {
		t.Errorf("extra root offset = %d, want untouched", off)
	}
}

// A later walk with a shorter list (a concurrent walk after `claude-dirs
// remove`) must not drop a root an earlier walk's paths are still keyed under.
func TestWalkedExtraProjectsDirsOnlyGrow(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	rememberWalkedExtraProjectsDirs([]string{a, b})
	rememberWalkedExtraProjectsDirs([]string{b})
	got := walkedExtraProjectsDirs()
	if !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Fatalf("walked dirs = %v, want both %s and %s", got, a, b)
	}
	if key := claudeProgressKey(filepath.Join(a, "slug", "s.jsonl")); key != "s.jsonl" {
		t.Fatalf("key = %q, want s.jsonl", key)
	}
}
