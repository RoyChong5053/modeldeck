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
const tail = (p) => (p || "").split("/").pop();

// ---- keyed table rows (updated in place so clicks never hit stale nodes) ----
const rows = new Map();

function cell() { return document.createElement("td"); }
function td(v) { const d = document.createElement("td"); d.textContent = v; return d; }
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
  refs.preset = button("预设", "ghost", () => presetRow(refs.id));
  refs.logs = button("日志", "ghost", () => openLog(refs.id));
  refs.rename = button("改名", "ghost", () => renameRow(refs.id, refs.title));
  refs.clone = button("克隆", "ghost", () => cloneRow(refs.id));
  refs.del = button("删除", "danger", () => deleteRow(refs.id));
  box.append(refs.start, refs.stop, refs.restart, refs.pin,
    refs.preset, refs.logs, refs.rename, refs.clone, refs.del);
  act.appendChild(box);

  tr.append(refs.name, refs.state, refs.port, refs.inflight, refs.rss, refs.vram, refs.idle, act);
  return { id: "", tr, refs, pinned: false };
}

function updateRow(row, e) {
  const r = row.refs;
  row.id = e.id;
  r.id = e.id;
  r.title = e.name || e.id;
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

// ---- entry actions ----
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

async function renameRow(id, curName) {
  const name = prompt("新的显示名", curName || id);
  if (name === null) return;
  const newID = prompt("新的 id（留空保持不变，旧 id 会保留为别名）", "");
  if (newID === null) return;
  const body = {};
  if (name !== "") body.name = name;
  if (newID && newID !== id) body.id = newID;
  if (!Object.keys(body).length) return;
  const { ok, data } = await api("POST", `/api/entries/${encodeURIComponent(id)}/rename`, body);
  if (!ok) { log(`改名失败: ${(data.error && data.error.message) || ""}`); return; }
  log(`已改名 ${id} → ${data.id}`);
  rows.clear();
  $("entries").querySelector("tbody").innerHTML = "";
  refresh();
}

async function cloneRow(id) {
  const newID = prompt("克隆出的新 id", id + "-copy");
  if (!newID) return;
  const name = prompt("新显示名（可留空）", "");
  if (name === null) return;
  const body = { clone_of: id, id: newID };
  if (name) body.name = name;
  const { ok, data } = await api("POST", "/api/entries", body);
  if (!ok) { log(`克隆失败: ${(data.error && data.error.message) || ""}`); return; }
  log(`已克隆 ${id} → ${data.id}`);
  refresh();
}

async function deleteRow(id) {
  if (!confirm(`删除 ${id}？\n配置会移入 state/trash 备份，可手动恢复。`)) return;
  const { ok, data } = await api("DELETE", `/api/entries/${encodeURIComponent(id)}`);
  if (!ok) { log(`删除失败: ${(data.error && data.error.message) || ""}`); return; }
  log(`已删除 ${id}（已入回收站）`);
  rows.delete(id);
  refresh();
}

async function presetRow(id) {
  const { ok, data } = await api("GET", `/api/entries/${encodeURIComponent(id)}/preset`);
  if (!ok) { log(`读取预设失败: ${(data.error && data.error.message) || ""}`); return; }
  const p = data.preset || {};
  const flags = (p.flags && p.flags.length) ? p.flags.join(", ") : "—";
  const lines = [
    `entry      : ${data.id}`,
    `script     : ${data.script}`,
    `binary     : ${p.binary || "?"}`,
    `model      : ${p.model || "—"}`,
    `listen     : ${p.host}:${p.port}  (ready: ${p.ready})`,
    `placement  : ${p.placement}`,
    `flags      : ${flags}`,
  ];
  for (const w of (p.warnings || [])) lines.push(`warning    : ${w}`);
  $("presettitle").textContent = "预设 · " + data.id;
  $("presetbody").textContent = lines.join("\n");
  $("presetmodal").classList.remove("hidden");
}
function closePreset() { $("presetmodal").classList.add("hidden"); }

// ---- discovery ----
async function doScan() {
  $("scaninfo").textContent = "扫描中…";
  const { ok, data } = await api("GET", "/api/scan");
  if (!ok) { $("scaninfo").textContent = "扫描失败"; return; }
  const cands = data.candidates || [];
  const found = cands.filter((c) => c.port).length;
  $("scaninfo").textContent = `${cands.length} 个脚本 · ${found} 个含端口 · ${(data.dirs || []).join(", ")}`;
  renderScan(cands);
}

function renderScan(cands) {
  const el = $("scanresult");
  el.innerHTML = "";
  if (!cands.length) { el.innerHTML = '<div class="muted" style="padding:10px 14px">未发现脚本</div>'; return; }
  const tbl = document.createElement("table");
  tbl.innerHTML = "<thead><tr><th>ID</th><th>端口</th><th>位置</th><th>二进制</th><th>脚本</th><th>操作</th></tr></thead>";
  const tb = document.createElement("tbody");
  for (const c of cands) {
    const tr = document.createElement("tr");
    tr.append(
      td(c.id),
      td(c.port || "—"),
      td(c.placement),
      td(tail(c.binary) || "?"),
      td(tail(c.script)),
    );
    const act = cell();
    const b = button(c.configured ? "已导入" : "导入", c.configured ? "" : "primary", () => importCandidate(c));
    b.disabled = !!c.configured || !c.port;
    act.appendChild(b);
    if (c.warnings && c.warnings.length) act.title = c.warnings.join("; ");
    tr.appendChild(act);
    tb.appendChild(tr);
  }
  tbl.appendChild(tb);
  el.appendChild(tbl);
}

async function importCandidate(c) {
  const body = {
    id: c.id, name: c.name, script: c.script, port: c.port,
    host: c.host, placement: c.placement, tags: ["scanned"],
  };
  const { ok, data } = await api("POST", "/api/entries", body);
  if (!ok) { log(`导入 ${c.id} 失败: ${(data.error && data.error.message) || ""}`); return; }
  log(`已导入 ${data.id}`);
  doScan();
  refresh();
}

// ---- log stream ----
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
$("presetclose").onclick = closePreset;
$("presetmodal").onclick = (e) => { if (e.target.id === "presetmodal") closePreset(); };
$("scanbtn").onclick = doScan;
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") { closeLog(); closePreset(); }
});

refresh();
setInterval(refresh, 2000);
