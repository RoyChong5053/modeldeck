"use strict";

const $ = (id) => document.getElementById(id);

// ---- access token ---------------------------------------------------------
// A deck with server.token set refuses /api calls without a token. The token
// can arrive three ways: the ?token= query parameter, the deck_session cookie
// the server hands out, or the Authorization header below. We keep it in
// localStorage so the dashboard keeps working on the next visit.
const TOKEN_KEY = "deck.token";

function getToken() {
  try { return localStorage.getItem(TOKEN_KEY) || ""; } catch (e) { return ""; }
}
function setToken(t) {
  try {
    if (t) localStorage.setItem(TOKEN_KEY, t);
    else localStorage.removeItem(TOKEN_KEY);
  } catch (e) { /* private mode */ }
}
// ?token=... on the page URL: remember it and clean the address bar
(function adoptTokenFromURL() {
  const t = new URLSearchParams(location.search).get("token");
  if (!t) return;
  setToken(t);
  try { history.replaceState({}, "", location.pathname); } catch (e) { /* ignore */ }
})();

async function api(method, path, body) {
  const opt = { method, headers: {} };
  const t = getToken();
  if (t) opt.headers["Authorization"] = "Bearer " + t;
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  let r;
  try {
    r = await fetch(path, opt);
  } catch (e) {
    return { ok: false, status: 0, data: { error: { message: "无法连接 deck: " + e.message } } };
  }
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
  if (!mb || mb < 0) return "—";
  if (mb >= 1024) return (mb / 1024).toFixed(1) + " GB";
  return mb + " MB";
}
// like fmtMB but 0 is a meaningful value (budgets, reserves)
function fmtMB0(mb) {
  if (mb === undefined || mb === null || mb < 0) return "—";
  if (mb >= 1024) return (mb / 1024).toFixed(1) + " GB";
  return mb + " MB";
}
function fmtPct(part, total) {
  if (!total) return "—";
  return Math.max(0, Math.min(100, Math.round((part / total) * 100))) + "%";
}
function fmtIdle(iso) {
  if (!iso || iso.startsWith("0001")) return "—";
  const s = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  return Math.floor(s / 3600) + "h";
}
function fmtUptime(s) {
  if (!s || s < 0) return "—";
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}
function short(s, n) {
  s = (s || "").trim();
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}
function log(msg) {
  const el = $("events");
  const line = new Date().toLocaleTimeString() + "  " + msg;
  el.textContent = (line + "\n" + el.textContent).split("\n").slice(0, 200).join("\n");
}
const tail = (p) => (p || "").split("/").pop();

// ---- keyed table rows (updated in place so clicks never hit stale nodes) ----
const rows = new Map();

function cell(label) { const d = document.createElement("td"); if (label) d.dataset.label = label; return d; }
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

  refs.name = cell(); refs.name.dataset.name = "1";
  refs.state = cell("状态");
  refs.port = cell("端口");
  refs.inflight = cell("在飞");
  refs.rss = cell("RSS");
  refs.vram = cell("VRAM");
  refs.idle = cell("空闲");

  const act = cell("操作");
  act.dataset.actions = "1";
  const box = document.createElement("div");
  box.className = "actions";
  refs.start = button("启动", "primary", () => actRow(refs.id, "start"));
  refs.stop = button("停止", "", () => actRow(refs.id, "stop"));
  refs.restart = button("重启", "", () => actRow(refs.id, "restart"));
  refs.lock = button("🔒 锁定", "", () => lockRow(refs.id, !refs.locked));
  refs.edit = button("编辑", "ghost", () => editRow(refs.id));
  refs.logs = button("日志", "ghost", () => openLog(refs.id));
  refs.rename = button("改名", "ghost", () => renameRow(refs.id, refs.title));
  refs.clone = button("克隆", "ghost", () => cloneRow(refs.id));
  refs.del = button("删除", "danger", () => deleteRow(refs.id));
  box.append(refs.start, refs.stop, refs.restart, refs.lock,
    refs.edit, refs.logs, refs.rename, refs.clone, refs.del);
  act.appendChild(box);

  tr.append(refs.name, refs.state, refs.port, refs.inflight, refs.rss, refs.vram, refs.idle, act);
  return { id: "", tr, refs, locked: false };
}

function updateRow(row, e) {
  const r = row.refs;
  row.id = e.id;
  r.id = e.id;
  r.title = e.name || e.id;
  row.locked = !!e.pinned;

  r.name.innerHTML = `<strong>${e.name || e.id}</strong>` +
    (e.pinned ? ' <span class="lockbadge" title="已锁定：不会被自动卸载，也不会被其它模型挤掉">🔒</span>' : "") +
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
  r.lock.textContent = e.pinned ? "🔓 已锁定" : "🔒 锁定";
  r.lock.title = e.pinned
    ? "已锁定：空闲也不会被自动卸载，也不会被其它模型挤掉（点一下解锁）"
    : "锁定：空闲也不会被自动卸载，也不会被其它模型挤掉";
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
  if (!entries.length) {
    let empty = tb.querySelector(".empty-row");
    if (!empty) {
      empty = document.createElement("tr");
      empty.className = "empty-row";
      const c = document.createElement("td");
      c.colSpan = 8;
      c.className = "muted";
      c.style.textAlign = "center";
      c.style.padding = "18px 12px";
      c.textContent = "没有条目";
      empty.appendChild(c);
      tb.appendChild(empty);
    }
  } else {
    const empty = tb.querySelector(".empty-row");
    if (empty) empty.remove();
  }
}

// ---- sticky top stack: what is happening right now ----
function renderOps(entries) {
  const el = $("ops");
  const ops = [];
  let online = 0, inflight = 0;
  for (const e of entries) {
    if (e.state === "ready") { online++; inflight += (e.inflight || 0); }
    if (e.state === "starting") {
      ops.push(`<span class="op starting"><span class="spin">◠</span><span class="who">${e.id}</span>启动中 ${fmtIdle(e.started_at)}</span>`);
    } else if (e.state === "stopping") {
      ops.push(`<span class="op stopping"><span class="spin">◠</span><span class="who">${e.id}</span>停止中</span>`);
    } else if (e.state === "failed") {
      ops.push(`<span class="op failed"><span class="who">${e.id}</span>启动失败` +
        `<button class="opx" data-reset="${e.id}" title="清除失败状态">✕</button></span>`);
    } else if (e.state === "ready" && e.inflight > 0) {
      ops.push(`<span class="op serving"><span class="who">${e.id}</span>${e.inflight} 个请求在飞</span>`);
    }
  }
  el.innerHTML = ops.join("");
  const parts = [`${online} 个模型在线`];
  if (inflight) parts.push(`${inflight} 个请求在飞`);
  if (ops.length) parts.push(`${ops.length} 项进行中`);
  else parts.push("空闲");
  $("opsinfo").textContent = parts.join(" · ");
}

// ---- dashboard: the hardware this deck runs on + the budgets in force ----
function kv(k, v, opts = {}) {
  const sub = opts.sub ? `<div class="sub">${opts.sub}</div>` : "";
  const bar = opts.pct !== undefined
    ? `<div class="bar ${opts.heat || ""}"><i style="width:${Math.max(0, Math.min(100, opts.pct))}%"></i></div>` : "";
  const cls = opts.wide ? "kv wide" : "kv";
  const vcls = opts.small ? "v small" : "v";
  return `<div class="${cls}"><div class="k">${k}</div><div class="${vcls}"${opts.title ? ` title="${opts.title}"` : ""}>${v}</div>${sub}${bar}</div>`;
}

function renderDashboard(host, budgets, entries) {
  const h = host || {};
  const g = h.gpu || {};
  const c = h.cpu || {};
  const m = h.mem || {};
  const b = budgets || {};

  const name = (h.hostname || "?");
  // a label that merely repeats the hostname adds nothing
  const hn = name.toLowerCase(), lb = (h.label || "").toLowerCase();
  const showLabel = h.label && lb !== hn && !hn.includes(lb) && !lb.includes(hn);
  $("hostlabel").textContent = showLabel ? `${name} · ${h.label}` : name;
  document.title = (showLabel ? `${name} · ${h.label}` : name) + " — ModelDeck";
  // a stable colour per host so five deck tabs are told apart at a glance
  let hue = 0;
  for (const ch of name) hue = (hue * 31 + ch.charCodeAt(0)) % 360;
  $("hostlabel").style.borderColor = `hsl(${hue} 55% 45%)`;
  $("hostlabel").style.color = `hsl(${hue} 70% 78%)`;

  const online = (entries || []).filter((e) => e.state === "ready").length;
  const cards = [];

  const gpuHeat = g.ok ? (g.used_mb > g.total_mb * 0.9 ? "hot" : (g.used_mb > g.total_mb * 0.75 ? "warm" : "")) : "";
  const memHeat = m.total_mb ? (m.available_mb / m.total_mb < 0.1 ? "hot" : "") : "";

  // The addresses other machines can open. With a wildcard bind this is every
  // local address — LAN plus Tailscale — so a phone can be handed a link that
  // works, and a DHCP change does not invalidate it.
  const allUrls = h.urls || [];
  const lanUrls = allUrls.filter((u) => !/127\.0\.0\.1|localhost|\[::1\]/.test(u));
  const urls = lanUrls.length ? lanUrls : allUrls;

  // phone strip: one row of chips that wraps instead of scrolling sideways
  const strip = [
    `<span class="s"><b>主机</b>${short(name, 16)}${showLabel ? " · " + short(h.label, 12) : ""}</span>`,
    g.ok ? `<span class="s ${gpuHeat}"><b>GPU</b>${fmtMB(g.used_mb)}/${fmtMB(g.total_mb)} · ${g.util_pct || 0}%</span>` : "",
    `<span class="s"><b>CPU</b>${(c.load1 || 0).toFixed(2)} · ${c.cores || "?"}核</span>`,
    `<span class="s ${memHeat}"><b>RAM</b>${fmtMB(m.used_mb)}/${fmtMB(m.total_mb)}</span>`,
    `<span class="s"><b>模型</b>${online} 在线</span>`,
    `<span class="s"><b>预留</b>${fmtMB(b.vram_reserve_mb)} VRAM</span>`,
    `<span class="s"><b>策略</b>${b.on_no_fit === "reject" ? "拒绝" : "排队"} · ${b.queue_timeout ?? "—"}s</span>`,
    urls.length ? `<span class="s"><b>地址</b>${short(urls[0].replace(/^https?:\/\//, ""), 30)}</span>` : "",
  ].filter(Boolean).join("");
  $("dashstrip").innerHTML = strip;

  cards.push(kv("主机", short(name, 18), {
    sub: `${h.version ? "v" + h.version : ""} · 运行 ${fmtUptime(h.uptime_s)}`,
    wide: false,
  }));

  if (g.ok) {
    const gpuName = short((g.name || "GPU").replace(/^NVIDIA\s+/i, ""), 22);
    cards.push(kv("GPU", gpuName, {
      sub: `${fmtMB(g.used_mb)} / ${fmtMB(g.total_mb)} · ${g.util_pct || 0}% · ${g.temp_c || 0}°C`,
      pct: g.total_mb ? (g.used_mb / g.total_mb) * 100 : 0,
      heat: gpuHeat,
      title: g.name || "",
    }));
  } else {
    cards.push(kv("GPU", "无", { sub: "未检测到 nvidia-smi" }));
  }

  cards.push(kv("CPU", short(c.model || "?", 20), {
    sub: `${c.cores || "?"} 核 · 负载 ${(c.load1 || 0).toFixed(2)}`,
    title: c.model || "",
  }));

  cards.push(kv("内存", `${fmtMB(m.used_mb)} / ${fmtMB(m.total_mb)}`, {
    sub: `可用 ${fmtMB(m.available_mb)}`,
    pct: m.total_mb ? (m.used_mb / m.total_mb) * 100 : 0,
    heat: m.total_mb ? (m.available_mb / m.total_mb < 0.1 ? "hot" : "") : "",
  }));

  cards.push(kv("运行中", `${online} 个模型`, {
    sub: `并发启动上限 ${b.max_starting ?? "—"} · 默认 TTL ${b.ttl_default ?? "—"}s`,
  }));

  cards.push(kv("显存预留", fmtMB0(b.vram_reserve_mb), { sub: `RAM 预留 ${fmtMB0(b.ram_reserve_mb)}` }));
  cards.push(kv("预算不足", b.on_no_fit === "reject" ? "立即拒绝" : "排队等待", {
    sub: `排队超时 ${b.queue_timeout ?? "—"}s`,
  }));

  // The addresses other machines can open, with the token so a tap on a
  // phone lands straight on a working dashboard.
  const linkHTML = urls.length
    ? urls.map((u) => {
        const t = getToken();
        const href = u + (t ? (u.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(t) : "");
        return `<a href="${href}" target="_blank" rel="noreferrer">${u.replace(/^https?:\/\//, "")}</a>`;
      }).join("")
    : `<span class="muted">只有回环地址，别处的设备打不开</span>`;
  cards.push(`<div class="kv wide"><div class="k">地址</div><div class="urls">${linkHTML}</div></div>`);

  $("dash").innerHTML = cards.join("");
  $("dashinfo").textContent = `${h.listen || ""}`;
}

// ---- entry actions ----
async function actRow(id, action) {
  log(`${action} ${id} …`);
  const { ok, status, data } = await api("POST", `/api/entries/${encodeURIComponent(id)}/${action}?async=1`);
  if (!ok) log(`${action} ${id} 失败: ${(data.error && data.error.message) || data.raw || ("HTTP " + status)}`);
  else log(`${action} ${id} 已受理`);
  setTimeout(refresh, 300);
}

// lockRow locks an entry in place: it is never unloaded by the idle reaper
// and never evicted to make room for another model.
async function lockRow(id, locked) {
  const { ok, data } = await api("POST", `/api/entries/${encodeURIComponent(id)}/lock`, { locked });
  if (!ok) log(`锁定 ${id} 失败: ${(data.error && data.error.message) || ""}`);
  else log(locked ? `已锁定 ${id}（不会被自动卸载）` : `已解锁 ${id}`);
  setTimeout(refresh, 200);
}

// resetRow clears a failed state so the error stops sticking to the entry.
async function resetRow(id) {
  const { ok, data } = await api("POST", `/api/entries/${encodeURIComponent(id)}/reset`);
  if (!ok) log(`清除 ${id} 失败状态失败: ${(data.error && data.error.message) || ""}`);
  else log(`已清除 ${id} 的失败状态`);
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

// ---- edit an entry: the start script and the knobs around it --------------
// Editing the script here and pressing 启动 on the same screen is the
// shortest path from "I wrote a new launcher" to "it runs".
let editID = null;

async function editRow(id) {
  const { ok, data } = await api("GET", `/api/entries/${encodeURIComponent(id)}/config`);
  if (!ok) { log(`读取配置失败: ${(data.error && data.error.message) || ""}`); return; }
  editID = id;
  $("edittitle").textContent = "编辑 · " + id;
  $("ed_script").value = data.script || "";
  $("ed_name").value = data.name || "";
  $("ed_port").value = data.port || "";
  $("ed_ready_path").value = (data.ready && data.ready.path) || "/health";
  $("ed_args").value = (data.args || []).join("\n");
  $("ed_env").value = (data.env || []).join("\n");
  $("ed_ttl").value = (data.ttl === undefined || data.ttl === null) ? "" : data.ttl;
  $("ed_msg").textContent = "";
  renderParsed(data.parsed);
  $("editmodal").classList.remove("hidden");
}

function renderParsed(p) {
  const el = $("ed_parsed");
  if (!p) { el.textContent = "（脚本还没写好，解析不了）"; return; }
  const flags = (p.flags && p.flags.length) ? p.flags.join(", ") : "—";
  const lines = [
    `binary     : ${p.binary || "?"}`,
    `model      : ${p.model || "—"}`,
    `listen     : ${p.host}:${p.port}  (ready: ${p.ready})`,
    `placement  : ${p.placement || "—"}`,
    `flags      : ${flags}`,
  ];
  for (const w of (p.warnings || [])) lines.push(`warning    : ${w}`);
  el.textContent = lines.join("\n");
}

function closeEdit() { $("editmodal").classList.add("hidden"); }

async function saveEdit(andRun) {
  if (!editID) return;
  const body = {};
  const script = $("ed_script").value.trim();
  if (script) body.script = script;
  const name = $("ed_name").value.trim();
  body.name = name;
  const port = parseInt($("ed_port").value, 10);
  if (port > 0) body.port = port;
  const readyPath = $("ed_ready_path").value.trim();
  if (readyPath) body.ready_path = readyPath;
  const ttlRaw = $("ed_ttl").value.trim();
  if (ttlRaw !== "") body.ttl = parseInt(ttlRaw, 10);
  body.args = $("ed_args").value.split("\n").map((s) => s.trim()).filter(Boolean);
  body.env = $("ed_env").value.split("\n").map((s) => s.trim()).filter(Boolean);

  const { ok, data } = await api("POST", `/api/entries/${encodeURIComponent(editID)}/config`, body);
  if (!ok) {
    $("ed_msg").textContent = "保存失败: " + ((data.error && data.error.message) || data.raw || ("HTTP " + data.status));
    return;
  }
  renderParsed(data.parsed);
  log(`已保存 ${editID}`);
  refresh();
  if (andRun) {
    const running = data.running;
    closeEdit();
    actRow(editID, running ? "restart" : "start");
  }
}

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
  tbl.className = "responsive";
  tbl.innerHTML = "<thead><tr><th>ID</th><th>端口</th><th>位置</th><th>二进制</th><th>脚本</th><th>操作</th></tr></thead>";
  const tb = document.createElement("tbody");
  for (const c of cands) {
    const tr = document.createElement("tr");
    const cells = [td(c.id), td(c.port || "—"), td(c.placement), td(tail(c.binary) || "?"), td(tail(c.script))];
    const labels = ["ID", "端口", "位置", "二进制", "脚本"];
    cells.forEach((c2, i) => { c2.dataset.label = labels[i]; });
    cells[0].dataset.name = "1";
    tr.append(...cells);
    const act = cell("操作");
    act.dataset.actions = "1";
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
  // EventSource cannot set headers, so the token rides in the query string
  // (the server accepts it anywhere and hands back a session cookie).
  let url = `/api/entries/${encodeURIComponent(id)}/logs/stream`;
  const t = getToken();
  if (t) url += "?token=" + encodeURIComponent(t);
  logSource = new EventSource(url);
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

// copyLink hands the phone a link that opens the dashboard already
// authenticated — the one address that is not the loopback.
function copyLink() {
  const urls = (lastHost.urls || []);
  const lan = urls.filter((u) => !/127\.0\.0\.1|localhost|\[::1\]/.test(u));
  const pick = (lan.length ? lan : urls)[0];
  if (!pick) { log("没有可用的地址"); return; }
  const t = getToken();
  const link = pick + (t ? (pick.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(t) : "");
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(link).then(
      () => log("已复制: " + link),
      () => log("复制失败，地址是: " + link)
    );
  } else {
    log("地址: " + link);
  }
}

// ---- shell ----
let lastHost = {};

function syncHeaderHeight() {
  const h = $("topbar").offsetHeight;
  if (h) document.documentElement.style.setProperty("--hdr", h + "px");
}

function applyFold(folded) {
  const stack = $("stack");
  stack.classList.toggle("folded", folded);
  for (const id of ["evpanel", "dashpanel"]) {
    const p = $(id);
    if (folded) { p.dataset.wasOpen = p.open ? "1" : ""; p.open = false; }
    else { p.open = p.dataset.wasOpen !== ""; }
  }
  $("fold").textContent = folded ? "▸" : "▾";
  try { localStorage.setItem("deck.folded", folded ? "1" : "0"); } catch (e) { /* ignore */ }
}

async function refresh() {
  const { ok, status, data } = await api("GET", "/api/status");
  const dot = $("dot");
  if (!ok) {
    dot.className = "dot dead";
    if (status === 401) {
      showAuthWall("需要访问令牌", "这台 deck 开启了令牌保护，/api 被拒绝。把令牌填进下面即可连接。");
      $("summary").textContent = "需要令牌";
    } else {
      showAuthWall("无法连接 deck", (data.error && data.error.message) || ("HTTP " + status));
      $("summary").textContent = "连接失败";
    }
    return;
  }
  hideAuthWall();
  dot.className = "dot live";
  const m = data.mem || {}, g = data.gpu || {};
  let s = `RAM ${fmtMB(m.available_mb)} 可用`;
  if (g.ok) s += ` · GPU ${fmtMB(g.free_mb)} 空闲`;
  $("summary").textContent = s;
  $("listen").textContent = "listen " + (data.listen || "");
  const entries = data.entries || [];
  lastHost = data.host || {};
  renderEntries(entries);
  renderOps(entries);
  renderDashboard(data.host, data.budgets, entries);
}

// ---- auth wall ------------------------------------------------------------
function showAuthWall(title, msg) {
  $("authtitle").textContent = title;
  $("authmsg").textContent = msg;
  $("authwall").classList.remove("hidden");
}
function hideAuthWall() {
  $("authwall").classList.add("hidden");
}
function applyToken() {
  const t = $("tokeninput").value.trim();
  if (!t) return;
  setToken(t);
  $("tokeninput").value = "";
  log("已设置令牌，重新连接…");
  refresh();
}
function promptToken() {
  $("tokeninput").value = getToken();
  $("authwall").classList.remove("hidden");
  $("tokeninput").focus();
}

$("refresh").onclick = refresh;
$("tokenbtn").onclick = promptToken;
$("tokenok").onclick = applyToken;
$("tokeninput").addEventListener("keydown", (e) => { if (e.key === "Enter") applyToken(); });
$("logclose").onclick = closeLog;
$("logclear").onclick = () => { $("logbody").textContent = ""; };
$("logmodal").onclick = (e) => { if (e.target.id === "logmodal") closeLog(); };
$("eventclear").onclick = (e) => { e.preventDefault(); e.stopPropagation(); $("events").textContent = ""; };
$("copylink").onclick = (e) => { e.preventDefault(); e.stopPropagation(); copyLink(); };
$("editclose").onclick = closeEdit;
$("editmodal").onclick = (e) => { if (e.target.id === "editmodal") closeEdit(); };
$("edsave").onclick = () => saveEdit(false);
$("edsave_run").onclick = () => saveEdit(true);
$("scanbtn").onclick = doScan;
$("fold").onclick = () => applyFold(!$("stack").classList.contains("folded"));
// the failed chip carries a ✕ that clears the stale error
$("ops").addEventListener("click", (ev) => {
  const b = ev.target.closest("[data-reset]");
  if (!b) return;
  resetRow(b.dataset.reset);
});
window.addEventListener("resize", syncHeaderHeight);
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") { closeLog(); closeEdit(); }
});

let folded = false;
try { folded = localStorage.getItem("deck.folded") === "1"; } catch (e) { /* ignore */ }
applyFold(folded);
syncHeaderHeight();
refresh();
setInterval(refresh, 2000);
