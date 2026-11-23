"use strict";

// Admin dashboard: polls the admin API every 5 s and draws plain SVG.

const $ = (s) => document.querySelector(s);
let windowSel = "1h";
let logFilter = "error";
let timer = null;

function token() {
  try { return sessionStorage.getItem("gw.admin") || ""; } catch { return ""; }
}
function setToken(t) {
  try { t ? sessionStorage.setItem("gw.admin", t) : sessionStorage.removeItem("gw.admin"); } catch { /* storage blocked */ }
}

async function api(path) {
  const res = await fetch(path, { headers: { Authorization: "Bearer " + token() } });
  if (res.status === 401) {
    setToken("");
    askToken();
    throw new Error("Admin token required");
  }
  if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error?.message || res.statusText);
  return res.json();
}

function askToken() {
  const d = $("#login");
  if (!d.open) d.showModal();
}

const usd = (n) => (n === 0 ? "$0" : n < 0.01 ? "$" + n.toFixed(5) : n < 100 ? "$" + n.toFixed(2) : "$" + Math.round(n).toLocaleString());
const num = (n) => Math.round(n).toLocaleString();
const ms = (n) => (n >= 1000 ? (n / 1000).toFixed(2) + " s" : Math.round(n) + " ms");
const pct = (a, b) => (b ? Math.round((a / b) * 1000) / 10 + "%" : "–");
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

function kpis(t) {
  const tiles = [
    { l: "Requests", v: num(t.requests) },
    { l: "Error rate", v: pct(t.errors, t.requests), s: num(t.errors) + " failed", bad: t.errors > 0 && t.errors / t.requests > 0.05 },
    { l: "Served by a fallback", v: num(t.fallbacks), s: pct(t.fallbacks, t.requests) + " of requests" },
    { l: "Spend", v: usd(t.cost_usd), s: num(t.input_tokens) + " in / " + num(t.output_tokens) + " out tokens" },
    { l: "Embedding cache hits", v: pct(t.emb_cache_hits, t.emb_inputs), s: usd(t.saved_usd) + " not spent" },
    { l: "Latency p50 / p95", v: ms(t.p50_ms), s: "p95 " + ms(t.p95_ms) },
  ];
  $("#kpis").innerHTML = tiles.map((x) =>
    `<div class="kpi ${x.bad ? "bad" : ""}"><div class="v">${x.v}</div><div class="l">${x.l}</div>${x.s ? `<div class="s">${x.s}</div>` : ""}</div>`).join("");
}

function timeLabel(iso, bucket) {
  const d = new Date(iso);
  return bucket === "minute" ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit" });
}

function frame(W, H, P, max, fmt) {
  let g = "";
  for (let i = 0; i <= 4; i++) {
    const y = P.t + ((H - P.t - P.b) * i) / 4;
    g += `<line class="grid" x1="${P.l}" x2="${W - P.r}" y1="${y}" y2="${y}"/>`;
    g += `<text class="axis" x="${P.l - 6}" y="${y + 3}" text-anchor="end">${fmt(max * (1 - i / 4))}</text>`;
  }
  return g;
}

function xLabels(series, bucket, x, H, P) {
  const step = Math.max(1, Math.ceil(series.length / 6));
  return series.map((b, i) => (i % step === 0
    ? `<text class="axis" x="${x(i)}" y="${H - P.b + 14}" text-anchor="middle">${timeLabel(b.start, bucket)}</text>` : "")).join("");
}

function barChart(el, series, bucket) {
  if (!series.length) { el.innerHTML = `<div class="empty">No requests in this window.</div>`; return; }
  const W = 560, H = 180, P = { l: 40, r: 8, t: 8, b: 22 };
  const max = Math.max(1, ...series.map((b) => b.requests));
  const bw = (W - P.l - P.r) / series.length;
  const y = (v) => P.t + (H - P.t - P.b) * (1 - v / max);
  const x = (i) => P.l + bw * i + bw / 2;
  let bars = "";
  series.forEach((b, i) => {
    const okN = b.requests - b.errors;
    const w = Math.max(1, bw * 0.7), x0 = P.l + bw * i + (bw - w) / 2;
    bars += `<rect class="ok" x="${x0}" y="${y(okN)}" width="${w}" height="${y(0) - y(okN)}"><title>${timeLabel(b.start, bucket)}: ${okN} ok, ${b.errors} failed</title></rect>`;
    if (b.errors) bars += `<rect class="err" x="${x0}" y="${y(b.requests)}" width="${w}" height="${y(0) - y(b.errors)}"/>`;
  });
  el.innerHTML = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Requests over time">${frame(W, H, P, max, num)}${bars}${xLabels(series, bucket, x, H, P)}</svg>`;
}

function lineChart(el, series, bucket) {
  if (!series.length) { el.innerHTML = `<div class="empty">No requests in this window.</div>`; return; }
  const W = 560, H = 180, P = { l: 48, r: 8, t: 8, b: 22 };
  const max = Math.max(1, ...series.map((b) => b.p50_ms)) * 1.1;
  const bw = (W - P.l - P.r) / series.length;
  const x = (i) => P.l + bw * i + bw / 2;
  const y = (v) => P.t + (H - P.t - P.b) * (1 - v / max);
  const pts = series.map((b, i) => `${x(i)},${y(b.p50_ms)}`).join(" ");
  const dots = series.length < 40 ? series.map((b, i) => `<circle class="dot" cx="${x(i)}" cy="${y(b.p50_ms)}" r="2.5"><title>${ms(b.p50_ms)}</title></circle>`).join("") : "";
  el.innerHTML = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Median latency over time">${frame(W, H, P, max, ms)}<polyline class="line" points="${pts}"/>${dots}${xLabels(series, bucket, x, H, P)}</svg>`;
}

function targets(stats, breakers) {
  const state = Object.fromEntries(breakers.map((b) => [b.target, b.breaker]));
  const rows = stats.map((t) => ({ ...t, key: t.provider + "/" + t.model }));
  for (const b of breakers) if (!rows.find((r) => r.key === b.target)) {
    const [provider, ...m] = b.target.split("/");
    rows.push({ key: b.target, provider, model: m.join("/"), requests: 0, errors: 0, cost_usd: 0, p50_ms: 0, p95_ms: 0, p50_ttfb_ms: 0 });
  }
  $("#targets").innerHTML = `<thead><tr><th>Target</th><th>Circuit</th><th class="r">Requests</th><th class="r">Errors</th><th class="r">p50</th><th class="r">p95</th><th class="r">p50 first token</th><th class="r">Cost</th></tr></thead><tbody>` +
    rows.map((t) => {
      const b = state[t.key];
      const circuit = b ? `<span class="pill ${b.state}" title="${esc(b.last_error || "")}">${b.state.replace("_", "-")}</span>` : `<span class="muted small">cache</span>`;
      return `<tr><td class="mono">${esc(t.key)}</td><td>${circuit}</td><td class="r">${num(t.requests)}</td><td class="r">${pct(t.errors, t.requests)}</td>` +
        `<td class="r">${t.p50_ms ? ms(t.p50_ms) : "–"}</td><td class="r">${t.p95_ms ? ms(t.p95_ms) : "–"}</td><td class="r">${t.p50_ttfb_ms ? ms(t.p50_ttfb_ms) : "–"}</td><td class="r">${usd(t.cost_usd)}</td></tr>`;
    }).join("") + "</tbody>";
}

function projects(ps) {
  if (!ps.length) { $("#projects").innerHTML = `<div class="empty">No projects yet. Create one with <code>gateway project</code>.</div>`; return; }
  $("#projects").innerHTML = ps.map((p) => {
    const cap = p.monthly_budget_usd;
    const share = cap > 0 ? Math.min(100, (p.spent_this_month_usd / cap) * 100) : 0;
    return `<div class="proj"><div class="row-between"><strong>${esc(p.name)}</strong><span class="small muted">${usd(p.spent_this_month_usd)} ${cap > 0 ? "of " + usd(cap) : "· no cap"} · ${num(p.requests_this_month)} requests · ${p.active_keys} keys</span></div>` +
      (cap > 0 ? `<div class="bar"><div class="${share > 90 ? "hot" : ""}" style="width:${share}%"></div></div>` : "") + `</div>`;
  }).join("");
}

function routes(rs) {
  $("#routes").innerHTML = rs.map((r) => `<div class="route"><code>${esc(r.id)}</code> <span class="muted small">${r.kind}</span><ol>${r.targets.map((t) => `<li class="mono">${esc(t)}</li>`).join("")}</ol></div>`).join("");
}

async function loadLog() {
  const rows = await api(`/admin/api/requests?filter=${logFilter === "all" ? "" : logFilter}&limit=50`);
  if (!rows.length) { $("#log").innerHTML = `<tbody><tr><td class="muted">Nothing to show.</td></tr></tbody>`; return; }
  $("#log").innerHTML = `<thead><tr><th>Time</th><th>Route</th><th>Served by</th><th>Status</th><th class="r">Latency</th><th class="r">Cost</th><th>Detail</th></tr></thead><tbody>` +
    rows.map((r) => {
      const status = r.status === "error" ? `<span class="pill err">${r.http_status} ${esc(r.error_kind)}</span>`
        : r.fallback_from ? `<span class="pill fb">fallback</span>` : `<span class="small muted">ok</span>`;
      const detail = r.status === "error" ? esc(r.error)
        : r.fallback_from ? `from ${esc(r.fallback_from)}`
        : r.endpoint === "embeddings" ? `${r.emb_cache_hits}/${r.emb_inputs} from cache` : "";
      return `<tr><td>${new Date(r.ts).toLocaleTimeString()}</td><td class="mono">${esc(r.route)}</td><td class="mono">${esc(r.provider ? r.provider + "/" + r.model : "–")}</td>` +
        `<td>${status}</td><td class="r">${ms(r.latency_ms)}</td><td class="r">${usd(r.cost_usd)}</td><td class="wrap small">${detail}</td></tr>`;
    }).join("") + "</tbody>";
}

async function refresh() {
  if (!token()) { askToken(); return; }
  try {
    const o = await api(`/admin/api/overview?window=${windowSel}`);
    $("#error").hidden = true;
    kpis(o.stats.totals);
    document.querySelectorAll(".bucket").forEach((e) => (e.textContent = o.stats.bucket));
    barChart($("#reqChart"), o.stats.series, o.stats.bucket);
    lineChart($("#latChart"), o.stats.series, o.stats.bucket);
    targets(o.stats.targets, o.targets);
    projects(o.projects);
    routes(o.routes);
    await loadLog();
    $("#updated").textContent = "Updated " + new Date().toLocaleTimeString();
  } catch (e) {
    $("#error").textContent = e.message;
    $("#error").hidden = false;
  }
}

function segmented(sel, attr, set) {
  document.querySelectorAll(`${sel} button`).forEach((b) => b.addEventListener("click", () => {
    document.querySelectorAll(`${sel} button`).forEach((x) => x.classList.toggle("on", x === b));
    set(b.dataset[attr]);
    refresh();
  }));
}

segmented(".controls .seg", "w", (v) => (windowSel = v));
segmented("#logFilter", "f", (v) => (logFilter = v));
$("#loginForm").addEventListener("submit", () => { setToken($("#tokenInput").value.trim()); refresh(); });
$("#signout").addEventListener("click", () => { setToken(""); askToken(); });
refresh();
timer = setInterval(refresh, 5000);
