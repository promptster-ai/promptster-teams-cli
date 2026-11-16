package selfupdate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// pnpm 10's global virtual store.
//
// A global install there is a SYMLINK:
//
//	<PNPM_HOME>/global/v11/<id>/node_modules/@promptster/teams-cli
//	  -> <storeDir>/v11/links/@promptster/teams-cli/<ver>/<hash>/node_modules/@promptster/teams-cli
//
// and its platform binary sits in a sibling package that the teams-cli
// package's node_modules links to. A running binary only ever sees the resolved
// store path, which carries no "global" segment, so the layout rules in
// isProjectLocalInstall / nudgeFor read it as a project-local install. Project
// installs can resolve into the same store, so the path cannot decide. It is
// global only if a pnpm global directory actually links to this copy.
//
// Mirrors npm/lib/resolve.js linkedFromPnpmGlobal. Any filesystem error reads
// as "not linked", the old (project-local) behaviour.

var storeLinkRe = regexp.MustCompile(`[/\\]v\d+[/\\]links[/\\]`)

// isPnpmStoreLinkPath reports whether segs contain <storeDir>/v<N>/links.
func isPnpmStoreLinkPath(segs []string) bool {
	for i := 0; i+1 < len(segs); i++ {
		if len(segs[i]) > 1 && segs[i][0] == 'v' && allDigits(segs[i][1:]) && segs[i+1] == "links" {
			return true
		}
	}
	return false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// packageRoot returns the directory of the package containing p: the path up
// to the package name after the LAST node_modules segment (two segments for a
// scoped name), or "" when p is not inside a package.
func packageRoot(p string) string {
	norm := strings.ReplaceAll(p, `\`, "/")
	const marker = "/node_modules/"
	at := strings.LastIndex(norm, marker)
	if at < 0 {
		return ""
	}
	rest := strings.Split(norm[at+len(marker):], "/")
	take := 1
	if strings.HasPrefix(rest[0], "@") {
		take = 2
	}
	if len(rest) < take || rest[take-1] == "" {
		return ""
	}
	return p[:at+len(marker)+len(strings.Join(rest[:take], "/"))]
}

// pnpmGlobalDirs lists where pnpm keeps global installs: $PNPM_HOME/global, and
// the "global" directory beside the store p resolved into (the default store is
// <PNPM_HOME>/store, which covers an unset PNPM_HOME).
func pnpmGlobalDirs(p string) []string {
	var dirs []string
	if home := os.Getenv("PNPM_HOME"); home != "" {
		dirs = append(dirs, filepath.Join(home, "global"))
	}
	if loc := storeLinkRe.FindStringIndex(p); loc != nil {
		d := filepath.Join(filepath.Dir(p[:loc[0]]), "global")
		if len(dirs) == 0 || dirs[0] != d {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// linkedFromPnpmGlobal reports whether a pnpm global install links to the
// package containing self, either directly or as a dependency of a globally
// installed package (the platform-binary package is a sibling in the global
// package's own node_modules).
func linkedFromPnpmGlobal(self string) bool {
	root := packageRoot(self)
	if root == "" {
		return false
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	base := filepath.Base(root)
	scope := filepath.Base(filepath.Dir(root))
	if !strings.HasPrefix(scope, "@") {
		scope = ""
	}
	for _, g := range pnpmGlobalDirs(self) {
		versions, err := os.ReadDir(g)
		if err != nil {
			continue
		}
		for _, v := range versions {
			ids, err := os.ReadDir(filepath.Join(g, v.Name()))
			if err != nil {
				continue
			}
			for _, id := range ids {
				nm := filepath.Join(g, v.Name(), id.Name(), "node_modules", scope)
				entries, err := os.ReadDir(nm)
				if err != nil {
					continue
				}
				for _, e := range entries {
					real, err := filepath.EvalSymlinks(filepath.Join(nm, e.Name()))
					if err != nil {
						continue
					}
					if real == rootReal {
						return true
					}
					// A dependency of the global package: <its node_modules>/<scope>/<base>.
					dep, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(real), base))
					if err == nil && dep == rootReal {
						return true
					}
				}
			}
		}
	}
	return false
}

// isPnpmGlobalStoreInstall is the one predicate the layout rules call.
func isPnpmGlobalStoreInstall(self string, segs []string) bool {
	return isPnpmStoreLinkPath(segs) && linkedFromPnpmGlobal(self)
}
