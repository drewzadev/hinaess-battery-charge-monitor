"use strict";

// app.js — Slice 3 frontend (Step 8, FR-9).
//
// A dark single-page dashboard, vanilla JS + uPlot 1.6.31 (no build step):
//   - a live header row polling /api/latest every 5 s,
//   - three uPlot charts (16 cell-voltage lines + a 3.65 V threshold line, a
//     client-side cell-delta line, and a dual-axis pack V/A chart) loaded from
//     /api/range and rebuilt when the range buttons change,
//   - live append: each header tick pushes the latest sample onto every chart
//     without re-fetching the whole range.
//
// The header row + three charts are the "four chart areas" of Goal 2.

const HEADER_INTERVAL_MS = 5000; // poll /api/latest every 5 s (FR-9)

// Range buttons map a label to a window length in ms; "all" is handled specially.
const RANGES = {
  "1h": 60 * 60 * 1000,
  "6h": 6 * 60 * 60 * 1000,
  "12h": 12 * 60 * 60 * 1000,
  "24h": 24 * 60 * 60 * 1000,
  "7d": 7 * 24 * 60 * 60 * 1000,
  "all": null,
};

let currentRange = "24h";

// customBounds holds the {from, to} epoch-ms window for the "custom" range,
// set by the Apply handler (FR-2). Undefined until the user applies a window.
let customBounds = null;

// Cell-chart thresholds (mV). 3650 is the overcharge danger line (AC-6); the
// other two are optional reference lines (FR-9). All drawn dashed via a hooks
// plugin so they stay out of the legend.
const THRESHOLDS = [
  { value: 3650, color: "#ff5d5d", label: "3.65 V danger" },
  { value: 3450, color: "#5dd28f", label: "3.45 V target" },
  { value: 2500, color: "#6b7280", label: "2.50 V floor" },
];

// Axis tick-value + tick-mark color. uPlot draws axes on the canvas and
// defaults the stroke to #000 (black), which is unreadable on the dark panel;
// CSS .u-axis color does not affect canvas-drawn text. Match the page --fg.
const AXIS_STROKE = "#e6e9ef";

// 16 distinct cell colors (AC-2).
const CELL_COLORS = [
  "#4ea1ff", "#ff7847", "#5dd28f", "#e85cc4",
  "#f5c542", "#7d6bff", "#2dd4bf", "#ff5d5d",
  "#9ad04e", "#ff9ad0", "#4ed4e8", "#c08457",
  "#a78bfa", "#84cc16", "#f97316", "#38bdf8",
];

const fmt = {
  v: (mv) => (mv / 1000).toFixed(2) + " V",
  a: (ma) => (ma / 1000).toFixed(2) + " A",
  pct: (p) => p.toFixed(1) + " %",
  mv: (mv) => mv + " mV",
};

function el(id) {
  return document.getElementById(id);
}

// charts holds the live uPlot instance and its mutable data arrays for each
// chart, so live append can push a point and call setData without re-fetching.
// data is uPlot's [xs, ...series] layout; xs are unix seconds.
const charts = {
  cells: { u: null, data: null },
  delta: { u: null, data: null },
  pack: { u: null, data: null },
};

let lastAppendedTS = 0; // epoch ms of the most-recent point pushed to the charts

// --- live header row -------------------------------------------------------

async function refreshHeader() {
  try {
    const res = await fetch("/api/latest");
    if (!res.ok) throw new Error("HTTP " + res.status);
    const s = await res.json();
    updateHeader(s);
    appendLive(s); // live append on each tick (FR-9)
  } catch (err) {
    el("updated").textContent = "fetch error: " + err.message;
  }
}

function updateHeader(s) {
  const cells = s.cells_mv || [];
  if (cells.length === 0) {
    el("updated").textContent = "no samples recorded yet";
    return;
  }
  const min = Math.min(...cells);
  const max = Math.max(...cells);
  el("m-pack-v").textContent = fmt.v(s.pack_mv);
  el("m-pack-a").textContent = fmt.a(s.pack_ma);
  el("m-soc").textContent = fmt.pct(s.soc_pct);
  el("m-delta").textContent = fmt.mv(max - min); // delta computed client-side (FR-9)
  el("m-min").textContent = fmt.mv(min);
  el("m-max").textContent = fmt.mv(max);
  el("updated").textContent = "updated " + new Date(s.ts).toLocaleTimeString();
}

// appendLive pushes the latest sample onto every chart's data and redraws,
// avoiding a full /api/range reload (FR-9). Points older than or equal to the
// last appended one (or before charts exist) are ignored.
function appendLive(s) {
  const cells = s.cells_mv || [];
  if (currentRange === "custom") return; // custom window is static (Risks/FR-2)
  if (cells.length === 0 || !s.ts) return;
  if (!charts.cells.u) return; // charts not built yet
  if (s.ts <= lastAppendedTS) return; // dup or stale tick

  const x = s.ts / 1000; // uPlot time scale is unix seconds

  const cd = charts.cells.data;
  cd[0].push(x);
  for (let i = 0; i < 16; i++) {
    cd[i + 1].push(i < cells.length ? cells[i] : null);
  }
  charts.cells.u.setData(cd);

  const min = Math.min(...cells);
  const max = Math.max(...cells);
  const dd = charts.delta.data;
  dd[0].push(x);
  dd[1].push(max - min);
  charts.delta.u.setData(dd);

  const pd = charts.pack.data;
  pd[0].push(x);
  pd[1].push(s.pack_mv);
  pd[2].push(s.pack_ma);
  charts.pack.u.setData(pd);

  lastAppendedTS = s.ts;
}

// --- range selection -------------------------------------------------------

function rangeBounds(label) {
  const now = Date.now();
  if (label === "custom") return customBounds; // {from, to} epoch ms, set by Apply (FR-2)
  if (label === "all") {
    return { from: 0, to: now + 60 * 1000 }; // all → from=0, to=now+1min (FR-9)
  }
  return { from: now - RANGES[label], to: now };
}

// toLocalInputValue formats an epoch-ms instant as a datetime-local value
// (YYYY-MM-DDTHH:mm) in the browser's local timezone (FR-2 prefill).
function toLocalInputValue(ms) {
  const d = new Date(ms);
  const pad = (n) => String(n).padStart(2, "0");
  return (
    d.getFullYear() +
    "-" + pad(d.getMonth() + 1) +
    "-" + pad(d.getDate()) +
    "T" + pad(d.getHours()) +
    ":" + pad(d.getMinutes())
  );
}

function setRange(label) {
  const prevRange = currentRange;
  currentRange = label;
  for (const btn of document.querySelectorAll("#ranges button")) {
    btn.classList.toggle("active", btn.dataset.range === label);
  }
  const panel = el("custom-range");
  if (label === "custom") {
    // Reveal the picker and prefill from the window the user was just viewing.
    // Don't reload — the window is undefined until Apply is clicked (FR-2).
    const prev = rangeBounds(prevRange === "custom" ? "24h" : prevRange);
    if (prev) {
      el("custom-from").value = toLocalInputValue(prev.from);
      el("custom-to").value = toLocalInputValue(prev.to);
    }
    el("custom-error").textContent = "";
    panel.hidden = false;
    return;
  }
  panel.hidden = true;
  loadCharts(); // full /api/range reload on range change (FR-9)
}

// applyCustom reads the picker inputs, validates, and on success stores the
// window in customBounds and reloads. Picker values are interpreted in the
// browser's local timezone via new Date(value).getTime(), matching what the
// user typed and the rest of the local-time-facing UI (FR-2, Q1=local).
function applyCustom() {
  const errEl = el("custom-error");
  const fromStr = el("custom-from").value;
  const toStr = el("custom-to").value;
  if (!fromStr || !toStr) {
    errEl.textContent = "Both From and To are required.";
    return;
  }
  const from = new Date(fromStr).getTime();
  const to = new Date(toStr).getTime();
  if (from >= to) {
    errEl.textContent = "From must be before To.";
    return;
  }
  errEl.textContent = "";
  customBounds = { from, to };
  loadCharts();
}

// --- charts ----------------------------------------------------------------

// chartWidth returns the usable pixel width of a chart container.
function chartWidth(id) {
  const w = el(id).clientWidth;
  return Math.max(320, w);
}

// thresholdPlugin draws horizontal dashed reference lines on the cell chart via
// the draw hook, keyed off the "y" scale (FR-9). Lines outside the current plot
// area are skipped.
function thresholdPlugin(lines) {
  return {
    hooks: {
      draw: (u) => {
        const ctx = u.ctx;
        const { left, top, width, height } = u.bbox;
        ctx.save();
        ctx.lineWidth = 1;
        ctx.setLineDash([5, 4]);
        for (const ln of lines) {
          const y = Math.round(u.valToPos(ln.value, "y", true));
          if (y < top || y > top + height) continue;
          ctx.strokeStyle = ln.color;
          ctx.beginPath();
          ctx.moveTo(left, y);
          ctx.lineTo(left + width, y);
          ctx.stroke();
        }
        ctx.restore();
      },
    },
  };
}

// cellYRange keeps the cell chart's initial band at 3200–3650 mV (FR-9) while
// expanding to fit data on zoom; a little headroom above 3650 keeps the danger
// line visible (AC-6).
function cellYRange(u, dataMin, dataMax) {
  const lo = Math.min(3200, dataMin == null ? 3200 : dataMin);
  const hi = Math.max(3680, dataMax == null ? 3680 : dataMax);
  return [lo, hi];
}

function buildCellsChart(data) {
  const series = [{}];
  for (let i = 0; i < 16; i++) {
    series.push({
      label: "C" + (i + 1),
      stroke: CELL_COLORS[i],
      width: 1,
      scale: "y",
      value: (u, v) => (v == null ? "--" : v + " mV"),
      points: { show: false },
    });
  }
  const opts = {
    width: chartWidth("chart-cells"),
    height: 320,
    scales: { x: { time: true }, y: { range: cellYRange } },
    axes: [
      { stroke: AXIS_STROKE },
      { stroke: AXIS_STROKE, scale: "y", values: (u, vals) => vals.map((v) => v + "") },
    ],
    series,
    plugins: [thresholdPlugin(THRESHOLDS)],
    legend: { show: true },
  };
  return new uPlot(opts, data, el("chart-cells"));
}

function buildDeltaChart(data) {
  const opts = {
    width: chartWidth("chart-delta"),
    height: 200,
    scales: { x: { time: true }, y: {} },
    axes: [{ stroke: AXIS_STROKE }, { stroke: AXIS_STROKE }],
    series: [
      {},
      {
        label: "Δ",
        stroke: "#f5c542",
        width: 1.5,
        value: (u, v) => (v == null ? "--" : v + " mV"),
        points: { show: false },
      },
    ],
  };
  return new uPlot(opts, data, el("chart-delta"));
}

function buildPackChart(data) {
  const opts = {
    width: chartWidth("chart-pack"),
    height: 220,
    scales: { x: { time: true }, mv: {}, ma: {} },
    axes: [
      { stroke: AXIS_STROKE },
      {
        stroke: AXIS_STROKE,
        scale: "mv",
        side: 3,
        values: (u, vals) => vals.map((v) => (v / 1000).toFixed(1) + "V"),
      },
      {
        stroke: AXIS_STROKE,
        scale: "ma",
        side: 1,
        grid: { show: false },
        values: (u, vals) => vals.map((v) => (v / 1000).toFixed(1) + "A"),
      },
    ],
    series: [
      {},
      {
        label: "Pack V",
        scale: "mv",
        stroke: "#4ea1ff",
        width: 1.5,
        value: (u, v) => (v == null ? "--" : (v / 1000).toFixed(2) + " V"),
        points: { show: false },
      },
      {
        label: "Pack A",
        scale: "ma",
        stroke: "#ff7847",
        width: 1.5,
        value: (u, v) => (v == null ? "--" : (v / 1000).toFixed(2) + " A"),
        points: { show: false },
      },
    ],
  };
  return new uPlot(opts, data, el("chart-pack"));
}

// rangeToCharts turns an /api/range response into the three charts' uPlot data
// arrays. xs are unix seconds; the delta series is computed client-side from the
// per-bucket cell arrays (FR-9).
function rangeToCharts(r) {
  const ts = r.ts || [];
  const xs = ts.map((t) => t / 1000);

  const cellSeries = r.cells || [];
  const cellsData = [xs];
  for (let i = 0; i < 16; i++) {
    cellsData.push(cellSeries[i] ? cellSeries[i].slice() : new Array(xs.length).fill(null));
  }

  const delta = new Array(xs.length).fill(null);
  for (let j = 0; j < xs.length; j++) {
    let min = null;
    let max = null;
    for (let i = 0; i < 16; i++) {
      const v = cellSeries[i] ? cellSeries[i][j] : null;
      if (v == null) continue;
      if (min == null || v < min) min = v;
      if (max == null || v > max) max = v;
    }
    delta[j] = min == null ? null : max - min;
  }
  const deltaData = [xs.slice(), delta];

  const packData = [
    xs.slice(),
    (r.pack_mv || new Array(xs.length).fill(null)).slice(),
    (r.pack_ma || new Array(xs.length).fill(null)).slice(),
  ];

  return { cellsData, deltaData, packData };
}

// loadCharts fetches /api/range for the current window and (re)builds all three
// charts. A full reload happens on range change; live append handles per-tick
// updates between reloads (FR-9).
async function loadCharts() {
  // Show a spinner over each chart and disable the range controls for the
  // duration of the reload (FR-3, FR-4). The finally clears them on both the
  // success and fetch-error paths.
  const spinners = document.querySelectorAll(".spinner-overlay");
  const buttons = document.querySelectorAll("#ranges button, #custom-apply");
  spinners.forEach((s) => { s.hidden = false; });
  buttons.forEach((b) => { b.disabled = true; });
  try {
    const { from, to } = rangeBounds(currentRange);
    let r = { ts: [], cells: [], pack_mv: [], pack_ma: [] };
    try {
      const res = await fetch(
        `/api/range?from=${from}&to=${to}&fields=cells,pack_mv,pack_ma`
      );
      if (!res.ok) throw new Error("HTTP " + res.status);
      r = await res.json();
    } catch (err) {
      el("updated").textContent = "range fetch error: " + err.message;
    }

    const { cellsData, deltaData, packData } = rangeToCharts(r);

    for (const c of Object.values(charts)) {
      if (c.u) {
        c.u.destroy();
        c.u = null;
      }
    }

    charts.cells.data = cellsData;
    charts.cells.u = buildCellsChart(cellsData);
    charts.delta.data = deltaData;
    charts.delta.u = buildDeltaChart(deltaData);
    charts.pack.data = packData;
    charts.pack.u = buildPackChart(packData);

    // Reset the append watermark to the last loaded point so the next tick only
    // appends genuinely-newer samples.
    const ts = r.ts || [];
    lastAppendedTS = ts.length ? ts[ts.length - 1] : 0;
  } finally {
    spinners.forEach((s) => { s.hidden = true; });
    buttons.forEach((b) => { b.disabled = false; });
  }
}

// onResize re-fits every chart to its container width.
function onResize() {
  if (charts.cells.u) charts.cells.u.setSize({ width: chartWidth("chart-cells"), height: 320 });
  if (charts.delta.u) charts.delta.u.setSize({ width: chartWidth("chart-delta"), height: 200 });
  if (charts.pack.u) charts.pack.u.setSize({ width: chartWidth("chart-pack"), height: 220 });
}

// --- init ------------------------------------------------------------------

function init() {
  for (const btn of document.querySelectorAll("#ranges button")) {
    btn.addEventListener("click", () => setRange(btn.dataset.range));
  }
  el("custom-apply").addEventListener("click", applyCustom);
  window.addEventListener("resize", onResize);
  loadCharts();
  refreshHeader();
  setInterval(refreshHeader, HEADER_INTERVAL_MS);
}

document.addEventListener("DOMContentLoaded", init);
