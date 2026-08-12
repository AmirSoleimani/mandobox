#!/usr/bin/env node
// mando-shot — capture a screenshot of a running page with headless Chromium, so the agent can SEE
// its own change and self-correct before opening a PR (Claude Code reads the PNG back). Runs in the
// guest against localhost only — no egress. Baked into the golden image at /opt/mando-shot and
// symlinked onto PATH as /usr/local/bin/mando-shot.
'use strict';

// The browser is baked into the image at a fixed path; make Playwright look there regardless of the
// caller's environment (Docker ENV / the launching agent's env may not carry it through).
process.env.PLAYWRIGHT_BROWSERS_PATH ||= '/opt/ms-playwright';

function usage(code) {
  process.stdout.write(
`mando-shot — screenshot a running page (headless Chromium) for visual verification

usage: mando-shot <url> [options]
  --out FILE        output PNG path (default ./.mando/shot.png)
  --viewport WxH    viewport size (default 1280x800)
  --full-page       capture the full scrollable page, not just the viewport
  --wait WHAT       load | networkidle | <css-selector>   (default load)
  --timeout MS      navigation/wait timeout in ms (default 15000)
  -h, --help        show this help

notes:
  - Point it at a running dev server or a Storybook story, e.g.
      mando-shot http://localhost:3000
      mando-shot 'http://localhost:6006/iframe.html?id=button--primary' --out .mando/button.png
  - '--wait networkidle' is more precise but can hang on dev servers with a live HMR socket;
    'load' (the default) plus a brief settle is the robust choice. Pass a CSS selector to wait for
    specific content.
`);
  process.exit(code);
}

function fail(msg) { process.stderr.write(`mando-shot: ${msg}\n`); process.exit(2); }

function parseArgs(argv) {
  const o = { out: '.mando/shot.png', viewport: '1280x800', fullPage: false, wait: 'load', timeout: 15000, url: null };
  const need = (i, flag) => { if (i >= argv.length) fail(`${flag} needs a value`); return argv[i]; };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    switch (a) {
      case '-h': case '--help': usage(0); break;
      case '--out': o.out = need(++i, a); break;
      case '--viewport': o.viewport = need(++i, a); break;
      case '--full-page': o.fullPage = true; break;
      case '--wait': o.wait = need(++i, a); break;
      case '--timeout': o.timeout = parseInt(need(++i, a), 10); break;
      default:
        if (a.startsWith('-')) fail(`unknown option: ${a} (see --help)`);
        else if (o.url) fail(`unexpected argument: ${a}`);
        else o.url = a;
    }
  }
  if (!o.url) fail('missing <url> (see --help)');
  if (!Number.isFinite(o.timeout) || o.timeout <= 0) fail(`bad --timeout (want a positive number of ms)`);
  const m = /^(\d+)x(\d+)$/.exec(o.viewport);
  if (!m) fail(`bad --viewport ${o.viewport} (want WxH, e.g. 1280x800)`);
  o.width = +m[1]; o.height = +m[2];
  return o;
}

async function main() {
  const o = parseArgs(process.argv.slice(2)); // handles --help/validation before we need the browser

  const fs = require('fs');
  const path = require('path');
  const out = path.resolve(o.out);
  fs.mkdirSync(path.dirname(out), { recursive: true });

  let chromium;
  try {
    ({ chromium } = require('playwright'));
  } catch (e) {
    fail(`Playwright isn't available (${e.message}). This tool only runs inside a mandobox guest image.`);
  }

  let browser;
  try {
    // --no-sandbox: we run as root in a microVM (no user namespaces), where Chromium refuses its sandbox.
    // --disable-dev-shm-usage: /dev/shm is tiny in the guest; spilling to /tmp avoids renderer crashes.
    browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu'] });
  } catch (e) {
    fail(`could not launch Chromium: ${e.message} (PLAYWRIGHT_BROWSERS_PATH=${process.env.PLAYWRIGHT_BROWSERS_PATH})`);
  }

  try {
    const page = await browser.newPage({ viewport: { width: o.width, height: o.height } });
    const navWait = o.wait === 'networkidle' ? 'networkidle' : 'load';
    await page.goto(o.url, { waitUntil: navWait, timeout: o.timeout });
    if (o.wait !== 'load' && o.wait !== 'networkidle') {
      await page.waitForSelector(o.wait, { timeout: o.timeout }); // --wait is a CSS selector
    }
    await page.waitForTimeout(400); // brief settle for late SPA paint
    await page.screenshot({ path: out, fullPage: o.fullPage });
    process.stdout.write(`mando-shot: wrote ${o.out} (${o.width}x${o.height}${o.fullPage ? ', full page' : ''}) of ${o.url}\n`);
  } catch (e) {
    fail(`failed to capture ${o.url}: ${e.message}`);
  } finally {
    await browser.close().catch(() => {});
  }
}

main();
