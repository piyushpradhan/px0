import { readFileSync, writeFileSync } from "node:fs";

const root = new URL("../", import.meta.url);
const { version } = JSON.parse(readFileSync(new URL("package.json", root), "utf8"));
const source = new URL("index.ts", root);
const text = readFileSync(source, "utf8");
const declaration = /^export const SDK_VERSION = "[^"]+";/m;
if (!declaration.test(text)) throw new Error("SDK_VERSION declaration missing");
const updated = text.replace(declaration, `export const SDK_VERSION = "${version}";`);
if (process.argv.includes("--check")) {
  if (text !== updated) throw new Error("SDK_VERSION differs from package.json; run npm run version");
} else {
  writeFileSync(source, updated);
}
