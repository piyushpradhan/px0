// Minimal deterministic language server, reached through px0's real LSP client.
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";

const uri = pathToFileURL(resolve("main.go")).href;
const range = (line, character = 5) => ({ start: { line, character }, end: { line, character: character + 5 } });
const item = (name = "hello", line = 1) => ({ name, kind: 12, uri, range: range(line), selectionRange: range(line) });
const location = { uri, range: range(1) };
function answer(message) {
  if (message.method === "exit") process.exit(0);
  if (message.id === undefined) return;
  let result = null;
  switch (message.method) {
    case "initialize": result = { capabilities: { positionEncoding: "utf-16", textDocumentSync: 1, definitionProvider: true, referencesProvider: true, hoverProvider: true, documentSymbolProvider: true, callHierarchyProvider: true } }; break;
    case "textDocument/definition": result = [location]; break;
    case "textDocument/references": result = [location, { uri, range: range(2, 18) }]; break;
    case "textDocument/hover": result = { contents: { kind: "markdown", value: "```go\nfunc hello() int\n```\n\nGreeting." } }; break;
    case "textDocument/documentSymbol": result = [item()]; break;
    case "textDocument/prepareCallHierarchy": result = [item()]; break;
    case "callHierarchy/incomingCalls": result = [{ from: item("main", 2), fromRanges: [range(2, 18)] }]; break;
    case "callHierarchy/outgoingCalls": result = [{ to: item(), fromRanges: [range(2, 18)] }]; break;
  }
  const body = JSON.stringify({ jsonrpc: "2.0", id: message.id, result });
  process.stdout.write(`Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`);
}
let buffer = Buffer.alloc(0);
process.stdin.on("data", (chunk) => {
  buffer = Buffer.concat([buffer, chunk]);
  for (;;) {
    const end = buffer.indexOf("\r\n\r\n");
    if (end < 0) return;
    const length = Number(buffer.subarray(0, end).toString().match(/Content-Length: (\d+)/i)?.[1]);
    if (buffer.length < end + 4 + length) return;
    const message = JSON.parse(buffer.subarray(end + 4, end + 4 + length).toString());
    buffer = buffer.subarray(end + 4 + length);
    answer(message);
  }
});
