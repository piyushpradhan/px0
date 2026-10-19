# Tips & Tricks System

px0 includes an interactive tips and tricks system designed to introduce developers to powerful keyboard shortcuts, Git workflows, review capabilities, and editor features without obstructing startup.

---

## Overview & Architecture

Unlike heavy IDEs that display large intrusive modals on startup, px0 loads tips asynchronously in the background.

- **Non-Intrusive Floating Widget**: Appears gently in the bottom-right corner after an initial delay (~1.8 seconds), presenting one tip at a time with previous/next navigation and automatic progression.
- **Dedicated Tips Section in IDE**: An inspector tab labeled **Tips** (`#pane-right-tips`) displays all available tips in a searchable, filterable list directly alongside your code.
- **Left Bottom Bar Help Button**: The `#btn-help` button in the sidebar footer (`side-foot`) navigates directly to the Tips section in the inspector.
- **Public URL Delivery**: Tips are loaded live from `https://px0.ai/tips.json` (hosted in the `../web` repository) and cached locally, with built-in offline fallbacks (`web/src/tips-util.js`).
- **Permanent Dismissal**: Users can permanently hide the floating tip popups at any time via the "Don't show tips" button, the inspector checkbox, or the `tips.enabled` preference in Settings (`Cmd/Ctrl+,`).

---

## How to Access Tips

1. **Floating Tip Widget**: Automatically appears shortly after opening px0 unless permanently hidden. Use **‹** and **›** to browse tips one after another.
2. **Help Button**: Click the question mark icon in the left bottom bar (beside the theme and settings buttons) to open the Tips inspector panel.
3. **Inspector Tab**: Click the **Tips** tab in the right-side inspector header.
4. **Command Palette**: Press **`Cmd/Ctrl + Shift + P`** and choose **Help: Tips & Tricks (All Tips)** or **Help: Show Tip**.
5. **Keyboard Shortcuts Sheet**: Press **`?`** to open the keyboard cheat sheet, then click **💡 Tips & Tricks**.

---

## Keeping Tips Updated (Agent & Contributor Guidelines)

> [!IMPORTANT]
> Whenever rolling out a new user-facing feature, shortcut, command, or workflow improvement, add a corresponding tip entry so users can discover it.

### Tip Entry Schema

Each tip is a JSON object with the following fields:

```json
{
  "id": "feature-slug",
  "title": "Clear and Concise Title",
  "category": "Navigation | Git | AI | Editing | Viewer | Workbench | Performance",
  "tag": "ShortTag",
  "content": "Short markdown description of the feature. Supports **bold**, `code`, links, and <kbd> tags.",
  "shortcut": "Mod+Key" // optional keyboard shortcut combo
}
```

### Steps to Add or Update a Tip

1. **Add to Public Repository**:
   Add the new tip entry to `../web/public/tips.json`.
   Run `npm run build` in `../web` to verify the Astro static site compiles cleanly.

2. **Add to Built-in Fallbacks**:
   Add the identical entry to `DEFAULT_TIPS` in `web/src/tips-util.js` within `px0` so the tip is available offline.

3. **Verify and Rebuild**:
   - Run `npm test` in `px0` to run the frontend unit tests.
   - Run `npm run check` and `npm run build` to rebuild `web/app.js`.
   - Run `go test ./...` and `make build`.
