"use strict";

// isGlobalInstall against pnpm 10's global virtual store, built on disk.
//
// pnpm 10 installs a global package as a SYMLINK:
//   <home>/global/v11/<id>/node_modules/@promptster/teams-cli
//     -> <home>/store/v11/links/@promptster/teams-cli/<ver>/<hash>/node_modules/@promptster/teams-cli
// Node resolves __dirname through it, so the launcher only ever sees the store
// path. Before this check, that path read as project-local and the managed
// binary was never installed: Cursor hooks pointed at a file that did not exist.

const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const os = require("os");
const path = require("path");

const { isGlobalInstall } = require("../lib/resolve");

const WINDOWS = process.platform === "win32";

function layout(t) {
  const home = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "pt-pnpm-")));
  const prevHome = process.env.PNPM_HOME;
  delete process.env.PNPM_HOME;
  t.after(() => {
    if (prevHome === undefined) delete process.env.PNPM_HOME;
    else process.env.PNPM_HOME = prevHome;
    fs.rmSync(home, { recursive: true, force: true });
  });
  const pkg = (ver, hash) => {
    const root = path.join(
      home, "pnpm", "store", "v11", "links", "@promptster", "teams-cli", ver, hash,
      "node_modules", "@promptster", "teams-cli",
    );
    fs.mkdirSync(path.join(root, "lib"), { recursive: true });
    return root;
  };
  const linkGlobal = (target, id) => {
    const dir = path.join(home, "pnpm", "global", "v11", id, "node_modules", "@promptster");
    fs.mkdirSync(dir, { recursive: true });
    fs.symlinkSync(target, path.join(dir, "teams-cli"));
  };
  return { home, pkg, linkGlobal };
}

test("a pnpm 10 global install resolved into the store is global", { skip: WINDOWS }, (t) => {
  const { pkg, linkGlobal } = layout(t);
  const root = pkg("0.41.3", "aa77");
  linkGlobal(root, "3265-18e7-0");
  assert.strictEqual(isGlobalInstall(path.join(root, "lib")), true);
});

test("a store copy no global dir links to stays project-local", { skip: WINDOWS }, (t) => {
  // A project install sharing the global virtual store resolves to the same
  // kind of path; only the global link tells them apart.
  const { pkg } = layout(t);
  const root = pkg("0.41.3", "aa77");
  assert.strictEqual(isGlobalInstall(path.join(root, "lib")), false);
});

test("a global link to a DIFFERENT version does not make this copy global", { skip: WINDOWS }, (t) => {
  const { pkg, linkGlobal } = layout(t);
  const pinned = pkg("0.40.0", "bb11");
  const globalCopy = pkg("0.41.3", "aa77");
  linkGlobal(globalCopy, "3265-18e7-0");
  assert.strictEqual(isGlobalInstall(path.join(pinned, "lib")), false);
  assert.strictEqual(isGlobalInstall(path.join(globalCopy, "lib")), true);
});

test("PNPM_HOME with a store elsewhere still finds the global link", { skip: WINDOWS }, (t) => {
  const { home, linkGlobal } = layout(t);
  const root = path.join(
    home, "custom-store", "v11", "links", "@promptster", "teams-cli", "0.41.3", "cc22",
    "node_modules", "@promptster", "teams-cli",
  );
  fs.mkdirSync(path.join(root, "lib"), { recursive: true });
  // A configured storeDir: the global dir is not beside it, so only
  // $PNPM_HOME can find the link.
  linkGlobal(root, "3265-18e7-0");
  assert.strictEqual(isGlobalInstall(path.join(root, "lib")), false);
  process.env.PNPM_HOME = path.join(home, "pnpm");
  assert.strictEqual(isGlobalInstall(path.join(root, "lib")), true);
});

test("pre-10 pnpm global and project layouts are unchanged", () => {
  assert.strictEqual(
    isGlobalInstall("/home/e/Library/pnpm/global/5/.pnpm/@promptster+teams-cli@0.5.6/node_modules/@promptster/teams-cli/lib"),
    true,
  );
  assert.strictEqual(
    isGlobalInstall("/home/e/proj/node_modules/.pnpm/@promptster+teams-cli@0.5.6/node_modules/@promptster/teams-cli/lib"),
    false,
  );
});
