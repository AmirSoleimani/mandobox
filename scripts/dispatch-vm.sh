#!/usr/bin/env bash
# dispatch-vm.sh — hand-dispatch one agent VM by POSTing a launch to fleet-agent (M3).
#
# This is the manual stand-in for M4's Temporal workflow: it generates a session_id, builds
# the MMDS payload, and calls fleet-agent over mTLS. fleet-agent adds the network block and
# boots the VM; fc-supervisor runs Claude Code and opens the PR.
#
# The GitHub token must be a real installation token (contents:write + pull_requests:write,
# single repo). Until M4's credential minter exists, mint one by hand from the App.
set -euo pipefail

need() { [ -n "${!1:-}" ] || { echo "dispatch: \$$1 is required" >&2; exit 2; }; }
need FLEET_URL
need FLEET_TLS_CERT
need FLEET_TLS_KEY
need FLEET_SERVER_CA
need IMAGE_SHA
need REPO_CLONE_URL
need GITHUB_TOKEN
command -v jq >/dev/null || { echo "dispatch: jq is required" >&2; exit 2; }

# Single-machine build: NATS is host-local on the anchor, reachable by the guest there.
NATS_URL="${NATS_URL:-nats://172.31.0.1:4222}"

BASE_BRANCH="${BASE_BRANCH:-main}"
PROMPT="${PROMPT:-Make a small, well-scoped improvement and open a PR.}"
GATEWAY_URL="${GATEWAY_URL:-http://172.31.0.1:8080}"
VCPUS="${VCPUS:-2}"
MEM_MIB="${MEM_MIB:-4096}"
CLAUDE_MODEL="${CLAUDE_MODEL:-claude-sonnet-5}"
GITHUB_BOT_USER="${GITHUB_BOT_USER:-fleet-agent[bot]}"
GITHUB_BOT_EMAIL="${GITHUB_BOT_EMAIL:-fleet-agent[bot]@users.noreply.github.com}"
NATS_CREDS="${NATS_CREDS:-}" # empty for M3 no-auth NATS

# Derive owner/name from the clone URL if not given.
REPO_SLUG="${REPO_SLUG:-$(printf '%s' "$REPO_CLONE_URL" | sed -E 's#^https?://[^/]+/##; s/\.git$//')}"

# Generate a session_id: s_ + 26 Crockford base32 chars (matches ^s_[0-9A-HJKMNP-TV-Z]{26}$).
ALPHABET="0123456789ABCDEFGHJKMNPQRSTVWXYZ"
SID="s_"
for _ in $(seq 26); do SID+="${ALPHABET:$((RANDOM % 32)):1}"; done
GATEWAY_TOKEN="${GATEWAY_TOKEN:-sess-${SID}}"

MMDS="$(jq -n \
  --arg slug "$REPO_SLUG" --arg clone "$REPO_CLONE_URL" --arg base "$BASE_BRANCH" \
  --arg prompt "$PROMPT" \
  --arg gwurl "$GATEWAY_URL" --arg gwtok "$GATEWAY_TOKEN" \
  --arg ghtok "$GITHUB_TOKEN" --arg ghuser "$GITHUB_BOT_USER" --arg ghemail "$GITHUB_BOT_EMAIL" \
  --arg natsurl "$NATS_URL" --arg natscreds "$NATS_CREDS" \
  --arg model "$CLAUDE_MODEL" \
  '{
     repo:   {slug:$slug, clone_url:$clone, base_branch:$base},
     task:   {mode:"initial", prompt:$prompt},
     llm:    {base_url:$gwurl, auth_token:$gwtok},
     github: {token:$ghtok, bot_user:$ghuser, bot_email:$ghemail},
     nats:   {url:$natsurl, creds:$natscreds},
     claude: {model:$model}
   }')"

BODY="$(jq -n \
  --arg sid "$SID" --arg sha "$IMAGE_SHA" \
  --argjson vcpus "$VCPUS" --argjson mem "$MEM_MIB" --argjson mmds "$MMDS" \
  '{session_id:$sid, image_sha:$sha, vcpus:$vcpus, mem_mib:$mem, mmds_payload:$mmds}')"

echo "dispatch: launching ${SID} on ${REPO_SLUG} (image ${IMAGE_SHA})"
curl -fsS --cert "$FLEET_TLS_CERT" --key "$FLEET_TLS_KEY" --cacert "$FLEET_SERVER_CA" \
  -H 'Content-Type: application/json' \
  -X POST "$FLEET_URL/vms" -d "$BODY"
echo
echo "dispatch: session_id=${SID}  branch=agent/${SID}"
