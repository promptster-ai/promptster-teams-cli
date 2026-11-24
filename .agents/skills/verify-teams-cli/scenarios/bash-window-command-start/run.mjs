#!/usr/bin/env node
// Drives the real daemon (Claude watcher + git watcher) over an auto-mode Claude
// transcript whose one Bash call writes a file at its START and then runs for 90s (apply,
// check, commit — the shape agents use), and asserts commit attribution credits
// the file to that session. Before teams-cli kept a command's start, the window
// was the command's end point alone and this commit read
// `bash_window_miss_early` by the command's run.
//
// Control: a second commit whose file was saved 10 minutes before the command
// started (a human edit) stays unattributed, with its miss measured from the
// command's start.
//
// Usage (after `node $CT build`):  node scenarios/bash-window-command-start/run.mjs
// Exit 0 = pass. Takes ~3 minutes: the git watcher polls every 60s.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const CT = path.resolve(HERE, "..", "..", "control-teams-cli.mjs");
process.env.PROMPTSTER_VERIFY_ROOT ||= path.join(os.homedir(), ".promptster-verify", "teams-cli-bash-window-command-start");
const SBX = path.join(process.env.PROMPTSTER_VERIFY_ROOT, "sandbox");
const HOME = path.join(SBX, "home");
const STATE = path.join(HOME, ".promptster-teams");
const REPO = path.join(HOME, "repo");
const SID = "5b1d8f7e-0c2a-4d3e-9f10-bash0start01";

const ct = (args) => JSON.parse(spawnSync("node", [CT, ...args], { encoding: "utf8", env: process.env }).stdout);
const sleep = (s) => spawnSync("sleep", [String(s)]);
const git = (...a) => {
  const r = spawnSync("git", ["-C", REPO, ...a], {
    encoding: "utf8",
    env: { PATH: process.env.PATH, HOME, GIT_AUTHOR_NAME: "t", GIT_AUTHOR_EMAIL: "t@t", GIT_COMMITTER_NAME: "t", GIT_COMMITTER_EMAIL: "t@t" },
  });
  if (r.status !== 0) throw new Error(`git ${a.join(" ")}: ${r.stderr}`);
  return r.stdout.trim();
};
const waitFor = (secs, fn) => {
  for (const until = Date.now() + secs * 1000; Date.now() < until; sleep(2)) {
    const v = fn();
    if (v) return v;
  }
  return null;
};
const readJsonl = (p) => (fs.existsSync(p) ? fs.readFileSync(p, "utf8").split("\n").flatMap((l) => { try { return [JSON.parse(l)]; } catch { return []; } }) : []);

ct(["cleanup"]);
fs.mkdirSync(REPO, { recursive: true });
git("init", "-q", "-b", "main");
fs.writeFileSync(path.join(REPO, "README.md"), "baseline\n");
git("add", "-A");
git("commit", "-qm", "baseline");

const login = ct(["run", "login", "--key", "PSE-ABCD-EFGH-JKLM-NPQR-STUV-WXYZ"]);
waitFor(60, () => { try { return JSON.parse(fs.readFileSync(path.join(STATE, "claude-watcher.json"), "utf8")).lastHeartbeat; } catch { return null; } });
sleep(1);

// The command ran from 95s ago to 5s ago; it wrote gen.go 200ms in.
const now = Date.now();
const startMs = now - 95_000;
const endMs = now - 5_000;
const iso = (ms) => new Date(ms).toISOString();
const cmd = `cd ${REPO} && git apply ../gen.patch && make check && git commit -qam gen`;
const lines = [
  { type: "user", message: { role: "user", content: "generate the table and commit it" }, timestamp: iso(startMs - 20_000), cwd: REPO, sessionId: SID, permissionMode: "auto", promptSource: "typed", uuid: "u1" },
  { type: "assistant", requestId: "req-1", message: { id: "msg-1", model: "claude-opus-5-5", role: "assistant", content: [{ type: "tool_use", id: "toolu_bash1", name: "Bash", input: { command: cmd } }], usage: { input_tokens: 10, output_tokens: 5 } }, timestamp: iso(startMs), cwd: REPO, sessionId: SID, uuid: "a1" },
  { type: "user", message: { role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_bash1", content: "ok" }] }, toolUseResult: { stdout: "ok\n", stderr: "", interrupted: false }, timestamp: iso(endMs), cwd: REPO, sessionId: SID, uuid: "r1" },
];
const proj = path.join(HOME, ".claude", "projects", "-repo");
fs.mkdirSync(proj, { recursive: true });
fs.writeFileSync(path.join(proj, `${SID}.jsonl`), lines.map((l) => JSON.stringify(l)).join("\n") + "\n");

const ledger = path.join(STATE, "bash-windows.json");
const win = waitFor(60, () => {
  try { return JSON.parse(fs.readFileSync(ledger, "utf8")).sessions?.[SID]?.windows?.[0]; } catch { return null; }
});

// Let the git watcher baseline the repo before the commits land.
waitFor(90, () => fs.existsSync(path.join(STATE, "git-watch-cursors.json")) || readJsonl(path.join(STATE, "buffer.jsonl")).length > 0);
sleep(65);

// AI commit: gen.go's mtime is 200ms after the command started.
// Mtimes are set BEFORE each commit, so no poll can see a commit with a fresh one.
fs.writeFileSync(path.join(REPO, "gen.go"), "package main\n\nvar Gen = 1\n");
fs.utimesSync(path.join(REPO, "gen.go"), new Date(startMs + 200), new Date(startMs + 200));
git("add", "-A");
git("commit", "-qm", "gen");
const aiSha = git("rev-parse", "HEAD");
// Human control: notes.md saved 10 minutes before the command started.
fs.writeFileSync(path.join(REPO, "notes.md"), "by hand\n");
fs.utimesSync(path.join(REPO, "notes.md"), new Date(startMs - 600_000), new Date(startMs - 600_000));
git("add", "-A");
git("commit", "-qm", "notes");
const humanSha = git("rev-parse", "HEAD");

const outbox = path.join(STATE, "outbox.jsonl");
const attribution = (sha) => readJsonl(outbox).concat(readJsonl(path.join(STATE, "buffer.jsonl")))
  .find((e) => e?.kind === "commit_attribution" && e.data?.commitSha === sha);
waitFor(150, () => attribution(aiSha) && attribution(humanSha));
const ai = attribution(aiSha);
const human = attribution(humanSha);
const attrs = (e, p) => (e?.data?.files ?? []).filter((f) => f.path === p).flatMap((f) => f.lineRanges.map((r) => r.attribution));

const checks = {
  windowSpansTheRun: win?.startMs === startMs && win?.endMs === endMs,
  aiCommitAttributed: ai?.sessionId === SID && attrs(ai, "gen.go").length > 0 && attrs(ai, "gen.go").every((a) => a === "likely_ai"),
  aiCommitHasNoMissReason: !!ai && ai.data?.unattributedReason === undefined,
  humanEditStaysUnknown: !!human && attrs(human, "notes.md").every((a) => a === "unknown") && human.sessionId !== SID,
  humanMissMeasuredFromStart: human?.data?.unattributedReason === "bash_window_miss_early" && Math.abs((human?.data?.bashWindowGapMs ?? 0) - 600_000) <= 5,
};
const ok = Object.values(checks).every(Boolean);
console.log(JSON.stringify({
  ok, checks, window: win, commandRunMs: endMs - startMs,
  ai: ai && { sessionId: ai.sessionId, files: ai.data.files, unattributedReason: ai.data.unattributedReason },
  human: human && { sessionId: human.sessionId, files: human.data.files, unattributedReason: human.data.unattributedReason, bashWindowGapMs: human.data.bashWindowGapMs },
  evidence: login.evidence,
}, null, 1));
if (!process.env.PROMPTSTER_VERIFY_KEEP) ct(["cleanup"]);
process.exit(ok ? 0 : 1);
