#!/usr/bin/env node
// Drives the real daemon over a SECOND Claude config root (a tool running
// `claude -p` under its own CLAUDE_CONFIG_DIR, like pr-babysit) and asserts:
//   1. before `claude-dirs add`, its transcript is not captured;
//   2. after it, the running daemon captures it (no restart) and doctor lists it;
//   3. a commit, in a $TMPDIR worktree of a repo, of the file that transcript
//      wrote is attributed likely_ai to that session, not `no_ai_evidence`.
//
// Usage (after `node $CT build`):  node scenarios/claude-extra-config-dir/run.mjs
// Exit 0 = pass. Takes ~3 min: git-watch polls every 60s.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const CT = path.resolve(HERE, "..", "..", "control-teams-cli.mjs");
process.env.PROMPTSTER_VERIFY_ROOT ||= path.join(os.homedir(), ".promptster-verify", "teams-cli-claude-extra-dir");
const SBX = path.join(process.env.PROMPTSTER_VERIFY_ROOT, "sandbox");
const HOME = path.join(SBX, "home");
const STATE = path.join(HOME, ".promptster-teams");
const EXTRA = path.join(HOME, ".config", "second-account", "claude-config");
const SESSION = crypto.randomUUID();

const ct = (args) => JSON.parse(spawnSync("node", [CT, ...args], { encoding: "utf8", env: process.env }).stdout);
const sleep = (s) => spawnSync("sleep", [String(s)]);
const git = (cwd, ...args) => {
  const r = spawnSync("git", ["-C", cwd, ...args], { encoding: "utf8", env: { PATH: process.env.PATH, HOME, GIT_CONFIG_NOSYSTEM: "1" } });
  if (r.status !== 0) throw new Error(`git ${args.join(" ")}: ${r.stderr}`);
  return r.stdout.trim();
};
const events = () => {
  const p = path.join(STATE, "buffer.jsonl");
  if (!fs.existsSync(p)) return [];
  return fs.readFileSync(p, "utf8").split("\n").flatMap((l) => { try { return [JSON.parse(l)]; } catch { return []; } });
};
const waitFor = (secs, pred) => {
  for (const until = Date.now() + secs * 1000; Date.now() < until; sleep(2)) {
    const hit = pred();
    if (hit) return hit;
  }
  return null;
};

ct(["cleanup"]);
fs.mkdirSync(HOME, { recursive: true });
// A repo the engineer owns, and an agent's worktree of it in $TMPDIR — the
// shape pr-babysit uses.
const repo = path.join(HOME, "repo");
fs.mkdirSync(repo, { recursive: true });
git(repo, "init", "-q", "-b", "main");
git(repo, "config", "user.email", "dev@example.com");
git(repo, "config", "user.name", "Dev");
fs.writeFileSync(path.join(repo, "README.md"), "hi\n");
git(repo, "add", ".");
git(repo, "commit", "-qm", "init");
const wt = fs.realpathSync(fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "pr-babysit-verify-")));
fs.rmSync(wt, { recursive: true });
git(repo, "worktree", "add", "-q", "-b", "fix", wt);

const login = ct(["run", "login", "--key", "PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ"]);
waitFor(60, () => { try { return JSON.parse(fs.readFileSync(path.join(STATE, "claude-watcher.json"), "utf8")).lastHeartbeat; } catch { return null; } });

// The second account's transcript: a prompt in the worktree, then a Write.
const file = path.join(wt, "limiter.go");
const content = "package limiter\n\nfunc Allow(n, limit int) bool { return n < limit }\n";
const t = (s) => new Date(Date.now() + s * 1000).toISOString();
const base = { sessionId: SESSION, cwd: wt, isSidechain: false, userType: "external", version: "2.1.0", gitBranch: "fix" };
const lines = [
  { ...base, type: "user", uuid: "u1", timestamp: t(0), message: { role: "user", content: "add a limiter" } },
  { ...base, type: "assistant", uuid: "a1", timestamp: t(1), message: { id: "msg-1", role: "assistant", model: "claude-opus-5-5", content: [{ type: "tool_use", id: "toolu_1", name: "Write", input: { file_path: file, content } }], usage: { input_tokens: 10, output_tokens: 5 } } },
  { ...base, type: "user", uuid: "u2", timestamp: t(2), message: { role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_1", content: "wrote" }] }, toolUseResult: { filePath: file } },
];
fs.writeFileSync(file, content);
const slug = "-" + wt.replaceAll("/", "-").replace(/^-/, "");
fs.mkdirSync(path.join(EXTRA, "projects", slug), { recursive: true });
fs.writeFileSync(path.join(EXTRA, "projects", slug, `${SESSION}.jsonl`), lines.map((l) => JSON.stringify(l)).join("\n") + "\n");

const mine = () => events().filter((e) => e.sessionId === SESSION && e.source === "claude-code");
sleep(12); // four polls
const beforeAdd = mine().length;

const add = ct(["run", "claude-dirs", "add", EXTRA]);
const doctor = ct(["run", "doctor"]);
const diff = waitFor(60, () => mine().find((e) => e.kind === "file_diff"));

// Let git-watch discover the worktree (from the ledger the edit just wrote) and
// baseline it before committing. A checkout first seen on a device holding no
// cursors at all is baselined without replay (a first install), which a fresh
// sandbox always is — that is git-watch's rule, not this feature's.
const cursors = () => { try { return Object.keys(JSON.parse(fs.readFileSync(path.join(STATE, "git-watch-cursors.json"), "utf8")).cursors ?? {}).length; } catch { return 0; } };
const discovered = waitFor(150, () => cursors() >= 2);
git(wt, "add", "limiter.go");
git(wt, "commit", "-qm", "limiter");
const sha = git(wt, "rev-parse", "HEAD");
const attr = waitFor(200, () => events().find((e) => e.kind === "commit_attribution" && e.data?.commitSha === sha));
const f = attr?.data?.files?.find((x) => x.path === "limiter.go");

const checks = {
  notCapturedBeforeAdd: beforeAdd === 0,
  addListsExtraRoot: add.stdout.includes(`Claude Code transcripts (extra): ${fs.realpathSync(EXTRA)}/projects`),
  doctorListsBothRoots: doctor.stdout.includes("Claude Code transcript") && doctor.stdout.includes("(extra)"),
  extraRootEditCaptured: !!diff,
  worktreeDiscoveredFromThatEdit: !!discovered,
  commitAttributedToExtraSession: !!f && f.sessionId === SESSION && f.lineRanges.every((r) => r.attribution === "likely_ai"),
};
const ok = Object.values(checks).every(Boolean);
console.log(JSON.stringify({
  ok, checks, session: SESSION, worktree: wt, sha,
  kinds: mine().map((e) => e.kind),
  attribution: attr?.data ?? null,
  evidence: [login.evidence, add.evidence, doctor.evidence],
}, null, 1));
if (!process.env.PROMPTSTER_VERIFY_KEEP) {
  ct(["cleanup"]);
  fs.rmSync(wt, { recursive: true, force: true });
}
process.exit(ok ? 0 : 1);
