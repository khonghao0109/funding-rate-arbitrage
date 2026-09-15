// Crowding Reversal (phase 6) — prepared screen, no live source yet.
//
// Live ingestion of Binance's long/short account ratio is step 6.2, behind the
// 3.5 verdict and 3.4. Until then the tab draws the research package's frozen
// fixture: columns it RECORDED, copied into research/crowding-research.json by a
// test that fails if the copy drifts. The charts draw those columns as they
// are; the summary tiles (window mean, share beyond the entry threshold, sign
// flips) are counted on this page and say so. The target is a fraction of
// equity before any cost — not an order.

import { $, el, clear, setText, isNum, fmt, api, chartOptions, chartsReady, NEON } from "./core.js";
import { shell } from "./shell.js";

const state = { data: null, asset: "BTC", days: 365, charts: [], series: {}, syncing: false, loading: null };

async function load() {
  if (state.data) return state.data;
  if (!state.loading) {
    state.loading = api("research/crowding-research.json")
      .then((r) => {
        if (!r.ok) throw new Error(r.body.error_vi || `HTTP ${r.status}`);
        state.data = r.body;
        return r.body;
      })
      .catch((err) => {
        state.loading = null;
        throw err;
      });
  }
  return state.loading;
}

function ensureCharts() {
  if (state.charts.length || !chartsReady() || shell.active !== "crowding") return;
  const LWC = window.LightweightCharts;
  const make = (id) => LWC.createChart($(id), chartOptions({ timeScale: { secondsVisible: false }, rightPriceScale: { minimumWidth: 72 } }));
  const ratio = make("cr-ratio");
  const score = make("cr-score");
  const target = make("cr-target");
  state.series.ratio = ratio.addLineSeries({ color: NEON.cyan, lineWidth: 2, priceLineVisible: false });
  state.series.score = score.addLineSeries({ color: NEON.blue, lineWidth: 2, priceLineVisible: false });
  state.series.target = target.addHistogramSeries({ priceLineVisible: false, priceFormat: { type: "price", precision: 3, minMove: 0.001 } });
  state.charts = [ratio, score, target];
  // The three panes read as one: scrolling or zooming any of them moves all.
  for (const chart of state.charts) {
    chart.timeScale().subscribeVisibleLogicalRangeChange((range) => {
      if (state.syncing || !range) return;
      state.syncing = true;
      for (const other of state.charts) if (other !== chart) other.timeScale().setVisibleLogicalRange(range);
      state.syncing = false;
    });
  }
}

function column(asset, key) {
  return asset[key] || [];
}

function render() {
  const data = state.data;
  if (!data) return;
  ensureCharts();
  const asset = (data.assets || []).find((a) => a.asset === state.asset);
  setText("cr-asset-label", state.asset);
  if (!asset || !asset.t_sec || asset.t_sec.length === 0) {
    setText("cr-banner-note", `snapshot không có cột cho ${state.asset}`);
    return;
  }
  const t = asset.t_sec;
  const lastT = t[t.length - 1];
  const from = lastT - state.days * 86400;
  const idx = [];
  for (let i = 0; i < t.length; i++) if (t[i] > from) idx.push(i);

  const cfg = data.configuration || {};
  const entry = isNum(cfg.entry_z) ? cfg.entry_z : 1;
  const exit = isNum(cfg.exit_z) ? cfg.exit_z : 0.25;
  setText("cr-entry", String(entry).replace(".", ","));
  setText("cr-exit", String(exit).replace(".", ","));

  const ratio = column(asset, "long_short_account_ratio");
  const score = column(asset, "crowding_score");
  const signal = column(asset, "signal");
  const target = column(asset, "target_frac_of_equity");
  const point = (values) => idx.map((i) => (isNum(values[i]) ? { time: t[i], value: values[i] } : { time: t[i] }));

  if (state.series.ratio) {
    state.series.ratio.setData(point(ratio));
    state.series.score.setData(point(score));
    state.series.target.setData(idx.map((i) => (isNum(target[i])
      ? { time: t[i], value: target[i], color: target[i] >= 0 ? "rgba(0, 242, 254, 0.75)" : "rgba(255, 75, 75, 0.75)" }
      : { time: t[i] })));
    for (const line of state.series.priceLines || []) line.series.removePriceLine(line.line);
    state.series.priceLines = [
      { series: state.series.ratio, line: state.series.ratio.createPriceLine({ price: 1, color: "rgba(174,184,199,0.5)", lineStyle: 2, lineWidth: 1, axisLabelVisible: false }) },
      ...[entry, -entry].map((price) => ({ series: state.series.score, line: state.series.score.createPriceLine({ price, color: NEON.neg, lineStyle: 2, lineWidth: 1, axisLabelVisible: true, title: "vào" }) })),
      ...[exit, -exit].map((price) => ({ series: state.series.score, line: state.series.score.createPriceLine({ price, color: "rgba(174,184,199,0.55)", lineStyle: 1, lineWidth: 1, axisLabelVisible: false }) })),
    ];
    for (const chart of state.charts) chart.timeScale().fitContent();
  }

  const last = idx[idx.length - 1];
  const scored = idx.filter((i) => isNum(score[i]));
  const beyond = scored.filter((i) => Math.abs(score[i]) >= entry).length;
  const ratios = idx.map((i) => ratio[i]).filter(isNum);
  const meanRatio = ratios.length ? ratios.reduce((a, b) => a + b, 0) / ratios.length : NaN;
  // A flip is long → short or short → long; entering from or leaving to flat
  // is not one.
  let flips = 0;
  let lastSign = 0;
  for (const i of idx) {
    if (!isNum(target[i]) || target[i] === 0) continue;
    const s = Math.sign(target[i]);
    if (lastSign && s !== lastSign) flips++;
    lastSign = s;
  }

  const stats = $("cr-stats");
  clear(stats);
  const stat = (k, v, cls, s) => el("div", { cls: "stat" }, [
    el("div", { cls: "stat-k", text: k }),
    el("div", { cls: "stat-v " + (cls || ""), text: v }),
    s ? el("div", { cls: "stat-s", text: s }) : null,
  ]);
  stats.append(
    stat("Tỉ lệ L/S nến cuối", isNum(ratio[last]) ? ratio[last].toFixed(4) : "—", "", `${fmt.utc(t[last] * 1000)} · TB cửa sổ (tính tại trang) ${isNum(meanRatio) ? meanRatio.toFixed(3) : "—"}`),
    stat("Crowding score nến cuối", isNum(score[last]) ? fmt.bps(score[last], 3) : "—", isNum(score[last]) && Math.abs(score[last]) >= entry ? "warn" : "", "z trung bình ensemble; ≥ ngưỡng vào là vùng đám đông lệch"),
    stat("Tín hiệu ensemble", isNum(signal[last]) ? fmt.bps(signal[last], 3) : "—", "", "bội số của 1/12 — trung bình 12 thành viên"),
    stat("Mục tiêu (phần vốn)", isNum(target[last]) ? fmt.bps(target[last], 3) : "—", isNum(target[last]) ? (target[last] >= 0 ? "pos" : "neg") : "", "trước mọi chi phí · không phải lệnh"),
    stat("Nến có |score| ≥ vào", scored.length ? fmt.pct((beyond / scored.length) * 100, 1) : "—", "", `${beyond}/${scored.length} nến trong ${state.days} ngày · đếm tại trang`),
    stat("Đổi chiều mục tiêu", String(flips), "", "long ↔ short trong cửa sổ · đếm tại trang"),
  );

  setText("cr-banner-note",
    `${data.not_live_vi} Nguồn đang vẽ: ${data.source_vi} Panel ${data.panel_first_utc} → ${data.panel_last_utc}; tab này hiện ${data.window_days} ngày cuối (4 giờ/nến). Fixture ${data.fixture_path} · sha256 ${String(data.fixture_sha256).slice(0, 16)}…`);
  const members = Array.isArray(data.ensemble_members) ? data.ensemble_members.map((m) => `${m[0]}/${m[1]}/${m[2]}`).join(", ") : "—";
  setText("cr-config",
    `Cấu hình đóng băng của manifest: lookback ${cfg.lookback} nến · vào ±${entry} · ra ±${exit} · target_vol ${cfg.target_vol} · max_gross ${cfg.max_gross} · phí giả định ${isNum(cfg.fee) ? cfg.fee * 1e4 : "?"} bps/chiều · lọc xu hướng ${cfg.use_trend_filter ? "bật" : "tắt"} (${(cfg.trend_horizon_days || []).join("/")} ngày, cần ${cfg.trend_required}). Thành viên (lookback/vào/ra): ${members}. Quyết định của gói: ${data.manifest_decision}.`);
}

function press(group, attr, value) {
  for (const button of document.querySelectorAll(`#${group} button`)) {
    button.setAttribute("aria-pressed", button.dataset[attr] === String(value) ? "true" : "false");
  }
}

export function initCrowding() {
  for (const button of document.querySelectorAll("#cr-assets button")) {
    button.addEventListener("click", () => {
      state.asset = button.dataset.asset;
      press("cr-assets", "asset", state.asset);
      render();
    });
  }
  for (const button of document.querySelectorAll("#cr-ranges button")) {
    button.addEventListener("click", () => {
      state.days = Number(button.dataset.days) || 365;
      press("cr-ranges", "days", state.days);
      render();
    });
  }
  shell.onTab("crowding", {
    enter: async () => {
      try {
        await load();
        render();
      } catch (err) {
        setText("cr-banner-note", `không tải được snapshot nghiên cứu: ${err && err.message ? err.message : err}`);
      }
    },
  });
}
