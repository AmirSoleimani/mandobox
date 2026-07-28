# GitHub App setup (M3)

Manual, one-time setup. The App is the agent's identity: 1-hour installation tokens scoped
to one repo and one permission set, a distinct actor in PR history, and — critically — the
thing branch protection discriminates from a human (PLAN §4.6, §11). Rules 4 and 5 below are
the real safety guarantee: enforced by GitHub, not by our code being correct (I4, I5).

## 0. Prerequisite: repos under an organisation

On a personal account a GitHub App cannot create pull requests (it can't be a collaborator).
**Migrate the target repos to the Chelodo org before this step** (§4.6).

## 1. Create the App (Chelodo org → Settings → Developer settings → GitHub Apps → New)

**Repository permissions — grant only:**

| Permission | Level | Why |
|---|---|---|
| Contents | Read & write | push commits to `agent/*` |
| Pull requests | Read & write | open PRs, read review comments |
| Checks | Read-only | consume CI results (our D1: e2e runs on GitHub CI → `ci_status`) |

**Do NOT grant** `actions`, `administration`, `workflows`, or any org-level permission.
Checks:read is read-only and is the only addition to §11's set.

**Subscribe to webhook events:** `pull_request`, `pull_request_review`,
`pull_request_review_comment`, `issue_comment`, `check_suite`.

**Webhook URL:** point at `webhook-rx` (built in M4). Set a webhook secret and store it on the
control plane (Tier-0). Until M4 exists you can leave the webhook inactive.

## 2. Generate and store the private key

Generate a private key from the App page. It is **Tier-0** (§9) — it never enters a guest and
lives only on the control plane, where the credential minter (M4) exchanges it for short-lived
installation tokens. Keep it out of this repo (see `.gitignore`).

## 3. Install on target repos only

Install the App on the specific repos agents will work on — not "all repositories".

## 4. Branch protection on `main`

Repo → Settings → Branches (or Rulesets) → protect `main`:

- Require a pull request before merging.
- Require **1 approving review**.
- **Exclude the App from being able to approve** — the App must never satisfy its own review
  gate (I5). (Reviews from the App/bot don't count toward required approvals.)

## 5. Ruleset: the App may push to `agent/*` only

Add a repository ruleset (or branch rule) permitting the App's pushes to `agent/*` branches
and nothing else. Combined with branch protection on `main`, this means a bug in our code
cannot get agent output onto `main` — GitHub rejects the push (I4).

## Verifying (M3 acceptance, §14)

A hand-dispatched task should produce a PR **opened by the App bot** on an `agent/*` branch,
with **no credential in the guest** (dump `env` and grep the workspace), and a push to `main`
from the App must be **rejected by GitHub**.

## How tokens flow (context)

The minter (M4) issues a 1-hour installation token (`contents:write` + `pull_requests:write`,
single repo) delivered to the guest via MMDS (Tier-1). fc-supervisor consumes it through a git
credential helper — never a remote URL or env var — so it can be refreshed without a process
restart (§9). The bot commit identity is `<app-slug>[bot]` / `<id>+<app-slug>[bot]@users.noreply.github.com`,
set in `github.bot_user` / `github.bot_email` in the MMDS payload.
