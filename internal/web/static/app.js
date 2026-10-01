"use strict";

// ---- state ---------------------------------------------------------------
const S = {
  info: {}, status: {}, ticks: [], decisions: [], actions: [], outcomes: [],
  notes: [], candidates: [],
};
let rangeSec = 300;
let follow = true;
let selectedId = null;
let logFilter = "all";
let charts = [];
let dirty = { charts: true, panels: true };

const $ = (id) => document.getElementById(id);
const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
const ts = (t) => new Date(t).getTime() / 1000;
const fmtT = (t) => new Date(t).toLocaleTimeString([], { hour12: false });
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const apId = (b) => b ? "ap_" + b.replace(/:/g, "").slice(-6) : "";
const f1 = (x) => (x == null || isNaN(x)) ? "–" : (+x).toFixed(1);
const f2 = (x) => (x == null || isNaN(x)) ? "–" : (+x).toFixed(2);
const pct = (x) => (x == null || isNaN(x)) ? "–" : Math.round(x * 100) + "%";

const ACTIONS = ["stay", "scan_targeted", "scan_full", "roam"];
const ACTION_COLOR = { stay: "--stay", scan_targeted: "--scan-t", scan_full: "--scan-f", roam: "--roam" };
const NOULS = [
  ["link_degraded", "link degraded?", "--n-deg"],
  ["better_ap_available", "better AP available?", "--n-better"],
  ["scan_data_stale", "scan data stale?", "--n-stale"],
];

function mosColor(m) {
  if (!m) return css("--muted");
  if (m >= 4.03) return css("--good");
  if (m >= 3.6) return css("--fair");
  if (m >= 3.1) return css("--poor");
  return css("--bad");
}
function mosWord(m) {
  if (!m) return "no data";
  if (m >= 4.34) return "excellent";
  if (m >= 4.03) return "good";
  if (m >= 3.6) return "fair";
  if (m >= 3.1) return "poor";
  return "bad";
}

// ---- data loading ----------------------------------------------------------
async function load() {
  const r = await fetch("api/snapshot");
  const snap = await r.json();
  Object.assign(S, {
    info: snap.info || {}, status: snap.status || {},
    ticks: snap.ticks || [], decisions: snap.decisions || [],
    actions: snap.actions || [], outcomes: snap.outcomes || [],
    notes: snap.notes || [], candidates: snap.candidates || [],
  });
  if (S.info.mode === "replay") { rangeSec = 0; setSeg("range", "0"); }
  buildCharts();
  dirty.charts = dirty.panels = true;
  if (S.info.mode !== "replay") stream();
}

function stream() {
  const es = new EventSource("api/events");
  es.onmessage = (m) => {
    const e = JSON.parse(m.data);
    switch (e.type) {
      case "tick": S.ticks.push(e.data); if (S.ticks.length > 7200) S.ticks.shift(); dirty.charts = true; break;
      case "decision": S.decisions.push(e.data); dirty.charts = dirty.panels = true; break;
      case "action": S.actions.push(e.data); dirty.charts = dirty.panels = true; break;
      case "outcome": S.outcomes.push(e.data); dirty.panels = true; break;
      case "note": S.notes.push(e.data); dirty.charts = dirty.panels = true; break;
      case "candidates": S.candidates = e.data; break;
      case "status": S.status = e.data; renderHeader(); break;
      case "info": S.info = e.data; renderHeader(); break;
    }
  };
  es.onerror = () => { $("busy").textContent = "dashboard disconnected – retrying"; };
}

// ---- header & tiles ------------------------------------------------------
function renderHeader() {
  const i = S.info, s = S.status;
  const mode = (i.mode || "").split("+")[0];
  const m = $("mode");
  m.textContent = i.mode || "…";
  m.className = "badge " + mode;
  $("busy").textContent = s.busy || "";
  $("conn").innerHTML = [
    ["SSID", s.ssid], ["AP", s.bssid ? `${apId(s.bssid)} · ${s.band} ch ${s.channel}` : (s.wpa_state || "–")],
    ["gateway", s.gateway],
  ].map(([k, v]) => `<span>${k} <b>${esc(v || "–")}</b></span>`).join("");
  const budget = i.budget_usd ? ` / $${i.budget_usd.toFixed(2)}` : "";
  $("jevstats").innerHTML = [
    ["model", i.model], ["calls", s.calls ?? 0],
    ["avg", s.avg_latency_ms ? Math.round(s.avg_latency_ms) + " ms" : "–"],
    ["errors", s.errors ?? 0],
    ["spent", "$" + (s.spent_usd || 0).toFixed(4) + budget],
  ].map(([k, v]) => `<span>${k} <b>${esc(v)}</b></span>`).join("") + (s.paused ? ' <span class="blocked">paused (budget)</span>' : "");
}

function renderTiles() {
  const t = S.ticks[S.ticks.length - 1];
  if (!t) { $("tiles").innerHTML = ""; return; }
  const tiles = [
    ["MOS", t.connected && t.mos ? f2(t.mos) : "–", mosWord(t.mos), "mos"],
    ["Probe loss", f1(t.loss_pct) + "%", "last 3 s"],
    ["Latency", f1(t.latency_ms) + " ms", "RTT to gateway"],
    ["Jitter", f1(t.jitter_ms) + " ms", "mean |Δ RTT|"],
    ["RSSI", t.connected ? t.rssi + " dBm" : "–", apId(t.bssid)],
    ["MCS tx / rx", `${t.tx_mcs} / ${t.rx_mcs}`, `${Math.round(t.tx_mbps)} / ${Math.round(t.rx_mbps)} Mb/s`],
    ["TCP retrans", t.tcp_out_segs >= 200 ? f1(t.tcp_retrans_pct) + "%" : "–", t.tcp_out_segs + " segs / 10 s"],
  ];
  $("tiles").innerHTML = tiles.map(([k, v, s, cls]) =>
    `<div class="tile ${cls || ""}" ${cls === "mos" ? `style="--mos-c:${mosColor(t.mos)}"` : ""}><div class="k">${k}</div><div class="v">${esc(v)}</div><div class="s">${esc(s)}</div></div>`).join("");
}

// ---- charts --------------------------------------------------------------
function markersHook(u) {
  const ctx = u.ctx, { left, top, width, height } = u.bbox;
  const [xmin, xmax] = [u.scales.x.min, u.scales.x.max];
  ctx.save();
  ctx.beginPath(); ctx.rect(left, top, width, height); ctx.clip();
  for (const a of S.actions) {
    const t0 = ts(a.t), t1 = t0 + (a.duration_ms || 0) / 1000;
    if (t1 < xmin || t0 > xmax) continue;
    const x0 = u.valToPos(t0, "x", true);
    if (a.kind.startsWith("scan")) {
      const x1 = Math.max(x0 + 2, u.valToPos(t1, "x", true));
      ctx.fillStyle = (a.kind === "scan_full" ? css("--scan-f") : css("--scan-t")) + "40";
      ctx.fillRect(x0, top, x1 - x0, height);
    } else if (a.kind === "roam") {
      ctx.strokeStyle = a.success ? css("--good") : css("--bad");
      ctx.lineWidth = 2 * devicePixelRatio;
      ctx.beginPath(); ctx.moveTo(x0, top); ctx.lineTo(x0, top + height); ctx.stroke();
    }
  }
  for (const n of S.notes) {
    if (n.kind !== "external" && n.kind !== "link") continue;
    const t0 = ts(n.t);
    if (t0 < xmin || t0 > xmax) continue;
    const x0 = u.valToPos(t0, "x", true);
    ctx.strokeStyle = css("--poor");
    ctx.setLineDash([4 * devicePixelRatio, 3 * devicePixelRatio]);
    ctx.lineWidth = 1.5 * devicePixelRatio;
    ctx.beginPath(); ctx.moveTo(x0, top); ctx.lineTo(x0, top + height); ctx.stroke();
    ctx.setLineDash([]);
  }
  if (selectedId != null) {
    const d = S.decisions.find((d) => d.id === selectedId);
    if (d) {
      const x0 = u.valToPos(ts(d.t), "x", true);
      ctx.strokeStyle = css("--accent");
      ctx.lineWidth = 1 * devicePixelRatio;
      ctx.setLineDash([2 * devicePixelRatio, 2 * devicePixelRatio]);
      ctx.beginPath(); ctx.moveTo(x0, top); ctx.lineTo(x0, top + height); ctx.stroke();
      ctx.setLineDash([]);
    }
  }
  ctx.restore();
}

function axis(stroke, extra) {
  return Object.assign({
    stroke: css("--muted"), grid: { stroke: css("--grid"), width: 1 },
    ticks: { stroke: css("--grid"), width: 1 }, font: "11px " + css("--sans"),
  }, extra || {});
}

function baseOpts(height, series, axes, scales) {
  return {
    width: chartWidth(), height,
    cursor: { sync: { key: "rj" }, drag: { x: false, y: false } },
    scales: Object.assign({ x: { time: true } }, scales),
    series: [{}].concat(series),
    axes: [axis(css("--muted"), { values: (u, vs) => vs.map((v) => new Date(v * 1000).toLocaleTimeString([], { hour12: false })) })].concat(axes),
    legend: { live: true },
    hooks: { draw: [markersHook], ready: [attachClick] },
  };
}

function chartWidth() { return Math.max(300, document.querySelector(".charts").clientWidth - 30); }

function attachClick(u) {
  u.over.addEventListener("click", () => {
    const t = u.posToVal(u.cursor.left, "x");
    if (t == null || !S.decisions.length) return;
    let best = null, bd = Infinity;
    for (const d of S.decisions) {
      const dd = Math.abs(ts(d.t) - t);
      if (dd < bd) { bd = dd; best = d; }
    }
    if (best) select(best.id);
  });
}

function buildCharts() {
  charts.forEach((c) => c.destroy());
  const line = (label, color, scale, extra) => Object.assign({ label, stroke: css(color), width: 1.5, scale, spanGaps: false, points: { show: false } }, extra || {});
  const cq = new uPlot(baseOpts(170, [
    line("MOS", "--mos", "mos", { width: 2, value: (u, v) => v == null ? "–" : v.toFixed(2) }),
    line("loss %", "--loss", "pct", { fill: css("--loss") + "22", value: (u, v) => v == null ? "–" : v.toFixed(0) + "%" }),
  ], [
    axis(null, { scale: "mos", size: 40 }),
    axis(null, { scale: "pct", side: 1, size: 40, grid: { show: false } }),
  ], { mos: { range: [1, 4.5] }, pct: { range: [0, 100] } }), [[], [], []], $("c-q"));
  const cs = new uPlot(baseOpts(150, [
    line("RSSI", "--rssi", "dbm", { width: 2, value: (u, v) => v == null ? "–" : v + " dBm" }),
    line("tx MCS", "--mcs-tx", "mcs", { paths: uPlot.paths.stepped({ align: 1 }) }),
    line("rx MCS", "--mcs-rx", "mcs", { paths: uPlot.paths.stepped({ align: 1 }), dash: [4, 3] }),
  ], [
    axis(null, { scale: "dbm", size: 40 }),
    axis(null, { scale: "mcs", side: 1, size: 40, grid: { show: false } }),
  ], { dbm: { range: (u, mn, mx) => [Math.min(-90, mn ?? -90), Math.max(-30, mx ?? -30)] }, mcs: { range: [0, 13] } }), [[], [], [], []], $("c-s"));
  const cl = new uPlot(baseOpts(130, [
    line("latency", "--lat", "ms", { value: (u, v) => v == null ? "–" : v.toFixed(1) + " ms" }),
    line("jitter", "--jit", "ms", { value: (u, v) => v == null ? "–" : v.toFixed(1) + " ms" }),
    line("TCP retrans %", "--retx", "pct", { dash: [3, 3], value: (u, v) => v == null ? "–" : v.toFixed(1) + "%" }),
  ], [
    axis(null, { scale: "ms", size: 40 }),
    axis(null, { scale: "pct", side: 1, size: 40, grid: { show: false } }),
  ], { ms: { range: (u, mn, mx) => [0, Math.max(10, (mx ?? 10) * 1.1)] }, pct: { range: (u, mn, mx) => [0, Math.max(5, (mx ?? 5) * 1.2)] } }), [[], [], [], []], $("c-l"));

  // Stacked action probabilities: series hold cumulative sums, bands fill
  // between neighbors, and the legend shows the un-stacked value.
  const stackSeries = ACTIONS.map((a, i) => ({
    label: a, stroke: css(ACTION_COLOR[a]), width: 0, scale: "p",
    paths: uPlot.paths.stepped({ align: 1 }), points: { show: false },
    value: (u, v, si, di) => di == null ? "–" : pct(mindRaw[i][di]),
  }));
  const noulSeries = NOULS.map(([k, label, color]) => ({
    label, stroke: css(color), width: 2, dash: [5, 3], scale: "p", points: { show: false },
    paths: uPlot.paths.stepped({ align: 1 }), value: (u, v) => v == null ? "–" : pct(v),
  }));
  const mopts = baseOpts(170, stackSeries.concat(noulSeries), [
    axis(null, { scale: "p", size: 40, values: (u, vs) => vs.map((v) => Math.round(v * 100) + "%") }),
  ], { p: { range: [0, 1] } });
  mopts.bands = [2, 3, 4].map((i) => ({ series: [i, i - 1], fill: css(ACTION_COLOR[ACTIONS[i - 1]]) + "aa" }));
  mopts.series[1].fill = css(ACTION_COLOR.stay) + "aa";
  const cm = new uPlot(mopts, [[], [], [], [], [], [], [], []], $("c-m"));
  charts = [cq, cs, cl, cm];
}

let mindRaw = [[], [], [], []];

function xRange() {
  const now = S.info.mode === "replay" && S.ticks.length ? ts(S.ticks[S.ticks.length - 1].t) : Date.now() / 1000;
  const first = S.ticks.length ? ts(S.ticks[0].t) : now - 60;
  if (!rangeSec) return [first, now];
  return [now - rangeSec, now];
}

function renderCharts() {
  if (!charts.length) return;
  const [xmin, xmax] = xRange();
  const tk = S.ticks.filter((t) => ts(t.t) >= xmin - 2);
  const x = tk.map((t) => ts(t.t));
  const nul = (t, v) => t.connected ? v : null;
  charts[0].setData([x, tk.map((t) => t.connected && t.mos ? t.mos : null), tk.map((t) => t.mos ? t.loss_pct : null)], false);
  charts[1].setData([x, tk.map((t) => nul(t, t.rssi)), tk.map((t) => nul(t, t.tx_mcs)), tk.map((t) => nul(t, t.rx_mcs))], false);
  charts[2].setData([x, tk.map((t) => t.mos ? t.latency_ms : null), tk.map((t) => t.mos ? t.jitter_ms : null), tk.map((t) => t.tcp_out_segs >= 200 ? t.tcp_retrans_pct : null)], false);

  const ds = S.decisions.filter((d) => ts(d.t) >= xmin - 10);
  const mx = ds.map((d) => ts(d.t));
  mindRaw = ACTIONS.map((a) => ds.map((d) => d.answers?.action?.probabilities?.[a] ?? null));
  const cum = [];
  let acc = ds.map(() => 0);
  for (let i = 0; i < ACTIONS.length; i++) {
    acc = acc.map((v, j) => mindRaw[i][j] == null ? null : (v ?? 0) + mindRaw[i][j]);
    cum.push(acc.slice());
  }
  const nouls = NOULS.map(([k]) => ds.map((d) => d.answers?.[k]?.noul ?? null));
  charts[3].setData([mx, ...cum, ...nouls], false);
  for (const c of charts) c.setScale("x", { min: xmin, max: xmax });
}

// ---- inspector -----------------------------------------------------------
function select(id) {
  selectedId = id;
  follow = false;
  $("follow").checked = false;
  dirty.panels = dirty.charts = true;
}

function bars(probs, order, colorFor, top) {
  return `<div class="bars">` + order.map((k) => {
    const p = probs?.[k] ?? 0;
    return `<span class="lab ${k === top ? "top" : ""}" title="${esc(k)}">${esc(k)}</span>
      <span class="bar"><i style="width:${(p * 100).toFixed(1)}%;background:${colorFor(k)}"></i></span>
      <span class="num">${pct(p)}</span>`;
  }).join("") + `</div>`;
}

function renderInspector() {
  const d = selectedId != null ? S.decisions.find((x) => x.id === selectedId) : S.decisions[S.decisions.length - 1];
  if (!d) return;
  $("insp-id").textContent = "#" + d.id;
  const body = $("insp-body");
  const meta = `<div class="insp-meta">
    <span>at <b>${fmtT(d.t)}</b></span><span>trigger <b>${esc(d.trigger)}</b></span>
    <span>latency <b>${d.latency_ms ? Math.round(d.latency_ms) + " ms" : "–"}</b></span>
    <span>tokens <b>${d.tokens || "–"}</b></span><span>cost <b>$${(d.cost_usd || 0).toFixed(6)}</b></span>
    <span>model <b>${esc(d.model || "–")}</b></span></div>`;
  if (d.err) {
    body.innerHTML = meta + `<div class="verdict"><span class="pill err">error</span> ${esc(d.err)}</div>
      <div>Executed: <b>${esc(d.executed)}</b></div>` + detailsBlock(d);
    return;
  }
  const A = d.answers || {};
  const tgtNames = {};
  try { for (const c of d.state?.candidate_aps || []) tgtNames[c.id] = `${c.id} · ${c.band} ch${c.channel}`; } catch (e) {}
  const tProbs = A.target?.probabilities || {};
  const tOrder = Object.keys(tProbs).sort((a, b) => tProbs[b] - tProbs[a]);
  const urg = A.urgency;
  const urgLabel = urg?.legend ? urg.legend[String(Math.round(urg.score))] : "";
  const changed = d.executed !== d.chosen;
  body.innerHTML = meta + `
    <div class="verdict">
      Jev chose <span class="pill ${esc(d.chosen)}">${esc(d.chosen)}</span>
      <span class="muted">confidence ${f2(d.confidence)}</span>
      ${d.target ? `→ <b class="mono">${apId(d.target)}</b>` : ""}
      ${changed ? `· executed <span class="pill ${esc(d.executed)}">${esc(d.executed)}</span>` : ""}
    </div>
    ${d.blocked ? `<div class="blocked">Rail: ${esc(d.blocked)}</div>` : ""}
    <div><div class="sub">Action</div>${bars(A.action?.probabilities, ACTIONS, (k) => css(ACTION_COLOR[k]), d.chosen)}</div>
    <div><div class="sub">Target AP <span class="muted small" style="text-transform:none">(confidence ${f2(A.target?.confidence)})</span></div>
      ${bars(Object.fromEntries(tOrder.map((k) => [tgtNames[k] || k, tProbs[k]])), tOrder.map((k) => tgtNames[k] || k), () => css("--roam"), tgtNames[A.target?.choice] || A.target?.choice)}</div>
    <div><div class="sub">Diagnostics</div><div class="meters">
      ${NOULS.map(([k, label, color]) => meter(label, A[k]?.noul, A[k]?.noul, css(color), pct(A[k]?.noul))).join("")}
      ${meter("urgency", urg?.score, urg ? urg.score / 3 : null, css("--roam"), urg ? f2(urg.score) + " / 3" : "–", urgLabel)}
    </div></div>` + detailsBlock(d);
}

function meter(label, v, frac, color, text, sub) {
  return `<div class="meter"><div class="k">${esc(label)}</div><div class="v">${esc(text)}</div>
    <div class="track"><i style="width:${((frac ?? 0) * 100).toFixed(0)}%;background:${color}"></i></div>
    ${sub ? `<div class="k" style="margin-top:4px">${esc(sub)}</div>` : ""}</div>`;
}

function detailsBlock(d) {
  const j = (o) => esc(JSON.stringify(o, null, 2));
  return `<details><summary>State sent to Jev (${d.tokens || "?"} tokens)</summary><pre>${j(d.state)}</pre></details>
    <details><summary>Questions</summary><pre>${j(d.questions)}</pre></details>
    <details><summary>Raw answers</summary><pre>${j(d.answers || d.err)}</pre></details>`;
}

// ---- candidates ----------------------------------------------------------
function renderCandidates() {
  const d = selectedId != null ? S.decisions.find((x) => x.id === selectedId) : S.decisions[S.decisions.length - 1];
  const tbl = $("cands");
  if (!d || !d.state) { tbl.innerHTML = ""; return; }
  const st = d.state, cur = st.connection?.ap || {}, sig = st.connection?.signal || {};
  const probs = d.answers?.target?.probabilities || {};
  const rows = [];
  rows.push(`<tr class="current"><td class="mono">${esc(cur.id)} <span class="muted">(current)</span></td><td>${esc(cur.band || "")} ${cur.channel ?? ""}</td><td>${esc(cur.width || "")}</td>
    <td class="num">${sig.rssi_dbm ?? "–"}</td><td class="num">–</td><td class="num">${cur.channel_utilization_pct ?? "–"}</td><td class="num">–</td><td class="num">–</td>
    <td class="hist">${esc([sig.trend_10s, st.connection?.link_quality_last_10s?.rating].filter(Boolean).join(" · "))}</td><td></td></tr>`);
  for (const c of st.candidate_aps || []) {
    const p = probs[c.id] ?? 0;
    const pick = d.answers?.target?.choice === c.id;
    rows.push(`<tr class="${pick ? "pick" : ""}"><td class="mono">${esc(c.id)}</td><td>${esc(c.band)} ${c.channel}</td><td>${esc(c.width)}</td>
      <td class="num">${c.rssi_dbm}</td><td class="num">${c.rssi_vs_current_db > 0 ? "+" : ""}${c.rssi_vs_current_db}</td>
      <td class="num">${c.channel_utilization_pct ?? "–"}</td><td class="num">${c.est_throughput_mbps}</td><td class="num">${c.measured_seconds_ago}s</td>
      <td class="hist">${esc(c.history || "")}</td>
      <td><span class="minibar"><i style="width:${(p * 100).toFixed(0)}%"></i></span><span class="mono">${pct(p)}</span></td></tr>`);
  }
  tbl.innerHTML = `<thead><tr><th>AP</th><th>band ch</th><th>width</th><th>RSSI</th><th>Δ dB</th><th>util %</th><th>est Mb/s</th><th>age</th><th>history</th><th>P(target)</th></tr></thead><tbody>${rows.join("")}</tbody>`;
  if (!(st.candidate_aps || []).length) tbl.innerHTML += `<tbody><tr><td colspan="10" class="muted">No scan data in this decision${st.scan?.status ? " – " + esc(st.scan.status) : ""}.</td></tr></tbody>`;
}

// ---- scorecard & outcomes ------------------------------------------------
function median(xs) { if (!xs.length) return null; const s = xs.slice().sort((a, b) => a - b); return s[Math.floor(s.length / 2)]; }
function p95(xs) { if (!xs.length) return null; const s = xs.slice().sort((a, b) => a - b); return s[Math.min(s.length - 1, Math.floor(s.length * 0.95))]; }

function renderScore() {
  const roams = S.actions.filter((a) => a.kind === "roam");
  const ok = roams.filter((a) => a.success);
  const scansT = S.actions.filter((a) => a.kind === "scan_targeted");
  const scansF = S.actions.filter((a) => a.kind === "scan_full");
  const scanSec = S.actions.filter((a) => a.kind.startsWith("scan")).reduce((s, a) => s + a.duration_ms / 1000, 0);
  const verdicts = { better: 0, "no change": 0, worse: 0, unmeasured: 0 };
  S.outcomes.forEach((o) => verdicts[o.verdict] = (verdicts[o.verdict] || 0) + 1);
  const deltas = S.outcomes.filter((o) => o.verdict !== "unmeasured").map((o) => o.mos_delta);
  const avgDelta = deltas.length ? deltas.reduce((a, b) => a + b, 0) / deltas.length : null;
  const conn = S.ticks.filter((t) => t.connected && t.mos);
  const runSec = S.ticks.length ? (ts(S.ticks[S.ticks.length - 1].t) - ts(S.ticks[0].t)) : 0;
  const avgMos = conn.length ? conn.reduce((s, t) => s + t.mos, 0) / conn.length : null;
  const good = conn.length ? conn.filter((t) => t.mos >= 4.03).length / conn.length : null;
  const disc = S.ticks.length ? S.ticks.filter((t) => !t.connected).length / S.ticks.length : null;
  const lats = S.decisions.filter((d) => d.latency_ms).map((d) => d.latency_ms);
  const blocked = S.decisions.filter((d) => d.blocked).length;
  const ext = S.notes.filter((n) => n.kind === "external").length;
  const errs = S.decisions.filter((d) => d.err).length;
  const toks = S.decisions.filter((d) => d.tokens).map((d) => d.tokens);
  const cards = [
    ["Average MOS", f2(avgMos), `${pct(good)} of time ≥ 4.03 (good)`],
    ["Disconnected", pct(disc), `${ext} external roam(s)`],
    ["Roams", `${ok.length} / ${roams.length}`, `${roams.length - ok.length} failed · median ${median(ok.map((a) => a.duration_ms))?.toFixed(0) ?? "–"} ms`],
    ["Roam outcomes", `${verdicts.better}↑ ${verdicts["no change"]}· ${verdicts.worse}↓`, `avg ΔMOS ${avgDelta == null ? "–" : (avgDelta > 0 ? "+" : "") + avgDelta.toFixed(2)}`],
    ["Scans", `${scansT.length} tgt · ${scansF.length} full`, `${scanSec.toFixed(1)} s scanning (${runSec ? (100 * scanSec / runSec).toFixed(1) : "–"}% of run)`],
    ["Jev calls", String(S.decisions.length), `${errs} error(s) · ${blocked} blocked by rails`],
    ["Jev latency", lats.length ? Math.round(median(lats)) + " ms" : "–", `p95 ${lats.length ? Math.round(p95(lats)) + " ms" : "–"}`],
    ["Cost", "$" + S.decisions.reduce((s, d) => s + (d.cost_usd || 0), 0).toFixed(4), `${toks.length ? Math.round(toks.reduce((a, b) => a + b, 0) / toks.length) : "–"} tokens/call`],
  ];
  $("score").innerHTML = cards.map(([k, v, s]) => `<div class="card"><div class="k">${k}</div><div class="v">${esc(v)}</div><div class="s">${esc(s)}</div></div>`).join("");

  const rows = S.outcomes.slice().reverse().map((o) => {
    const cls = o.verdict === "better" ? "v-better" : o.verdict === "worse" ? "v-worse" : "v-no";
    return `<tr data-id="${o.decision_id}"><td>${fmtT(o.t)}</td><td class="mono">${apId(o.from)} → ${apId(o.to)}</td>
      <td class="num">${f2(o.pre.mos)} → ${f2(o.post.mos)}</td><td class="num ${cls}">${o.mos_delta > 0 ? "+" : ""}${f2(o.mos_delta)}</td>
      <td class="num">${f1(o.pre.loss_pct)} → ${f1(o.post.loss_pct)}%</td><td class="num">${o.rssi_before} → ${o.rssi_after}</td>
      <td class="num">${o.disruption.lost}/${o.disruption.sent}</td><td class="${cls}">${esc(o.verdict)}</td></tr>`;
  }).join("");
  const failed = S.actions.filter((a) => a.kind === "roam" && !a.success).reverse().map((a) =>
    `<tr data-id="${a.decision_id}"><td>${fmtT(a.t)}</td><td class="mono">${apId(a.from)} → ${apId(a.target)}</td><td colspan="5" class="muted">${esc(a.message)}</td><td class="v-worse">failed</td></tr>`).join("");
  $("outcomes").innerHTML = `<thead><tr><th>time</th><th>roam</th><th>MOS</th><th>Δ</th><th>loss</th><th>RSSI</th><th>lost probes</th><th>verdict</th></tr></thead><tbody>${rows}${failed}</tbody>` +
    (rows || failed ? "" : `<tbody><tr><td colspan="8" class="muted">No roams yet.</td></tr></tbody>`);
}

// ---- decision log --------------------------------------------------------
function renderLog() {
  let items = S.decisions.map((d) => ({ t: d.t, d }));
  if (logFilter === "act") items = items.filter((i) => i.d.executed !== "stay" || i.d.chosen !== "stay");
  if (logFilter === "blocked") items = items.filter((i) => i.d.blocked);
  if (logFilter === "err") items = items.filter((i) => i.d.err);
  if (logFilter === "notes") items = [];
  if (logFilter === "all" || logFilter === "notes") items = items.concat(S.notes.map((n) => ({ t: n.t, n })));
  items.sort((a, b) => ts(b.t) - ts(a.t));
  items = items.slice(0, 300);
  const actByDec = {};
  for (const a of S.actions) actByDec[a.decision_id] = a;
  $("log").innerHTML = `<thead><tr><th>time</th><th>#</th><th>trigger</th><th>Jev chose</th><th>conf</th><th>target</th><th>executed</th><th>result</th><th>ms</th></tr></thead><tbody>` +
    items.map((i) => {
      if (i.n) return `<tr class="note"><td>${fmtT(i.t)}</td><td></td><td>${esc(i.n.kind)}</td><td colspan="6">${esc(i.n.text)}</td></tr>`;
      const d = i.d, a = actByDec[d.id];
      let res = "";
      if (d.err) res = `<span class="v-worse">${esc(d.err)}</span>`;
      else if (d.blocked) res = `<span class="blocked">${esc(d.blocked)}</span>`;
      else if (a) res = a.kind === "roam" ? (a.success ? `<span class="v-better">ok ${Math.round(a.duration_ms)} ms</span>` : `<span class="v-worse">${esc(a.message)}</span>`) : `${a.found ?? 0} APs, ${(a.duration_ms / 1000).toFixed(1)} s`;
      return `<tr data-id="${d.id}" class="${d.id === selectedId ? "sel" : ""}"><td>${fmtT(d.t)}</td><td class="mono">${d.id}</td><td>${esc(d.trigger)}</td>
        <td>${d.chosen ? `<span class="pill ${esc(d.chosen)}">${esc(d.chosen)}</span>` : "–"}</td><td class="num">${f2(d.confidence)}</td>
        <td class="mono">${apId(d.target)}</td><td>${esc(d.executed)}</td><td>${res}</td><td class="num">${d.latency_ms ? Math.round(d.latency_ms) : "–"}</td></tr>`;
    }).join("") + `</tbody>`;
}

// ---- wiring --------------------------------------------------------------
function setSeg(id, val) {
  for (const b of $(id).querySelectorAll("button")) b.classList.toggle("on", (b.dataset.r ?? b.dataset.f) === val);
}

$("range").addEventListener("click", (e) => {
  const b = e.target.closest("button"); if (!b) return;
  rangeSec = +b.dataset.r; setSeg("range", b.dataset.r); dirty.charts = true;
});
$("logfilter").addEventListener("click", (e) => {
  const b = e.target.closest("button"); if (!b) return;
  logFilter = b.dataset.f; setSeg("logfilter", b.dataset.f); renderLog();
});
$("follow").addEventListener("change", (e) => {
  follow = e.target.checked;
  if (follow) selectedId = null;
  dirty.panels = dirty.charts = true;
});
for (const id of ["log", "outcomes"]) {
  $(id).addEventListener("click", (e) => {
    const tr = e.target.closest("tr[data-id]"); if (!tr) return;
    select(+tr.dataset.id);
  });
}
window.addEventListener("resize", () => {
  const w = chartWidth();
  charts.forEach((c) => c.setSize({ width: w, height: c.height }));
});

// Charts redraw at most twice a second; panels only when something changed.
setInterval(() => {
  renderTiles();
  if (dirty.charts) { renderCharts(); dirty.charts = false; }
  if (dirty.panels) {
    if (follow) selectedId = null;
    renderInspector(); renderCandidates(); renderScore(); renderLog();
    $("foot").textContent = S.info.journal ? `journal: ${S.info.journal} · started ${new Date(S.info.started).toLocaleString()}` : "";
    dirty.panels = false;
  }
}, 500);
// Live mode: keep the x axis sliding even between ticks.
setInterval(() => { if (S.info.mode !== "replay") dirty.charts = true; }, 1000);

load().then(renderHeader);
