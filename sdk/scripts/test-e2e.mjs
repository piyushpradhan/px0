// Always build the current editor; a missing or stale binary must not turn CI green.
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { join } from "node:path";

const repo = fileURLToPath(new URL("../../", import.meta.url));
const sdk = fileURLToPath(new URL("../", import.meta.url));
const temp = mkdtempSync(join(tmpdir(), "px0-sdk-build-"));
try {
  execFileSync(process.execPath, ["scripts/build-web.js"], { cwd: repo, stdio: "inherit" });
  const bin = join(temp, process.platform === "win32" ? "px0.exe" : "px0");
  execFileSync("go", ["build", "-o", bin, "."], { cwd: repo, stdio: "inherit" });
  execFileSync(process.execPath, ["--test", "test.ts", "contract.test.ts", "parity.test.ts"], {
    cwd: sdk,
    env: { ...process.env, PX0_BIN: bin },
    stdio: "inherit",
  });
} finally {
  rmSync(temp, { recursive: true, force: true });
}
