import { spawn, execFileSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve, delimiter } from "node:path";
import { fileURLToPath } from "node:url";
import { once } from "node:events";
import { Px0 } from "./index.ts";

export const bin = process.env.PX0_BIN && resolve(process.env.PX0_BIN);
if (bin && !existsSync(bin)) throw new Error(`PX0_BIN does not exist: ${bin}`);

export function editorFixture() {
  const temp = mkdtempSync(join(tmpdir(), "px0-sdk-"));
  const root = join(temp, "workspace");
  const home = join(temp, "home");
  const commands = join(temp, "bin");
  for (const dir of [root, home, commands]) mkdirSync(dir);
  const env = {
    ...process.env, HOME: home, XDG_CONFIG_HOME: join(home, "config"), XDG_STATE_HOME: join(home, "state"),
    XDG_CACHE_HOME: join(home, "cache"), GIT_CONFIG_GLOBAL: join(home, "gitconfig"), GIT_CONFIG_NOSYSTEM: "1",
    GIT_AUTHOR_NAME: "sdk", GIT_AUTHOR_EMAIL: "sdk@example.test", GIT_COMMITTER_NAME: "sdk", GIT_COMMITTER_EMAIL: "sdk@example.test",
    PATH: commands + delimiter + process.env.PATH,
  };
  writeFileSync(env.GIT_CONFIG_GLOBAL, "");
  const git = (...args: string[]) => execFileSync("git", ["-C", root, ...args], { env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  for (const [name, fixture] of [["claude", "harness.mjs"], ["gopls", "lsp.mjs"]]) {
    const script = fileURLToPath(new URL(`fixtures/${fixture}`, import.meta.url));
    writeFileSync(join(commands, name), `#!/bin/sh\nexec '${process.execPath.replaceAll("'", "'\\''")}' '${script.replaceAll("'", "'\\''")}' "$@"\n`, { mode: 0o755 });
  }
  git("init", "-q", "-b", "main");
  let proc: ChildProcess | undefined;
  let output = "";
  let px0: Px0;
  async function start(o: { basePath?: string; lsp?: boolean; agent?: boolean } = {}) {
    if (!bin) throw new Error("set PX0_BIN or run npm run test:e2e");
    output = "";
    const args = ["-host", "127.0.0.1", "-port", "0", "-no-open", "-no-telemetry", "-base-path", o.basePath ?? "/"];
    if (!o.lsp) args.push("-no-lsp");
    if (o.agent === false) args.push("-no-agent");
    else args.push("-agent", "claude");
    args.push(root);
    proc = spawn(bin, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    const address = await new Promise<string>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`px0 startup timed out:\n${output}`)), 10_000);
      const cleanup = () => { clearTimeout(timer); proc!.off("error", failed); proc!.off("exit", exited); };
      const failed = (error: Error) => { cleanup(); reject(error); };
      const exited = (code: number | null) => failed(new Error(`px0 exited (${code}):\n${output}`));
      proc!.on("error", failed);
      proc!.on("exit", exited);
      proc!.stderr!.on("data", (chunk) => { output += chunk; });
      proc!.stdout!.on("data", (chunk) => {
        output += chunk;
        const match = output.match(/http:\/\/127\.0\.0\.1:\d+(?:\/[^\s\x1b]*)?/);
        if (match) { cleanup(); resolve(match[0]); }
      });
    });
    px0 = new Px0({ url: address });
    await px0.ready({ timeout: 10_000 });
    return px0;
  }
  async function stop() {
    if (!proc || proc.exitCode !== null || proc.signalCode !== null) return;
    const exited = once(proc, "exit");
    proc.kill("SIGINT");
    const kill = setTimeout(() => proc?.kill("SIGKILL"), 3_000);
    try { await exited; } finally { clearTimeout(kill); }
  }
  return {
    root, temp, git, start, stop,
    get px0() { return px0; },
    async dispose() { await stop(); rmSync(temp, { recursive: true, force: true }); },
    async direct(path: string, query: Record<string, string | number> = {}, body?: unknown) {
      const url = new URL("api/" + path, px0.url);
      for (const [key, value] of Object.entries(query)) url.searchParams.set(key, String(value));
      const response = await fetch(url, body === undefined ? undefined : {
        method: "POST", headers: { Origin: url.origin, "Content-Type": "application/json" }, body: JSON.stringify(body),
      });
      if (!response.ok) throw new Error(`${url}: ${response.status} ${await response.text()}`);
      return response.json();
    },
  };
}
