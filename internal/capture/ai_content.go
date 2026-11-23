package capture

import (
	"crypto/sha1" // #nosec G505 -- git's own blob object name, compared with what git prints; not a security hash.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/promptster-ai/promptster-teams-cli/internal/event"
	"github.com/promptster-ai/promptster-teams-cli/internal/normalize"
	"github.com/promptster-ai/promptster-teams-cli/internal/sign"
	"github.com/promptster-ai/promptster-teams-cli/internal/state"
)

// The ai-content ledger remembers WHAT an agent wrote, not just where: the git
// blob name of a file's bytes right after an AI write, keyed by the repository
// (its common git dir, shared by every worktree) and the repo-relative path.
//
// It closes the gap the ai-paths ledger leaves by design. That ledger matches a
// committed path to the checkout the agent edited in, and only while that checkout
// still holds the commit — so an agent that works in a throwaway worktree, commits,
// and removes it (or whose branch is squashed into a new SHA, or first seen after a
// pull into the main checkout) reads `unknown`. A committed blob that is byte-for-byte
// what an agent wrote to the same path of the same repository is that agent's work
// whichever checkout, branch or SHA carried it; a human's different bytes never
// match, which is the line the lineage gate exists to hold.
//
// Local only: nothing here is ever emitted. Known under-counts, never inventions:
// bytes changed after the edit (a formatter or pre-commit hook), a relative path
// (Codex's exec-wrapped apply_patch), a SHA-256 repository (the name is computed
// as SHA-1), and a file git stores through a clean filter (autocrlf, LFS). Known
// residual: a commit that restores bytes an agent itself once wrote (a revert to
// them) is credited to that agent.
func aiContentLedgerPath() string { return filepath.Join(state.StateDir(), "ai-content.json") }

const (
	aiContentLedgerVersion = 1
	// aiContentVersionsKept is how many recent versions of one path one session
	// keeps: enough for the commits an agent makes along the way, without its
	// every intermediate save evicting other work's final bytes.
	aiContentVersionsKept = 16
	// aiContentMaxEntries bounds the file a busy device rewrites on every edit.
	aiContentMaxEntries = 4096
	// aiContentMtimeSlackMs: a file modified more than this after the edit event
	// was changed again since — possibly by a person — so its bytes are not recorded.
	aiContentMtimeSlackMs = 2000
	// aiContentWrittenBeforeMs: the write lands just before the event that
	// reports it; a file last modified earlier than this was not written by it.
	aiContentWrittenBeforeMs = 5000
	// aiContentMaxFileBytes skips generated blobs and fixtures: read inline in
	// the watcher, and not what this ledger is for.
	aiContentMaxFileBytes = 4 << 20
)

type aiContentLedger struct {
	V       int                       `json:"v"`
	Entries map[string]aiContentEntry `json:"entries"`
}

// aiContentEntry is filed under aiContentKey; Slot names its (repository, path,
// session) so the per-path version cap can find its siblings, and Repo its
// repository, so a commit in a repository with no entry spawns no git.
type aiContentEntry struct {
	SessionID string `json:"s"`
	TsMs      int64  `json:"t"`
	Slot      string `json:"p"`
	Repo      string `json:"r"`
}

func aiContentRepoTag(common string) string { return shortHash(common)[:12] }

func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func aiContentKey(common, repoRel, oid string) string { return shortHash(common, repoRel, oid) }

// gitBlobOID is the object name git gives these bytes as a blob (SHA-1 format).
func gitBlobOID(data []byte) string {
	h := sha1.New() // #nosec G401 -- git object naming, not a security use.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// repoCommonDir resolves a checkout to its repository's identity: the common git
// dir every worktree of it shares. Stat and file reads only, no git spawn.
func repoCommonDir(checkout string) (string, bool) {
	_, common, ok := checkoutGitDirs(checkout)
	if !ok {
		return "", false
	}
	return resolvePath(common), true
}

// recordAiContentFromEvent records the bytes a LIVE likely_ai file_diff left on
// disk. Only the Claude and Codex funnels call it, before paths are relativized:
// their events carry the record's own time, stamped just after the write (the
// tool_result line, patch_apply_end), and a path the tool reported absolute. A
// relative path (a wrapped apply_patch, relative to a cwd the event doesn't
// carry) is skipped rather than guessed. Cursor stamps a whole turn with its
// start, and its hook rail runs under a 2s budget.
//
// The bytes must be the edit's own result: the tool did not report failure (a
// failed or denied edit still reaches here as a likely_ai file_diff, marked
// normalize.ToolFailedKey), a Write's file equals its content, and every line
// an Edit's diff adds is in the file (a person's save over it fails that). The
// file must also have been written around the event (mtime within
// [ev-aiContentWrittenBeforeMs, ev+aiContentMtimeSlackMs]) and not changed
// while it was read.
func recordAiContentFromEvent(e *event.Event, replay bool) {
	if replay || e == nil || e.Kind != "file_diff" || e.Provenance == nil || e.Provenance.Attribution != "likely_ai" {
		return
	}
	data, _ := e.Data.(map[string]interface{})
	abs, _ := data["path"].(string)
	evMs, ok := eventTsMs(e.Ts)
	if !ok || !filepath.IsAbs(abs) {
		return
	}
	before, err := os.Stat(abs)
	if err != nil || !before.Mode().IsRegular() || before.Size() > aiContentMaxFileBytes {
		return
	}
	mtime := before.ModTime().UnixMilli()
	if mtime < evMs-aiContentWrittenBeforeMs || mtime > evMs+aiContentMtimeSlackMs {
		return
	}
	content, err := os.ReadFile(abs) // #nosec G304 -- a file the capture session just saw an agent write; hashed locally, never emitted.
	if err != nil {
		return
	}
	after, err := os.Stat(abs)
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != int64(len(content)) {
		return
	}
	if failed, _ := data[normalize.ToolFailedKey].(bool); failed || !aiEditLanded(data, content) {
		return
	}
	recordAiContent(e.SessionID, abs, content, evMs)
}

// aiEditLanded reports whether content holds the edit's own result: exactly a
// Write's content, or every line an Edit's unified diff adds. An edit that adds
// no line (a pure deletion) proves nothing and is not recorded.
func aiEditLanded(data map[string]interface{}, content []byte) bool {
	if written, ok := data["content"].(string); ok {
		return written == string(content)
	}
	diff, _ := data["diff"].(string)
	have := map[string]bool{}
	for _, line := range strings.Split(string(content), "\n") {
		have[strings.TrimSuffix(line, "\r")] = true
	}
	added := 0
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		added++
		if !have[strings.TrimSuffix(line[1:], "\r")] {
			return false
		}
	}
	return added > 0
}

// recordAiContent stores the blob name of data, written by an agent to absPath,
// under that file's repository. Best-effort, like every capture ledger.
func recordAiContent(sessionID, absPath string, data []byte, writeMs int64) {
	if sessionID == "" || !filepath.IsAbs(absPath) {
		return
	}
	root, ok := gitRootOf(filepath.Dir(absPath))
	if !ok {
		return
	}
	common, ok := repoCommonDir(root)
	if !ok {
		return
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	rel = filepath.ToSlash(rel)
	key := aiContentKey(common, rel, gitBlobOID(data))
	slot := shortHash(common, rel, sessionID)
	_ = sign.WithBufferLock(aiContentLedgerPath()+".lock", func() error {
		ledger := readAiContentLedger()
		ledger.Entries[key] = aiContentEntry{SessionID: sessionID, TsMs: writeMs, Slot: slot, Repo: aiContentRepoTag(common)}
		pruneAiContent(&ledger, time.Now().UnixMilli())
		out, err := json.Marshal(ledger)
		if err != nil {
			return err
		}
		tmp := aiContentLedgerPath() + ".tmp"
		if err := os.WriteFile(tmp, out, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, aiContentLedgerPath())
	})
}

var aiContentCache struct {
	sync.Mutex
	path    string
	info    os.FileInfo
	mtime   time.Time
	size    int64
	entries map[string]aiContentEntry
}

// cachedAiContentEntries is the ledger's entries for READING ONLY, parsed again
// only when the file changes: a poll reconciles many commits against one ledger.
func cachedAiContentEntries() map[string]aiContentEntry {
	path := aiContentLedgerPath()
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	aiContentCache.Lock()
	defer aiContentCache.Unlock()
	c := &aiContentCache
	// Every write renames a new file into place, so SameFile catches a rewrite
	// that left size and a coarse mtime unchanged.
	if c.entries == nil || c.path != path || !os.SameFile(info, c.info) || !info.ModTime().Equal(c.mtime) || info.Size() != c.size {
		c.entries = readAiContentLedger().Entries
		c.path, c.info, c.mtime, c.size = path, info, info.ModTime(), info.Size()
	}
	return aiContentCache.entries
}

// readAiContentLedger returns the ledger, or an empty one when it is missing,
// unreadable or from another version.
func readAiContentLedger() aiContentLedger {
	ledger := aiContentLedger{}
	if data, err := os.ReadFile(aiContentLedgerPath()); err == nil {
		_ = json.Unmarshal(data, &ledger)
	}
	if ledger.V != aiContentLedgerVersion || ledger.Entries == nil {
		ledger = aiContentLedger{V: aiContentLedgerVersion, Entries: map[string]aiContentEntry{}}
	}
	return ledger
}

// pruneAiContent drops entries past the ai-paths TTL, then all but the newest
// aiContentVersionsKept of each slot, then the oldest beyond the overall cap.
func pruneAiContent(ledger *aiContentLedger, nowMs int64) {
	keys := make([]string, 0, len(ledger.Entries))
	for k, e := range ledger.Entries {
		if nowMs-e.TsMs > aiPathsTTL.Milliseconds() {
			delete(ledger.Entries, k)
			continue
		}
		keys = append(keys, k)
	}
	// Newest first, deterministic on ties.
	sort.Slice(keys, func(i, j int) bool {
		a, b := ledger.Entries[keys[i]], ledger.Entries[keys[j]]
		return a.TsMs > b.TsMs || (a.TsMs == b.TsMs && keys[i] < keys[j])
	})
	perSlot := map[string]int{}
	kept := 0
	for _, k := range keys {
		slot := ledger.Entries[k].Slot
		perSlot[slot]++
		if perSlot[slot] > aiContentVersionsKept || kept >= aiContentMaxEntries {
			delete(ledger.Entries, k)
			continue
		}
		kept++
	}
}

// aiContentMatcher answers, for one commit, which AI session wrote a committed
// file's exact bytes. It reads the ledger and the commit's blob names once, on
// the first file the other passes left unknown, and never when the ledger is
// empty. A nil matcher matches nothing.
type aiContentMatcher struct {
	root, sha string
	loaded    bool
	common    string
	oids      map[string]string
	entries   map[string]aiContentEntry
}

func (m *aiContentMatcher) session(path string) (string, bool) {
	if m == nil {
		return "", false
	}
	if !m.loaded {
		m.loaded = true
		m.entries = cachedAiContentEntries()
		if common, ok := repoCommonDir(m.root); ok && aiContentHasRepo(m.entries, common) {
			m.common, m.oids = common, commitBlobOIDs(m.root, m.sha)
		}
	}
	oid, ok := m.oids[path]
	if !ok {
		return "", false
	}
	e, ok := m.entries[aiContentKey(m.common, path, oid)]
	if !ok || e.SessionID == "" || time.Now().UnixMilli()-e.TsMs > aiPathsTTL.Milliseconds() {
		return "", false
	}
	return e.SessionID, true
}

func aiContentHasRepo(entries map[string]aiContentEntry, common string) bool {
	tag := aiContentRepoTag(common)
	for _, e := range entries {
		if e.Repo == tag {
			return true
		}
	}
	return false
}

// commitBlobOIDs maps each file the commit adds or changes to its new blob name.
// A merge yields nothing (diff-tree without -m prints no diff for one): its
// files came from commits that are credited on their own, so crediting the
// merge too would count the same agent lines twice.
func commitBlobOIDs(root, sha string) map[string]string {
	// #nosec G204 -- constant flags; root is a polled checkout and sha came from git rev-list. Read-only.
	out, err := exec.Command("git", "-C", root, "diff-tree", "-r", "-z", "--root",
		"--no-commit-id", "--no-renames", sha).Output()
	if err != nil {
		return nil
	}
	oids := map[string]string{}
	// -z records: ":<mode> <mode> <old> <new> <status>\0<path>\0", one path each
	// with --no-renames.
	parts := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		fields := strings.Fields(parts[i])
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") || strings.HasPrefix(fields[4], "D") {
			continue
		}
		oids[parts[i+1]] = fields[3]
	}
	return oids
}
