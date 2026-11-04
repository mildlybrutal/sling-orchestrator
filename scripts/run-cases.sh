#!/usr/bin/env bash
# Runs Sling against every tagged commit of the demo app and checks two things
# for each one:
#   1. the deploy passes or fails as expected, and
#   2. the PUBLIC URL serves the right version afterwards. A failed deploy
#      must leave the previous version serving; a passed one must replace it.
#
#   ./scripts/run-cases.sh [path-to-sling-repo]     (default: ../sling)
#
# Takes about a minute: the bad-health case waits out the 30s deadline.
# Starts from a clean slate and removes everything it created when done.
set -u

DEMO="$(cd "${1:-../sling}" && pwd)" || exit 1
cd "$(dirname "$0")/.." || exit 1

PUBLIC="http://127.0.0.1:8080"

go build -o sling . || exit 1

cleanup() {
  ids="$(docker ps -aq --filter label=sling.managed=true)"
  [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
  docker rm -f sling-caddy >/dev/null 2>&1
}
cleanup
trap cleanup EXIT

# tag:expected
CASES=(
  "good-v1:pass"
  "crash:fail"
  "bad-health:fail"
  "slow-start:pass"
  "good-v2:pass"
)

failures=0
live=""   # SHA that should be serving on the public URL
printf '\n%-12s %-9s %-9s %-22s %s\n' CASE EXPECTED GOT "PUBLIC URL SERVES" RESULT
printf '%s\n' "-----------------------------------------------------------------------"

for c in "${CASES[@]}"; do
  tag="${c%%:*}"; want="${c##*:}"
  sha="$(git -C "$DEMO" rev-parse "$tag")" || { echo "unknown tag $tag"; exit 1; }

  log="$(mktemp)"
  if ./sling "$DEMO" "$sha" >"$log" 2>&1; then got=pass; else got=fail; fi
  [ "$got" = pass ] && live="$sha"

  # What do real users see right now?
  code="$(curl -s -m 3 -o /tmp/sling-body.$$ -w '%{http_code}' "$PUBLIC/" || true)"
  body="$(cat /tmp/sling-body.$$ 2>/dev/null)"; rm -f /tmp/sling-body.$$

  problems=""
  [ "$got" = "$want" ] || problems="deploy outcome;"
  if [ -z "$live" ]; then
    [ "$code" = 503 ] || problems="$problems public should be 503 (nothing live);"
    serves="503 (nothing live)"
  else
    case "$body" in
      *"$live"*) serves="${live:0:7}" ;;
      *)         serves="WRONG (HTTP $code)"; problems="$problems public serves wrong version;" ;;
    esac
  fi

  # A failed deploy must clean up after itself.
  if [ "$got" = fail ] && [ -n "$(docker ps -aq --filter "name=app-$sha")" ]; then
    problems="$problems leaked container;"
  fi

  if [ -z "$problems" ]; then verdict="ok"; else verdict="PROBLEM: $problems"; failures=$((failures+1)); fi
  printf '%-12s %-9s %-9s %-22s %s\n' "$tag" "$want" "$got" "$serves" "$verdict"
  [ -n "$problems" ] && sed 's/^/    | /' "$log"
  [ "$got" = fail ] && grep -m1 'Deployment failed' "$log" | cut -c1-160 | sed 's/^/    /'
  rm -f "$log"
done

echo
if [ "$failures" -eq 0 ]; then echo "All cases behaved as expected."; else echo "$failures problem(s) found."; fi
exit "$failures"
