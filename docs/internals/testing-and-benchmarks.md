# Testing, Micro-Benchmarks & Subsystem Coverage

This document outlines the testing architecture, benchmark methodology, coverage standards, and automated verification tools in `px0`.

---

## 1. Core Testing Philosophy

`px0` is engineered to deliver sub-millisecond responsiveness with a minimal memory footprint (~20–30 MB RSS). To protect both correctness and performance across every commit:

1. **100% Subsystem & Package Coverage**: Every Go package in the repository (the root package and all `internal/*` packages) maintains an active, automated unit test suite and micro-benchmark suite. No package is allowed to remain without tests or benchmarks.
2. **Zero Runtime Dependencies**: All tests run out of the box using pure Go (`go test`). No external services, databases, or containerized daemons are needed.
3. **Allocation & Latency Guardrails**: Critical path routines (fuzzy search, ignore pattern matching, table sniffing, syntax tokenization, terminal output painting) are accompanied by micro-benchmarks measuring both execution latency (`ns/op`) and heap allocations (`B/op`, `allocs/op`).
4. **End-to-End Real World Validation**: In addition to package unit tests and micro-benchmarks, macroscopic performance is validated against real-world repositories (spanning Flask to the Linux kernel) via [`benchmark.sh`](../../benchmark.sh).

---

## 2. Package & Subsystem Coverage Breakdown

All 11 packages in `px0` achieve **100% Package Test and Benchmark Coverage**:

| Package | Statement Coverage | Test Files | Primary Focus & Capabilities Tested |
| :--- | :--- | :--- | :--- |
| **`px0` (root core)** | **>62%** (54% short) | 7 test files | HTTP server routing, Git porcelain/diff parsing, LSP client, multi-repo workbench, forge providers (GitHub, GitLab, Gitea, Bitbucket), chat threads, AI agent harnesses. |
| **`internal/fuzzy`** | **100.0%** | `fuzzy_test.go` | Two-pass bounded fuzzy path finder, score calculation, min-heap top-K pruning (`worseResult`, `siftDownResult`), boundary detection. |
| **`internal/symbols`**| **100.0%** | `symbols_test.go`| Multi-language outline parser (Go, Python, JS/TS, Rust, C/C++, Java, Ruby, Shell, PHP, Lua, Zig, Elixir, Markdown), symbol regex trees, indent calculation. |
| **`internal/table`**  | **100.0%** | `table_test.go`  | CSV/TSV table rendering, ragged rows, quoted cell line wrapping, delimiter sniffing (`SniffDelim`). |
| **`internal/render`** | **80.9%** | `render_test.go` | Chroma syntax highlighting, windowed viewport lexing, unified diff coloration, GitHub-Flavored Markdown transformation. |
| **`internal/ignore`** | **98.4%** | `ignore_test.go` | Gitignore pattern compilation, fast-path segment shortcuts matching regex references, directory-only rule filtering. |
| **`internal/telemetry`** | **87.4%** | `telemetry_test.go` | PostHog telemetry event dispatch, session lifecycle, anonymous device ID management, file bucket categorization. |
| **`internal/settings`**| **67.0%** | `settings_test.go`| Persistent settings schema, atomic file saving, deep map merging, raw JSON round-trip validation. |
| **`internal/ui`**     | **66.9%** | `ui_test.go`      | ANSI color painting, TTY detection, spinner animation, status headers, byte/duration formatting helpers. |
| **`internal/metrics`**| **63.4%** | `metrics_test.go` | Process resident set size (RSS), peak memory tracking, CPU usage sampling across platforms. |
| **`internal/update`** | **58.9%** | `update_test.go`  | Version semver comparison, GitHub release manifest fetching, SHA256 checksum verification, atomic binary replacement. |

> [!NOTE]
> In Go, 100.0% line/statement coverage on a full application is blocked by unexecutable dead branches (e.g. fatal `os.Exit(1)` handlers in CLI `main()`, OS signal loops in server shutdown, unreachable filesystem syscall error handlers, and alternate platform files like `metrics_unix.go` vs `metrics_windows.go`).
> `px0` enforces **100% Package and Feature Coverage** across all packages with high statement coverage on testable subsystems.

---

## 3. Micro-Benchmarks & Allocation Profiles

px0 includes **over 80 micro-benchmarks** executing across all packages to catch performance regressions and allocation bloat:

```bash
# Run all micro-benchmarks across every package
make verify-all

# Or run with fast iteration (100ms sample window per benchmark)
make verify
```

### Critical Zero-Allocation Paths

Several core algorithms are tested to guarantee zero heap allocations (`0 B/op`, `0 allocs/op`) during steady-state execution:
- `BenchmarkFuzzyScore`: Character-by-character fuzzy scoring.
- `BenchmarkIgnoreSetMatch`: Path filtering through compiled ignore trees.
- `BenchmarkFamilyFor`: Extension-to-language family mapping.
- `BenchmarkPaint`: Terminal styling when outputting ANSI color sequences.
- `BenchmarkFilesBucket`: Telemetry path count categorization.

---

## 4. Unified Verification Runner: `scripts/test-bench.sh`

[`scripts/test-bench.sh`](../../scripts/test-bench.sh) is the primary programmatic utility for measuring tests, statement coverage, and micro-benchmarks.

### CLI Options

| Flag | Description | Typical Use Case |
| :--- | :--- | :--- |
| `(none)` or `--all` | Runs full test suite and all micro-benchmarks across all 11 packages | Pre-release and CI validation |
| `--fast` | Runs unit tests with `-short` and benchmarks with `-benchtime=100ms` | Fast inner-loop verification (~15s) |
| `--test` | Runs tests and computes statement coverage only (skips benchmarks) | Unit testing & coverage inspection |
| `--bench` | Runs micro-benchmarks only across all packages | Performance tuning & profiling |
| `--threshold <N>` | Fails (exit code 1) if overall statement coverage is below `N%` | Coverage gates in CI |
| `--json [file]` | Exports comprehensive test and benchmark metrics to JSON file (or stdout if `-`) | Machine ingestion / metrics tracking |
| `-v, --verbose` | Streams raw `go test` output in real time | Debugging individual test failures |

### Makefile Shortcuts

```bash
make verify       # Fast inner loop: runs test suite (-short) + benchmarks (-benchtime=100ms)
make verify-all   # Full verification suite
make coverage     # Runs test suite and prints per-package statement coverage
make test-short   # Standard short unit test pass
```

### Programmatic JSON Output Schema

When `--json <file>` (or `--json -`) is supplied, the runner produces structured data:

```json
{
  "summary": {
    "packages_total": 11,
    "packages_with_tests": 11,
    "packages_with_benchmarks": 11,
    "package_test_coverage_pct": 100.0,
    "package_benchmark_coverage_pct": 100.0,
    "statement_coverage_pct": 57.3,
    "covered_statements": 5982,
    "total_statements": 10442,
    "tests_ran": 347,
    "tests_passed": 347,
    "tests_failed": 0,
    "benchmarks_ran": 80
  },
  "packages": {
    "px0": {
      "coverage_pct": 54.2,
      "tests": 283,
      "test_status": "PASS",
      "benchmarks": 56,
      "bench_status": "PASS"
    },
    "px0/internal/symbols": {
      "coverage_pct": 98.4,
      "tests": 6,
      "test_status": "PASS",
      "benchmarks": 5,
      "bench_status": "PASS"
    }
  }
}
```

---

## 5. End-to-End Macroscopic Benchmarks

In addition to in-process micro-benchmarks, macroscopic performance is benchmarked against real-world repositories via [`benchmark.sh`](../../benchmark.sh).

```bash
# Clone the 7-repo benchmark corpus (~3 GB)
./benchmark.sh --clone

# Run standard benchmarks across all repositories
./benchmark.sh

# Run comparative benchmark vs baseline JSON
./benchmark.sh --compare baseline.json
```

See [`BENCHMARKS.md`](../../BENCHMARKS.md) for full benchmark results, memory traces, and comparisons against VS Code and Neovim.
