#!/usr/bin/env bash
# Submit an artefact to Apple's notary service and poll for the
# result with retries. Equivalent to `xcrun notarytool submit --wait`
# but resilient to transient network errors during the polling
# phase — we observed those on GitHub-hosted macOS runners losing
# DNS for a few seconds mid-poll, which would tear down the long-
# lived `--wait` connection and fail the build despite Apple's
# queue having the submission.
#
# Usage:  notarize-wait.sh <artefact> <keychain-profile>
#
# Exits 0 only when Apple reports status "Accepted". On rejection,
# fetches and prints the developer log so the build log explains
# why instead of leaving you to look it up.

set -euo pipefail

ARTEFACT="${1:?artefact path required}"
PROFILE="${2:?keychain profile required}"
# Optional 3rd arg: an existing submission ID to poll instead of
# submitting fresh. Used by `make release-staple` to resume after
# Apple's queue overran the CI runner's timeout — the artefact is
# already in Apple's pipeline; we just need to wait for it.
EXISTING_ID="${3:-}"

# Tunables — generous defaults so a slow Apple queue doesn't trip
# the timeout. Override via env if you ever need to.
POLL_INTERVAL="${POLL_INTERVAL:-30}"      # seconds between polls
MAX_WAIT="${MAX_WAIT:-3600}"              # 1 hr hard cap (v1.0.3's DMG submission landed at 28m20s — too tight)
NETWORK_RETRIES="${NETWORK_RETRIES:-5}"   # consecutive poll failures before giving up

if [ -n "$EXISTING_ID" ]; then
  echo "polling existing submission: $EXISTING_ID (artefact: $ARTEFACT)"
  SUBMISSION_ID="$EXISTING_ID"
else
  echo "submitting: $ARTEFACT"
  SUBMIT_OUTPUT="$(xcrun notarytool submit "$ARTEFACT" --keychain-profile "$PROFILE" --output-format json)"
  echo "$SUBMIT_OUTPUT"
  SUBMISSION_ID="$(echo "$SUBMIT_OUTPUT" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")"
  echo "submission id: $SUBMISSION_ID"
fi

elapsed=0
network_failures=0
while :; do
  if (( elapsed >= MAX_WAIT )); then
    echo "timed out after ${MAX_WAIT}s waiting for notarization" >&2
    exit 1
  fi

  if INFO_OUTPUT="$(xcrun notarytool info "$SUBMISSION_ID" --keychain-profile "$PROFILE" --output-format json 2>&1)"; then
    network_failures=0
    STATUS="$(echo "$INFO_OUTPUT" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")"
    echo "[$(date -u +%H:%M:%S)] status: $STATUS"
    case "$STATUS" in
      Accepted)
        echo "notarization accepted"
        exit 0
        ;;
      "In Progress")
        :
        ;;
      Invalid|Rejected)
        echo "notarization rejected; fetching developer log" >&2
        xcrun notarytool log "$SUBMISSION_ID" --keychain-profile "$PROFILE" >&2 || true
        exit 1
        ;;
      *)
        echo "unexpected status: $STATUS" >&2
        echo "$INFO_OUTPUT" >&2
        exit 1
        ;;
    esac
  else
    network_failures=$((network_failures + 1))
    echo "[$(date -u +%H:%M:%S)] info query failed (attempt $network_failures/$NETWORK_RETRIES)" >&2
    if (( network_failures >= NETWORK_RETRIES )); then
      echo "too many consecutive network failures; aborting" >&2
      echo "$INFO_OUTPUT" >&2
      exit 1
    fi
  fi

  sleep "$POLL_INTERVAL"
  elapsed=$((elapsed + POLL_INTERVAL))
done
