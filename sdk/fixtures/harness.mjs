// Deterministic harness protocol fixture: exercises px0's process runner,
// streaming, edits and cancellation without an account or a paid model.
import { appendFileSync } from "node:fs";
import { setTimeout } from "node:timers/promises";

const prompt = process.argv.at(-1) ?? "";
const command = [...prompt.matchAll(/sdk:(\w+)/g)].at(-1)?.[1];
const native = process.argv.includes("--output-format");
const emit = (event) => process.stdout.write(JSON.stringify(event) + "\n");
if (native) emit({ type: "system", session_id: "sdk-fixture-session" });
if (command === "hold") await setTimeout(60_000);
if (command === "fail") {
  process.stderr.write("sdk fixture failure\n");
  process.exit(7);
}
if (command === "edit" || command === "batch") appendFileSync("a.js", "// edited\n");
if (command === "batch") appendFileSync("b.js", "// edited too\n");
const reply = command === "message" ? "test: fixture commit" : `Reply é: ${prompt}`;
if (native) {
  emit({ type: "assistant", message: { content: [{ type: "tool_use", name: "Read", input: { file_path: "a.js" } }] } });
  await setTimeout(30);
  emit({ type: "assistant", message: { content: [{ type: "text", text: reply }] } });
  emit({ type: "result", result: reply });
} else {
  process.stdout.write(reply + "\n");
}
