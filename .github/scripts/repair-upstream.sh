#!/usr/bin/env bash
set -euo pipefail

# A completed Codex process can report unfinished work and still exit zero.
# Only the checks decide whether a repair succeeded.
for attempt in 1 2 3; do
  echo "::group::Upstream repair attempt $attempt/3"
  grep -E 'error TS[0-9]+|error:|ERR_|failed|Error:|FAIL|\.go:[0-9]+:|not ok' \
    "$RUNNER_TEMP/sync-upstream-checks.log" \
    > "$RUNNER_TEMP/sync-upstream-check-errors.log" || true

  if ! timeout 15m npx -y @openai/codex@latest exec \
    --dangerously-bypass-approvals-and-sandbox \
    -m "$MODEL" -c model_reasoning_effort=high \
    "$PROMPT

    This is repair attempt $attempt of 3. Earlier attempts did not pass verification.
    Start with the concise diagnostics in
    $RUNNER_TEMP/sync-upstream-check-errors.log, then inspect the full log as needed.
    Existing edits from earlier attempts are still present. Continue repairing them.
    Do the work now; do not end with a promise to continue in another turn."; then
    echo "::warning::Codex attempt $attempt exited unsuccessfully; checking its changes."
  fi

  # Always refresh diagnostics, including after a failed or timed-out agent.
  if bash .github/scripts/check-upstream.sh 2>&1 | tee "$RUNNER_TEMP/sync-upstream-checks.log"; then
    if [ -z "$(git diff --name-only --diff-filter=U)" ] && \
      ! git grep -qE '^(<<<<<<<|>>>>>>>) ' -- .; then
      git add --all
      echo "::endgroup::"
      exit 0
    fi
    echo 'Unresolved merge conflicts remain.' >> "$RUNNER_TEMP/sync-upstream-checks.log"
  fi
  echo "::endgroup::"
done

echo "::error::Upstream repair failed after 3 verified attempts; master was not pushed."
exit 1
