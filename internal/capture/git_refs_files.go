package capture

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Reading refs from a repository's files instead of spawning git.
//
// git-watch asks four ref questions of every polled root, every poll: HEAD's
// commit (gitHead), HEAD's branch (gitSymbolicRef "HEAD"), the default branch's
// tip (durabilityDefaultTip), and every branch and remote tip (gitRefTips). Each
// was its own `git` process, so ~4 spawns per root per minute: on one laptop
// with ~105 live roots, ~420 processes a minute to learn what a few small files
// already say. checkoutHeadSig (the sibling-lineage memo key) reads the same way.
//
// For the "files" ref backend those answers are exactly: HEAD in the checkout's
// own git dir; a loose ref file under the common dir, which wins over its line
// in packed-refs; a loose file reading `ref: <name>` is a symbolic ref.
//
// ORDER MATTERS, and it is git's own: the loose file is read FIRST, and
// packed-refs only after it is found missing, freshly each time. `git pack-refs`
// writes the new packed-refs before deleting the loose files, so this order can
// never see a ref in neither place; the reverse order can.
//
// Anything this reader cannot answer exactly returns ok=false and the caller
// asks git as before: a reftable repository, a stray `.git`, a submodule or
// separate git dir (see checkoutGitDirs), an unreadable file, a malformed ref, a
// symbolic chain that does not end. "Does not exist" is reported, but no caller
// treats it as final: an unborn or vanished ref also falls back to git, so a race
// costs one spawn, never a wrong answer.

// fileRefs is one repository's refs as read from disk.
type fileRefs struct {
	gitdir, common string
}

// maxSymrefDepth matches git's own limit on following symbolic refs.
const maxSymrefDepth = 5

// openFileRefs prepares to read root's refs from files, or ok=false when root's
// layout is one it cannot vouch for.
func openFileRefs(root string) (fileRefs, bool) {
	gitdir, common, ok := checkoutGitDirs(root)
	if !ok {
		return fileRefs{}, false
	}
	if _, err := os.Stat(filepath.Join(common, "HEAD")); err != nil {
		return fileRefs{}, false // a stray .git with no repository in it
	}
	if _, err := os.Stat(filepath.Join(common, "reftable")); err == nil {
		return fileRefs{}, false
	}
	return fileRefs{gitdir: gitdir, common: common}, true
}

// packedRaw reads packed-refs now. A missing file is empty.
func (r fileRefs) packedRaw() ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(r.common, "packed-refs")) // #nosec G304 G703 -- the repository's own packed-refs; read locally, never emitted.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true
	}
	return data, err == nil
}

// packedRef finds one ref's line in packed-refs, read now, without building a
// map: the common question needs one line of a file that can be megabytes.
func (r fileRefs) packedRef(name string) (sha string, found, ok bool) {
	data, ok := r.packedRaw()
	if !ok {
		return "", false, false
	}
	for len(data) > 0 {
		line := data
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		sha, ref, cut := strings.Cut(strings.TrimSpace(string(line)), " ")
		if cut && ref == name {
			return sha, isFullObjectName(sha), isFullObjectName(sha)
		}
	}
	return "", false, true
}

// packedRefs reads all of packed-refs now, as name -> sha. A missing file is an
// empty set; anything malformed is ok=false.
func (r fileRefs) packedRefs() (map[string]string, bool) {
	data, ok := r.packedRaw()
	if !ok {
		return nil, false
	}
	packed := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue // header, or the peeled target of the tag above
		}
		sha, name, found := strings.Cut(line, " ")
		if !found || !isFullObjectName(sha) || !strings.HasPrefix(name, "refs/") {
			return nil, false
		}
		packed[name] = sha
	}
	return packed, true
}

// fileRefName reports whether name is a ref this reader resolves: a branch or
// remote-tracking ref (both live in the common dir) whose name git itself
// accepts (git's check_refname_format: no empty or dot-led component, no "..",
// no "@{", no ".lock" or "." ending, none of space ~ ^ : ? * [ \ or control
// characters). Per-worktree refs (refs/worktree/, refs/bisect/) are not.
func fileRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/remotes/") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part[0] == '.' || strings.HasSuffix(part, ".lock") {
			return false
		}
		for _, c := range part {
			if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
				return false
			}
		}
	}
	return true
}

// parseRefValue reads a ref file's content: a sha, or `ref: <name>`.
func parseRefValue(data []byte) (sha, symbolic string, ok bool) {
	v := strings.TrimSpace(string(data))
	if to, isSym := strings.CutPrefix(v, "ref: "); isSym {
		return "", strings.TrimSpace(to), true
	}
	return v, "", isFullObjectName(v)
}

// target reads a ref's own value without following it: a sha, or the name of
// the ref it points at (symbolic). exists=false means the ref was in neither
// place when read. ok=false means this reader cannot say.
func (r fileRefs) target(name string) (sha, symbolic string, exists, ok bool) {
	if !fileRefName(name) {
		return "", "", false, false
	}
	data, err := os.ReadFile(filepath.Join(r.common, filepath.FromSlash(name))) // #nosec G304 G703 -- a branch or remote ref (validated by fileRefName) under the repository's own git dir; read locally, never emitted.
	switch {
	case err == nil:
		sha, to, ok := parseRefValue(data)
		return sha, to, ok, ok
	case errors.Is(err, fs.ErrNotExist):
		sha, found, ok := r.packedRef(name) // read AFTER the loose miss; see ORDER MATTERS
		return sha, "", found, ok
	default:
		return "", "", false, false // a directory where the ref would be, permissions, ...
	}
}

// resolve follows name to a commit sha. exists=false (with ok) means the ref,
// or the end of its symbolic chain, was not found.
func (r fileRefs) resolve(name string) (sha string, exists, ok bool) {
	for range maxSymrefDepth {
		s, to, ex, k := r.target(name)
		if !k || !ex {
			return "", false, k
		}
		if to == "" {
			return s, true, true
		}
		name = to
	}
	return "", false, false
}

// head returns HEAD's branch (empty when detached) and the commit it resolves
// to (empty when the branch was not found: unborn, or mid-update).
func (r fileRefs) head() (branch, sha string, ok bool) {
	data, err := os.ReadFile(filepath.Join(r.gitdir, "HEAD")) // #nosec G304 G703 -- the checkout's own HEAD file; read locally, never emitted.
	if err != nil {
		return "", "", false
	}
	sha, branch, ok = parseRefValue(data)
	if !ok || branch == "" {
		return "", sha, ok
	}
	sha, exists, ok := r.resolve(branch)
	if !ok {
		return "", "", false
	}
	if !exists {
		return branch, "", true
	}
	return branch, sha, true
}

// tips returns the commit of every branch and remote-tracking ref, as
// `git rev-parse --branches --remotes` lists them: symbolic refs resolved to
// their target's commit, dangling ones and names git rejects skipped. Unsorted,
// with duplicates.
func (r fileRefs) tips() ([]string, bool) {
	values := map[string]string{}   // loose ref -> sha
	symbolic := map[string]string{} // loose symbolic ref -> target name
	for _, top := range []string{"heads", "remotes"} {
		base := filepath.Join(r.common, "refs", top)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == base && errors.Is(err, fs.ErrNotExist) {
					return filepath.SkipDir
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(r.common, p)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)
			if !fileRefName(name) {
				return nil // git ignores a ref with a broken name, or an update's .lock
			}
			data, err := os.ReadFile(p) // #nosec G304 G703 -- a loose ref under the repository's own refs/; read locally, never emitted.
			if errors.Is(err, fs.ErrNotExist) {
				return nil // packed or deleted mid-walk; packed-refs is read after
			}
			if err != nil {
				return err
			}
			sha, to, ok := parseRefValue(data)
			if !ok {
				return errors.New("malformed ref")
			}
			if to != "" {
				symbolic[name] = to
			} else {
				values[name] = sha
			}
			return nil
		})
		if err != nil {
			return nil, false
		}
	}
	// AFTER the walk (see ORDER MATTERS): a ref packed during the walk is here.
	packed, ok := r.packedRefs()
	if !ok {
		return nil, false
	}
	lookup := func(name string) (sha, to string, found bool) {
		if to, ok := symbolic[name]; ok {
			return "", to, true
		}
		if sha, ok := values[name]; ok {
			return sha, "", true
		}
		sha, found = packed[name]
		return sha, "", found
	}
	var out []string
	unsure := false
	add := func(name string) {
		for range maxSymrefDepth {
			sha, to, found := lookup(name)
			if !found {
				// Dangling, and git skips it too, but only if name is somewhere
				// this walk looked. A target elsewhere (a loose tag) is not
				// something it can rule out, so git decides.
				if !fileRefName(name) {
					unsure = true
				}
				return
			}
			if to == "" {
				out = append(out, sha)
				return
			}
			name = to
		}
		unsure = true // a chain git would reject or resolve differently
	}
	for name := range values {
		add(name)
	}
	for name := range symbolic {
		add(name)
	}
	for name := range packed {
		_, loose := values[name]
		_, looseSym := symbolic[name]
		if !loose && !looseSym && fileRefName(name) {
			add(name)
		}
	}
	if unsure {
		return nil, false
	}
	return out, true
}
