"use strict";

// Minimal vanilla SPA: three views (sessions / config / tools), each backed by one JSON endpoint.
// No framework, no build step — served straight from the embedded filesystem.

const $ = (sel) => document.querySelector(sel);
const el = (tag, attrs = {}, ...kids) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else if (k === "html") n.innerHTML = v;
    else if (v !== null && v !== undefined) n.setAttribute(k, v);
  }
  for (const c of kids) n.append(c?.nodeType ? c : document.createTextNode(c ?? ""));
  return n;
};
const esc = (s) => String(s ?? "");

async function api(path, opts) {
  opts = opts || {};
  // Every state-changing request carries Content-Type: application/json so the server's same-origin
  // guard (which requires it) can never reject a legitimate dashboard call.
  const method = (opts.method || "GET").toUpperCase();
  if (method !== "GET" && method !== "HEAD") {
    opts.headers = Object.assign({ "Content-Type": "application/json" }, opts.headers || {});
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let body = {};
  try { body = text ? JSON.parse(text) : {}; } catch { body = { error: text }; }
  if (!res.ok) throw new Error(body.error || `${res.status} ${res.statusText}`);
  return body;
}

function setConn(ok, msg) {
  const c = $("#conn");
  c.textContent = msg || (ok ? "connected" : "");
  c.className = "status" + (ok ? "" : " err");
}

// ---- view switching ------------------------------------------------------
const views = ["sessions", "vms", "models", "connectors", "costs", "health", "config", "instructions", "tools", "secrets"];
function show(view) {
  if (!views.includes(view)) view = "sessions";
  for (const v of views) {
    $(`#view-${v}`).hidden = v !== view;
    const item = document.querySelector(`.nav-item[data-view="${v}"]`);
    if (!item) continue;
    if (v === view) item.setAttribute("aria-current", "true");
    else item.removeAttribute("aria-current");
  }
  location.hash = view;
  if (view === "sessions") loadSessions();
  if (view === "vms") { loadVMs(); scheduleVMs(); }
  if (view === "models") loadProvider();
  if (view === "connectors") loadConnectors();
  if (view === "costs") loadCosts();
  if (view === "health") loadHealth();
  if (view === "config") { loadConfig(); loadConfigHelp(); }
  if (view === "instructions") loadDocs();
  if (view === "tools") loadTools();
  if (view === "secrets") loadSecrets();
  if (view !== "vms") clearInterval(vmsTimer);
  if (view !== "sessions" && activityES) closeActivity();
}
document.querySelectorAll(".nav-item").forEach((t) =>
  t.addEventListener("click", () => show(t.dataset.view))
);
$("#reload-models").addEventListener("click", loadProvider);

// ---- sidebar: icons, collapse (persisted), tooltips ----------------------
const NAV_ICONS = {
  sessions: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>',
  vms: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="3" width="20" height="8" rx="1"/><rect x="2" y="13" width="20" height="8" rx="1"/><line x1="6" y1="7" x2="6.01" y2="7"/><line x1="6" y1="17" x2="6.01" y2="17"/></svg>',
  models: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="4" width="16" height="16" rx="2"/><rect x="9" y="9" width="6" height="6"/><path d="M9 2v2M15 2v2M9 20v2M15 20v2M20 9h2M20 15h2M2 9h2M2 15h2"/></svg>',
  connectors: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>',
  config: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="4" y1="21" x2="4" y2="14"/><line x1="4" y1="10" x2="4" y2="3"/><line x1="12" y1="21" x2="12" y2="12"/><line x1="12" y1="8" x2="12" y2="3"/><line x1="20" y1="21" x2="20" y2="16"/><line x1="20" y1="12" x2="20" y2="3"/><line x1="2" y1="14" x2="6" y2="14"/><line x1="10" y1="8" x2="14" y2="8"/><line x1="18" y1="16" x2="22" y2="16"/></svg>',
  instructions: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="16" y1="13" x2="8" y2="13"/><line x1="16" y1="17" x2="8" y2="17"/></svg>',
  tools: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/></svg>',
  secrets: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="7.5" cy="15.5" r="5.5"/><path d="m21 2-9.6 9.6"/><path d="m15.5 7.5 3 3L22 7l-3-3"/></svg>',
  costs: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="20" x2="12" y2="10"/><line x1="18" y1="20" x2="18" y2="4"/><line x1="6" y1="20" x2="6" y2="16"/></svg>',
  health: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 1 0-7.78 7.78L12 21.23l8.84-8.84a5.5 5.5 0 0 0 0-7.78z"/></svg>',
};
document.querySelectorAll(".nav-item").forEach((it) => {
  const label = it.textContent.trim();
  it.setAttribute("data-tip", label);
  it.innerHTML = `<span class="nav-ico">${NAV_ICONS[it.dataset.view] || ""}</span><span class="nav-txt">${label}</span>`;
});
$("#nav-toggle .nav-ico").innerHTML =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 18 9 12 15 6"/></svg>';
function applyNavCollapsed(c) { $(".app").classList.toggle("nav-collapsed", c); }
$("#nav-toggle").addEventListener("click", () => {
  const c = !$(".app").classList.contains("nav-collapsed");
  applyNavCollapsed(c);
  try { localStorage.setItem("mando.nav", c ? "collapsed" : "expanded"); } catch (_) {}
});
try { applyNavCollapsed(localStorage.getItem("mando.nav") === "collapsed"); } catch (_) {}

// ---- connectors (integrations: GitHub, Slack, …) -------------------------
function connMsg(kind, text) {
  const m = $("#conn-msg");
  if (!text) { m.hidden = true; return; }
  m.hidden = false; m.className = "msg " + kind; m.textContent = text;
}
function connectorSecretRow(s) {
  const row = el("div", { class: "prov-row" });
  row.append(el("span", { class: "conn-sec-name" }, s.label || s.name),
    s.present
      ? el("span", { class: "prov-ok" }, "✓ set" + (s.fingerprint ? " · " + s.fingerprint : ""))
      : el("span", { class: "prov-missing" }, "⚠ not set"));
  const input = el("input", { type: "password", class: "prov-secret", placeholder: s.hint || ("paste " + (s.label || s.name)), autocomplete: "off", spellcheck: "false" });
  const btn = el("button", { class: "btn btn-sm" }, s.present ? "Rotate" : "Save");
  btn.addEventListener("click", () => setConnectorSecret(s.name, input));
  row.append(input, btn);
  return row;
}
function connectorCard(c) {
  const card = el("div", { class: "prov-card" });
  const head = el("div", { class: "prov-card-head" },
    el("span", { class: "prov-name" }, c.label),
    el("span", { class: "prov-spacer" }),
    el("span", { class: "badge " + (c.connected ? "badge-ok" : "badge-warn") }, c.connected ? "connected" : "needs setup"));
  if (c.connected) {
    head.append(el("span", { class: "badge " + (c.enabled ? "badge-ok" : "badge-warn") }, c.enabled ? "enabled" : "disabled"));
    const toggle = el("button", { class: "btn btn-sm" }, c.enabled ? "Disable" : "Enable");
    toggle.addEventListener("click", () => setConnectorEnabled(c.id, !c.enabled));
    head.append(toggle);
  }
  card.append(head);
  card.append(el("div", { class: "prov-blurb" }, c.blurb));
  if (c.steps && c.steps.length) {
    const det = el("details", { class: "conn-guide" });
    det.append(el("summary", {}, "Setup guide"));
    const ol = el("ol", { class: "conn-steps" });
    c.steps.forEach((st) => ol.append(el("li", {}, st)));
    det.append(ol);
    if (c.doc) det.append(el("div", { class: "conn-doc muted" }, "Full guide: " + c.doc));
    card.append(det);
  }
  (c.secrets || []).forEach((s) => card.append(connectorSecretRow(s)));
  return card;
}
function renderConnectors(list) {
  const wrap = $("#conn-cards");
  wrap.innerHTML = "";
  (list || []).forEach((c) => wrap.append(connectorCard(c)));
}
async function loadConnectors() {
  try { renderConnectors((await api("/api/connectors")).connectors); connMsg("", ""); }
  catch (e) { connMsg("err", "Load failed: " + e.message); }
}
async function setConnectorSecret(name, input) {
  const value = input.value.trim();
  if (!value) { connMsg("err", "Paste the value first."); return; }
  try {
    const res = await api("/api/secrets/rotate", { method: "POST", headers: JSON_HDR, body: JSON.stringify({ name, value }) });
    input.value = "";
    const restarted = res.restarted && res.restarted.length ? " · restarted " + res.restarted.join(", ") : "";
    connMsg(res.warning ? "err" : "ok", res.warning || ("Saved" + restarted + "."));
    loadConnectors();
  } catch (e) { connMsg("err", e.message); }
}
async function setConnectorEnabled(id, enabled) {
  try {
    const res = await api("/api/connectors/enable", { method: "POST", headers: JSON_HDR, body: JSON.stringify({ id, enabled }) });
    const restarted = res.restarted && res.restarted.length ? " · restarted " + res.restarted.join(", ") : "";
    connMsg("ok", (enabled ? "Enabled" : "Disabled") + restarted + ".");
    loadConnectors();
  } catch (e) { connMsg("err", e.message); }
}
$("#reload-connectors").addEventListener("click", loadConnectors);

// ---- sessions ------------------------------------------------------------
let sessionsTimer = null;

function cost(n) { return n ? "$" + Number(n).toFixed(2) : "—"; }
// How a session ran, from its durable meta: a compact provider + model badge (e.g. "sub · sonnet-5").
function shortModel(m) { return m ? m.replace(/^claude-/, "").replace(/-\d{8}$/, "") : ""; }
function providerShort(p) {
  return p === "claude_subscription" ? "sub" : p === "claude_api" ? "api" : p === "codex" ? "codex" : (p || "");
}
function agentCell(s) {
  const prov = providerShort(s.provider), model = shortModel(s.model);
  if (!prov && !model) return document.createTextNode("—");
  const wrap = el("span", { class: "agent-tag" + (s.subscription ? " sub" : ""), title: (s.provider || "") + (s.model ? " · " + s.model : "") });
  if (prov) wrap.append(el("span", { class: "agent-prov" }, prov));
  if (model) wrap.append(el("span", { class: "agent-model" }, model));
  return wrap;
}
function shortTime(s) {
  if (!s) return "—";
  const d = new Date(s);
  if (isNaN(d)) return s;
  return d.toISOString().slice(0, 16).replace("T", " ") + "Z";
}

function renderSessions(list) {
  const body = $("#sessions-body");
  body.innerHTML = "";
  if (!list || list.length === 0) {
    body.append(el("div", { class: "empty" }, "No sessions found."));
    return;
  }
  const head = el("tr", {},
    el("th", {}, ""), el("th", {}, "Status"), el("th", {}, "Repo"), el("th", {}, "Phase"),
    el("th", {}, "Branch"), el("th", {}, "PR"), el("th", {}, "VM"),
    el("th", {}, "Round"), el("th", {}, "Cost"), el("th", {}, "Agent"), el("th", {}, "Started"),
    el("th", {}, "Workflow"));
  const rows = list.map((s) => {
    const dotCls = "dot " + esc(s.status).toLowerCase();
    const pr = s.pr_number
      ? el("a", { href: s.pr_url || "#", target: "_blank", rel: "noreferrer" }, "#" + s.pr_number)
      : document.createTextNode("—");
    const vm = s.vm_state
      ? el("span", { class: "vm-" + esc(s.vm_state) }, s.vm_state)
      : document.createTextNode("—");
    const running = s.status === "Running";
    const watch = el("button", { class: "btn btn-sm", title: running ? "Connect to this session's agent" : "View this session's agent activity (read-only)" }, running ? "connect" : "view");
    watch.addEventListener("click", () => openActivity(s.workflow_id, (s.repo || "") + " · " + ellipsis(s.workflow_id, 20), running));
    const actions = [watch];
    if (s.stuck) {
      const term = el("button", { class: "btn btn-sm btn-danger", title: "Force-terminate this stalled workflow" }, "terminate");
      term.addEventListener("click", () => terminateSession(s.workflow_id));
      actions.push(term);
    }
    const phaseCell = s.stuck
      ? el("td", {}, el("span", { class: "badge badge-warn", title: "Running, but its status query failed — likely a stalled workflow task" }, "stuck"))
      : el("td", {}, phaseText(s));
    return el("tr", { class: s.stuck ? "row-warn" : "" },
      el("td", {}, el("div", { class: "row-actions" }, ...actions)),
      el("td", {}, el("span", { class: dotCls }), s.status),
      el("td", { class: "wrap" }, s.repo || "—"),
      phaseCell,
      el("td", { class: "wrap" }, s.branch || "—"),
      el("td", {}, pr),
      el("td", {}, vm),
      el("td", {}, s.review_round ? String(s.review_round) : "—"),
      el("td", {}, cost(s.cost_usd)),
      el("td", {}, agentCell(s)),
      el("td", {}, shortTime(s.start_time)),
      el("td", {}, el("span", { class: "tag", title: s.workflow_id }, ellipsis(s.workflow_id, 22))));
  });
  const table = el("table", {}, el("thead", {}, head), el("tbody", {}, ...rows));
  body.append(table);
}

function ellipsis(s, n) { s = esc(s); return s.length > n ? s.slice(0, n - 1) + "…" : s; }

// phaseText distinguishes a live phase, a still-open workflow whose status query didn't answer
// (worker busy, or the workflow task is stalled), and a normally-closed workflow.
function phaseText(s) {
  if (s.phase) return s.phase;
  if (s.status === "Running") return s.live ? "—" : "(no live data)";
  return "—";
}

async function loadSessions() {
  try {
    const { sessions } = await api("/api/sessions");
    renderSessions(sessions);
    setConn(true, `${sessions.length} session(s)`);
  } catch (e) {
    setConn(false, "temporal: " + e.message);
    $("#sessions-body").innerHTML = "";
    $("#sessions-body").append(el("div", { class: "empty" }, "Could not load sessions: " + e.message));
  }
}

function scheduleSessions() {
  clearInterval(sessionsTimer);
  if ($("#autorefresh").checked && !$("#view-sessions").hidden) {
    sessionsTimer = setInterval(loadSessions, 5000);
  }
}
$("#autorefresh").addEventListener("change", scheduleSessions);
$("#reload-sessions").addEventListener("click", loadSessions);

// ---- activity (live agent feed) ------------------------------------------
let activityES = null;
let activityID = null;
const ACTIVITY_MAX_ROWS = 2000;
const KIND = {
  meta: "•", system: "⚙", thinking: "💭", text: "💬", tool: "🔧", tool_result: "↳", result: "🏁", you: "›",
};

function openActivity(id, title, running) {
  closeActivity();
  activityID = id;
  clearInterval(sessionsTimer); // pause the table refresh while connected
  $("#sessions-body").hidden = true;
  document.querySelector("#view-sessions .bar").hidden = true;
  $("#activity-panel").hidden = false;
  $("#activity-title").textContent = title;
  $("#activity-feed").innerHTML = "";
  setActivityConn("", "connecting…");

  // The prompt line is live only for a Running session (a closed workflow can't take a message).
  const msg = $("#activity-msg"), send = $("#activity-input button");
  msg.disabled = !running;
  send.disabled = !running;
  msg.placeholder = running ? "Send a message to the agent…" : "session ended — read-only";
  document.querySelector(".activity-actions").hidden = !running; // abort/attach only for live sessions

  activityES = new EventSource(`/api/sessions/${encodeURIComponent(id)}/activity`);
  activityES.onopen = () => setActivityConn("badge-ok", running ? "connected" : "replay");
  activityES.onmessage = (e) => { try { appendActivity(JSON.parse(e.data)); } catch {} };
  activityES.onerror = () => setActivityConn("badge-warn", "reconnecting…");
}

// Playful "waking the agent" filler shown after you send, until the turn's first real output streams
// in (the workflow picking up the signal + a possible cold VM relaunch can take a few seconds).
const WAITING_MSGS = [
  "hehehe… nudging the agent",
  "tee-hee, waking it up",
  "*giggles* gathering the tools",
  "boop… poking the brain",
  "heh heh, spinning up",
  "warming the microVM",
  "cracking knuckles",
  "almost got it, hehe",
];
let waitTimer = null, waitEl = null;

function startWaiting() {
  stopWaiting();
  const feed = $("#activity-feed");
  waitEl = el("div", { class: "a-row a-waiting" },
    el("span", { class: "a-icon" }, "⏳"),
    el("span", { class: "a-body" }, WAITING_MSGS[0] + " …"));
  feed.append(waitEl);
  if ($("#activity-follow").checked) feed.scrollTop = feed.scrollHeight;
  let i = 0;
  waitTimer = setInterval(() => {
    i = (i + 1) % WAITING_MSGS.length;
    if (waitEl) waitEl.lastChild.textContent = WAITING_MSGS[i] + " …";
  }, 1800);
}
function stopWaiting() {
  if (waitTimer) { clearInterval(waitTimer); waitTimer = null; }
  if (waitEl) { waitEl.remove(); waitEl = null; }
}

async function sendAgentMessage(e) {
  e.preventDefault();
  const inp = $("#activity-msg");
  const text = inp.value.trim();
  if (!text || !activityID) return;
  inp.value = "";
  appendActivity({ kind: "you", text }); // echo immediately
  startWaiting();
  try {
    await api(`/api/sessions/${encodeURIComponent(activityID)}/message`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text }),
    });
  } catch (err) {
    stopWaiting();
    appendActivity({ kind: "meta", text: "⚠ could not send — " + err.message });
  }
}
$("#activity-input").addEventListener("submit", sendAgentMessage);

// console controls: abort (graceful stop) + attach (VS Code tunnel)
$("#activity-abort").addEventListener("click", () => {
  if (!activityID || !confirm("Gracefully stop this session and tear down its VM?")) return;
  api(`/api/sessions/${encodeURIComponent(activityID)}/abort`, {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ reason: "aborted from dashboard" }),
  }).then(() => appendActivity({ kind: "meta", text: "■ abort requested — the session will stop and tear down." }))
    .catch((e) => appendActivity({ kind: "meta", text: "⚠ abort failed — " + e.message }));
});
$("#activity-attach").addEventListener("click", () => {
  if (!activityID) return;
  api(`/api/sessions/${encodeURIComponent(activityID)}/attach`, { method: "POST" })
    .then(() => appendActivity({ kind: "meta", text: "🖥 attach requested — the VS Code link posts to this session's Slack thread as the tunnel comes up." }))
    .catch((e) => appendActivity({ kind: "meta", text: "⚠ attach failed — " + e.message }));
});

// ---- new session (dispatch) ----------------------------------------------
function nsMsg(kind, text) {
  const m = $("#ns-msg");
  if (!text) { m.hidden = true; return; }
  m.hidden = false; m.className = "msg " + kind; m.textContent = text;
}
function openNewSession() {
  $("#newsession-modal").hidden = false;
  nsMsg("", "");
  setTimeout(() => $("#ns-repo").focus(), 0);
}
function closeNewSession() { $("#newsession-modal").hidden = true; nsMsg("", ""); }
$("#new-session").addEventListener("click", openNewSession);
$("#ns-cancel").addEventListener("click", closeNewSession);
$("#ns-x").addEventListener("click", closeNewSession);
$("#newsession-modal").addEventListener("click", (e) => { if (e.target.id === "newsession-modal") closeNewSession(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !$("#newsession-modal").hidden) closeNewSession(); });
$("#newsession-panel").addEventListener("submit", async (e) => {
  e.preventDefault();
  const repo = $("#ns-repo").value.trim(), prompt = $("#ns-prompt").value.trim();
  if (!repo || !prompt) { nsMsg("err", "Repo and task are required."); return; }
  // Model is governed centrally by the active provider (Models view), so it's not sent here.
  const body = { repo, prompt, base_branch: $("#ns-base").value.trim(), model: "", keep_alive: $("#ns-keepalive").value.trim() };
  const btn = e.target.querySelector('button[type="submit"]');
  btn.disabled = true;
  nsMsg("", "dispatching…");
  try {
    const r = await api("/api/dispatch", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    closeNewSession();
    $("#ns-prompt").value = "";
    await loadSessions();
    if (r.session_id) openActivity(r.session_id, repo + " · " + ellipsis(r.session_id, 20), true);
  } catch (err) {
    nsMsg("err", "Dispatch failed — " + err.message);
  } finally {
    btn.disabled = false;
  }
});

async function terminateSession(id) {
  if (!confirm(`Force-terminate ${id}? This stops the workflow immediately.`)) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}/terminate`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ reason: "terminated from dashboard" }),
    });
    loadSessions();
  } catch (e) {
    setConn(false, "terminate: " + e.message);
  }
}

// ---- health --------------------------------------------------------------
async function loadHealth() {
  try {
    const { checks } = await api("/api/health");
    renderHealth(checks);
    setConn(true, "");
  } catch (e) {
    setConn(false, e.message);
    $("#health-body").innerHTML = "";
    $("#health-body").append(el("div", { class: "empty" }, "Could not load health: " + e.message));
  }
}
function renderHealth(checks) {
  const body = $("#health-body");
  body.innerHTML = "";
  const okCount = checks.filter((c) => c.ok).length;
  const badge = $("#health-summary");
  badge.className = "badge " + (okCount === checks.length ? "badge-ok" : "badge-warn");
  badge.textContent = `${okCount}/${checks.length} healthy`;
  const groups = { services: "Services", reachability: "Reachability", resources: "Resources", config: "Config" };
  for (const [g, label] of Object.entries(groups)) {
    const rows = checks.filter((c) => c.group === g);
    if (!rows.length) continue;
    body.append(el("div", { class: "col-head" }, label));
    const panel = el("div", { class: "panel" });
    for (const c of rows) {
      panel.append(el("div", { class: "health-row" },
        el("span", { class: "health-dot " + (c.ok ? "ok" : "bad") }),
        el("span", { class: "health-name" }, c.name),
        el("span", { class: "health-detail muted" }, c.detail || "")));
    }
    body.append(panel);
  }
}
$("#reload-health").addEventListener("click", loadHealth);

// ---- costs ---------------------------------------------------------------
async function loadCosts() {
  try {
    renderCosts(await api("/api/costs"));
    setConn(true, "");
  } catch (e) {
    setConn(false, e.message);
  }
}
function renderCosts(c) {
  const sum = $("#costs-summary");
  sum.innerHTML = "";
  sum.append(
    costCell("total spend", "$" + (c.total_usd || 0).toFixed(2)),
    costCell("tokens", (c.total_tokens || 0).toLocaleString()),
    costCell("sessions", String(c.sessions || 0)));
  // Subscription spend is notional (flat-rate), so call it out rather than mixing it into the total.
  const note = $("#costs-subnote");
  if (note) {
    if (c.subscription_sessions) {
      note.hidden = false;
      note.textContent = `${c.subscription_sessions} of these ran on a Claude subscription — their $${(c.subscription_usd || 0).toFixed(2)} is a notional, token-priced figure (flat-rate plan, not per-token billed).`;
    } else {
      note.hidden = true;
    }
  }
  fillCostTable($("#costs-repo"), ["Repo", "Sessions", "Cost"], (c.by_repo || []).map((r) => [r.repo, String(r.sessions), "$" + r.cost_usd.toFixed(2)]));
  fillCostTable($("#costs-day"), ["Day", "Sessions", "Cost"], (c.by_day || []).map((d) => [d.day, String(d.sessions), "$" + d.cost_usd.toFixed(2)]));
  fillCostTable($("#costs-top"), ["Session", "Repo", "Ran on", "When", "Cost"], (c.top || []).map((t) => [ellipsis(t.session_id, 22), t.repo, ranOnStr(t), shortTime(t.time), "$" + t.cost_usd.toFixed(2)]));
}
function ranOnStr(t) {
  const p = providerShort(t.provider), m = shortModel(t.model);
  const s = (p || "") + (p && m ? " · " : "") + (m || "");
  return s || "—";
}
function costCell(k, v) { return el("div", { class: "tool-card" }, el("div", { class: "tool-name" }, k), el("div", { class: "tool-version" }, v)); }
function fillCostTable(box, headers, rows) {
  box.innerHTML = "";
  if (!rows.length) { box.append(el("div", { class: "empty" }, "No data.")); return; }
  const head = el("tr", {}, ...headers.map((h) => el("th", {}, h)));
  const trs = rows.map((r) => el("tr", {}, ...r.map((v) => el("td", { class: "wrap" }, v))));
  box.append(el("table", {}, el("thead", {}, head), el("tbody", {}, ...trs)));
}
$("#reload-costs").addEventListener("click", loadCosts);

function closeActivity() {
  stopWaiting();
  if (activityES) { activityES.close(); activityES = null; }
  activityID = null;
  $("#activity-panel").hidden = true;
  $("#sessions-body").hidden = false;
  const bar = document.querySelector("#view-sessions .bar");
  if (bar) bar.hidden = false;
  scheduleSessions(); // guarded — only re-arms if the sessions view is visible
}

function setActivityConn(cls, text) {
  const c = $("#activity-conn");
  c.className = "badge " + cls;
  c.textContent = text;
}

function appendActivity(ev) {
  // The agent's first real output means the turn started — clear any "waking up" filler.
  if (ev.kind !== "you" && ev.kind !== "meta") stopWaiting();
  const feed = $("#activity-feed");
  const row = el("div", { class: "a-row a-" + esc(ev.kind) },
    el("span", { class: "a-icon" }, KIND[ev.kind] || "•"),
    ev.tool ? el("span", { class: "a-toolname" }, ev.tool) : document.createTextNode(""),
    el("span", { class: "a-body" }, ev.text || ""));
  feed.append(row);
  while (feed.childElementCount > ACTIVITY_MAX_ROWS) feed.removeChild(feed.firstChild);
  if ($("#activity-follow").checked) feed.scrollTop = feed.scrollHeight;
}

$("#activity-close").addEventListener("click", closeActivity);
$("#activity-follow").addEventListener("change", () => {
  if ($("#activity-follow").checked) $("#activity-feed").scrollTop = $("#activity-feed").scrollHeight;
});

// ---- config --------------------------------------------------------------
function renderConfigSummary(parsed) {
  const box = $("#config-summary");
  box.innerHTML = "";
  if (!parsed || Object.keys(parsed).length === 0) {
    box.append(el("div", { class: "muted" }, "No parsed config (empty or built-in defaults)."));
    return;
  }
  const dl = el("dl", { class: "kv" });
  const walk = (obj, prefix) => {
    for (const [k, v] of Object.entries(obj)) {
      if (v && typeof v === "object" && !Array.isArray(v)) {
        dl.append(el("div", { class: "group" }, prefix ? `${prefix}.${k}` : k));
        walk(v, prefix ? `${prefix}.${k}` : k);
      } else {
        dl.append(el("dt", {}, k), el("dd", {}, Array.isArray(v) ? v.join(", ") : String(v)));
      }
    }
  };
  walk(parsed, "");
  box.append(dl);
}

function configMsg(kind, text) {
  const m = $("#config-msg");
  if (!text) { m.hidden = true; return; }
  m.hidden = false;
  m.className = "msg " + kind;
  m.textContent = text;
}

async function loadConfig() {
  try {
    const v = await api("/api/config");
    $("#config-editor").value = v.raw || "";
    $("#config-meta").textContent = v.exists
      ? `${v.path} · modified ${shortTime(v.modified)}`
      : `${v.path} · (not present — using built-in defaults)`;
    renderConfigSummary(v.parsed);
    configMsg("", "");
    setConn(true, "");
  } catch (e) {
    configMsg("err", "Load failed: " + e.message);
  }
}

async function saveConfig() {
  const btn = $("#save-config");
  btn.disabled = true;
  try {
    const v = await api("/api/config", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ raw: $("#config-editor").value }),
    });
    renderConfigSummary(v.parsed);
    $("#config-meta").textContent = `${v.path} · modified ${shortTime(v.modified)}`;
    configMsg("ok", "Saved. Takes effect on the next dispatch (the worker re-reads per task).");
  } catch (e) {
    configMsg("err", "Not saved — " + e.message);
  } finally {
    btn.disabled = false;
  }
}
$("#save-config").addEventListener("click", saveConfig);
$("#reload-config").addEventListener("click", loadConfig);

// ---- model & provider (the one place that governs the whole box) ---------
const JSON_HDR = { "Content-Type": "application/json" };

function provMsg(kind, text) {
  const m = $("#prov-msg");
  if (!text) { m.hidden = true; return; }
  m.hidden = false; m.className = "msg " + kind; m.textContent = text;
}

// modelPicker is a dropdown of the provider's known models plus a "Custom…" entry that reveals a
// free-text field, so the operator can point at any model identifier (a preview model, a fine-tune).
function modelPicker(options, current) {
  const wrap = el("span", { class: "prov-picker" });
  const sel = el("select", { class: "prov-select" });
  const list = (options || []).slice();
  const isCustom = current && !list.includes(current);
  list.forEach((o) => {
    const opt = el("option", { value: o }, o);
    if (o === current) opt.selected = true;
    sel.append(opt);
  });
  const customOpt = el("option", { value: "__custom__" }, "Custom…");
  if (isCustom) customOpt.selected = true;
  sel.append(customOpt);
  const input = el("input", { class: "prov-custom", type: "text", placeholder: "model id", value: isCustom ? current : "", spellcheck: "false", autocomplete: "off" });
  input.hidden = !isCustom;
  sel.addEventListener("change", () => {
    input.hidden = sel.value !== "__custom__";
    if (!input.hidden) input.focus();
  });
  wrap.append(sel, input);
  return { node: wrap, value: () => (sel.value === "__custom__" ? input.value.trim() : sel.value) };
}

function providerCard(p) {
  const card = el("div", { class: "prov-card" + (p.active ? " active" : "") });

  // head: status glyph · name · harness · (single-user tag) · spacer · Active/Activate
  const head = el("div", { class: "prov-card-head" },
    el("span", { class: "prov-radio" }, p.active ? "●" : "○"),
    el("span", { class: "prov-name" }, p.label),
    el("span", { class: "prov-harness" }, p.harness));
  if (p.subscription) head.append(el("span", { class: "prov-tag" }, "single-user"));
  head.append(el("span", { class: "prov-spacer" }));
  if (p.active) {
    head.append(el("span", { class: "prov-active-badge" }, "active"));
  } else {
    const b = el("button", { class: "btn btn-primary btn-sm" }, "Activate");
    if (!p.ready) { b.disabled = true; b.title = "Set its key first"; }
    b.addEventListener("click", () => activateProvider(p.id));
    head.append(b);
  }
  card.append(head);

  card.append(el("div", { class: "prov-blurb" }, p.blurb));

  // key row — set/rotate the provider's secret inline
  const sec = el("div", { class: "prov-row" });
  sec.append(el("span", { class: "prov-lbl" }, "Key"),
    p.secret_present
      ? el("span", { class: "prov-ok" }, "✓ set" + (p.secret_fingerprint ? " · " + p.secret_fingerprint : ""))
      : el("span", { class: "prov-missing" }, "⚠ not set"));
  const secInput = el("input", { type: "password", class: "prov-secret", placeholder: p.secret_hint || ("paste " + (p.secret_label || "secret")), autocomplete: "off", spellcheck: "false" });
  const secBtn = el("button", { class: "btn btn-sm" }, p.secret_present ? "Rotate" : "Save");
  secBtn.addEventListener("click", () => setProviderSecret(p.id, secInput));
  sec.append(secInput, secBtn);
  card.append(sec);

  // model row — main + helper, both with Custom… support
  const mr = el("div", { class: "prov-row" });
  const model = modelPicker(p.models, p.model);
  const cheap = modelPicker(p.cheap_models, p.cheap_model);
  const modelBtn = el("button", { class: "btn btn-sm" }, "Save");
  modelBtn.addEventListener("click", () => setProviderModel(p.id, model.value(), cheap.value()));
  mr.append(el("span", { class: "prov-lbl" }, "Model"), model.node,
    el("span", { class: "prov-lbl" }, "Helper"), cheap.node, modelBtn);
  card.append(mr);

  return card;
}

function renderProvider(v) {
  const wrap = $("#prov-cards");
  wrap.innerHTML = "";
  (v.providers || []).forEach((p) => wrap.append(providerCard(p)));
  const active = (v.providers || []).find((p) => p.active);
  const pill = $("#models-active");
  if (pill) {
    pill.textContent = active ? "active · " + active.label : "none active";
    pill.className = "pill" + (active ? " on" : "");
  }
}

async function loadProvider() {
  try { renderProvider(await api("/api/provider")); provMsg("", ""); }
  catch (e) { provMsg("err", "Load failed: " + e.message); }
}

async function activateProvider(id) {
  const nice = id.replace(/_/g, " ");
  if (id === "claude_subscription" && !confirm("Run the whole box on your Claude subscription? Single-user only — the token runs inside each VM and helper calls use it too. See docs/subscription-auth.md.")) return;
  try {
    renderProvider(await api("/api/provider/activate", { method: "POST", headers: JSON_HDR, body: JSON.stringify({ id }) }));
    provMsg("ok", "Now running on " + nice + " — agent and helpers. Applies on the next dispatch.");
  } catch (e) { provMsg("err", e.message); loadProvider(); }
}

async function setProviderModel(id, model, cheap_model) {
  try {
    renderProvider(await api("/api/provider/model", { method: "POST", headers: JSON_HDR, body: JSON.stringify({ id, model, cheap_model }) }));
    provMsg("ok", "Model saved for " + id.replace(/_/g, " ") + ".");
  } catch (e) { provMsg("err", e.message); }
}

async function setProviderSecret(id, input) {
  const value = input.value.trim();
  if (!value) { provMsg("err", "Paste the secret first."); return; }
  try {
    const res = await api("/api/provider/secret", { method: "POST", headers: JSON_HDR, body: JSON.stringify({ id, value }) });
    input.value = "";
    renderProvider(res.view);
    const restarted = res.restarted && res.restarted.length ? " · restarted " + res.restarted.join(", ") : "";
    provMsg(res.warning ? "err" : "ok", res.warning || ("Secret saved" + restarted + "."));
  } catch (e) { provMsg("err", e.message); }
}

// ---- tools ---------------------------------------------------------------
let toolsPollTimer = null;

// One card per agent tool (its API key: claude → --claude, codex → --codex).
const TOOLS = [
  { key: "claude", label: "claude-code", hint: "e.g. 2.1.220", desc: "Anthropic's Claude Code CLI" },
  { key: "codex", label: "codex", hint: "e.g. latest", desc: "OpenAI's Codex CLI (used when a repo picks the codex harness)" },
];

function renderTools(v) {
  $("#tools-image").textContent = v.current_sha ? "active image · " + ellipsis(v.current_sha, 16) : "";

  const cards = $("#tools-cards");
  cards.innerHTML = "";
  const versions = { claude: v.claude_version, codex: v.codex_version };
  for (const t of TOOLS) cards.append(toolCard(t, versions[t.key]));

  const a = $("#tools-audit");
  a.innerHTML = "";
  if (!v.audit || v.audit.length === 0) {
    a.append(el("div", { class: "empty" }, "No updates recorded yet."));
  } else {
    const head = el("tr", {}, el("th", {}, "When"), el("th", {}, "claude-code"),
      el("th", {}, "codex"), el("th", {}, "image sha"));
    const rows = v.audit.map((e) => el("tr", {},
      el("td", {}, shortTime(e.timestamp)),
      el("td", {}, e.claude || "—"),
      el("td", {}, e.codex || "—"),
      el("td", {}, el("span", { class: "tag", title: e.sha }, ellipsis(e.sha, 16)))));
    a.append(el("table", {}, el("thead", {}, head), el("tbody", {}, ...rows)));
  }

  applyJob(v.job);
}

function toolCard(t, version) {
  const input = el("input", { type: "text", placeholder: t.hint, autocomplete: "off" });
  const btn = el("button", { class: "btn btn-primary btn-sm" }, "Update");
  const go = () => runUpdateTool(t, input.value.trim());
  btn.addEventListener("click", go);
  input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); go(); } });
  return el("div", { class: "tool-card" },
    el("div", { class: "tool-name" }, t.label),
    el("div", { class: "tool-desc muted" }, t.desc),
    el("div", { class: "tool-version" }, version || "—"),
    el("div", { class: "tool-update" }, input, btn));
}

function toolButtons() { return document.querySelectorAll("#tools-cards .tool-update button"); }

function applyJob(job) {
  const out = $("#update-output");
  const setDisabled = (d) => toolButtons().forEach((b) => (b.disabled = d));
  if (!job) { setDisabled(false); return; }
  out.hidden = false;
  out.textContent = job.output || (job.running ? "starting…" : "");
  out.scrollTop = out.scrollHeight;
  if (job.running) {
    setDisabled(true); // only one image assemble runs at a time
    if (!toolsPollTimer) toolsPollTimer = setInterval(loadTools, 2000);
  } else {
    setDisabled(false);
    clearInterval(toolsPollTimer); toolsPollTimer = null;
    const tail = job.ok ? "\n\n✓ update complete" : `\n\n✗ update failed: ${job.error || "unknown"}`;
    if (!out.textContent.includes(tail.trim())) out.textContent += tail;
  }
}

async function loadTools() {
  try {
    const v = await api("/api/tools");
    renderTools(v);
    setConn(true, "");
  } catch (e) {
    setConn(false, e.message);
  }
}

async function runUpdateTool(t, version) {
  if (!version && !confirm(`Re-assemble the image at the current ${t.label} pin (no version change)?`)) return;
  const body = {};
  if (version) body[t.key] = version;
  toolButtons().forEach((b) => (b.disabled = true));
  $("#update-output").hidden = false;
  $("#update-output").textContent = `updating ${t.label}…`;
  try {
    const { job } = await api("/api/tools/update", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    applyJob(job);
  } catch (e) {
    $("#update-output").textContent = "could not start: " + e.message;
    toolButtons().forEach((b) => (b.disabled = false));
  }
}
$("#reload-tools").addEventListener("click", loadTools);

// ---- vms -----------------------------------------------------------------
let vmsTimer = null;

function fmtUptime(sec) {
  if (!sec || sec < 0) return "—";
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}

function renderVMs(list) {
  const body = $("#vms-body");
  body.innerHTML = "";
  if (!list || list.length === 0) {
    body.append(el("div", { class: "empty" }, "No VMs running."));
    return;
  }
  const head = el("tr", {},
    el("th", {}, "Session"), el("th", {}, ""), el("th", {}, "Image"),
    el("th", {}, "Guest IP"), el("th", {}, "Host IP"), el("th", {}, "vCPU"),
    el("th", {}, "Mem"), el("th", {}, "Uptime"), el("th", {}, "PID"));
  const rows = list.map((v) => {
    const flag = v.orphan
      ? el("span", { class: "badge badge-warn", title: "No Running workflow for this session" }, "orphan")
      : (v.session_known ? el("span", { class: "badge badge-ok" }, "live") : document.createTextNode(""));
    return el("tr", { class: v.orphan ? "row-warn" : "" },
      el("td", { class: "wrap", title: v.session_id }, ellipsis(v.session_id, 26)),
      el("td", {}, flag),
      el("td", {}, el("span", { class: "tag", title: v.image_sha }, ellipsis(v.image_sha, 12))),
      el("td", {}, v.guest_ip || "—"),
      el("td", {}, v.host_ip || "—"),
      el("td", {}, String(v.vcpus || "—")),
      el("td", {}, v.mem_mib ? (v.mem_mib + " MiB") : "—"),
      el("td", { title: v.started_iso || "" }, fmtUptime(v.uptime_sec)),
      el("td", {}, String(v.pid || "—")));
  });
  body.append(el("table", {}, el("thead", {}, head), el("tbody", {}, ...rows)));
}

async function loadVMs() {
  try {
    const { vms } = await api("/api/vms");
    renderVMs(vms);
    const orphans = (vms || []).filter((v) => v.orphan).length;
    setConn(true, `${(vms || []).length} VM(s)` + (orphans ? ` · ${orphans} orphan` : ""));
  } catch (e) {
    setConn(false, "fleet-agent: " + e.message);
    $("#vms-body").innerHTML = "";
    $("#vms-body").append(el("div", { class: "empty" }, "Could not load VMs: " + e.message));
  }
}

function scheduleVMs() {
  clearInterval(vmsTimer);
  if ($("#vms-autorefresh").checked && !$("#view-vms").hidden) {
    vmsTimer = setInterval(loadVMs, 5000);
  }
}
$("#vms-autorefresh").addEventListener("change", scheduleVMs);
$("#reload-vms").addEventListener("click", loadVMs);

// ---- config helper (snippets + reference) --------------------------------
let configHelpLoaded = false;

async function loadConfigHelp() {
  if (configHelpLoaded) return;
  try {
    const h = await api("/api/config/schema");
    const snip = $("#config-snippets");
    snip.innerHTML = "";
    for (const s of h.snippets || []) {
      const b = el("button", { class: "btn btn-sm", title: "Append this block to the editor" }, s.label);
      b.addEventListener("click", () => insertSnippet(s.yaml));
      snip.append(b);
    }
    $("#config-load-example").onclick = () => {
      if (!$("#config-editor").value.trim() || confirm("Replace the editor contents with the annotated example?"))
        $("#config-editor").value = h.example || "";
    };
    renderConfigReference(h.keys || []);
    configHelpLoaded = true;
  } catch { /* help is optional; editor still works */ }
}

function insertSnippet(yaml) {
  const ed = $("#config-editor");
  const cur = ed.value.replace(/\s*$/, "");
  ed.value = (cur ? cur + "\n\n" : "") + yaml.replace(/\s*$/, "") + "\n";
  ed.focus();
}

function renderConfigReference(keys) {
  const box = $("#config-reference");
  box.innerHTML = "";
  const head = el("tr", {}, el("th", {}, "Key"), el("th", {}, "Default"), el("th", {}, "Notes"));
  const rows = keys.map((k) => el("tr", {},
    el("td", { class: "wrap" }, el("code", {}, k.path)),
    el("td", { class: "wrap" }, k.default || "—"),
    el("td", { class: "wrap" }, el("div", {}, k.desc), el("div", { class: "muted" }, k.allowed ? ("→ " + k.allowed) : ""))));
  box.append(el("table", {}, el("thead", {}, head), el("tbody", {}, ...rows)));
}

document.querySelectorAll(".subtab").forEach((t) =>
  t.addEventListener("click", () => {
    document.querySelectorAll(".subtab").forEach((x) => x.removeAttribute("aria-current"));
    t.setAttribute("aria-current", "true");
    for (const pane of ["summary", "reference"])
      document.querySelector(`[id^="config-"][data-pane="${pane}"]`).hidden = pane !== t.dataset.pane;
  }));

// ---- instructions --------------------------------------------------------
function instructionsMsg(kind, text) {
  const m = $("#instructions-msg");
  if (!text) { m.hidden = true; return; }
  m.hidden = false;
  m.className = "msg " + kind;
  m.textContent = text;
}

// A single document editor with a left rail to switch between the three editable files. Each doc's
// unsaved edits are kept in memory, so switching files never loses work.
const PREAMBLE_WARN =
  'The agent\'s built-in base prompt for this turn. Editing replaces it for this box (repos can\'t). ' +
  '⚠ Some lines are load-bearing — e.g. <em>"do NOT run git commit/push or gh"</em> is required because ' +
  'the supervisor does the commit/push/PR; drop it and the agent may commit itself, leaving the tree ' +
  'clean so <strong>work never gets pushed</strong>. Use <em>Diff</em> to see your changes and ' +
  '<em>Reset to default</em> if unsure. Applies on the next dispatch.';

const DOCS = [
  { id: "instructions", kind: "instructions", label: "Box instructions", sub: "appended to every session",
    placeholder: "e.g. Follow our commit-message convention. Prefer small, focused PRs. Flag any change under infra/.",
    note: 'Appended to every session\'s system prompt. A repo\'s <code>.mandobox.yml instructions:</code> overrides this per repo. Empty → none. Guidance the agent follows, not a hard guardrail.' },
  { id: "autonomous", kind: "preamble", name: "autonomous", label: "Autonomous preamble", sub: "base prompt · first / headless turn", note: PREAMBLE_WARN },
  { id: "collaborate", kind: "preamble", name: "collaborate", label: "Collaborate preamble", sub: "base prompt · review / resume turn", note: PREAMBLE_WARN },
];

const docState = {};   // id → { server, def, hasOverride, value, modified, path, exists }
let curDoc = "instructions";
let wrapOn = true;
let diffOn = false;

function docBaseline(id) {
  const s = docState[id];
  if (!s) return "";
  return s.def !== undefined && s.hasOverride === false && DOCS.find((d) => d.id === id).kind === "preamble"
    ? s.def : s.server;
}
function docDirty(id) {
  const s = docState[id];
  return s && s.value !== docBaseline(id);
}
function anyDirty() { return DOCS.some((d) => docDirty(d.id)); }

async function loadDocs() {
  try {
    const [ins, pre] = await Promise.all([api("/api/instructions"), api("/api/preambles")]);
    docState.instructions = {
      server: ins.raw || "", def: undefined, hasOverride: undefined,
      value: keepEdit("instructions", ins.raw || ""), modified: ins.modified, path: ins.path, exists: ins.exists,
    };
    for (const p of pre.preambles) {
      const baseline = p.has_override ? p.override : (p.default || "");
      docState[p.name] = {
        server: p.override || "", def: p.default || "", hasOverride: p.has_override,
        value: keepEdit(p.name, baseline), modified: p.modified, path: p.path,
      };
    }
    renderRail();
    selectDoc(curDoc, true);
    setConn(true, "");
  } catch (e) {
    instructionsMsg("err", "Load failed: " + e.message);
  }
}
// keepEdit preserves an in-memory unsaved edit across reloads; else adopts the server value.
function keepEdit(id, serverVal) {
  const s = docState[id];
  return s && s.value !== undefined && docDirty(id) ? s.value : serverVal;
}

function docBadge(id) {
  const d = DOCS.find((x) => x.id === id), s = docState[id];
  if (!s) return null;
  if (d.kind === "preamble") {
    return s.hasOverride ? el("span", { class: "badge badge-warn" }, "overridden") : el("span", { class: "badge badge-ok" }, "default");
  }
  return (s.exists && s.server.trim()) ? el("span", { class: "badge badge-ok" }, "set") : el("span", { class: "badge" }, "empty");
}

function renderRail() {
  const rail = $("#doc-rail");
  rail.innerHTML = "";
  for (const d of DOCS) {
    const item = el("button", { class: "doc-item" + (d.id === curDoc ? " active" : "") },
      el("div", { class: "doc-item-top" },
        el("span", {}, d.label),
        docDirty(d.id) ? el("span", { class: "dot-dirty", title: "unsaved" }) : document.createTextNode("")),
      el("div", { class: "doc-item-sub muted" }, d.sub),
      el("div", { class: "doc-item-badge" }, docBadge(d.id) || document.createTextNode("")));
    item.addEventListener("click", () => selectDoc(d.id));
    rail.append(item);
  }
}

function selectDoc(id, force) {
  if (!force && id === curDoc) return;
  if (!force) instructionsMsg("", ""); // clear stale status on a user switch, keep it after a save
  curDoc = id;
  diffOn = false;
  const d = DOCS.find((x) => x.id === id), s = docState[id] || { value: "", path: "" };
  $("#doc-title").textContent = d.label;
  $("#doc-sub").textContent = d.sub;
  $("#doc-note").innerHTML = d.note;
  const badge = $("#doc-badge"); badge.innerHTML = ""; const b = docBadge(id); if (b) badge.append(b);
  const ed = $("#doc-editor");
  ed.value = s.value;
  ed.placeholder = d.placeholder || "";
  $("#doc-diff").hidden = d.kind !== "preamble";
  $("#doc-reset").hidden = d.kind !== "preamble";
  $("#doc-diffview").hidden = true;
  $("#doc-diff").textContent = "Diff";
  ed.hidden = false;
  applyWrap();
  refreshDocUI();
  renderRail();
}

function refreshDocUI() {
  const id = curDoc, s = docState[id] || {}, d = DOCS.find((x) => x.id === id);
  const dirty = docDirty(id);
  $("#doc-dirty").hidden = !dirty;
  $("#doc-save").disabled = !dirty;
  $("#doc-revert").disabled = !dirty;
  $("#doc-reset").disabled = !(d.kind === "preamble" && s.hasOverride);
  $("#doc-meta").textContent = s.path
    ? (s.modified ? `${s.path} · modified ${shortTime(s.modified)}` : `${s.path} · not set`)
    : "";
  const v = s.value || "";
  $("#doc-count").textContent = `${v.split("\n").length} lines · ${v.length} chars`;
  const badge = $("#doc-badge"); badge.innerHTML = ""; const b = docBadge(id); if (b) badge.append(b);
}

$("#doc-editor").addEventListener("input", (e) => {
  if (docState[curDoc]) docState[curDoc].value = e.target.value;
  refreshDocUI();
  renderRail();
});
// Tab inserts two spaces instead of leaving the field.
$("#doc-editor").addEventListener("keydown", (e) => {
  if (e.key === "Tab") {
    e.preventDefault();
    const t = e.target, s = t.selectionStart;
    t.value = t.value.slice(0, s) + "  " + t.value.slice(t.selectionEnd);
    t.selectionStart = t.selectionEnd = s + 2;
    docState[curDoc].value = t.value; refreshDocUI(); renderRail();
  }
});

async function saveDoc() {
  const id = curDoc, d = DOCS.find((x) => x.id === id), s = docState[id];
  $("#doc-save").disabled = true;
  try {
    if (d.kind === "instructions") {
      await api("/api/instructions", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ raw: s.value }) });
    } else {
      await api("/api/preambles", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: d.name, raw: s.value }) });
    }
    instructionsMsg("ok", `${d.label} saved. Applies on the next dispatch.`);
    await loadDocs();
  } catch (e) {
    instructionsMsg("err", "Not saved — " + e.message);
    refreshDocUI();
  }
}

function revertDoc() {
  const s = docState[curDoc];
  if (!s) return;
  s.value = docBaseline(curDoc);
  $("#doc-editor").value = s.value;
  instructionsMsg("", "");
  refreshDocUI(); renderRail();
}

async function resetDoc() {
  const d = DOCS.find((x) => x.id === curDoc);
  if (d.kind !== "preamble") return;
  if (!confirm(`Reset the ${d.label} to the built-in default? This clears your override.`)) return;
  try {
    await api("/api/preambles", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: d.name, raw: "" }) });
    if (docState[d.id]) docState[d.id].value = undefined; // drop any edit so reload adopts the default
    instructionsMsg("ok", `${d.label} reset to the built-in default.`);
    await loadDocs();
  } catch (e) {
    instructionsMsg("err", "Reset failed — " + e.message);
  }
}

function applyWrap() {
  const ed = $("#doc-editor");
  ed.wrap = wrapOn ? "soft" : "off";
  ed.classList.toggle("nowrap", !wrapOn);
  $("#doc-wrap").textContent = "Wrap: " + (wrapOn ? "on" : "off");
}
function toggleWrap() { wrapOn = !wrapOn; applyWrap(); }

function toggleDiff() {
  const d = DOCS.find((x) => x.id === curDoc), s = docState[curDoc];
  if (d.kind !== "preamble") return;
  diffOn = !diffOn;
  const ed = $("#doc-editor"), dv = $("#doc-diffview");
  if (diffOn) {
    dv.innerHTML = "";
    dv.append(renderDiff(s.def || "", s.value || ""));
    ed.hidden = true; dv.hidden = false; $("#doc-diff").textContent = "Edit";
  } else {
    ed.hidden = false; dv.hidden = true; $("#doc-diff").textContent = "Diff";
  }
}

// renderDiff shows a line diff of the built-in default (old) vs the current text (new): what you
// removed from the default and what you added.
function renderDiff(oldText, newText) {
  const a = oldText.split("\n"), b = newText.split("\n"), n = a.length, m = b.length;
  const dp = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--)
    dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  const box = el("div", {});
  let i = 0, j = 0;
  const line = (cls, sign, text) => el("div", { class: "dline " + cls }, el("span", { class: "dsign" }, sign), text);
  while (i < n && j < m) {
    if (a[i] === b[j]) { box.append(line("same", " ", a[i])); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) { box.append(line("del", "-", a[i])); i++; }
    else { box.append(line("add", "+", b[j])); j++; }
  }
  while (i < n) box.append(line("del", "-", a[i++]));
  while (j < m) box.append(line("add", "+", b[j++]));
  if (!box.children.length) box.append(el("div", { class: "muted" }, "Identical to the default."));
  return box;
}

$("#doc-save").addEventListener("click", saveDoc);
$("#doc-revert").addEventListener("click", revertDoc);
$("#doc-reset").addEventListener("click", resetDoc);
$("#doc-wrap").addEventListener("click", toggleWrap);
$("#doc-diff").addEventListener("click", toggleDiff);
window.addEventListener("beforeunload", (e) => { if (anyDirty()) { e.preventDefault(); e.returnValue = ""; } });

// ---- secrets -------------------------------------------------------------
function secretMsg(kind, text) {
  const m = $("#secret-msg");
  if (!text) { m.hidden = true; return; }
  m.hidden = false;
  m.className = "msg " + kind;
  m.textContent = text;
}

function renderSecrets(list) {
  const body = $("#secrets-body");
  body.innerHTML = "";
  const head = el("tr", {},
    el("th", {}, "Secret"), el("th", {}, "Status"), el("th", {}, "Fingerprint"),
    el("th", {}, "Perms"), el("th", {}, "Changed"), el("th", {}, "Restarts"), el("th", {}, ""));
  const rows = [];
  for (const s of list) {
    const status = s.present
      ? el("span", { class: "badge badge-ok" }, "set")
      : el("span", { class: "badge badge-warn" }, "missing");
    const rotateBtn = el("button", { class: "btn btn-sm" }, "Rotate");
    rotateBtn.addEventListener("click", () => toggleRotate(s));
    const tr = el("tr", {},
      el("td", { class: "wrap" },
        el("div", {}, s.label),
        s.desc ? el("div", { class: "muted desc" }, s.desc) : document.createTextNode(""),
        el("div", { class: "muted path", title: s.path }, ellipsis(s.path, 34))),
      el("td", {}, status),
      el("td", {}, s.fingerprint ? el("span", { class: "tag" }, s.fingerprint) : document.createTextNode("—")),
      el("td", {}, s.mode || "—"),
      el("td", { title: s.last_rotated ? ("rotated via dashboard " + shortTime(s.last_rotated)) : "" },
        shortTime(s.modified) + (s.last_rotated ? " *" : "")),
      el("td", { class: "wrap muted" }, (s.restarts || []).join(", ") || "—"),
      el("td", {}, s.editable ? rotateBtn : el("span", { class: "muted" }, "—")));
    rows.push(tr);
    // hidden editor row
    const editRow = el("tr", { class: "rotate-row", id: `rotate-${s.name}`, hidden: "hidden" },
      el("td", { colspan: "7" }));
    rows.push(editRow);
  }
  body.append(el("table", {}, el("thead", {}, head), el("tbody", {}, ...rows)));
}

function toggleRotate(s) {
  const row = $(`#rotate-${s.name}`);
  if (!row.hidden) { row.hidden = true; row.firstChild.innerHTML = ""; return; }
  const cell = row.firstChild;
  cell.innerHTML = "";
  const ta = el("textarea", { class: "secret-input", spellcheck: "false", placeholder: s.hint || "new value", rows: s.kind === "file" ? "6" : "1" });
  const warn = el("div", { class: "muted" }, `Writes the new value (0600) and restarts: ${(s.restarts || []).join(", ") || "nothing"}.`);
  const save = el("button", { class: "btn btn-primary btn-sm" }, "Rotate + restart");
  const cancel = el("button", { class: "btn btn-sm" }, "Cancel");
  cancel.addEventListener("click", () => toggleRotate(s));
  save.addEventListener("click", () => doRotate(s, ta.value, save));
  cell.append(warn, ta, el("div", { class: "rotate-actions" }, save, cancel));
  row.hidden = false;
  ta.focus();
}

async function doRotate(s, value, btn) {
  if (!value.trim()) { secretMsg("err", "Enter a value first."); return; }
  if (!confirm(`Rotate ${s.label} and restart ${(s.restarts || []).join(", ") || "nothing"}?`)) return;
  btn.disabled = true;
  try {
    const r = await api("/api/secrets/rotate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: s.name, value }),
    });
    if (r.warning) secretMsg("err", `${s.label}: ${r.warning}`);
    else secretMsg("ok", `${s.label} rotated. Restarted: ${(r.restarted || []).join(", ") || "nothing"}.`);
    loadSecrets();
  } catch (e) {
    secretMsg("err", "Rotation failed: " + e.message);
    btn.disabled = false;
  }
}

async function loadSecrets() {
  try {
    const { secrets } = await api("/api/secrets");
    renderSecrets(secrets);
    setConn(true, "");
  } catch (e) {
    setConn(false, e.message);
    $("#secrets-body").innerHTML = "";
    $("#secrets-body").append(el("div", { class: "empty" }, "Could not load secrets: " + e.message));
  }
}
$("#reload-secrets").addEventListener("click", loadSecrets);

// ---- boot ----------------------------------------------------------------
document.addEventListener("visibilitychange", () => {
  if (document.hidden) { clearInterval(sessionsTimer); clearInterval(vmsTimer); }
  else { scheduleSessions(); scheduleVMs(); }
});

const initial = views.includes(location.hash.slice(1)) ? location.hash.slice(1) : "sessions";
show(initial);
scheduleSessions();
