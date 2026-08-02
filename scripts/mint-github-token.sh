#!/usr/bin/env bash
# mint-github-token.sh — mint a 1-hour GitHub App installation token (App JWT -> token).
#
# Manual stand-in for M4's credential minter. Prints the token on stdout (nothing else),
# so it composes: GITHUB_TOKEN="$(scripts/mint-github-token.sh)".
#
# Env:
#   GITHUB_APP_ID           the App ID (numeric)
#   GITHUB_APP_KEY          path to the App private-key .pem
#   GITHUB_ORG              org whose installation to use (or set GITHUB_INSTALLATION_ID)
#   GITHUB_INSTALLATION_ID  installation id (skips org lookup)
set -euo pipefail

APP_ID="${GITHUB_APP_ID:?set GITHUB_APP_ID}"
KEY="${GITHUB_APP_KEY:?set GITHUB_APP_KEY to the App private-key .pem}"
ORG="${GITHUB_ORG:-}"
INSTALL_ID="${GITHUB_INSTALLATION_ID:-}"

[ -r "$KEY" ] || { echo "mint: key not readable: $KEY" >&2; exit 2; }
command -v jq >/dev/null || { echo "mint: jq required" >&2; exit 2; }

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

now="$(date +%s)"
header='{"alg":"RS256","typ":"JWT"}'
payload="$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$((now + 540))" "$APP_ID")"
h="$(printf '%s' "$header" | b64url)"
p="$(printf '%s' "$payload" | b64url)"
sig="$(printf '%s' "$h.$p" | openssl dgst -sha256 -sign "$KEY" -binary | b64url)"
JWT="$h.$p.$sig"

api() {
  curl -sS -H "Authorization: Bearer $JWT" -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" "$@"
}

if [ -z "$INSTALL_ID" ]; then
  [ -n "$ORG" ] || { echo "mint: set GITHUB_ORG or GITHUB_INSTALLATION_ID" >&2; exit 2; }
  INSTALL_ID="$(api "https://api.github.com/orgs/$ORG/installation" | jq -r '.id // empty')"
  if [ -z "$INSTALL_ID" ]; then
    echo "mint: could not resolve installation for org '$ORG':" >&2
    api "https://api.github.com/orgs/$ORG/installation" >&2
    exit 1
  fi
fi

TOKEN="$(api -X POST "https://api.github.com/app/installations/$INSTALL_ID/access_tokens" | jq -r '.token // empty')"
[ -n "$TOKEN" ] || { echo "mint: installation token request failed" >&2; exit 1; }
printf '%s\n' "$TOKEN"
