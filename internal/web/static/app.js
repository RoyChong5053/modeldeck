"use strict";

const $ = (id) => document.getElementById(id);

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  const text = await r.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch (e) { data = { raw: text }; }
  return { ok: r.ok, status: r.status, data };
}

function fmtKB(kb) {
  if (!kb) return "—";
  if (kb < 1024) return kb + " KB";
  return (kb / 1024).toFixed(0) + " MB";
}
function fmtMB(mb) {
  if (!mb) return "—";
  if (mb >= 1024) return (mb / 1024).toFixed(1) + " GB";
  return mb + " MB";
}
function fmtIdle(iso) {
  if (!iso || iso.startsWith("0001")) return "—";
  const s = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  return Math.floor(s / 3600) + "h";
}
function log(msg) {
  const el = $("events");
  const line = new Date().toLocaleTimeString() + "  " + msg;
  el.textContent = (line + "\n" + el.textContent).split("\n").slice(0, 200).join("\n");
}

// ---- keyed table rows (updated in place so clicks never hit stale nodes) ----
const rows = new Map();

function cell() { return document.createElement("td"); }
function button(label, cls, fn) {
  const b = document.createElement("button");
  b.textContent = label;
  if (cls) b.className = cls;
  b.onclick = fn;
  return b;
}

function makeRow() {
  const tr = document.createElement("tr");
  const refs = {};

  refs.name = cell();
  refs.state = cell();
  refs.port = cell();
  refs.inflight = cell();
  refs.rss = cell();
  refs.vram = cell();
  refs.idle = cell();

  const act = cell();
  const box = document.createElement("div");
  box.className = "actions";
  refs.start = button("启动", "primary", () => actRow(refs.id, "start"));
  refs.stop = button("停止", "", () => actRow(refs.id, "stop"));
  refs.restart = button("重启", "", () => actRow(refs.id, "restart"));
  refs.pin = button("📌 固定", "", () => pinRow(refs.id, !refs.pinned));
  refs.logs = button("日志", "ghost", () => openLog(refs.id));
  box.append(refs.start, refs.stop, refs.restart, refs.pin, refs.logs);
  act.appendChild(box);

  tr.append(refs.name, refs.state, refs.port, refs.inflight, refs.rss, refs.vram, refs.idle, act);
  return { id: "", tr, refs, pinned: false };
}

function updateRow(row, e) {
  const r = row.refs;
  row.id = e.id;
  r.id = e.id;
  row.pinned = !!e.pinned;
  r.pinned = !!e.pinned;

  r.name.innerHTML = `<strong>${e.name || e.id}</strong>` +
    `<div class="muted" style="font-size:12px">${e.id}` +
    ((e.aliases && e.aliases.length) ? " · " + e.aliases.join(", ") : "") + `</div>`;

  r.state.innerHTML = `<span class="pill ${e.state}">${e.state}</span>`;
  if (e.last_error) r.state.title = e.last_error; else r.state.removeAttribute("title");

  r.port.textContent = e.port || "—";
  r.inflight.textContent = e.inflight || 0;
  r.rss.textContent = fmtKB(e.cur_rss_kb);
  r.vram.textContent = fmtMB(e.cur_vram_mb);
  r.idle.textContent = e.state === "ready" ? fmtIdle(e.last_used) : "—";

  const running = e.state === "ready" || e.state === "starting";
  r.start.disabled = running;
  r.stop.disabled = !running;
  r.restart.disabled = !running;
  r.pin.textContent = e.pinned ? "📌 已固定" : "📌 固定";
}

function renderEntries(entries) {
  const tb = $("entries").querySelector("tbody");
  const seen = new Set();
  entries.sort((a, b) => (a.name || a.id).localeCompare(b.name || b.id));
  for (const e of entries) {
    seen.add(e.id);
    let row = rows.get(e.id);
    if (!row) { row = makeRow(); rows.set(e.id, row); }
    updateRow(row, e);
    tb.appendChild(row.tr); // moves existing node, does not recreate it
  }
  for (const [id, row] of rows) {
    if (!seen.has(id)) { row.tr.remove(); rows.delete(id); }
  }
}

function renderBudgets(b) {
  const items = [
    ["VRAM 预留", fmtMB(b.vram_reserve_mb)],
    ["RAM 预留", fmtMB(b.ram_reserve_mb)],
    ["最大并发启动", b.max_starting],
    ["预算不足策略", b.on_no_fit],
    ["排队超时", (b.queue_timeout || 0) + "s"],
    ["默认 TTL", (b.ttl_default || 0) + "s"],
  ];
  $("budgets").innerHTML = items.map(([k, v]) =>
    `<div class="kv"><div class="k">${k}</div><div class="v">${v}</div></div>`).join("");
}

async function refresh() {
  const { ok, data } = await api("GET", "/api/status");
  const dot = $("dot");
  if (!ok) {
    dot.className = "dot dead";
    $("summary").textContent = "API 不可用";
    return;
  }
  dot.className = "dot live";
  const m = data.mem || {}, g = data.gpu || {};
  let s = `RAM 可用 ${fmtMB(m.available_mb)} / ${fmtMB(m.total_mb)}`;
  if (g.ok) s += ` · GPU 空闲 ${fmtMB(g.free_mb)} / ${fmtMB(g.total_mb)}`;
  $("summary").textContent = s;
  $("listen").textContent = "listen " + (data.listen || "");
  renderEntries(data.entries || []);
  renderBudgets(data.budgets || {});
}

async function actRow(id, action) {
  log(`${action} ${id} …`);
  const { ok, status, data } = await api("POST", `/api/entries/${encodeURIComponent(id)}/${action}?async=1`);
  if (!ok) log(`${action} ${id} 失败: ${(data.error && data.error.message) || data.raw || ("HTTP " + status)}`);
  else log(`${action} ${id} 已受理`);
  setTimeout(refresh, 300);
}

async function pinRow(id, pinned) {
  const { ok, data } = await api("POST", `/api/entries/${encodeURIComponent(id)}/pin`, { pinned });
  if (!ok) log(`pin ${id} 失败: ${(data.error && data.error.message) || ""}`);
  else log(`pin ${id} = ${pinned}`);
  setTimeout(refresh, 200);
}

let logSource = null;
function openLog(id) {
  if (logSource) { logSource.close(); logSource = null; }
  $("logtitle").textContent = "logs · " + id;
  $("logbody").textContent = "";
  $("logmodal").classList.remove("hidden");
  logSource = new EventSource(`/api/entries/${encodeURIComponent(id)}/logs/stream`);
  logSource.onmessage = (ev) => {
    const body = $("logbody");
    body.textContent += ev.data + "\n";
    body.scrollTop = body.scrollHeight;
  };
  logSource.onerror = () => { $("logbody").textContent += "[stream closed]\n"; };
}
function closeLog() {
  if (logSource) { logSource.close(); logSource = null; }
  $("logmodal").classList.add("hidden");
}

$("refresh").onclick = refresh;
$("logclose").onclick = closeLog;
$("logmodal").onclick = (e) => { if (e.target.id === "logmodal") closeLog(); };
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeLog(); });

refresh();
setInterval(refresh, 2000);
