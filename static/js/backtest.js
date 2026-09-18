// Backtest 3 Năm — the three-year replay of the auto-trader's 4.5f rules,
// read from /api/backtest, which serves a file built OUTSIDE this process by
// tools/report/bt3y.py.
//
// This module SENDS NOTHING and computes no rule. Every number it draws is a
// field the report already carried; where it does arithmetic at all it is to
// pick a colour or a share, never to restate a profit. The figures are a
// SIMULATION over history and the banner says so — nothing here was traded.
//
// What the numbers are (CLAUDE.md rule 2): the report's own assumptions list is
// printed on the page rather than summarized, because the round trip is a
// STATED cost — there is no historical order book — and a reader who cannot see
// that cannot read the profit.

import { $, el, clear, setText, isNum, fmt, api, schedule, chartOptions, chartsReady, emptyRow, signCls, NEON, registerChart } from "./core.js";
import { shell } from "./shell.js";

const POLL_MS = 300000;

const state = { chart: null, series: null, poll: null, report: null, filter: "", venue: "" };

const tag = () => el("span", { cls: "tag paper", text: "BACKTEST" });
const usdt = (v, d) => (isNum(v) ? fmt.quote(v, d === undefined ? 2 : d, true) : "—");

function ensureChart() {
  if (state.chart || !chartsReady() || shell.active !== "backtest") return;
  state.chart = registerChart(window.LightweightCharts.createChart($("bt-equity"), chartOptions({ timeScale: { secondsVisible: false } })));
  state.series = state.chart.addAreaSeries({
    lineColor: NEON.cyan,
    topColor: "rgba(0, 242, 254, 0.28)",
    bottomColor: "rgba(79, 172, 254, 0.02)",
    lineWidth: 2,
    priceLineVisible: false,
  });
  if (state.report) drawChart(state.report);
}

async function refresh() {
  if (!shell.isActive("backtest")) return;
  const r = await api("/api/backtest");
  if (!r.ok) {
    setText("bt-status", r.body.error_vi || `HTTP ${r.status}`, "note neg");
    setText("bt-source", "không đọc được");
    return;
  }
  state.report = r.body;
  render(r.body);
}

function tile(label, value, cls, sub) {
  return el("div", { cls: "stat" }, [
    el("div", { cls: "stat-k" }, [tag(), label]),
    el("div", { cls: "stat-v " + (cls || ""), text: value }),
    sub ? el("div", { cls: "stat-s", text: sub }) : null,
  ]);
}

function drawChart(r) {
  const pts = r.equity_curve || [];
  $("bt-equity-empty").hidden = pts.length > 0;
  if (!state.series || !pts.length) return;
  state.series.setData(pts.map((p) => ({ time: p.time, value: p.value })));
  if (state.chart) state.chart.timeScale().fitContent();
  const worst = pts.reduce((a, b) => (b.drawdown < a.drawdown ? b : a), pts[0]);
  setText("bt-equity-note",
    `${pts.length} điểm ngày · vốn mở đầu ${fmt.quote(r.summary.capital_start_quote, 2)} quote trên ${r.summary.slots} chỗ · `
    + `sụt vốn sâu nhất ${fmt.pct(-r.summary.max_drawdown_pct, 3)} vào ${worst.time}. `
    + `Đường này là vốn danh mục: mỗi cặp một chỗ, chỗ chưa vào lệnh không sinh lời.`);
}

function renderTiles(r) {
  const s = r.summary;
  const box = $("bt-tiles");
  clear(box);
  const beat = s.beats_hold_through;
  box.append(
    tile("Lợi nhuận ròng", `${usdt(s.net_profit_quote)} quote`, signCls(s.net_profit_quote),
      `${fmt.pct(s.net_profit_pct_on_capital, 3, true)} trên vốn danh mục`),
    tile("Mỗi chuỗi / năm", fmt.pct(s.apr_on_capital_pct, 3, true), signCls(s.apr_on_capital_pct),
      `tổng ${fmt.pct(s.return_per_series_on_capital_pct, 3, true)} qua ${r.window.years} năm`),
    tile("So với giữ suốt", `${fmt.pct(s.hold_through_apr_on_capital_pct, 3, true)}/năm`, beat ? "pos" : "neg",
      beat ? "luật VƯỢT mốc giữ suốt" : "luật THUA mốc giữ suốt — mốc chuẩn của repo"),
    tile("Sụt vốn tối đa", fmt.pct(-s.max_drawdown_pct, 3), s.max_drawdown_pct > 0 ? "neg" : "",
      s.max_drawdown_day || "—"),
    tile("Tỷ lệ thắng", fmt.pct(s.win_rate_pct, 1), "",
      `${s.total_trades} lệnh đóng · ${s.still_open} còn mở · giữ TB ${s.avg_hold_days} ngày`),
    tile("Funding bù phí", fmt.pct(s.funding_covers_fees_pct, 0), s.funding_covers_fees_pct >= 100 ? "pos" : "neg",
      `funding ${usdt(s.total_funding_quote, 0)} · trôi giá ${usdt(s.total_drift_quote, 0)} · phí ${usdt(s.total_fees_quote, 0)}`),
  );
}

// The venue table is the one place a corpus-depth mistake is easy to make, so
// every row carries the days that venue ACTUALLY covered and the APR is the
// server's own figure over those days. A venue with a quarter of history is not
// comparable with one that has three years, and the row says which it is.
function renderVenues(r) {
  const body = $("bt-venues");
  clear(body);
  const rows = r.venues_breakdown || [];
  if (!rows.length) return emptyRow(body, 10, "—");
  let bridged = 0;
  for (const v of rows) {
    if (v.quote_bridged) bridged++;
    const beat = v.apr_on_capital_pct > v.hold_through_apr_on_capital_pct;
    const name = el("td", {}, [
      el("strong", { text: v.label }),
      v.quote_bridged
        ? el("div", { cls: "cell-sub warn", text: "perp USD ↔ spot USDT — hở rủi ro quy đổi, KHÔNG trừ ở đâu cả" })
        : el("div", { cls: "cell-sub", text: `${v.perp_venue} ← ${v.spot_venue}` }),
    ]);
    body.append(el("tr", {}, [
      name,
      el("td", { cls: "r num", text: v.interval_sec === 3600 ? "1h" : `${Math.round(v.interval_sec / 3600)}h` }),
      el("td", { cls: "r num", text: String(v.series) }),
      el("td", { cls: "r num", text: String(v.trades) }),
      el("td", { cls: "r num " + signCls(v.return_on_capital_pct), text: fmt.pct(v.return_on_capital_pct, 3, true) }),
      el("td", { cls: "r num " + signCls(v.apr_on_capital_pct), text: fmt.pct(v.apr_on_capital_pct, 3, true) }),
      el("td", { cls: "r num " + (beat ? "" : "warn"), text: fmt.pct(v.hold_through_apr_on_capital_pct, 3, true) }),
      el("td", { cls: "r num", text: v.trades ? fmt.pct(v.win_rate_pct, 0) : "—" }),
      el("td", { cls: "r num", text: fmt.bps(v.round_trip_cost_frac * 1e4, 0) + " bps" }),
      el("td", { cls: "r num" }, [
        el("span", { text: String(Math.round(v.covered_days)) }),
        el("div", { cls: "cell-sub", text: `${v.covered_from} → ${v.covered_to}` }),
      ]),
    ]));
  }
  setText("bt-venues-note",
    `${rows.length} sàn · ${bridged} sàn ghép perp USD với spot USDT (nhãn vàng): delta-neutral theo coin nhưng MỞ rủi ro USDT/USD, và không con số nào trên trang này trừ khoản đó. `
    + `Nhịp settle đo từ chính chuỗi: các luật đếm theo MỐC, nên sàn giữ ${r.params.min_hold_epochs} mốc là ${r.params.min_hold_epochs * 8} giờ ở sàn 8h nhưng chỉ ${r.params.min_hold_epochs} giờ ở sàn 1h.`);
}

function renderYearly(r) {
  const body = $("bt-yearly");
  clear(body);
  const rows = r.yearly_breakdown || [];
  if (!rows.length) return emptyRow(body, 8, "—");
  for (const y of rows) {
    body.append(el("tr", {}, [
      el("td", {}, [el("strong", { text: y.label }), el("div", { cls: "cell-sub", text: `${y.from_day} → ${y.to_day}` })]),
      el("td", { cls: "r num", text: String(y.trades) }),
      el("td", { cls: "r num", text: `${y.series_active}/${y.venues_active}` }),
      el("td", { cls: "r num " + signCls(y.pnl_quote), text: usdt(y.pnl_quote, 0) }),
      el("td", { cls: "r num " + signCls(y.return_on_capital_pct), text: fmt.pct(y.return_on_capital_pct, 3, true) }),
      el("td", { cls: "r num", text: y.trades ? fmt.pct(y.win_rate_pct, 0) : "—" }),
      el("td", { cls: "r num", text: usdt(y.funding_quote, 0) }),
      el("td", { cls: "r num neg", text: usdt(y.fees_quote, 0) }),
    ]));
  }
}

function renderSymbols(r) {
  const body = $("bt-symbols");
  clear(body);
  const rows = r.symbols_breakdown || [];
  if (!rows.length) return emptyRow(body, 7, "—");
  for (const x of rows) {
    // The pair's own hold-through sits beside its figure, never subtracted from
    // it: the comparison the repo ranks on is "did the rule beat holding".
    const beat = x.return_on_capital_pct > x.hold_through_on_capital_pct;
    body.append(el("tr", {}, [
      el("td", {}, [el("strong", { text: x.symbol }),
        x.still_open ? el("div", { cls: "cell-sub warn", text: `${x.still_open} còn mở` }) : null]),
      el("td", { cls: "r num", text: String(x.trades) }),
      el("td", { cls: "r num " + signCls(x.return_on_capital_pct), text: fmt.pct(x.return_on_capital_pct, 3, true) }),
      el("td", { cls: "r num " + signCls(x.apr_on_capital_pct), text: fmt.pct(x.apr_on_capital_pct, 3, true) }),
      el("td", { cls: "r num " + (beat ? "" : "warn"), text: fmt.pct(x.hold_through_on_capital_pct, 3, true) }),
      el("td", { cls: "r num", text: x.trades ? fmt.pct(x.win_rate_pct, 0) : "—" }),
      el("td", { cls: "r num", text: x.trades ? `${x.avg_days}d` : "—" }),
    ]));
  }
}

function renderFilter(r) {
  const sel = $("bt-filter");
  const want = state.filter;
  clear(sel);
  sel.append(el("option", { text: "Tất cả", attrs: { value: "" } }));
  for (const x of r.symbols_breakdown || []) {
    sel.append(el("option", { text: x.symbol, attrs: { value: x.symbol } }));
  }
  sel.value = want;

  const vsel = $("bt-venue-filter");
  const wantV = state.venue;
  clear(vsel);
  vsel.append(el("option", { text: "Tất cả sàn", attrs: { value: "" } }));
  for (const v of r.venues_breakdown || []) {
    vsel.append(el("option", { text: v.label, attrs: { value: v.perp_venue } }));
  }
  vsel.value = wantV;
}

function renderTrades(r) {
  const body = $("bt-trades");
  clear(body);
  const all = r.trades || [];
  const rows = all.filter((t) => (!state.filter || t.symbol === state.filter)
    && (!state.venue || t.perp_venue === state.venue));
  setText("bt-trade-count", `${rows.length} / ${all.length} lệnh`);
  if (!rows.length) return emptyRow(body, 12, "Không có lệnh nào khớp bộ lọc.");
  for (const t of rows) {
    const reason = el("td", {}, [
      el("span", { cls: "pill " + (t.still_open ? "warn" : "ok"), text: t.still_open ? "CÒN MỞ" : "ĐÃ ĐÓNG" }),
      el("div", { cls: "cell-sub", text: t.exit_reason_vi || t.exit_reason }),
    ]);
    body.append(el("tr", {}, [
      el("td", {}, [
        el("strong", { text: t.symbol }),
        el("div", { cls: "cell-sub mono " + (t.quote_bridged ? "warn" : ""), text: t.venue_label || "" }),
      ]),
      el("td", { cls: "mono", text: t.entry_time }),
      el("td", { cls: "mono", text: t.exit_time }),
      el("td", { cls: "r num", text: String(t.duration_days) }),
      el("td", { cls: "r num", text: String(t.epochs) }),
      el("td", { cls: "r num", text: `${fmt.bps(t.entry_basis_bps, 1)} → ${fmt.bps(t.exit_basis_bps, 1)}` }),
      el("td", { cls: "r num " + signCls(t.funding_received), text: usdt(t.funding_received, 2) }),
      el("td", { cls: "r num " + signCls(t.drift_quote), text: usdt(t.drift_quote, 2) }),
      el("td", { cls: "r num neg", text: usdt(t.fees_paid, 2) }),
      el("td", { cls: "r num " + signCls(t.pnl_quote), text: usdt(t.pnl_quote, 2) }),
      el("td", { cls: "r num " + signCls(t.pnl_pct), text: fmt.pct(t.pnl_pct, 3, true) }),
      reason,
    ]));
  }
}

const PARAM_LABELS = {
  min_entry_basis_bps: "Trụ cột 1 · Basis lúc vào tối thiểu (bps)",
  min_hold_epochs: "Trụ cột 2 · Sàn giữ khấu hao phí (mốc)",
  target_take_profit_net_pct: "Trụ cột 3 · Chốt lời hội tụ (% trên vốn)",
  exit_negative_funding_rate_bps: "Trụ cột 4 · Ngưỡng funding âm (bps)",
  exit_negative_consecutive_epochs: "Trụ cột 4 · Số mốc âm liên tiếp",
  max_basis_widen_bps: "Trụ cột 4 · Cắt lỗ basis giãn (bps)",
  min_net_apr_pct: "Lối vào · Net APR dự phóng tối thiểu (%)",
  projection_hold_days: "Lối vào · Thời gian giữ dự phóng (ngày)",
  trailing_days: "Lối vào · Cửa sổ trung bình trượt (ngày)",
  max_exit_spread_bps: "Trụ cột 3 · Trần spread khi gửi lệnh chốt lời (bps)",
  capital_per_notional: "Vốn mỗi quote notional",
  notional_quote: "Notional mỗi chân (quote)",
  capital_per_slot_quote: "Vốn mỗi chỗ (quote)",
};

function renderMethod(r) {
  setText("bt-engine", `${r.engine} · cửa sổ ${r.window.from_day} → ${r.window.to_day} (${r.window.days} ngày) · dựng lúc ${fmt.utc(r.built_at_ms)}`);
  const list = $("bt-assumptions");
  clear(list);
  for (const a of r.assumptions_vi || []) list.append(el("li", { text: a }));

  const body = $("bt-params");
  clear(body);
  const p = r.params || {};
  const keys = Object.keys(PARAM_LABELS).filter((k) => p[k] !== undefined);
  if (!keys.length) return emptyRow(body, 2, "—");
  for (const k of keys) {
    body.append(el("tr", {}, [
      el("td", { text: PARAM_LABELS[k] }),
      el("td", { cls: "r num mono", text: String(p[k]) }),
    ]));
  }
  // The round trip is no longer one number: each venue pays its own verified
  // taker schedule, so it belongs in the venue table and is pointed at here.
  body.append(el("tr", {}, [
    el("td", { text: "Chi phí vòng" }),
    el("td", { cls: "r", text: "theo từng sàn — xem cột \"Vòng phí\" ở bảng Hiệu suất theo sàn" }),
  ]));
}

function render(r) {
  const s = r.summary;
  setText("bt-source", `${r.label_vi || "backtest"} · ${s.slots} chuỗi / ${s.venues} sàn`);
  const reasons = Object.entries(s.exit_reasons || {}).map(([k, v]) => `${k} ${v}`).join(" · ") || "không có";
  setText("bt-status",
    `cửa sổ ${r.window.from_day} → ${r.window.to_day} (${r.window.years} năm) · ${s.slots} chuỗi trên ${s.venues} sàn · phủ TB ${Math.round(s.mean_covered_days)} ngày/chuỗi · ${s.total_trades} lệnh đóng, ${s.still_open} còn mở · lý do ra: ${reasons}`,
    "note");
  setText("bt-equity-unit", `quote · vốn = notional × ${r.params.capital_per_notional}`);
  renderTiles(r);
  ensureChart();
  drawChart(r);
  renderVenues(r);
  renderYearly(r);
  renderSymbols(r);
  renderFilter(r);
  renderTrades(r);
  renderMethod(r);
}

export function initBacktest() {
  $("bt-refresh").addEventListener("click", () => state.poll && state.poll.kick());
  $("bt-filter").addEventListener("change", (e) => {
    state.filter = e.target.value;
    if (state.report) renderTrades(state.report);
  });
  $("bt-venue-filter").addEventListener("change", (e) => {
    state.venue = e.target.value;
    if (state.report) renderTrades(state.report);
  });
  state.poll = schedule(refresh, () => (shell.isActive("backtest") ? POLL_MS : 15000));
  shell.onTab("backtest", {
    enter: () => {
      ensureChart();
      state.poll.kick();
    },
  });
}
