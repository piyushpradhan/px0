# SDK changelog

SDK versions are independent of px0 binary versions. Release tags use `sdk-v<version>`.

## 0.2.0

- Expose persistent threads: create, list, read, send, wait, scope, cancel, delete, and live transcript/list events.
- Link inline and batch edit jobs to their persisted threads through `threadId`.
- Expose CSV/TSV table previews, unpushed commits, commit files, and commit details.
- Accept commit refs for file, diff, and gutter reads; preserve refs in session tabs.
- Type highlighted diff hunks, deleted files, peak memory, LSP activity, and settings schemas.
- Expose language-server status, start by name/all, and stop controls.
- Send the SSE Accept header for thread streams, including when gzip is enabled.
- Add API/JSON contract checks, real-editor parity tests, and installed-package checks.
- Prevent background editor git status reads from locking the index during SDK/editor commits.
- Add `SDK_VERSION`, an npm version hook, and a separate SDK release workflow.

## 0.1.9

- Initial typed HTTP client for workspace reads, settings/session, LSP, git, agent editing, PR reviews, and workspace events.
