package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// pnpmLayout builds pnpm 10's global virtual store on disk: the teams-cli
// package in the store, its platform-binary package beside it, the dependency
// link between them, and (optionally) the global install's symlink.
func pnpmLayout(t *testing.T, linkGlobal bool) (bin string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink layout")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PNPM_HOME", "")
	links := filepath.Join(home, "pnpm", "store", "v11", "links", "@promptster")
	cli := filepath.Join(links, "teams-cli", "0.41.3", "aa77", "node_modules", "@promptster", "teams-cli")
	plat := filepath.Join(links, "teams-cli-darwin-arm64", "0.41.3", "e24a", "node_modules", "@promptster", "teams-cli-darwin-arm64")
	for _, d := range []string{cli, plat} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin = filepath.Join(plat, "promptster-teams")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// teams-cli's own node_modules links its platform dependency.
	if err := os.Symlink(plat, filepath.Join(filepath.Dir(cli), "teams-cli-darwin-arm64")); err != nil {
		t.Fatal(err)
	}
	if linkGlobal {
		g := filepath.Join(home, "pnpm", "global", "v11", "3265-0", "node_modules", "@promptster")
		if err := os.MkdirAll(g, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cli, filepath.Join(g, "teams-cli")); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func TestPnpmGlobalStoreInstallIsGlobal(t *testing.T) {
	bin := pnpmLayout(t, true)
	if isProjectLocalInstall(bin) {
		t.Fatalf("pnpm 10 global install %q misclassified as project-local", bin)
	}
	if got := nudgeFor(bin, testCurlDest); got != nudgePnpmGlobal {
		t.Fatalf("nudgeFor = %q, want nudgePnpmGlobal", got)
	}
}

func TestPnpmStoreCopyWithoutGlobalLinkStaysProjectLocal(t *testing.T) {
	// A project install sharing the virtual store resolves to the same kind of
	// path; only a global link tells them apart.
	bin := pnpmLayout(t, false)
	if !isProjectLocalInstall(bin) {
		t.Fatalf("unlinked store copy %q read as global", bin)
	}
	if got := nudgeFor(bin, testCurlDest); got == nudgePnpmGlobal {
		t.Fatalf("unlinked store copy got the global pnpm hint")
	}
}

func TestPackageRoot(t *testing.T) {
	cases := map[string]string{
		"/a/node_modules/@s/pkg/bin/x": "/a/node_modules/@s/pkg",
		"/a/node_modules/pkg/x":        "/a/node_modules/pkg",
		"/a/node_modules/@s":           "",
		"/a/b/c":                       "",
	}
	for in, want := range cases {
		if got := packageRoot(in); got != want {
			t.Errorf("packageRoot(%q) = %q, want %q", in, got, want)
		}
	}
}
