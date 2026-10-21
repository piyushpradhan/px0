# Direct File Editing & Creation

Implementation notes for in-browser editing. User-facing behavior is in [Direct File Editing](../features/file-editing.md).

## 1. Endpoints

Both are `POST` with a JSON body and are resolved through `safePath`, so paths outside the workspace are rejected.

| Endpoint | Body | Behavior |
| :--- | :--- | :--- |
| `/api/file/save` | `{path, content}` | Creates missing parent directories, refuses a directory target, writes the file (`0644`). Then `Evict` drops the highlight cache entry, the LSP document is closed and open documents refreshed, the git watcher is triggered, and the index rebuilds in the background. Returns `{ok, path, size}`. |
| `/api/fs/new` | `{path, type}` | `type` is `file` or `dir`. Returns `409` if the path exists. Creates parents, writes an empty file or the directory, triggers the git watcher, rebuilds the index. Returns `{ok, path, type}`. |

Handlers are `handleFileSave` and `handleFSNew` in [`server.go`](../../server.go); tests are in [`px0_test.go`](../../px0_test.go) (save, creation, conflict, traversal).

## 2. Frontend

[`web/src/editview.js`](../../web/src/editview.js) owns the edit surface: a `<textarea id="editview-textarea">` and a gutter, inside `#editview`, which sits over the virtualized viewport (`vp` is hidden while editing).

- **State** lives on the tab doc: `isEditing`, `dirty`, `editContent` (the working text), `editScrollTop`.
- **Text swap.** `loadedDoc` tracks whose text is in the textarea. `syncEditUI()` calls `loadEditContent()` whenever the active doc is editing, which stores the outgoing doc's text, loads the incoming doc's `editContent`, and restores its scroll. Every path that changes the active tab (`openFile`, `switchTab`, `closeTab`) calls `syncEditUI()`, so none of them needs to copy text itself.
- **Diff requests.** `openFile` with `view: 'diff'` on an editing doc clears `isEditing` first, so the diff view is not covered by the editor. `editContent` is kept.
- **Gutter.** Without wrap, line numbers are one text node and `updateGutter()` skips updates when the line count is unchanged. With wrap (`body.word-wrap`), `updateGutter()` coalesces work into `requestAnimationFrame`, reuses existing row nodes in the hidden mirror element (`#editview-mirror`, same font, padding, and width as the textarea) and `#editview-gutter`, and gives each gutter number its corresponding line's measured height. It reruns on input, on textarea resize (`ResizeObserver`), and when `word-wrap` or `hide-line-numbers` changes on `document.body`.
- **Keys.** Tab, Enter auto-indent, `Mod+S`, and `Esc` are handled on the textarea; edits go through `document.execCommand('insertText')` so the browser's native undo stack is kept.
- **Markdown and table preview.** `previewing()` ([`markdown.js`](../../web/src/markdown.js)) is false while a doc is editing, and `enterEditMode()` calls `syncPreview()`, so the preview is hidden through `markdown.js` rather than by hiding `#mdview` directly. Otherwise `mdShown` would still name the doc, and on exit `syncPreview()` would see no change and leave the preview hidden. A hidden `#mdview` also measures every block at 0, so `previewTopLine()` would return the last line. The Source/Preview switch is hidden and `togglePreview()` does nothing while editing.
- **Where editing starts and ends.** `enterEditMode()` starts at the preview's top line (`previewTopLine()`) when the preview is showing, else at `d.cur`. `exitEditMode()` takes the line at the top of the textarea (`editTopLine()`, which reads the wrapped gutter rows' offsets when wrap is on), sets `d.cur`, the viewport scroll and `d.mdLine` from it, and calls `reloadOpenTabs()`. The reload replaces the doc, so the source and the preview are both refetched after a save; `mdLine` is carried over so the preview lands on that line.
- **After save.** `saveActiveFile()` clears `dirty`, updates `size`/`total`, reloads the diff gutter, and calls `triggerRefresh()` ([`gitstream.js`](../../web/src/gitstream.js)) so the git panel does not wait for the next stream tick. [`newitem.js`](../../web/src/newitem.js) does the same after creating an item.

`Mod+E` and `Mod+S` are registered in [`shortcuts.js`](../../web/src/shortcuts.js); the palette commands are in [`palette.js`](../../web/src/palette.js). Tab close and `beforeunload` guards for dirty tabs are in [`tabs.js`](../../web/src/tabs.js).
