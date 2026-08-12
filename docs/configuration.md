# Configuration & pluggable agents

**Status:** design (approved) → implementing in slices (see [Build sequence](#build-sequence)).

Mandobox is meant to be *a generic agent-friendly development environment* you adopt, not just a
box you run. This document defines how a user configures it — box-wide defaults set by whoever
self-hosts, and per-repo settings a **repo author** commits as `.mandobox.yml` — and how a repo
can pick a different **agent harness** (Claude Code, Codex, …) and model.

## Goals

- A **repo carries its own agent setup**, versioned with the code (`.mandobox.yml`).
- The self-hoster sets **defaults and guardrails** once, box-wide.
- **Model** and **agent harness** are pluggable — not hard-wired to Claude.
- Friendly = a small, well-documented YAML file with **presets and good defaults**, not a wall of knobs.

## Non-goals (for now)

- A web UI. File-based config first; a UI can come later if it earns its keep.
- Hardening against a *hostile* repo. Target users are **solo / small teams** — the repo author and
  the operator are one trust circle. Guardrails here prevent footguns, not attacks. (If mandobox
  later runs on repos with external contributors, see [Threat model](#threat-model) for what tightens.)

## Trust model

`.mandobox.yml` is written by the repo author and read by the operator's box. For our target
(solo / small team) that's a **cooperative** relationship, so:

- Guarded settings are **clamped to operator limits and a warning is posted** — not rejected.
- The file is read from the **working branch** (no `pull_request_target`-style ref split).
- Keys split into three classes:

| Class | Examples | Repo can set? |
|---|---|---|
| **Behavior** | instructions, model & agent (within allowlist), review depth, PR style | Yes, freely |
| **Guarded** | vCPUs/mem/disk, keep-alive, cost ceiling, TTL | Requests **clamped** to box limits + warn |
| **Operator-only** | egress hosts, secrets, concurrency | Ignored in the repo file + warn (opt-in later) |

## Config sources & precedence

```
effective = clamp( task_override  ??  repo(.mandobox.yml)  ??  box_default,  box_limit )
```

1. **Box operator config** — `/etc/fleet/mandobox.yml`, provisioned by Ansible. Defaults + limits.
2. **Repo `.mandobox.yml`** — committed at repo root; fetched via the GitHub API at dispatch.
3. **Task overrides** — Slack flags (`/mando --agent codex --model gpt-5-codex …`) for one-offs.

The config is resolved **in the control plane at dispatch time**, before the VM launches — because
resources, model, and harness must be decided before boot. It is fetched over the **GitHub API**
(the App already has read access), not read from inside the guest.

**Both files are re-read on every dispatch**, so any edit — an operator toggling an agent in
`agents_allowed`, or a repo author changing `.mandobox.yml` — takes effect on the next `/mando` with
no worker restart and no image rebuild. (Agent CLIs are baked into the image, so enabling/disabling a
harness is pure config.)

## Operator config (box)

```yaml
# /etc/fleet/mandobox.yml — set by the self-hoster (Ansible-managed)
defaults:
  agent: claude                 # claude | codex | …
  model: claude-sonnet-5
  resources: medium
  keep_alive: 15m
  review: { max_rounds: 5, auto_fix_ci: false }

limits:                         # a repo may REQUEST up to these; over-cap is clamped + warned
  resources:
    profiles:
      small:  { vcpus: 1, mem: 2048, disk: 4096 }
      medium: { vcpus: 2, mem: 4096, disk: 8192 }
      large:  { vcpus: 4, mem: 8192, disk: 16384 }
    max_profile: large
  cost_ceiling_usd: 15
  hard_ttl: 24h
  concurrency: 8                # operator-only
  agents_allowed:  [claude, codex]
  models_allowed:  [claude-sonnet-5, claude-haiku-4-5, gpt-5-codex]
  egress: { mode: strict, extra_hosts: [], allow_repo_hosts: false }
```

## Repo config (`.mandobox.yml`)

```yaml
# Committed at the repo root. Written by the repo author. All keys optional — omit to inherit.
agent: codex                    # must be in agents_allowed, else → box default + warn
model: gpt-5-codex              # must be in models_allowed, else → box default + warn

resources: large                # named profile, clamped to max_profile
keep_alive: 2h                  # clamped to hard_ttl
review: { max_rounds: 3, auto_fix_ci: true, draft_pr: true }

# Visual self-verification — how to bring up a preview so the agent can screenshot and check a UI
# change before opening the PR (see docs/preview.md). Omit to let the agent auto-detect from
# package.json; prefer a Storybook story over the whole app for reliable rendering.
preview: { start: "npm run dev", port: 3000, path: "/" }

# Agent behavior — author-controlled (runs in the sandboxed VM; output is human-reviewed)
instructions: |
  Rust CLI. Small, focused commits. Run `cargo test` && `cargo clippy` before opening a PR.
pr: { title_prefix: "[bot] ", labels: [automated], reviewers: [alice] }

# Ignored here (operator-only): egress hosts, secrets, cost_ceiling, concurrency → a warning is posted.
```

## Resolution & clamping

A single `resolveConfig(box, repo, taskOverrides) → (WorkflowInput, Policy, []warning)`:

1. Layer the three sources by precedence.
2. **Behavior** keys pass through as-is.
3. **Guarded** keys are clamped to `box.limits`; each clamp appends a warning.
4. **Operator-only** keys present in the repo file are dropped; each appends a warning.
5. `agent`/`model` not in the allowlist → fall back to box default + warning.
6. Warnings are posted into the session's Slack thread, so the author sees exactly what was adjusted
   (e.g. *"`resources: xlarge` isn't allowed here — using `large` (box max)."*).

Nothing here is a new data model: the output maps onto **existing fields** —
`WorkflowInput.{VCPUs, MemMiB, Model}` and `Policy.{MaxReviewRounds, CostCeilingUSD, HardTTL,
KeepAlive}`. Today those come from baked `withDefaults()`; after this they come from `resolveConfig`.

## Pluggable agent harnesses

The supervisor already defines the seam:

```go
type AgentRunner interface {
    Run(ctx, spec AgentSpec, onLine func([]byte)) (Result, error)
}
```

Only `ClaudeRunner` exists today; `fc-supervisor` hard-codes it. The change: select the runner from
`BootConfig.Agent`.

- **`ClaudeRunner`** — `claude -p … --output-format stream-json`; reads repo `CLAUDE.md`.
- **`CodexRunner`** (new) — `codex exec "<prompt>"` (headless); reads repo `AGENTS.md`.

**Credentials generalize for free (invariant I1).** A harness never holds a real key. It points at
the host **LiteLLM** endpoint via env — Claude at the Anthropic-style base URL, Codex at LiteLLM's
**OpenAI-compatible** endpoint (`OPENAI_BASE_URL` + a session `OPENAI_API_KEY`) — and LiteLLM injects
the real upstream key. Adding a provider = a LiteLLM route + an `agents_allowed`/`models_allowed`
entry; no secret ever reaches the guest.

**Per-repo instructions map to the harness's native convention file** — `instructions:` is written
into `CLAUDE.md` for Claude, `AGENTS.md` for Codex. No custom prompt plumbing.

### The runner contract (what each adapter must do)

| Responsibility | Claude | Codex |
|---|---|---|
| Invoke headless in the repo dir | `claude -p` | `codex exec` |
| Stream lines → `onLine` (for the log) | yes | yes |
| Terminal `Result`: cost, tokens | from `stream-json` | **parse from Codex output — the fiddly part** |
| Resume a prior session | `--resume <id>` | v2 (single-turn first) |

The microVM, git/PR flow, review loop, egress gateway, and VS Code attach are **harness-agnostic** —
they sit above the runner and don't change.

## Mapping to existing code

| Concept | Where it lands | New or existing |
|---|---|---|
| Fetch `.mandobox.yml` | `GitHubApp.FetchFile(repo, path)` | **new** (small) |
| Resolve + clamp | `internal/control` `resolveConfig()` | **new** (the important bit) |
| Resources | `WorkflowInput.{VCPUs, MemMiB}`, agent `workspace-size` | existing (feed from config) |
| Policy knobs | `Policy{…}` | existing (stop baking defaults) |
| Model | `WorkflowInput.Model` → LiteLLM | existing |
| Agent harness | `BootConfig.Agent` → runner select in `fc-supervisor` | **new** field + switch |
| Instructions | write to `CLAUDE.md`/`AGENTS.md` in the guest | **new** (small) |
| Warnings | `PostMessage` into the thread | existing |

## Build sequence

1. **Config + clamp (the spine). ✅ done.** `GitHubApp.FetchFile` + `resolveConfig` + warn-on-drop,
   feeding the existing `WorkflowInput`/`Policy`. Resources, model, keep-alive, review rounds, and the
   cost/TTL guardrails are live and version-gated. Operator sample: `mandobox.example.yml`. *(Verified:
   a repo requesting `resources: large` boots a 4-vCPU VM, and operator-only keys are dropped + warned
   in the thread.)*
2. **Per-repo instructions + model allowlist (Layer 1). ✅ done.** `instructions:` flow through the
   MMDS `agent` section to the guest and are injected via Claude Code's `--append-system-prompt` —
   *on top of* the repo's own `CLAUDE.md`, not replacing it. `model` allowlist enforced in slice 1.
   *(Verified: a repo convention "every .md starts with `<!-- authored via mandobox -->`" was obeyed
   on an unrelated task.)* Note: Claude's **prompt-injection defense refuses injection-shaped
   instructions** ("do X regardless of the task"), so a repo can't smuggle malicious directives through
   `instructions` — legitimate conventions apply, adversarial ones get flagged by the agent.
3. **`CodexRunner` (Layer 2). 🟡 built, unverified.** The adapter and seam are in place —
   `fc-supervisor` picks the runner by `cfg.Agent.Harness`, `CodexRunner` runs `codex exec` pointed at
   LiteLLM's OpenAI-compatible endpoint (key injected host-side, gateway is path-agnostic so it needs
   no change). Not yet run against a live Codex CLI (needs an OpenAI key on the box). See below.

### Enabling Codex

The Codex CLI is **baked into the image by default** — available, not enabled — so switching harnesses
is a config toggle, never an image rebuild. (`build-mmdebstrap.sh` installs `@openai/codex` unless
`CODEX_VERSION=""`.) Turning it on is:

- **One-time provider setup** (you need an OpenAI account): drop the key at `secrets/openai.key` and
  add a route to `litellm_model_overrides`, e.g. `{name: "gpt-5-codex", model: "openai/gpt-5-codex"}`
  (a LiteLLM reload). The gateway already forwards OpenAI-style paths to LiteLLM unchanged; no key ever
  reaches the guest (I1).
- **Enable / disable — instant, no restart:** add or remove `codex` in `agents_allowed` (and the model
  in `models_allowed`) in `mandobox.yml`. The box config is **re-read on every dispatch**, so the
  change lands on the next `/mando`. A repo's `.mandobox.yml` can then set `agent: codex`.

**Verify on the first real run** (the two spots only a live CLI can confirm): the exact `codex exec`
flags for non-interactive full-auto file access, and cost/token extraction (best-effort `0` today, so
the cost ceiling degrades to the wall-clock/TTL guardrail for Codex sessions until wired). Resume is
also not yet wired — each Codex turn is independent (a review round re-states context via the prompt).
Everything above the runner — the microVM, git/PR flow, review loop, VS Code attach — is
harness-agnostic and unchanged.

## Threat model

Designed for **cooperative** use (solo / small team). If mandobox is later pointed at repos with
untrusted external contributors, the tightenings are: read guarded/operator keys from the **default
branch** only (a PR can't escalate its own resources/model), and keep `allow_repo_hosts: false`.
The class split above already isolates what would need locking down.

## Open questions

- **Presets vs raw numbers** for `resources` — start with named profiles only (`small/medium/large`)?
- **`draft_pr`** and **`reviewers`/`labels`** — nice-to-have in slice 1 or defer?
- **Codex cost/token reporting** — if the CLI doesn't expose it cleanly, cost ceilings degrade to
  wall-clock/TTL for Codex sessions until it does. Acceptable v1?
