import { describe, test, before, after } from "node:test";
import assert from "node:assert/strict";
import { writeFileSync, readFileSync, rmSync, statSync } from "node:fs";
import { join } from "node:path";
import { setTimeout } from "node:timers/promises";
import { Px0, Px0Error, type ThreadEvent } from "./index.ts";
import { bin, editorFixture } from "./test-server.ts";
import { assertShape } from "./test-support.ts";

const status = (code: number) => (error: unknown) => error instanceof Px0Error && error.status === code;
const bounded = () => ({ signal: AbortSignal.timeout(10_000) });

// The baseline is an independent fetch of the editor's own API. Git and file
// assertions also verify effects, so returning the same wrong payload twice
// cannot make an edit or commit pass.
describe("editor parity", { skip: !bin && "run npm run test:e2e or set PX0_BIN", timeout: 60_000 }, () => {
  let f: ReturnType<typeof editorFixture>;
  let px0: Px0;
  let commit: string;
  let savedThread: string;
  const source = (n: number) => `function hello() {\n  return ${n};\n}\n`;
  before(async () => {
    f = editorFixture();
    const files = {
      "a.js": source(1), "b.js": "const other = 1;\n", "gone.js": "const gone = true;\n",
      "a b & é.csv": '\uFEFFname,value\n"line1\nline2",é\n<script>,=SUM(A1)\n',
      "data.tsv": "id\ttext\n1\t<script>\n", "empty.csv": "",
      "large.csv": "id,value\n" + Array.from({ length: 1002 }, (_, i) => `${i},ok\n`).join(""),
      "README.md": "# Hello\n\n```js\nconst n = 1;\n```\n", "old.txt": "renamed\n",
    };
    for (const [name, text] of Object.entries(files)) writeFileSync(join(f.root, name), text);
    f.git("add", "."); f.git("commit", "-qm", "baseline");
    const remote = join(f.temp, "remote.git");
    f.git("init", "-q", "--bare", remote);
    f.git("remote", "add", "origin", remote);
    f.git("push", "-qu", "origin", "main");
    writeFileSync(join(f.root, "a.js"), source(2));
    rmSync(join(f.root, "gone.js"));
    f.git("mv", "old.txt", "new.txt");
    f.git("add", "-A"); f.git("commit", "-qm", "history change\n\nFull commit body.");
    commit = f.git("rev-parse", "HEAD");
    writeFileSync(join(f.root, "a.js"), source(3));
    px0 = await f.start({ basePath: "/sdk-review/" });
  });
  after(async () => f?.dispose());

  test("reads, previews and settings retain the editor's complete response shapes", async () => {
    assertShape(await px0.meta(), "Meta");
    const sync = await px0.git.syncState();
    assertShape(sync, "GitSyncState");
    assert.deepEqual(sync, await f.direct("git/sync-state"));
    assert.equal(sync.branch, "main");
    assert.equal(sync.upstream, "origin/main");
    assert.equal(sync.ahead, 1);
    assert.equal(sync.dirty, true);
    assertShape(await px0.metrics(), "Metrics");
    assert.deepEqual(await px0.tree(), (await f.direct("tree")).children);
    assert.deepEqual(await px0.find("ajs", { limit: 5 }), (await f.direct("find", { q: "ajs", limit: 5 })).results);
    const initialFind = await px0.find("", { limit: 1 });
    initialFind.forEach((hit) => assertShape(hit, "FuzzyResult"));
    assert.deepEqual(initialFind, (await f.direct("find", { q: "", limit: 1 })).results);
    let file = await px0.file("a.js", { start: 1, count: 2 });
    // Highlighting refines asynchronously on first open. Compare settled
    // snapshots, while still checking the initial response's declared shape.
    assertShape(file, "FileChunk");
    for (const deadline = Date.now() + 5000; "refine" in file && file.refine;) {
      assert.ok(Date.now() < deadline, "highlighting did not settle");
      await setTimeout(5);
      file = await px0.file("a.js", { start: 1, count: 2 });
    }
    assert.deepEqual(file, await f.direct("file", { path: "a.js", start: 1, count: 2 }));
    assert.equal(await px0.text("a.js"), readFileSync(join(f.root, "a.js"), "utf8"));
    assert.equal(await (await px0.raw("a.js")).text(), source(3));
    assert.equal(await px0.markdown("README.md"), (await f.direct("markdown", { path: "README.md" })).html);
    assert.deepEqual(await px0.search("return", { word: true, glob: "*.js" }), await f.direct("search", { q: "return", word: 1, glob: "*.js" }));
    assert.deepEqual(await px0.outline("a.js"), (await f.direct("outline", { path: "a.js" })).symbols);
    assert.deepEqual(await px0.def("hello", { path: "a.js" }), await f.direct("def", { sym: "hello", path: "a.js" }));
    assertShape(await px0.settings(), "Settings");
    await px0.updateSettings({ "editor.vimMode": true });
    assert.equal((await f.direct("settings")).settings["editor.vimMode"], true);
    await f.direct("settings", {}, { "editor.vimMode": false });
    assert.equal((await px0.settings()).settings["editor.vimMode"], false);
    await px0.close("a.js");
  });

  test("CSV/TSV parsing, Unicode paths, multiline source lines and limits match table preview", async () => {
    for (const path of ["a b & é.csv", "data.tsv", "large.csv", "empty.csv"]) {
      const result = await px0.table(path);
      assertShape(result, "TableResult");
      assert.deepEqual(result, await f.direct("table", { path }));
    }
    const csv = await px0.table("a b & é.csv");
    assert.deepEqual(csv.header, ["name", "value"]);
    assert.deepEqual(csv.rows.map((r) => r.line), [2, 4]);
    assert.equal(csv.rows[0].cells[0], "line1\nline2");
    assert.equal(csv.rows[1].cells[0], "<script>");
    const large = await px0.table("large.csv");
    assert.equal(large.rows.length, 1000);
    assert.equal(large.truncated, true);
    await assert.rejects(px0.table("a.js"), status(415));
    await assert.rejects(px0.table("../secret.csv"), status(400));
  });

  test("commit history, renamed/deleted files and frozen highlighted diffs match the editor", async () => {
    const unpushed = await px0.git.unpushed({ limit: 5 });
    assert.deepEqual(unpushed, await f.direct("unpushed", { limit: 5 }));
    assert.equal(unpushed.available, true);
    assert.equal(unpushed.commits[0].hash, commit);
    assertShape(unpushed.commits[0], "UnpushedCommit");
    const log = await px0.git.log({ limit: 2 });
    assert.deepEqual(log, await f.direct("git/log", { limit: 2 }));
    assert.equal(log.commits[0].full, commit);
    assertShape(log.commits[0], "GitCommit");
    const detail = await px0.git.commitDetail(commit);
    assertShape(detail, "CommitDetail");
    assert.match(detail.message, /Full commit body/);
    assert.deepEqual(detail, await f.direct("commitdetail", { sha: commit }));
    const files = await px0.git.commitFiles(commit);
    assert.deepEqual(files, await f.direct("commitfiles", { sha: commit }));
    files.files.forEach((file) => assertShape(file, "CommitFile"));
    assert.equal(files.files.find((file) => file.path === "gone.js")?.status, "D");
    assert.ok(files.files.some((file) => file.path === "new.txt"));
    for (const ref of [undefined, commit]) {
      const query = { path: "a.js", ...(ref ? { ref } : {}) };
      const diff = await px0.diff("a.js", { ref });
      assertShape(diff, "Diff");
      assert.deepEqual(diff, await f.direct("diff", query));
      const rows = diff.hunks!.flatMap((h) => h.rows);
      assert.equal(rows.find((r) => r.type === "add")?.text, `  return ${ref ? 2 : 3};`);
      assert.ok(rows.some((r) => r.html?.includes("<i")));
      assert.deepEqual(await px0.gutter("a.js", { ref }), await f.direct("gutter", query));
    }
    const deleted = await px0.file("gone.js", { ref: commit });
    assertShape(deleted, "FileChunk");
    assert.ok("deleted" in deleted && deleted.deleted && deleted.total === 0);
    assert.deepEqual(deleted, await f.direct("file", { path: "gone.js", ref: commit }));
    const tabs = [{ path: "a.js", ref: commit }, { path: "a.js" }];
    assert.deepEqual((await px0.updateSession({ tabs, active: 0, openDirs: [] })).tabs, tabs);
    assert.deepEqual(await px0.session(), await f.direct("session"));
    await assert.rejects(px0.git.commitDetail("not-a-sha"), status(404));
  });

  test("thread transcripts, live tools/deltas, follow-ups and review scopes match the editor", async () => {
    const thread = await px0.threads.create({ path: "a.js", l1: 1, l2: 3, message: "sdk:explain first turn", scope: "selection" });
    savedThread = thread.id;
    assertShape(thread, "Thread");
    assert.match(thread.snippet!, /return 3/);
    const first = await px0.threads.wait(thread.id, bounded());
    assert.equal(first.turns[0].error, undefined);
    assert.match(first.turns[0].reply, /first turn/);
    assert.deepEqual(first, await f.direct("threads/get", { id: thread.id }));
    assert.equal((await px0.threads.scope(thread.id, "mine")).scope, "mine");
    assert.equal((await f.direct("threads/get", { id: thread.id })).scope, "mine");
    await f.direct("threads/scope", {}, { id: thread.id, scope: "pr" });
    assert.equal((await px0.threads.get(thread.id)).scope, "pr");
    const events = px0.threads.events({ id: thread.id, ...bounded() });
    assert.equal((await events.next()).value!.type, "thread");
    const seen: ThreadEvent[] = [];
    const collect = (async () => {
      for await (const event of events) {
        assertShape(event, "ThreadEvent");
        seen.push(event);
        if (event.type === "thread" && event.data.turns.length === 2 && !event.data.turns[1].running) break;
      }
    })();
    await px0.threads.send(thread.id, "sdk:followup second turn");
    await collect;
    assert.ok(seen.some((e) => e.type === "tool"));
    assert.ok(seen.some((e) => e.type === "delta" && e.data.text.includes("second turn")));
    const done = await px0.threads.wait(thread.id, bounded());
    assert.equal(done.turns.length, 2);
    assert.equal(done.turns[1].error, undefined);
    assertShape(done, "Thread");
    assert.deepEqual(await px0.threads.list(), (await f.direct("threads")).threads);
    const list = px0.threads.events(bounded());
    const initial = (await list.next()).value!;
    assert.equal(initial.type, "list");
    assertShape(initial, "ThreadEvent");
    const deleted = (async () => {
      for await (const event of list) if (event.type === "deleted") return event.data.id;
    })();
    const disposable = await px0.threads.create({ message: "sdk:hold" });
    assert.equal(await px0.threads.delete(disposable.id), true);
    assert.equal(await deleted, disposable.id);
    await assert.rejects(px0.threads.get(disposable.id), status(404));
  });

  test("busy conflicts, aborts, cancellation, failures and missing streams are bounded", async () => {
    const hold = await px0.threads.create({ message: "sdk:hold" });
    await assert.rejects(px0.threads.send(hold.id, "busy"), status(409));
    await assert.rejects(px0.threads.wait(hold.id, { signal: AbortSignal.timeout(30) }), { name: "TimeoutError" });
    assert.equal(await px0.threads.cancel(hold.id), true);
    const cancelled = await px0.threads.wait(hold.id, bounded());
    assert.ok(cancelled.turns[0].error);
    assert.equal(cancelled.turns[0].running, false);
    const failed = await px0.threads.create({ message: "sdk:fail" });
    const result = await px0.threads.wait(failed.id, bounded());
    assert.match(result.turns[0].error!, /fixture failure/);
    assert.ok((await px0.threads.list()).find((t) => t.id === failed.id)?.failed);
    await assert.rejects(px0.threads.create({ message: "" }), status(400));
    await assert.rejects(px0.threads.create({ path: "../bad", message: "bad" }), status(400));
    const missing = px0.threads.events({ id: "missing", ...bounded() });
    await assert.rejects(missing.next(), status(404));
  });

  test("inline and batch jobs bridge to threads and produce real file and git changes", async () => {
    const job = await px0.agent.edit([
      { path: "a.js", l1: 1, l2: 3, instruction: "sdk:batch" },
      { path: "b.js", l1: 1, l2: 1, instruction: "sdk:batch" },
    ]);
    assertShape(job, "AgentJob");
    assert.equal(job.batchCount, 2);
    assert.ok(job.threadId);
    const done = await px0.agent.wait(job.id, bounded());
    assert.equal(done.error, undefined);
    assert.deepEqual(done.changed, ["a.js", "b.js"]);
    assertShape(done, "AgentJob");
    assert.match(await px0.text("b.js"), /edited too/);
    assert.equal((await px0.threads.get(job.threadId!)).kind, "batch");
    await px0.threads.send(job.threadId!, "sdk:followup discuss this edit");
    assert.equal((await px0.threads.wait(job.threadId!, bounded())).turns.length, 2);
    const hold = await px0.agent.edit({ path: "a.js", l1: 1, l2: 3, instruction: "sdk:hold" });
    await assert.rejects(px0.agent.edit({ path: "a.js", l1: 2, l2: 2, instruction: "overlap" }), status(409));
    assert.equal(await px0.agent.cancel(hold.id), true);
    assert.ok((await px0.agent.wait(hold.id, bounded())).error);
    assert.equal(await px0.agent.job(999999), null);
    await px0.git.stage(".");
    assert.ok(f.git("diff", "--cached", "--name-only").includes("b.js"));
    await px0.git.unstage("b.js");
    assert.ok(!f.git("diff", "--cached", "--name-only").includes("b.js"));
    await px0.git.stage("b.js");
    await px0.git.commit("SDK parity commit");
    assert.equal(f.git("log", "-1", "--format=%s"), "SDK parity commit");
    assert.equal((await px0.git.status()).gitChanges, 0);
    await px0.git.push();
    assert.equal((await px0.git.unpushed()).available, false);
    assert.equal((await px0.git.pull()).ok, true);
  });

  test("threads, settings and commit-pinned sessions survive restarting the editor", async () => {
    const before = await px0.threads.get(savedThread);
    const session = await px0.session();
    await f.stop();
    px0 = await f.start({ basePath: "/sdk-review/" });
    assert.deepEqual(await px0.threads.get(savedThread), before);
    assert.deepEqual(await px0.session(), session);
    assert.equal((await px0.settings()).settings["editor.vimMode"], false);
    assertShape(await px0.lsp.servers(), "LspServers");
    await assert.rejects(px0.pr.meta(), status(404));
  });
});

describe("file writes", { skip: !bin && "run npm run test:e2e or set PX0_BIN", timeout: 30_000 }, () => {
  let f: ReturnType<typeof editorFixture>;
  let px0: Px0;
  const path = "draft & é.txt";
  before(async () => {
    f = editorFixture();
    writeFileSync(join(f.root, path), "old\n");
    f.git("add", "."); f.git("commit", "-qm", "file write fixture");
    px0 = await f.start({ basePath: "/file-writes/", agent: false });
  });
  after(async () => f?.dispose());

  test("saving persists UTF-8 content, refreshes cached reads and creates missing parents", async () => {
    await px0.file(path);
    const content = "é 🐙\nupdated\n";
    const size = Buffer.byteLength(content);
    assert.deepEqual(await px0.saveFile(path, content), { ok: true, path, size });
    assert.equal(readFileSync(join(f.root, path), "utf8"), content);
    assert.equal(await px0.text(path), content);
    assert.equal((await px0.file(path)).size, size);
    const nested = "missing parents/empty.txt";
    assert.deepEqual(await px0.saveFile(nested, ""), { ok: true, path: nested, size: 0 });
    assert.equal(readFileSync(join(f.root, nested), "utf8"), "");
    await assert.rejects(px0.saveFile("../escape.txt", "bad"), status(400));
    await assert.rejects(px0.saveFile("missing parents", "bad"), status(400));
  });

  test("creating files and directories preserves types, rejects duplicates and stays inside the workspace", async () => {
    const dir = "new & é/nested";
    assert.deepEqual(await px0.createDirectory(dir), { ok: true, path: dir, type: "dir" });
    assert.ok(statSync(join(f.root, dir)).isDirectory());
    const file = "other parents/new & é.txt";
    assert.deepEqual(await px0.createFile(file), { ok: true, path: file, type: "file" });
    assert.ok(statSync(join(f.root, file)).isFile());
    assert.equal(readFileSync(join(f.root, file), "utf8"), "");
    assert.ok((await px0.tree("other parents")).some((entry) => entry.path === file));
    await assert.rejects(px0.createFile(file), status(409));
    await assert.rejects(px0.createDirectory(dir), status(409));
    await assert.rejects(px0.createFile("../escape.txt"), status(400));
    await assert.rejects(px0.createDirectory("../escape"), status(400));
  });
});

describe("language-server parity", { skip: !bin && "run npm run test:e2e or set PX0_BIN", timeout: 30_000 }, () => {
  let f: ReturnType<typeof editorFixture>;
  let px0: Px0;
  before(async () => {
    f = editorFixture();
    writeFileSync(join(f.root, "main.go"), "package main\nfunc hello() int { return 1 }\nfunc main() { _ = hello() }\n");
    f.git("add", "."); f.git("commit", "-qm", "LSP fixture");
    px0 = await f.start({ basePath: "/lsp/", lsp: true });
  });
  after(async () => f?.dispose());
  test("server lifecycle and semantic answers go through the real editor LSP client", async () => {
    assertShape(await px0.lsp.servers(), "LspServers");
    assert.deepEqual(await px0.lsp.servers(), await f.direct("lsp/servers"));
    const started = await px0.lsp.startServer("gopls");
    assertShape(started, "LspServers");
    const warm = await px0.lsp.warm("main.go", { wait: 5000 });
    assertShape(warm, "LspBrief");
    assert.equal(warm.state, "ready");
    assert.equal(warm.anyRunning, true);
    const definition = await px0.lsp.definition("main.go", 3, 18);
    assert.deepEqual(definition, await f.direct("lsp/def", { path: "main.go", line: 3, col: 18 }));
    assert.equal(definition.hits[0].line, 2);
    assertShape(definition.hits[0], "NavHit");
    const references = await px0.lsp.references("main.go", 2, 5);
    assert.deepEqual(references, await f.direct("lsp/refs", { path: "main.go", line: 2, col: 5 }));
    assert.equal(references.hits.length, 2);
    const hover = await px0.lsp.hover("main.go", 2, 5);
    assertShape(hover, "Hover");
    assert.match(hover.signature!, /hello/);
    assert.deepEqual(hover, await f.direct("lsp/hover", { path: "main.go", line: 2, col: 5 }));
    const symbols = await px0.lsp.symbols("main.go");
    assert.deepEqual(symbols, await f.direct("lsp/symbols", { path: "main.go" }));
    assert.equal(symbols.symbols[0].name, "hello");
    const calls = await px0.lsp.calls("main.go", 2, 5);
    assert.equal(calls.nodes[0].name, "hello");
    assertShape(calls.nodes[0], "CallNode");
    const item = calls.nodes[0].item;
    for (const dir of ["in", "out"] as const) {
      const expanded = await px0.lsp.expandCalls("main.go", item, { dir });
      assert.deepEqual(expanded, await f.direct("lsp/calls", { path: "main.go", item, dir }));
      assert.equal(expanded.nodes[0].name, dir === "in" ? "main" : "hello");
    }
    const stopped = await px0.lsp.stop("gopls");
    assert.equal(stopped.anyRunning, false);
    assert.equal(stopped.servers!.find((s) => s.name === "gopls")?.state, "stopped");
    await px0.lsp.startServer("all");
    assert.equal((await px0.lsp.warm("main.go", { wait: 5000 })).state, "ready");
    assertShape(await px0.lsp.start("main.go"), "LspBrief");
    assert.equal((await px0.lsp.stop()).anyRunning, false);
    await assert.rejects(px0.lsp.install("sdk-nonexistent-server"), status(400));
  });
});
