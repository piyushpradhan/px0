# @px0/sdk

Typed TypeScript client for a running px0 server. Zero dependencies — uses the global `fetch` (Node 18+, Bun, Deno, browsers). ESM only.

```ts
import { Px0, SDK_VERSION } from "@px0/sdk";

const px0 = new Px0({ url: "http://127.0.0.1:7777/" }); // include -base-path if set
await px0.ready(); // waits through startup and indexing
console.log("SDK", SDK_VERSION, "editor", (await px0.meta()).version);

const hits = await px0.search("TODO", { glob: "*.go" });
const refs = await px0.lsp.references("server.go", 42, 10, { wait: 30_000 });

// Dispatch the selected coding harness at a line range and wait for it to finish
const job = await px0.agent.edit({ path: "server.go", l1: 40, l2: 60, instruction: "handle the nil case" });
const done = await px0.agent.wait(job.id);
console.log(done.error || done.changed);

for await (const ev of px0.events({ metrics: false })) {
  if (ev.type === "git-status") console.log(ev.data.gitChanges, "changed");
}
```

## Surface

| Area | Methods |
| --- | --- |
| Workspace | `meta` `ready` `metrics` `tree` `find` `file` `close` `raw` `text` `markdown` `table` `diff` `gutter` `search` `outline` `def` `reindex` `events` |
| Settings / session | `settings` `updateSettings` `session` `updateSession` |
| `lsp` | `definition` `references` `hover` `symbols` `calls` `expandCalls` `warm` `setup` `install` `start` `servers` `startServer` `stop` |
| `git` | `status` `log` `unpushed` `commitFiles` `commitDetail` `stage` `unstage` `commit` `pull` `push` `commitMessage` |
| `agent` | `harnesses` `select` `edit` `job` `wait` `cancel` |
| `threads` | `list` `get` `create` `send` `scope` `wait` `cancel` `delete` `events` |
| `pr` (px0 launched with a PR URL) | `meta` `comments` `drafts` `addDraft` `deleteDraft` `submit` `comment` `reply` `launch` |

Request payloads (`agent.edit`, `threads.create`, settings/session updates) are objects; other required arguments are positional. Optional call settings go in a trailing object accepting `signal` and `timeout`. Streams accept `signal` and `reconnect`. For a raw response: `px0.request(method, path, { query, body })`.

Paths are workspace-relative. Lines are 1-based; LSP columns are 0-based UTF-16 offsets.

## Threads and edit follow-ups

```ts
const thread = await px0.threads.create({
  path: "server.go", l1: 40, l2: 60,
  message: "Explain this handler", scope: "selection",
});
await px0.threads.wait(thread.id, { signal: AbortSignal.timeout(60_000) });
await px0.threads.send(thread.id, "Also check its callers");

for await (const event of px0.threads.events({ id: thread.id })) {
  if (event.type === "delta") process.stdout.write(event.data.text);
  if (event.type === "thread" && !event.data.turns.at(-1)?.running) break;
}

// Inline edits and batches use the same persisted thread model.
if (job.threadId) await px0.threads.send(job.threadId, "Also update the tests");
```

`threads.create` starts the first turn immediately; omit `path` for a workspace conversation. `threads.events()` sends `list`, `summary`, and `deleted`; passing `id` sends `thread` snapshots, `delta`, `tool`, and `deleted`. Reconnection sends a fresh snapshot. `scope(id, "pr" | "mine" | "selection")` changes PR review context for the next turn. A busy thread rejects another message with 409. Check the last turn's `error` after `wait`; stopping a turn keeps edits already written by the harness.

`agent.edit(single)` dispatches one inline edit; `agent.edit([a, b])` dispatches a batch. Both return `threadId` on current px0. Running edit ranges cannot overlap. Current px0 permits edits to dirty files; the `force` option remains accepted for older SDK callers.

## Tables, commits and language servers

```ts
const csv = await px0.table("data.csv"); // header, rows with source line numbers, cols, truncated
const pending = await px0.git.unpushed({ limit: 20 });
if (pending.available) {
  const sha = pending.commits[0].hash;
  const changed = await px0.git.commitFiles(sha);
  const detail = await px0.git.commitDetail(sha);
  const diff = await px0.diff(changed.files[0].path, { ref: sha });
  console.log(detail.message, diff.hunks); // highlighted rows, original text and line numbers
  await px0.updateSession({ tabs: [{ path: changed.files[0].path, ref: sha }], active: 0 });
}
await px0.lsp.startServer("all");
console.log(await px0.lsp.servers());
await px0.lsp.stop("gopls"); // omit the name to stop all
```

`file`, `diff`, and `gutter` accept `{ref}` for commit review. A ref selects that commit's changes; `file` still reads the current source and reports whether the commit touched it, including a zero-line response for deleted files. `diff` exposes `hunks`, plus `prHunks` and `yourHunks` in PR mode; binary diffs may have null hunks. Table cells are plain text and follow the editor's 1,000-row / 1 MiB preview limits. Markdown (including Mermaid fences) comes from the same renderer as the editor; browser-only diagram rendering, layout, keyboard controls and animations remain UI behavior.

`lsp.start(path)` keeps the existing file-based start behavior. `lsp.startServer(name)` starts a named installed server; `"all"` starts relevant installed servers. `lsp.stop()` returns the updated server list. File LSP summaries include `anyRunning`; metrics include `peakRSSBytes`. Settings include the typed schema used by the editor's settings form, including textarea fields.

## Failure modes

| What happened | Rejects with |
| --- | --- |
| px0 answered non-2xx | `Px0Error` — `.status`, `.endpoint`, server's message |
| px0 not reachable | `Px0Error` with `.status === 0`, original error in `.cause` |
| Call exceeded its timeout | `DOMException` named `TimeoutError` |
| Your `signal` aborted | the signal's reason (`AbortError` by default) |

- **Timeouts**: 30s per call by default (`new Px0({ timeout })`, `0` disables). LSP calls stretch to cover their `wait`. `git.pull`/`git.push` default to 5 min since they talk to the remote. `agent.wait` and `threads.wait` bound each poll with `timeout`; use a signal for an overall deadline. Streams have no overall timeout.
- **Cancellation**: aborting a `search` also stops the search on the server.
- **Events**: breaking out of the loop or aborting closes the connection. A stream that drops after connecting reconnects with backoff (250ms → 5s) and gets a fresh snapshot; a failed *first* connect throws. `reconnect: false` turns this off. Workspace streams also expose the editor's `ping` heartbeat.
- LSP endpoints report language-server trouble in the body (`error`, `state`) with HTTP 200, not as a rejection.

## Performance

- **No gzip on loopback.** px0 compresses whenever asked and Node's fetch always asks; on 127.0.0.1 that is pure CPU. For loopback URLs the SDK sends `Accept-Encoding: identity` (override with `compress`). Median round trips against a 57k-file workspace:

  | Call | gzip | identity |
  | --- | --- | --- |
  | `text` (113 KB) | 0.77 ms | 0.20 ms |
  | `file` (1000 lines) | 0.83 ms | 0.34 ms |
  | `tree` | 0.25 ms | 0.15 ms |
  | `find` (500 results) | 2.69 ms | 2.14 ms |

- **Linear SSE parsing.** Each chunk is scanned once; a 3.1 MB `git-status` event (100k changed files) parses in ~12 ms.
- `events({ metrics: false })` uses the git-only stream and spares the server a metrics sample every 2.5s.
- `agent.wait` polls from 50ms, backing off to 500ms.
- Use `text()` rather than `file()` when you don't need highlighted HTML, and `close()` files you are done with to free server caches.

## Writes and browsers

px0 refuses POSTs whose `Origin` isn't px0 itself. The SDK sets it, which works from Node/Bun/Deno. In a browser it only works same-origin (e.g. a script served alongside px0) — that restriction is the CSRF guard, not a bug. Browsers also ignore `Accept-Encoding`, so `compress` has no effect there.

## Develop

Use Node 24+ for the source TypeScript tests; the built package supports Node 18+.

```bash
npm ci
npm run check                    # build, unit tests, route/type drift checks
npm run test:e2e                 # bundle and build the current editor, then run all tests
npm run test:package             # install a tarball, import its ESM and compile a TS consumer
```

`PX0_BIN=/absolute/path/to/px0 npm test` tests an explicitly chosen binary. Without `PX0_BIN`, `npm test` skips real-server suites; `test:e2e` always builds and runs them, and a bad explicit binary path fails. CI builds the editor and runs every suite on pull requests and master. Package checks also run on Node 18 and 24.

The contract suite invokes a typed wrapper for every route registered in `server.go` and compares public type fields/optionality against Go JSON structs. The parity suite independently fetches the editor APIs and checks SDK responses against those results and the SDK declarations. It also checks disk/git effects, commit-pinned and deleted files, CSV limits, threads across restart, streaming and cancellation. Temporary git remotes, a deterministic Claude protocol fixture, and a small language server keep these tests repeatable and offline after dependency installation. Tests use dynamic ports, deadlines, isolated user state, and cleanup. These fixtures test px0 integration; live harness services, forge submission and downloading LSP installers require their own credentials/network checks.

## Versioning and releases

The SDK has its own semantic version in `package.json`, exported as `SDK_VERSION`; `meta().version` reports the editor binary version. SDK 0.2.0 covers the current editor API, including committed-change review from editor commit `02db0c7`. Older binaries may return 404 for newer endpoints. The [changelog](CHANGELOG.md) records SDK changes.

- Patch: compatible bug fixes. Minor: new APIs; before 1.0, breaking API/type changes also use a minor bump. From 1.0, breaking changes require a major bump.
- Run `npm version patch --no-git-tag-version` (or `minor` / an explicit version) inside `sdk/`. The version hook updates `SDK_VERSION`; npm updates the lockfile. Build and contract checks reject mismatches.
- Update `CHANGELOG.md`, run `npm run test:e2e` and `npm run test:package`, then commit the release files.
- Tag that commit `sdk-v<version>` and push the tag. The SDK release workflow verifies the version, reruns parity/package checks and attaches the npm tarball to its own GitHub release. Binary tags remain `v<version>`.
- For npm registry publication, an authorized maintainer can run `npm publish --access public` from `sdk/`; its prepublish hook requires editor parity and package checks. The GitHub workflow packages releases without requiring npm credentials.
