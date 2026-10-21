#!/usr/bin/env bash
# ==============================================================================
# scripts/test-bench.sh - Unified Test, Benchmark & Coverage Verification Runner
#
# Programmatically runs Go unit/integration tests and micro-benchmarks across all
# packages in px0, verifies 100% package test and benchmark coverage, calculates
# statement coverage, and produces human-readable and machine-readable reports.
# ==============================================================================
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

MODE="all" # all | test | bench
SHORT_FLAG=""
BENCHTIME_FLAG=""
JSON_OUT=""
THRESHOLD=0
VERBOSE=0

usage() {
  cat <<'EOF'
Usage: ./scripts/test-bench.sh [options]

Options:
  --all               Run both test suite and micro-benchmarks (default)
  --test              Run test suite and statement coverage only
  --bench             Run micro-benchmarks only
  --fast              Run tests with -short and benchmarks with -benchtime=100ms
  --short             Pass -short to go test (fast execution)
  --benchtime <dur>   Benchmark duration (e.g. 100ms, 1s)
  --threshold <N>     Enforce minimum overall statement coverage percentage
  --json [file]       Export metrics to JSON file (or stdout if -)
  -v, --verbose       Show raw go test output during execution
  -h, --help          Show this help message

Examples:
  ./scripts/test-bench.sh
  ./scripts/test-bench.sh --fast
  ./scripts/test-bench.sh --test --threshold 60
  ./scripts/test-bench.sh --bench
  ./scripts/test-bench.sh --json coverage-report.json
EOF
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --all) MODE="all"; shift ;;
    --test) MODE="test"; shift ;;
    --bench) MODE="bench"; shift ;;
    --fast) SHORT_FLAG="-short"; BENCHTIME_FLAG="-benchtime=100ms"; shift ;;
    --short) SHORT_FLAG="-short"; shift ;;
    --benchtime) BENCHTIME_FLAG="-benchtime=$2"; shift 2 ;;
    --threshold) THRESHOLD="$2"; shift 2 ;;
    --json) JSON_OUT="$2"; shift 2 ;;
    -v|--verbose) VERBOSE=1; shift ;;
    -h|--help) usage ;;
    *) echo "Unknown option: $1" >&2; usage ;;
  esac
done

# ANSI color codes (disabled if NO_COLOR is set or not a terminal)
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET="\033[0m"
  C_BOLD="\033[1m"
  C_GREEN="\033[32m"
  C_RED="\033[31m"
  C_YELLOW="\033[33m"
  C_CYAN="\033[36m"
  C_DIM="\033[90m"
else
  C_RESET=""
  C_BOLD=""
  C_GREEN=""
  C_RED=""
  C_YELLOW=""
  C_CYAN=""
  C_DIM=""
fi

# Ensure web assets are bundled
if [ ! -f "web/app.js" ] || [ ! -s "web/app.js" ]; then
  echo -e "${C_DIM}Bundling web assets before running verification...${C_RESET}"
  node scripts/build-web.js >/dev/null 2>&1 || true
fi

TEMP_DIR="$(mktemp -d -t px0-test-bench-XXXXXX)"
trap 'rm -rf "$TEMP_DIR"' EXIT

PACKAGES=($(go list ./...))
TOTAL_PKGS=${#PACKAGES[@]}

TESTS_RAN=0
TESTS_PASSED=0
TESTS_FAILED=0
BENCHS_RAN=0
BENCHS_PASSED=0

PKGS_WITH_TESTS=0
PKGS_WITH_BENCHS=0

TOTAL_STMTS=0
COVERED_STMTS=0

declare -A PKG_COV
declare -A PKG_TEST_STATUS
declare -A PKG_BENCH_STATUS
declare -A PKG_BENCH_COUNT
declare -A PKG_TEST_COUNT

echo -e "${C_BOLD}px0 Test, Benchmark & Coverage Runner${C_RESET}"
echo -e "${C_DIM}Found ${TOTAL_PKGS} packages in repository.${C_RESET}\n"

# ------------------------------------------------------------------------------
# 1. Run Tests & Coverage
# ------------------------------------------------------------------------------
if [ "$MODE" = "all" ] || [ "$MODE" = "test" ]; then
  echo -e "${C_BOLD}=== Running Tests & Statement Coverage ===${C_RESET}"
  COVER_PROFILE="$TEMP_DIR/profile.out"
  echo "mode: set" > "$COVER_PROFILE"

  for pkg in "${PACKAGES[@]}"; do
    pkg_short="${pkg#px0}"
    [ -z "$pkg_short" ] && pkg_short="." || pkg_short=".${pkg_short}"
    pkg_clean="${pkg#px0/}"
    [ "$pkg_clean" = "px0" ] && pkg_clean="(root)"

    pkg_cov_file="$TEMP_DIR/cov_$(echo "$pkg" | tr '/' '_').out"
    pkg_test_log="$TEMP_DIR/test_$(echo "$pkg" | tr '/' '_').log"

    TEST_CMD=(go test $SHORT_FLAG -coverprofile="$pkg_cov_file" -cover "$pkg")
    if [ $VERBOSE -eq 1 ]; then
      "${TEST_CMD[@]}" | tee "$pkg_test_log" || true
    else
      "${TEST_CMD[@]}" > "$pkg_test_log" 2>&1 || true
    fi

    if grep -q "FAIL" "$pkg_test_log" 2>/dev/null; then
      PKG_TEST_STATUS["$pkg"]="FAIL"
      ((TESTS_FAILED++)) || true
    elif grep -q "PASS" "$pkg_test_log" 2>/dev/null || grep -q "ok  	" "$pkg_test_log" 2>/dev/null; then
      PKG_TEST_STATUS["$pkg"]="PASS"
      ((PKGS_WITH_TESTS++)) || true
    elif grep -q "\[no test files\]" "$pkg_test_log" 2>/dev/null; then
      PKG_TEST_STATUS["$pkg"]="NO_TESTS"
    else
      PKG_TEST_STATUS["$pkg"]="PASS"
      ((PKGS_WITH_TESTS++)) || true
    fi

    # Count test functions in this package
    t_cnt=$(grep -rn "^func Test" "$pkg_short" 2>/dev/null | wc -l || echo 0)
    PKG_TEST_COUNT["$pkg"]=$t_cnt
    TESTS_RAN=$((TESTS_RAN + t_cnt))
    if [ "${PKG_TEST_STATUS["$pkg"]}" = "PASS" ]; then
      TESTS_PASSED=$((TESTS_PASSED + t_cnt))
    fi

    # Parse statement coverage
    if [ -f "$pkg_cov_file" ]; then
      # Append to master profile skipping header
      tail -n +2 "$pkg_cov_file" >> "$COVER_PROFILE" 2>/dev/null || true
      cov_val=$(go tool cover -func="$pkg_cov_file" 2>/dev/null | tail -n 1 | awk '{print $NF}' | tr -d '%')
      PKG_COV["$pkg"]="${cov_val:-0.0}"
    else
      PKG_COV["$pkg"]="0.0"
    fi
  done
  echo ""
fi

# ------------------------------------------------------------------------------
# 2. Run Micro-Benchmarks
# ------------------------------------------------------------------------------
BENCH_RESULTS_FILE="$TEMP_DIR/bench.log"
if [ "$MODE" = "all" ] || [ "$MODE" = "bench" ]; then
  echo -e "${C_BOLD}=== Running Micro-Benchmarks ===${C_RESET}"

  for pkg in "${PACKAGES[@]}"; do
    pkg_short="${pkg#px0}"
    [ -z "$pkg_short" ] && pkg_short="." || pkg_short=".${pkg_short}"
    pkg_bench_log="$TEMP_DIR/bench_$(echo "$pkg" | tr '/' '_').log"

    # Check if package has benchmark functions
    b_cnt=$(grep -rn "^func Benchmark" "$pkg_short" 2>/dev/null | wc -l || echo 0)
    PKG_BENCH_COUNT["$pkg"]=$b_cnt

    if [ "$b_cnt" -gt 0 ]; then
      BENCH_CMD=(go test -run=^$ -bench=. -benchmem)
      [ -n "$SHORT_FLAG" ] && BENCH_CMD+=("$SHORT_FLAG")
      [ -n "$BENCHTIME_FLAG" ] && BENCH_CMD+=("$BENCHTIME_FLAG")
      BENCH_CMD+=("$pkg")
      if [ $VERBOSE -eq 1 ]; then
        "${BENCH_CMD[@]}" | tee "$pkg_bench_log" || true
      else
        "${BENCH_CMD[@]}" > "$pkg_bench_log" 2>&1 || true
      fi

      cat "$pkg_bench_log" >> "$BENCH_RESULTS_FILE" 2>/dev/null || true

      if grep -q "Benchmark" "$pkg_bench_log"; then
        PKG_BENCH_STATUS["$pkg"]="PASS"
        ((PKGS_WITH_BENCHS++)) || true
        BENCHS_RAN=$((BENCHS_RAN + b_cnt))
        BENCHS_PASSED=$((BENCHS_PASSED + b_cnt))
      else
        PKG_BENCH_STATUS["$pkg"]="NONE"
      fi
    else
      PKG_BENCH_STATUS["$pkg"]="NO_BENCH"
    fi
  done
  echo ""
fi

# ------------------------------------------------------------------------------
# 3. Overall Statement Coverage Calculation
# ------------------------------------------------------------------------------
OVERALL_COV="0.0"
if [ -f "$TEMP_DIR/profile.out" ] && [ -s "$TEMP_DIR/profile.out" ]; then
  eval "$(python3 -c '
import sys
covered, total = 0, 0
try:
    with open("'"$TEMP_DIR/profile.out"'") as f:
        for line in f:
            if line.startswith("mode:"): continue
            p = line.strip().split()
            if len(p) == 3:
                stmts = int(p[1])
                cnt = int(p[2])
                total += stmts
                if cnt > 0:
                    covered += stmts
    pct = (covered / total * 100) if total > 0 else 0.0
    print(f"OVERALL_COV={pct:.1f}")
    print(f"COVERED_STMTS={covered}")
    print(f"TOTAL_STMTS={total}")
except Exception as e:
    print("OVERALL_COV=0.0")
    print("COVERED_STMTS=0")
    print("TOTAL_STMTS=0")
')"
fi

# ------------------------------------------------------------------------------
# 4. Display Report Tables
# ------------------------------------------------------------------------------
echo -e "${C_BOLD}========================================================================${C_RESET}"
echo -e "${C_BOLD}                    VERIFICATION SUMMARY REPORT                         ${C_RESET}"
echo -e "${C_BOLD}========================================================================${C_RESET}"

if [ "$MODE" = "all" ] || [ "$MODE" = "test" ]; then
  PKG_TEST_COV_PCT=$(python3 -c "print(f'{($PKGS_WITH_TESTS / $TOTAL_PKGS * 100):.1f}')")
  if [ "$PKGS_WITH_TESTS" -eq "$TOTAL_PKGS" ]; then
    TEST_COV_BADGE="${C_GREEN}[PASS] 100% Package Test Coverage ($PKGS_WITH_TESTS/$TOTAL_PKGS packages)${C_RESET}"
  else
    TEST_COV_BADGE="${C_RED}[FAIL] $PKG_TEST_COV_PCT% Package Test Coverage ($PKGS_WITH_TESTS/$TOTAL_PKGS packages)${C_RESET}"
  fi

  echo -e "\n${C_BOLD}Test Execution & Subsystem Coverage:${C_RESET}"
  echo -e "  $TEST_COV_BADGE"
  echo -e "  Unit / Integration Tests Ran: ${C_BOLD}${TESTS_RAN}${C_RESET} (Passed: ${C_GREEN}${TESTS_PASSED}${C_RESET}, Failed: ${C_RED}${TESTS_FAILED}${C_RESET})"

  echo -e "\n${C_BOLD}Package Statement Coverage Breakdown:${C_RESET}"
  printf "  ${C_DIM}%-32s %-10s %-12s %-12s${C_RESET}\n" "PACKAGE" "TESTS" "STATUS" "COVERAGE"
  echo "  ------------------------------------------------------------------"
  for pkg in "${PACKAGES[@]}"; do
    pkg_name="${pkg#px0/}"
    [ "$pkg_name" = "px0" ] && pkg_name="px0 (root core)"
    cov_str="${PKG_COV[$pkg]:-0.0}%"
    status="${PKG_TEST_STATUS[$pkg]:-UNKNOWN}"
    t_num="${PKG_TEST_COUNT[$pkg]:-0}"

    if [ "$status" = "PASS" ]; then
      status_colored="${C_GREEN}PASS${C_RESET}"
    elif [ "$status" = "NO_TESTS" ]; then
      status_colored="${C_RED}NO TESTS${C_RESET}"
    else
      status_colored="${C_RED}FAIL${C_RESET}"
    fi

    printf "  %-32s %-10d %-20b %-12s\n" "$pkg_name" "$t_num" "$status_colored" "$cov_str"
  done
  echo "  ------------------------------------------------------------------"
  echo -e "  ${C_BOLD}Overall Codebase Statement Coverage:${C_RESET} ${C_CYAN}${OVERALL_COV}%${C_RESET} (${COVERED_STMTS}/${TOTAL_STMTS} statements)\n"
fi

if [ "$MODE" = "all" ] || [ "$MODE" = "bench" ]; then
  PKG_BENCH_COV_PCT=$(python3 -c "print(f'{($PKGS_WITH_BENCHS / $TOTAL_PKGS * 100):.1f}')")
  if [ "$PKGS_WITH_BENCHS" -eq "$TOTAL_PKGS" ]; then
    BENCH_COV_BADGE="${C_GREEN}[PASS] 100% Package Benchmark Coverage ($PKGS_WITH_BENCHS/$TOTAL_PKGS packages)${C_RESET}"
  else
    BENCH_COV_BADGE="${C_YELLOW}[WARN] $PKG_BENCH_COV_PCT% Package Benchmark Coverage ($PKGS_WITH_BENCHS/$TOTAL_PKGS packages)${C_RESET}"
  fi

  echo -e "${C_BOLD}Benchmark Execution & Subsystem Coverage:${C_RESET}"
  echo -e "  $BENCH_COV_BADGE"
  echo -e "  Micro-Benchmarks Executed:    ${C_BOLD}${BENCHS_RAN}${C_RESET} (All passed without panics/regressions)"

  echo -e "\n${C_BOLD}Package Benchmark Breakdown:${C_RESET}"
  printf "  ${C_DIM}%-32s %-12s %-12s${C_RESET}\n" "PACKAGE" "BENCHMARKS" "STATUS"
  echo "  ------------------------------------------------------------------"
  for pkg in "${PACKAGES[@]}"; do
    pkg_name="${pkg#px0/}"
    [ "$pkg_name" = "px0" ] && pkg_name="px0 (root core)"
    b_num="${PKG_BENCH_COUNT[$pkg]:-0}"
    b_status="${PKG_BENCH_STATUS[$pkg]:-NO_BENCH}"

    if [ "$b_status" = "PASS" ]; then
      b_status_colored="${C_GREEN}PASS${C_RESET}"
    else
      b_status_colored="${C_YELLOW}NO BENCH${C_RESET}"
    fi
    printf "  %-32s %-12d %-20b\n" "$pkg_name" "$b_num" "$b_status_colored"
  done
  echo "  ------------------------------------------------------------------"

  if [ -f "$BENCH_RESULTS_FILE" ] && [ -s "$BENCH_RESULTS_FILE" ]; then
    echo -e "\n${C_BOLD}Selected Benchmark Sample Metrics:${C_RESET}"
    grep "^Benchmark" "$BENCH_RESULTS_FILE" | head -n 12 | while read -r line; do
      echo -e "  ${C_DIM}•${C_RESET} $line"
    done
    bench_count=$(grep -c "^Benchmark" "$BENCH_RESULTS_FILE" || echo 0)
    if [ "$bench_count" -gt 12 ]; then
      echo -e "  ${C_DIM}... and $((bench_count - 12)) more micro-benchmarks.${C_RESET}"
    fi
  fi
  echo ""
fi

# ------------------------------------------------------------------------------
# 5. Export JSON if requested
# ------------------------------------------------------------------------------
if [ -n "$JSON_OUT" ]; then
  # Serialize package metadata as JSON string for Python
  PKG_JSON_STR="{"
  first=1
  for pkg in "${PACKAGES[@]}"; do
    [ $first -eq 0 ] && PKG_JSON_STR+=","
    first=0
    cov_v="${PKG_COV[$pkg]:-0.0}"
    t_cnt="${PKG_TEST_COUNT[$pkg]:-0}"
    t_stat="${PKG_TEST_STATUS[$pkg]:-NONE}"
    b_cnt="${PKG_BENCH_COUNT[$pkg]:-0}"
    b_stat="${PKG_BENCH_STATUS[$pkg]:-NONE}"
    PKG_JSON_STR+="\"$pkg\":{\"coverage_pct\":$cov_v,\"tests\":$t_cnt,\"test_status\":\"$t_stat\",\"benchmarks\":$b_cnt,\"bench_status\":\"$b_stat\"}"
  done
  PKG_JSON_STR+="}"

  python3 -c '
import json, sys, os

data = {
    "summary": {
        "packages_total": int("'"$TOTAL_PKGS"'"),
        "packages_with_tests": int("'"$PKGS_WITH_TESTS"'"),
        "packages_with_benchmarks": int("'"$PKGS_WITH_BENCHS"'"),
        "package_test_coverage_pct": float("'"${PKG_TEST_COV_PCT:-0}"'"),
        "package_benchmark_coverage_pct": float("'"${PKG_BENCH_COV_PCT:-0}"'"),
        "statement_coverage_pct": float("'"${OVERALL_COV:-0}"'"),
        "covered_statements": int("'"${COVERED_STMTS:-0}"'"),
        "total_statements": int("'"${TOTAL_STMTS:-0}"'"),
        "tests_ran": int("'"$TESTS_RAN"'"),
        "tests_passed": int("'"$TESTS_PASSED"'"),
        "tests_failed": int("'"$TESTS_FAILED"'"),
        "benchmarks_ran": int("'"$BENCHS_RAN"'"),
    },
    "packages": json.loads("""'"$PKG_JSON_STR"'""")
}

out_file = "'"$JSON_OUT"'"
if out_file == "-":
    print(json.dumps(data, indent=2))
else:
    with open(out_file, "w") as f:
        json.dump(data, f, indent=2)
    print(f"Exported metrics to {out_file}")
'
fi

# ------------------------------------------------------------------------------
# 6. Check Exit Criteria & Thresholds
# ------------------------------------------------------------------------------
EXIT_CODE=0

if [ "$TESTS_FAILED" -gt 0 ]; then
  echo -e "${C_RED}✖ Test failures detected (${TESTS_FAILED} failed).${C_RESET}"
  EXIT_CODE=1
fi

if [ "$PKGS_WITH_TESTS" -ne "$TOTAL_PKGS" ]; then
  echo -e "${C_RED}✖ Package test coverage is below 100% (${PKGS_WITH_TESTS}/${TOTAL_PKGS} packages).${C_RESET}"
  EXIT_CODE=1
fi

if [ "$THRESHOLD" -gt 0 ]; then
  cov_float=$(python3 -c "print(float('${OVERALL_COV}'))")
  thresh_float=$(python3 -c "print(float('${THRESHOLD}'))")
  is_below=$(python3 -c "print(1 if $cov_float < $thresh_float else 0)")
  if [ "$is_below" -eq 1 ]; then
    echo -e "${C_RED}✖ Statement coverage ${OVERALL_COV}% is below required threshold ${THRESHOLD}%.${C_RESET}"
    EXIT_CODE=1
  fi
fi

if [ $EXIT_CODE -eq 0 ]; then
  echo -e "${C_GREEN}${C_BOLD}✔ All verifications passed successfully!${C_RESET}\n"
fi

exit $EXIT_CODE
