# Visual self-verification (preview)

The agent edits frontend code, but without seeing the result it can only reason about the code — so
"looks right in the diff, broken in the render" (overlap, cut-off text, a blown-out mobile layout)
slips into PRs. Mandobox's agent is Claude Code, which is **multimodal**: it can look at an image. So
the golden image ships a headless browser and a one-command screenshot tool, and the agent is told to
**render its own change, look at the screenshot, and fix it before opening the PR.**

The screenshot's first consumer is the *agent*, not you — this is about correctness, not just a nice
picture. (Sharing screenshots into the Slack/Telegram thread is a separate, later feature.)

## How it works

- **In-guest, no egress.** Chromium runs inside the task's micro-VM and hits the app on `localhost`.
  It never leaves the VM, so it doesn't touch the egress allowlist or the trust boundary. (If your app
  itself calls external services, those still go through the gateway allowlist as usual — a screenshot
  may show broken states unless those hosts are permitted.)
- **`mando-shot`** is the tool the agent calls — a thin wrapper over Playwright, preinstalled at
  `/usr/local/bin/mando-shot`:
  ```
  mando-shot <url> [--out FILE] [--viewport WxH] [--full-page] [--wait load|networkidle|<selector>] [--timeout MS]
  ```
  It launches headless Chromium, waits for the page, and writes a PNG (default `./.mando/shot.png`).
  The agent then reads the PNG and iterates (up to ~3 times) until the requested change is visibly
  correct.
- **Fail-soft.** If the app can't render (needs a backend, secrets, seed data, or a login), the agent
  says so in its summary and opens the PR anyway. Visual verification never blocks a change.

## Telling the agent how to run your app

The agent auto-detects the common case (a `dev`/`start` script in `package.json`, a framework's default
port). For anything non-obvious, add a `preview:` block to your repo's `.mandobox.yml`:

```yaml
preview:
  start: "npm run dev"     # command that brings up the app (or "npm run storybook")
  port: 3000               # the port it listens on
  path: "/"                # the page to screenshot (or a specific Storybook story URL)
  wait: networkidle        # optional: load | networkidle | a CSS selector to wait for
  viewport: 1280x800       # optional
```

`preview:` is read by the agent straight from your checked-out repo; it needs no operator config and is
ignored by everything else.

## Recommended: render a component, not the whole app

Booting a full app is the least reliable part of this — a bare `npm run dev` often lands on an error
page or a login wall. If your repo has **Storybook** (or any component playground), point the agent at
a single story instead:

```yaml
preview:
  start: "npm run storybook"
  port: 6006
  path: "/iframe.html?id=button--primary"
```

Component-level rendering needs no backend, no seed data, and no auth — it's faster and far more
reliable than full-app end-to-end. Prefer it when you can.

## Resources

Chromium wants headroom (~0.5–1 GB on top of your dev server). The default `medium` profile (4 GB) is
fine for simple apps; bump heavier ones in `.mandobox.yml`:

```yaml
resources: large   # 8 GB — for heavy dev servers / big component trees
```

See [configuration.md](configuration.md) for resource profiles.

## Limitations (honest)

- **App boot is the real friction.** Apps that need a database, external APIs, secrets, or a login
  won't render meaningfully from a bare start command. Use `preview:` to supply a build/seed step, or
  prefer component/Storybook rendering, or accept that the agent will note it couldn't render.
- **Dev-server HMR + `networkidle`.** Vite/Next dev servers hold a live hot-reload socket, so
  `--wait networkidle` can hang; `mando-shot` defaults to `--wait load` (plus a brief settle). Pass a
  CSS selector to wait for specific content.
- **Claude-only.** The look-and-iterate loop relies on a multimodal agent. Other harnesses (Codex,
  Aider) can still run `mando-shot` but won't "see" the result.
- **Cost.** Each screenshot the agent reviews costs vision tokens; the ~3-iteration cap and the box
  cost ceiling bound it.

## What ships this

The browser + `mando-shot` are baked into the **base** golden image (`image/` — all three builders),
pinned like every other tool. The instruction that makes the agent use them lives in the built-in agent
preamble (`internal/supervisor/supervisor.go`, `visualCheck`).
