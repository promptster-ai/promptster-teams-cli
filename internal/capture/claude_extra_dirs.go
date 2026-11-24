package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/promptster-ai/promptster-teams-cli/internal/sign"
	"github.com/promptster-ai/promptster-teams-cli/internal/state"
)

// Extra Claude config roots: other CLAUDE_CONFIG_DIRs on this machine whose
// transcripts are tailed like the primary one's. A tool that runs `claude -p`
// under a second account (CLAUDE_CONFIG_DIR=~/.config/<tool>/claude-config)
// writes its sessions there, and without this they were never read: the edits
// and commands those agents make left no AI evidence, so their commits read
// `no_ai_evidence`.
//
// Opt-in and persisted in the state dir, never an env var: the daemon runs
// under launchd with no shell environment, and it re-reads this file every
// poll, so `claude-dirs add` takes effect without a restart.
//
// WHAT AN EXTRA ROOT FEEDS, deliberately: transcript capture (and with it the
// AI-edit and bash-window ledgers commit attribution reads) plus the census
// transcript counts. NOT the statusline shim, plan tier, usage-limit hits or
// census repo discovery, which stay on the primary root (claudeConfigDir): a
// second account's plan and its 429s are that account's, and reporting them
// as this engineer's would raise plan alerts about a subscription they do not
// hold. Hooks are not installed there either, so while the watcher is
// degraded (hooks own emission) an extra root's sessions are not captured.
type claudeExtraDirsFile struct {
	Dirs []string `json:"dirs"`
}

func claudeExtraDirsPath() string {
	return filepath.Join(state.StateDir(), "claude-config-dirs.json")
}

// ExtraClaudeConfigDirs returns the registered extra config roots. A missing
// or unreadable file means none, which is exactly the behavior before it.
func ExtraClaudeConfigDirs() []string {
	data, err := os.ReadFile(claudeExtraDirsPath())
	if err != nil {
		return nil
	}
	var f claudeExtraDirsFile
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f.Dirs
}

// ClaudeConfigRoots is every config root whose transcripts are captured: the
// primary first, then each extra one that is not the primary itself.
func ClaudeConfigRoots() []string {
	primary := claudeConfigDir()
	roots := []string{primary}
	for _, d := range ExtraClaudeConfigDirs() {
		if d != "" && resolvePath(d) != resolvePath(primary) && !slices.Contains(roots, d) {
			roots = append(roots, d)
		}
	}
	return roots
}

// claudeProjectsDirs is ClaudeConfigRoots' transcript stores.
func claudeProjectsDirs() []string {
	roots := ClaudeConfigRoots()
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, filepath.Join(r, "projects"))
	}
	return out
}

// walkedExtra holds every extra projects dir a transcript walk in this process
// has used. claudeProgressKey keys an extra root's path against THIS list, not
// a fresh read of the file: a `claude-dirs remove` landing between a poll's
// walk and its keying would otherwise give that poll's paths their absolute
// path as a key, which has no offset, and the transcript would be read again
// from byte 0. It only grows: several goroutines walk (the poll, keepalive,
// reconstruction), and a later walk replacing the list could drop a root an
// earlier walk's paths are still being keyed against. A removed root's keys
// stay correct forever, since they are relative to that root's own dir.
var (
	walkedExtraMu sync.Mutex
	walkedExtra   []string
	walkedAny     bool
)

func rememberWalkedExtraProjectsDirs(dirs []string) {
	walkedExtraMu.Lock()
	defer walkedExtraMu.Unlock()
	walkedAny = true
	for _, d := range dirs {
		if !slices.Contains(walkedExtra, d) {
			walkedExtra = append(walkedExtra, d)
		}
	}
}

// walkedExtraProjectsDirs is every list a walk used, or the registered one
// when this process has not walked yet.
func walkedExtraProjectsDirs() []string {
	walkedExtraMu.Lock()
	if walkedAny {
		out := slices.Clone(walkedExtra)
		walkedExtraMu.Unlock()
		return out
	}
	walkedExtraMu.Unlock()
	return claudeProjectsDirs()[1:]
}

// AddExtraClaudeConfigDir registers dir (made absolute and symlink-resolved)
// and returns whether the list changed.
//
// The primary root is NOT refused here. Which root is primary depends on the
// process's CLAUDE_CONFIG_DIR, and the launchd daemon has none while this
// shell may: refusing by this shell's primary could block the one root the
// daemon never reads. Registering a root the daemon already reads is harmless,
// since ClaudeConfigRoots drops it there and its keys are the same either way.
func AddExtraClaudeConfigDir(dir string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	abs = resolvePath(abs)
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", abs)
	}
	return editExtraClaudeConfigDirs(func(dirs []string) []string {
		if slices.Contains(dirs, abs) {
			return dirs
		}
		return append(dirs, abs)
	})
}

// RemoveExtraClaudeConfigDir unregisters dir, matched as given or resolved.
// The directory may already be gone, so its parent is resolved too: add
// stored /private/tmp/x for /tmp/x, and a deleted x no longer resolves.
func RemoveExtraClaudeConfigDir(dir string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	forms := []string{abs, resolvePath(abs), filepath.Join(resolvePath(filepath.Dir(abs)), filepath.Base(abs))}
	return editExtraClaudeConfigDirs(func(dirs []string) []string {
		return slices.DeleteFunc(dirs, func(d string) bool { return slices.Contains(forms, d) })
	})
}

// editExtraClaudeConfigDirs is a locked read-modify-write with an atomic
// rename, so two concurrent edits cannot lose one another's entry and the
// daemon, reading mid-poll, never sees a half-written list.
func editExtraClaudeConfigDirs(edit func([]string) []string) (changed bool, err error) {
	path := claudeExtraDirsPath()
	err = sign.WithBufferLock(path+".lock", func() error {
		// Read strictly: a file that exists but can't be parsed must not be
		// overwritten by an edit, which would drop every root it held.
		var f claudeExtraDirsFile
		if data, err := os.ReadFile(path); err == nil { // #nosec G304 -- path is claudeExtraDirsPath(), StateDir-derived, not user input.
			if err := json.Unmarshal(data, &f); err != nil {
				return fmt.Errorf("%s is unreadable (%v); fix or delete it first", path, err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		before := f.Dirs
		after := edit(slices.Clone(before))
		if slices.Equal(before, after) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		data, err := json.Marshal(claudeExtraDirsFile{Dirs: after})
		if err != nil {
			return err
		}
		tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}
