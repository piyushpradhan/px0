import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, mkdtempSync, mkdirSync, copyFileSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import ts from "typescript";
import { Px0, SDK_VERSION } from "./index.ts";
import { checker, publicType } from "./test-support.ts";

test("SDK version matches the package, independently of the editor version", () => {
  const pkg = JSON.parse(readFileSync(new URL("package.json", import.meta.url), "utf8"));
  assert.equal(SDK_VERSION, pkg.version);
  assert.match(SDK_VERSION, /^\d+\.\d+\.\d+(?:-[\w.-]+)?$/);
});

test("npm version synchronizes the package, lockfile and runtime version", () => {
  const temp = mkdtempSync(join(tmpdir(), "px0-sdk-version-"));
  try {
    mkdirSync(join(temp, "scripts"));
    for (const name of ["package.json", "package-lock.json", "index.ts", "scripts/version.mjs"]) {
      copyFileSync(new URL(name, import.meta.url), join(temp, name));
    }
    const [major, minor, patch] = SDK_VERSION.split("-")[0].split(".").map(Number);
    const next = `${major}.${minor}.${patch + 1}`;
    execFileSync(process.platform === "win32" ? "npm.cmd" : "npm", ["version", next, "--no-git-tag-version"], { cwd: temp, stdio: "pipe" });
    assert.equal(JSON.parse(readFileSync(join(temp, "package.json"), "utf8")).version, next);
    const lock = JSON.parse(readFileSync(join(temp, "package-lock.json"), "utf8"));
    assert.equal(lock.version, next);
    assert.equal(lock.packages[""].version, next);
    assert.ok(readFileSync(join(temp, "index.ts"), "utf8").includes(`export const SDK_VERSION = "${next}";`));
  } finally {
    rmSync(temp, { recursive: true, force: true });
  }
});

test("every registered editor API has a callable SDK wrapper", async () => {
  let calls: { url: URL; init: RequestInit }[] = [];
  const px0 = new Px0({ url: "http://localhost:7777/review/", fetch: async (url, init = {}) => {
    calls.push({ url: new URL(String(url)), init });
    if (String(url).includes("stream")) return new Response('event: ping\ndata: {}\n\n');
    return Response.json({ ready: true, running: false, turns: [], children: [], results: [], symbols: [], threads: [], comments: [] });
  } });
  const edit = { path: "a b.js", l1: 1, l2: 2, instruction: "explain & fix" };
  const oneEvent = async (events: AsyncIterable<unknown>) => { for await (const _ of events) break; };
  const cases: [string, string, () => unknown][] = [
    ["GET", "meta", () => px0.meta()], ["GET", "meta", () => px0.ready()],
    ["GET", "metrics", () => px0.metrics()], ["GET", "tree", () => px0.tree("src")],
    ["GET", "find", () => px0.find("ab", { limit: 5 })], ["GET", "file", () => px0.file(edit.path, { start: 1, count: 2, ref: "HEAD" })],
    ["GET", "close", () => px0.close(edit.path)], ["GET", "raw", () => px0.raw(edit.path)], ["GET", "raw", () => px0.text(edit.path)],
    ["GET", "markdown", () => px0.markdown("README.md")], ["GET", "table", () => px0.table("a.csv")],
    ["GET", "diff", () => px0.diff(edit.path, { ref: "HEAD" })], ["GET", "gutter", () => px0.gutter(edit.path, { ref: "HEAD" })],
    ["GET", "search", () => px0.search("foo", { regex: true, caseSensitive: true, word: true, glob: "*.js" })],
    ["GET", "outline", () => px0.outline(edit.path)], ["GET", "def", () => px0.def("foo", { path: edit.path })],
    ["GET", "reindex", () => px0.reindex()], ["GET", "settings", () => px0.settings()],
    ["POST", "settings", () => px0.updateSettings({ "editor.vimMode": true })],
    ["GET", "session", () => px0.session()], ["POST", "session", () => px0.updateSession({ tabs: [{ path: edit.path, ref: "HEAD" }] })],
    ["GET", "stream", () => oneEvent(px0.events())], ["GET", "git/stream", () => oneEvent(px0.events({ metrics: false }))],
    ["GET", "lsp/def", () => px0.lsp.definition(edit.path, 1, 0)], ["GET", "lsp/refs", () => px0.lsp.references(edit.path, 1, 0)],
    ["GET", "lsp/hover", () => px0.lsp.hover(edit.path, 1, 0)], ["GET", "lsp/symbols", () => px0.lsp.symbols(edit.path)],
    ["GET", "lsp/calls", () => px0.lsp.calls(edit.path, 1, 0)], ["GET", "lsp/calls", () => px0.lsp.expandCalls(edit.path, "opaque", { dir: "out" })],
    ["GET", "lsp/warm", () => px0.lsp.warm(edit.path)], ["GET", "lsp/setup", () => px0.lsp.setup(edit.path)],
    ["POST", "lsp/install", () => px0.lsp.install("gopls")], ["POST", "lsp/start", () => px0.lsp.start(edit.path)],
    ["GET", "lsp/servers", () => px0.lsp.servers()], ["POST", "lsp/start", () => px0.lsp.startServer("all")],
    ["POST", "lsp/stop", () => px0.lsp.stop()], ["GET", "git/refresh", () => px0.git.status()],
    ["GET", "git/log", () => px0.git.log()], ["GET", "unpushed", () => px0.git.unpushed({ limit: 10 })],
    ["GET", "commitfiles", () => px0.git.commitFiles("HEAD")], ["GET", "commitdetail", () => px0.git.commitDetail("HEAD")],
    ["POST", "git/stage", () => px0.git.stage(edit.path)], ["POST", "git/unstage", () => px0.git.unstage(edit.path)],
    ["POST", "git/commit", () => px0.git.commit("message")], ["POST", "git/pull", () => px0.git.pull()],
    ["POST", "git/push", () => px0.git.push()], ["POST", "git/commit-message", () => px0.git.commitMessage()],
    ["GET", "agent/harnesses", () => px0.agent.harnesses()], ["POST", "agent/select", () => px0.agent.select("codex", { model: "model" })],
    ["POST", "agent/edit", () => px0.agent.edit(edit)], ["POST", "agent/batch", () => px0.agent.edit([edit])],
    ["GET", "agent/job", () => px0.agent.job(1)], ["GET", "agent/job", () => px0.agent.wait(1)], ["POST", "agent/cancel", () => px0.agent.cancel(0)],
    ["GET", "threads", () => px0.threads.list()], ["GET", "threads/get", () => px0.threads.get("t1")],
    ["POST", "threads/create", () => px0.threads.create({ message: "explain", path: edit.path, scope: "selection" })],
    ["POST", "threads/send", () => px0.threads.send("t1", "follow up")], ["POST", "threads/scope", () => px0.threads.scope("t1", "mine")],
    ["POST", "threads/cancel", () => px0.threads.cancel("t1")], ["POST", "threads/delete", () => px0.threads.delete("t1")],
    ["GET", "threads/get", () => px0.threads.wait("t1")], ["GET", "threads/stream", () => oneEvent(px0.threads.events({ id: "t1" }))],
    ["GET", "pr/meta", () => px0.pr.meta()], ["GET", "pr/existing-comments", () => px0.pr.comments()],
    ["GET", "pr/comments", () => px0.pr.drafts()], ["POST", "pr/comments", () => px0.pr.addDraft({ path: edit.path, line: 1, body: "review" })],
    ["POST", "pr/comments/delete", () => px0.pr.deleteDraft(1)], ["POST", "pr/submit", () => px0.pr.submit("COMMENT")],
    ["POST", "pr/comments/issue", () => px0.pr.comment("review")], ["POST", "pr/comments/review-reply", () => px0.pr.reply(1, "reply")],
    ["POST", "pr/launch", () => px0.pr.launch("https://github.com/owner/repo/pull/1")],
  ];
  const covered = new Set<string>();
  for (const [method, path, invoke] of cases) {
    calls = [];
    await invoke();
    assert.equal(calls.length, 1, `${method} ${path}: should make one request`);
    assert.equal(calls[0].url.pathname, `/review/api/${path}`);
    assert.equal(calls[0].init.method, method);
    if (["file", "diff", "gutter"].includes(path)) assert.equal(calls[0].url.searchParams.get("ref"), "HEAD");
    if (method === "POST") assert.equal(new Headers(calls[0].init.headers).get("Origin"), "http://localhost:7777");
    if (path.includes("stream")) assert.equal(new Headers(calls[0].init.headers).get("Accept"), "text/event-stream");
    covered.add(path);
  }
  const server = readFileSync(new URL("../server.go", import.meta.url), "utf8");
  const routes = [...server.matchAll(/s\.mux\.HandleFunc\(s\.routePath\("\/api\/([^"\n]+)"\)/g)].map((m) => m[1]);
  assert.ok(routes.length > 50, "route extraction must not silently stop working");
  assert.deepEqual([...covered].sort(), routes.sort(), "editor API changed: add the SDK wrapper and its contract case");
});

test("public SDK fields and optionality match the editor's JSON structs", () => {
  const contracts = [
    ["index.go", "Node", "TreeNode"], ["internal/fuzzy/fuzzy.go", "Result", "FuzzyResult"],
    ["internal/metrics/metrics.go", "ProcessMetrics", "Metrics"], ["search.go", "Match", "Match"],
    ["symbols.go", "Symbol", "OutlineSymbol"], ["lspnav.go", "NavHit", "NavHit"], ["calls.go", "CallNode", "CallNode"],
    ["highlight.go", "DiffRow", "DiffRow"], ["highlight.go", "DiffHunk", "DiffHunk"],
    ["internal/table/table.go", "Row", "TableRow"], ["internal/table/table.go", "Result", "TableResult"],
    ["git.go", "GitCommit", "GitCommit"], ["git.go", "UnpushedCommit", "UnpushedCommit"],
    ["git.go", "CommitFile", "CommitFile"], ["git.go", "CommitDetail", "CommitDetail"],
    ["git_watcher.go", "GitStatusPayload", "GitStatus"], ["agent.go", "agentHarness", "Harness"],
    ["agent.go", "agentBatchItem", "EditRequest"], ["agent.go", "agentJob", "AgentJob"],
    ["thread.go", "threadTurn", "ThreadTurn"], ["thread.go", "thread", "Thread"], ["thread.go", "threadSummary", "ThreadSummary"],
    ["session.go", "SessionTab", "SessionTab"], ["session.go", "WorkspaceSession", "Session"],
    ["settings.go", "settingSchemaItem", "SettingSchemaItem"], ["github.go", "prComment", "DraftComment"],
    ["provider.go", "PRComment", "PRComment"], ["lspsetup.go", "lspJob", "LspInstallJob"],
  ];
  for (const [file, goName, sdkName] of contracts) {
    const go = readFileSync(new URL(`../${file}`, import.meta.url), "utf8");
    const body = go.match(new RegExp(`^type ${goName} struct \\{([\\s\\S]*?)^\\}`, "m"))?.[1];
    assert.ok(body, `missing Go struct ${goName} in ${file}`);
    const fields = [...body.matchAll(/json:"([^",]+)([^"\n]*)"/g)].filter((m) => m[1] !== "-");
    const props = checker.getPropertiesOfType(publicType(sdkName));
    assert.deepEqual(props.map((p) => p.name).sort(), fields.map((m) => m[1]).sort(), `${file}: ${goName} != ${sdkName}`);
    for (const [, key, flags] of fields) {
      const prop = props.find((p) => p.name === key)!;
      assert.equal(!!(prop.flags & ts.SymbolFlags.Optional), flags.includes("omitempty"), `${sdkName}.${key} optionality`);
    }
  }
});
