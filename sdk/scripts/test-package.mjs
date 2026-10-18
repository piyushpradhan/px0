import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

const sdk = fileURLToPath(new URL("../", import.meta.url));
const temp = mkdtempSync(join(tmpdir(), "px0-sdk-package-"));
const npm = process.platform === "win32" ? "npm.cmd" : "npm";
try {
  const [pack] = JSON.parse(execFileSync(npm, ["pack", "--json", "--ignore-scripts", "--pack-destination", temp], { cwd: sdk, encoding: "utf8" }));
  for (const name of ["dist/index.js", "dist/index.d.ts", "README.md", "CHANGELOG.md"]) {
    assert.ok(pack.files.some((file) => file.path === name), `tarball missing ${name}`);
  }
  const { version } = JSON.parse(readFileSync(join(sdk, "package.json"), "utf8"));
  writeFileSync(join(temp, "package.json"), '{"private":true,"type":"module"}');
  execFileSync(npm, ["install", "--ignore-scripts", "--omit=dev", "--no-audit", "--no-fund", join(temp, pack.filename)], { cwd: temp, stdio: "pipe" });
  writeFileSync(join(temp, "smoke.mjs"), `
    import assert from "node:assert/strict";
    import { Px0, Px0Error, SDK_VERSION } from "@px0/sdk";
    assert.equal(SDK_VERSION, ${JSON.stringify(version)});
    const client = new Px0({ fetch: async () => Response.json({ id: "t1", turns: [] }) });
    assert.equal((await client.threads.create({ message: "hello" })).id, "t1");
    assert.equal(typeof client.table, "function");
    assert.equal(typeof client.git.commitDetail, "function");
    assert.equal(typeof client.lsp.stop, "function");
    assert.equal(new Px0Error(409, "busy", "threads/send").status, 409);
  `);
  execFileSync(process.execPath, ["smoke.mjs"], { cwd: temp, stdio: "inherit" });
  writeFileSync(join(temp, "consumer.mts"), `
    import { Px0, SDK_VERSION, type Thread, type ThreadEvent, type TableResult, type Diff, type CommitDetail } from "@px0/sdk";
    async function consumer(client: Px0) {
      const version: string = SDK_VERSION;
      const table: TableResult = await client.table("data.csv");
      const thread: Thread = await client.threads.create({ message: "review", scope: "pr" });
      const scoped: Thread = await client.threads.scope(thread.id, "mine");
      const diff: Diff = await client.diff("a.js", { ref: "abcd" });
      const detail: CommitDetail = await client.git.commitDetail("abcd");
      const saved: { ok: true; path: string; size: number } = await client.saveFile("notes.txt", "é\\n", { timeout: 5000 });
      const file: { ok: true; path: string; type: "file" } = await client.createFile("new.txt");
      const dir: { ok: true; path: string; type: "dir" } = await client.createDirectory("new-dir");
      for await (const event of client.threads.events({ id: thread.id })) {
        const typed: ThreadEvent = event;
        if (typed.type === "thread") console.log(typed.data.turns[0].reply);
      }
      await client.updateSession({ tabs: [{ path: "a.js", ref: "abcd" }] });
      await client.lsp.startServer("all");
      await client.lsp.stop("gopls");
      console.log(version, table.cols, scoped.scope, diff.hunks?.[0].rows[0].html, detail.message, saved.size, file.type, dir.type);
    }
  `);
  execFileSync(process.execPath, [join(sdk, "node_modules/typescript/bin/tsc"), "--strict", "--noEmit", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "consumer.mts"], { cwd: temp, stdio: "inherit" });
  console.log(`Verified @px0/sdk@${version}: installed tarball, ESM import and public TypeScript declarations`);
} finally {
  rmSync(temp, { recursive: true, force: true });
}
