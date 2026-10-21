# Operational Guidelines for AI Agents

Welcome agent! This document contains essential instructions and workflows for working productively and quickly in `px0`.

---

## 1. Quick Development Workflow (Fast Inner Loop)

To keep iteration fast and avoid unnecessary delays:

> [!IMPORTANT]
> **Do NOT run tests after every intermediate change.** Running test suites repeatedly after every small edit severely slows things down. Complete all planned code and asset changes first. Run tests only once all changes are in place, and then iterate on fixing any failures until everything passes cleanly.

| Task | Command | Typical Time | Notes |
| :--- | :--- | :--- | :--- |
| **Frontend Check** | `make web-check` or `npm run check` | **<100ms** | Validates JS syntax, relative imports, and runs frontend tests. |
| **Frontend Bundle** | `make web` or `node scripts/build-web.js` | **<50ms** | Run whenever modifying files under `web/src/`. |
| **Build Binary** | `make build` | **~2s** | Bundles frontend (`web/app.js`) and compiles local `./px0` binary. |
| **Fast Verification** | `make check` or `go test -short ./...` | **~7s** | Runs frontend validation, asset bundling, and fast Go unit tests. |
| **Full Test Suite** | `make test` or `go test ./...` | **~20s** | Run once all changes are done; iterate until all tests pass before concluding. |
| **Subsystem Verification** | `make verify` or `./scripts/test-bench.sh --fast` | **~15s** | Verifies 100% package test/benchmark coverage + statement coverage. |
| **Full Verification & Bench**| `make verify-all` or `./scripts/test-bench.sh` | **~45s** | Comprehensive verification running all tests, coverage, and micro-benchmarks. |

> [!TIP]
> Isolated components in `internal/` can be tested in milliseconds without running the full test suite (e.g. `go test ./internal/table`, `go test ./internal/ignore`, `go test ./internal/fuzzy`, `go test ./internal/metrics`, `go test ./internal/render`, `go test ./internal/settings`, `go test ./internal/symbols` take ~5–20ms).

### Unified Test & Benchmark Verification Utility

`px0` provides a unified script to run all unit tests, compute statement coverage across all packages, run all micro-benchmarks, and assert **100% package test and benchmark coverage**:

```bash
# Fast verification (recommended during development): runs tests (-short) and micro-benchmarks (-benchtime=100ms)
./scripts/test-bench.sh --fast
# or:
make verify

# Check test statement coverage per package without running benchmarks
./scripts/test-bench.sh --test
# or:
make coverage

# Enforce statement coverage threshold (e.g. 60%)
./scripts/test-bench.sh --test --threshold 60

# Run full benchmarks only
./scripts/test-bench.sh --bench

# Export machine-readable metrics to JSON
./scripts/test-bench.sh --fast --json coverage-report.json
```

Whenever introducing a new Go package or subsystem, you must add both a `*_test.go` test suite and at least one micro-benchmark (`func Benchmark...`) in the package to maintain 100% package coverage. See [`docs/internals/testing-and-benchmarks.md`](docs/internals/testing-and-benchmarks.md) for full architectural documentation.

---

## 2. Codebase Organization

- **Backend Architecture**: Core HTTP/workspace orchestration lives in the root package (`package main`), while self-contained subsystems live in `internal/` packages:
  - [`internal/fuzzy/`](internal/fuzzy/): Bounded two-pass fuzzy path finder and scoring algorithm
  - [`internal/ignore/`](internal/ignore/): `.gitignore` parsing, fnmatch globbing, and ignore trees
  - [`internal/metrics/`](internal/metrics/): System resource usage, RSS, CPU sampling, and OS-specific rusage
  - [`internal/render/`](internal/render/): Syntax highlighting (Chroma), diff highlighting, and GitHub-Flavored Markdown rendering
  - [`internal/settings/`](internal/settings/): Persistent user settings (`settings.json`), schema definitions, and merging
  - [`internal/symbols/`](internal/symbols/): Regex-based symbol outline extraction and definition classification
  - [`internal/table/`](internal/table/): Tabular formatting and delimiter sniffing (CSV/TSV)
  - [`internal/telemetry/`](internal/telemetry/): Anonymous PostHog telemetry client and persistence
  - [`internal/ui/`](internal/ui/): Terminal styling, ANSI status logging, spinners, and OS-specific TTY detection
  - [`internal/update/`](internal/update/): Self-update version checking, binary download, and atomic replacement
  Assets are statically embedded via `//go:embed` in `server.go`.
- **Frontend Architecture**: Modular ES modules reside in [`web/src/`](web/src/) and are bundled into [`web/app.js`](web/app.js) via [`scripts/build-web.js`](scripts/build-web.js). Checked via [`scripts/check-web.js`](scripts/check-web.js).
- **Benchmark Corpus**: Heavy repositories for benchmarking live in `../bench-repos` (outside the project root) to keep workspace searches and indexers fast.
- **Detailed Map**: For a complete file catalog and UI element mapping, see [`docs/agents/README.md`](docs/agents/README.md).

---

## 3. Core Architectural Tenets

1. **Reads First, Edits via Harness**: `px0` is an ultra-fast code reader and reviewer. File modifications are delegated to external coding harnesses (`agent.go`), never authored directly to disk by `px0` (with the exception of explicit user-initiated Git operations in the sidebar).
2. **Zero Runtime Dependencies**: The output must remain a single, standalone static binary. No CGO, no Node.js runtime requirement for the user, no external databases.
3. **No Workspace Litter**: `px0` never creates `.px0/` folders or cache files inside a workspace tree. Persistent configuration and thread transcripts live in `$XDG_CONFIG_HOME/px0/` or `~/.px0/`; per-workspace session files (PR review drafts) and the workbench store live in `$XDG_STATE_HOME/px0/` or `~/.px0/`.
4. **Fast & Non-Blocking**: Operations must remain responsive. All background tasks, network calls, and git operations must respect timeouts and set `GIT_TERMINAL_PROMPT=0`.

---

## 4. Documentation & Tips Policy

- **During Iterative Work**: Focus on completing code changes first before running verifications. Do not block your workflow with premature documentation edits.
- **On Feature Completion**: If you introduce a new feature, CLI flag, shortcut, or major architectural change, update the corresponding documentation:
  - CLI flags / Shortcuts -> [`README.md`](README.md)
  - Major architectural patterns -> [`docs/internals/`](docs/internals/)
  - User features -> [`docs/features/`](docs/features/)
- **Keeping Tips & Tricks Updated**: Whenever rolling out an important user-facing feature, shortcut, command, or workflow improvement:
  - Add a concise, helpful tip entry to both:
    1. [`../web/public/tips.json`](../web/public/tips.json): Served publicly at `https://px0.ai/tips.json`.
    2. [`web/src/tips-util.js`](web/src/tips-util.js): Built-in offline fallback in `DEFAULT_TIPS`.
  - Tips use the standard schema:
    ```json
    {
      "id": "feature-slug",
      "title": "Concise Feature Title",
      "category": "Navigation | Git | AI | Editing | Viewer | Workbench | Performance",
      "tag": "TagPill",
      "content": "One to two sentences explaining the feature with **bold**, `code`, or <kbd> tags.",
      "shortcut": "Mod+Key" // optional
    }
    ```
  - Rebundle frontend assets via `make web` (`npm run build`) and run `npm run check`.

---

## 5. Version Bump & Release Protocol

When committing a version bump (triggered after updating the `VERSION` file):
1. **Verify Release Pipeline**:
   - Verify frontend bundling: `node ./scripts/build-web.js`
   - Verify full test suite: `make test`
   - Verify builds: `build.sh` / `.github/workflows/release.yml`
2. **Commit Release Fixes First**: If bundlers or scripts need fixes, commit those separately first.
3. **Commit Version Bump**: Stage `VERSION` with the new version number using the standard release commit message.
