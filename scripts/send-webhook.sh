#!/usr/bin/env bash
# Send a signed GitHub-style push webhook to a running "sling serve".
#
#   SLING_WEBHOOK_SECRET=... ./scripts/send-webhook.sh <owner/repo> <40-char-sha> [branch] [url]
#
# The commit must exist on GitHub: webhook deploys clone from there, never
# from a local path.
set -euo pipefail

: "${SLING_WEBHOOK_SECRET:?set SLING_WEBHOOK_SECRET to the same value the server uses}"
repo=${1:?usage: send-webhook.sh <owner/repo> <sha> [branch] [url]}
sha=${2:?usage: send-webhook.sh <owner/repo> <sha> [branch] [url]}
branch=${3:-main}
url=${4:-http://127.0.0.1:9000/webhook}

body=$(printf '{"ref":"refs/heads/%s","after":"%s","deleted":false,"repository":{"full_name":"%s"}}' \
  "$branch" "$sha" "$repo")
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$SLING_WEBHOOK_SECRET" | awk '{print $NF}')

curl -sS -i -X POST "$url" \
  -H "Content-Type: application/json" \
  -H "X-GitHub-Event: push" \
  -H "X-GitHub-Delivery: local-$(date +%s)" \
  -H "X-Hub-Signature-256: sha256=$sig" \
  --data-binary "$body"
