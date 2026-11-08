#!/usr/bin/env bash
# The README's recording: a good deploy, a crash that never gets traffic, and a
# release that goes live, turns sick in the grace window and is rolled back.
#
#   ./scripts/demo.sh [path-to-sling-repo]     (default: ../sling)
#
# Needs the demo app's tags (make-cases.sh) plus `fails-later`: a commit whose
# /health passes at first and returns 500 ten seconds after boot.
# Uses a throwaway history (SLING_DATA_DIR) and removes what it created when done.
set -u

DEMO="$(cd "${1:-../sling}" && pwd)" || exit 1
cd "$(dirname "$0")/.." || exit 1
go build -o bin/sling ./cmd/sling || exit 1

SLING_DATA_DIR=""
export BUILDKIT_PROGRESS=quiet   # the build's own progress bars redraw too much for a recording
cleanup() {
  ids="$(docker ps -aq --filter label=sling.managed=true)"
  [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
  docker rm -f sling-caddy >/dev/null 2>&1
  [ -n "$SLING_DATA_DIR" ] && rm -rf "$SLING_DATA_DIR"
}
cleanup >/dev/null 2>&1; export SLING_DATA_DIR="$(mktemp -d)"
trap cleanup EXIT

B=$'\e[1m'; D=$'\e[2m'; C=$'\e[36m'; R=$'\e[0m'
say()  { printf '\n%s# %s%s\n' "$D" "$1" "$R"; sleep 1; }
run()  { printf '%s$%s %s\n' "$C" "$R" "$B$1$R"; sleep .6; eval "$2"; }
sha()  { git -C "$DEMO" rev-parse "$1"; }

say "1. A good commit: built, health checked, switched in, watched for 30s"
run "sling ./app \$(git rev-parse good-v1)" "./bin/sling '$DEMO' $(sha good-v1)"
run "curl localhost:8080" "curl -s localhost:8080"

say "2. A commit that crashes on boot never gets traffic"
run "sling ./app \$(git rev-parse crash)" "./bin/sling '$DEMO' $(sha crash)"
run "curl localhost:8080" "curl -s localhost:8080"

say "3. A commit that goes live, then fails its probes in the grace window"
run "sling ./app \$(git rev-parse fails-later)" "./bin/sling '$DEMO' $(sha fails-later)"
run "curl localhost:8080" "curl -s localhost:8080"

say "Every attempt is on record"
run "sling history" "./bin/sling history"
sleep 2
