// Auto-Trader view — draws the portfolio the server's testnet bot reports
// (PLAN Q18): the result strip, the equity curve and the funding bars, the
// pairs it holds, the opportunity radar, its trades and its console.
//
// This module SENDS NOTHING. Every write — start, stop, kill, one pair's switch,
// closing one pair — belongs to execution.js, which hands the row buttons their
// handlers and confirms before any of them reaches the server. The server tests
// hold that line by reading this file.
//
// What the figures are (CLAUDE.md rule 2): the server labels every total with
// what it takes off and what it leaves out, and those labels are printed under
// the chart. A gain and a loss differ by sign and by word as well as by colour.

import { $, el, clear, setText, isNum, fmt, signCls, chartOptions, chartsReady, emptyRow, registerChart } from "./core.js";
import { shell } from "./shell.js";
import { t, onLanguageChange } from "./i18n.js";

// Chart marks only — text keeps the page's .pos/.neg ink. Teal against rose,
// checked for colour-vision separation on the page's dark surface.
const GAIN = "#14b8a6";
const LOSS = "#f43f5e";
const LOG_LINES = 8;

const view = {
  handlers: null,
  writable: false,
  status: null,
  pnl: null,
  filter: "",
  charts: null,
  pointsByTime: new Map(),
  barsByTime: new Map(),
  lastPointCount: -1,
  lastBarCount: -1,
  countdowns: [],
};

// ---------------------------------------------------------------- formatting

const usdt = (x, digits) => (isNum(x) ? `${fmt.quote(x, digits === undefined ? 4 : digits, true)} USDT` : "—");

// resultWord says gain or loss in words. A figure that includes an open pair
// is provisional — its exit's fees and slippage are still to come — and says so.
function resultWord(x, provisional) {
  if (!isNum(x) || Math.abs(x) < 5e-9) return t("at_flat");
  const word = x > 0 ? t("at_profit") : t("at_loss");
  return provisional ? (x > 0 ? t("at_unrealized_profit") : t("at_unrealized_loss")) : word;
}

// The chart library draws UTC; the page prints local time everywhere else. The
// offset is folded into the stamp so the axis and the tooltip agree.
function chartTime(ms) {
  return Math.floor(ms / 1000) - new Date(ms).getTimezoneOffset() * 60;
}

function pairsOf(s) {
  return (s && s.pairs) || [];
}

function tradeFor(intentID) {
  return ((view.pnl && view.pnl.trades) || []).find((t) => t.intent_id === intentID) || null;
}

// ----------------------------------------------------------------- the strip

function renderStateTile(s) {
  const badge = $("at-badge");
  const max = s.portfolio ? s.portfolio.max_concurrent_positions : 0;
  let word = s.state_vi || s.state;
  if (s.state === "running") word = `${t("at_running_prefix")} · ${s.open_positions}/${max} ${t("at_pairs_suffix")}`;
  if (s.busy) word += ` · ĐANG ${s.busy === "kill" ? "KILL" : s.busy === "stop" ? "DỪNG" : "ĐÓNG CẶP"}`;
  badge.dataset.state = s.state;
  badge.dataset.busy = s.busy ? "true" : "false";
  setText(badge, word);
  badge.title = `${s.state_vi || s.state} từ ${fmt.time(s.state_since_ms)}`;

  const parts = [];
  if (s.state === "running") {
    parts.push(`chạy từ ${fmt.time(s.state_since_ms)}`);
    if (s.last_scan_at_ms) parts.push(`quét ${fmt.time(s.last_scan_at_ms)}`);
  } else if (s.state === "emergency_halted") {
    parts.push("chờ người vận hành xác nhận");
  } else {
    parts.push("chưa kích hoạt");
  }
  if (s.halted_pairs > 0) parts.push(`${s.halted_pairs} cặp DỪNG BẢO VỆ`);
  setText("at-kpi-state-sub", parts.join(" · "), "at-kpi-sub " + (s.halted_pairs > 0 ? "neg" : ""));

  const halt = $("at-halt");
  const lines = [];
  if (s.halt_reason_vi) lines.push(`BOT DỪNG BẢO VỆ — ${s.halt_reason_vi}. Xử lý nguyên nhân, rồi XÁC NHẬN & TẮT trước khi bật lại.`);
  // The halted pairs AS THIS RENDER SHOWS THEM, each with its number: ACK ALL
  // quotes exactly these, so a halt raised after this render is not
  // acknowledged by a click on it.
  const halted = pairsOf(s)
    .filter((p) => p.state === "emergency_halted")
    .map((p) => ({ symbol: p.symbol, halt_seq: p.halt_seq, halt_reason_vi: p.halt_reason_vi }));
  for (const p of halted) {
    lines.push(`${p.symbol} DỪNG BẢO VỆ #${p.halt_seq} — ${p.halt_reason_vi}. Xác nhận ở Radar cơ hội sau khi đã xử lý.`);
  }
  // Rebuilt only when what it says changes: the region is role=alert, and a
  // button rebuilt on every poll would drop keyboard focus from under a press.
  // Writable is NOT part of the key: it flips on every write and busy poll, and
  // rebuilding the alert for it would re-announce every halt line.
  const key = JSON.stringify([lines, halted.map((p) => p.halt_seq)]);
  if (halt.dataset.key !== key) {
    halt.dataset.key = key;
    halt.hidden = lines.length === 0;
    clear(halt);
    for (const line of lines) halt.append(el("div", { text: line }));
    if (halted.length > 0) {
      const ackAll = el("button", {
        cls: "btn primary small mt8",
        text: `⚡ XÁC NHẬN TẤT CẢ (${halted.length} CẶP)`,
        title: "xác nhận DỪNG BẢO VỆ của mọi cặp đang hiển thị, theo đúng số trên màn hình; mỗi cặp chuyển sang TẠM DỪNG",
        attrs: { type: "button", id: "at-ack-all" },
      });
      ackAll.addEventListener("click", () => view.handlers.ackAll(halted));
      halt.append(ackAll);
    }
  }
  const ackAllButton = halt.querySelector("button");
  if (ackAllButton) ackAllButton.disabled = !view.writable;
  if (s.notice_vi) setText("at-notice", s.notice_vi);
}

function renderCapitalTile(s) {
  const cap = s.portfolio ? s.portfolio.total_capital_cap_quote : 0;
  setText("at-kpi-capital-val", `${fmt.quote(s.capital_deployed_quote, 2)} / ${fmt.quote(cap, 0)}`);
  const frac = cap > 0 ? Math.min(1, s.capital_committed_quote / cap) : 0;
  const meter = $("at-capital-meter");
  meter.firstElementChild.style.width = `${(frac * 100).toFixed(1)}%`;
  meter.classList.toggle("hot", frac > 0.8);
  const unsized = s.unsized_pairs || [];
  meter.setAttribute("aria-label", `vốn đã cam kết ${unsized.length ? "ít nhất " : ""}${fmt.pct(frac * 100, 0)} hạn mức`);
  const slots = s.portfolio ? `${s.slots_used}/${s.portfolio.max_concurrent_positions} chỗ` : "";
  // A pair whose size no reading states is counted at a stand-in (the bot's own
  // position, or the configured notional): the committed figure is then a
  // floor, and no entry opens.
  const unknown = unsized.length ? ` · CHƯA RÕ VỐN: ${unsized.join(", ")} — con số cam kết chỉ là mức tối thiểu, không mở cặp mới` : "";
  setText("at-kpi-capital-sub",
    `${fmt.quote(s.notional_deployed_quote, 2)} notional × ${s.capital_per_notional} (spot + ký quỹ perp) · ${slots}${unknown}`);
}

function renderResultTiles(v, s) {
  if (!v) return;
  setText("at-kpi-closed-val", usdt(v.closed_cash_result_quote), "at-kpi-val num " + signCls(v.closed_cash_result_quote));
  setText("at-kpi-closed-sub", v.closed_trades
    ? `${v.closed_trades} cặp đã đóng · ${resultWord(v.closed_cash_result_quote)} · funding ${fmt.quote(v.closed_funding_quote, 4, true)} · phí ${fmt.quote(-v.closed_commission_quote, 4, true)}`
    : "chưa có cặp nào đóng");
  $("at-kpi-closed").title = v.closed_label_vi || "";

  if (v.open_positions === 0) {
    setText("at-kpi-open-val", usdt(0), "at-kpi-val num");
    setText("at-kpi-open-sub", "không giữ cặp nào");
  } else {
    setText("at-kpi-open-val", v.open_priced > 0 ? usdt(v.open_result_quote) : "chưa định giá", "at-kpi-val num " + signCls(v.open_result_quote));
    const missing = v.open_positions - v.open_priced;
    setText("at-kpi-open-sub", `${v.open_positions} cặp · trôi giá ${fmt.quote(v.open_drift_quote, 4, true)} · funding ${fmt.quote(v.open_funding_quote, 4, true)}${missing > 0 ? ` · ${missing} cặp chưa định giá (bot không quét)` : ""}`,
      "at-kpi-sub " + (missing > 0 ? "warn" : ""));
  }
  $("at-kpi-open").title = v.open_label_vi || "";

  if (isNum(v.return_on_peak_capital_pct)) {
    setText("at-kpi-roi-val", fmt.pct(v.return_on_peak_capital_pct, 3, true), "at-kpi-val num " + signCls(v.return_on_peak_capital_pct));
    const onDeployed = isNum(v.open_return_on_deployed_pct) ? ` · đang mở ${fmt.pct(v.open_return_on_deployed_pct, 3, true)}` : "";
    setText("at-kpi-roi-sub", `tổng ${usdt(v.total_quote)} ÷ vốn cao nhất cùng lúc ${fmt.quote(v.peak_capital_quote, 2)}${onDeployed}`);
  } else {
    setText("at-kpi-roi-val", "—", "at-kpi-val num");
    setText("at-kpi-roi-sub", "chưa có cặp nào của bot để tính");
  }
  $("at-kpi-roi").title = v.total_label_vi || "";
}

// ----------------------------------------------------------------- the charts

function ensureCharts() {
  if (view.charts || !chartsReady() || shell.active !== "autotrade") return;
  const LWC = window.LightweightCharts;
  const equity = registerChart(LWC.createChart($("at-pnl-chart"), chartOptions({
    rightPriceScale: { minimumWidth: 84, scaleMargins: { top: 0.15, bottom: 0.15 } },
    timeScale: { secondsVisible: false },
  })));
  const curve = equity.addBaselineSeries({
    baseValue: { type: "price", price: 0 },
    topLineColor: GAIN,
    topFillColor1: "rgba(20, 184, 166, 0.28)",
    topFillColor2: "rgba(20, 184, 166, 0.02)",
    bottomLineColor: LOSS,
    bottomFillColor1: "rgba(244, 63, 94, 0.02)",
    bottomFillColor2: "rgba(244, 63, 94, 0.28)",
    lineWidth: 2,
    // Steps, not slopes: a closed result changes at the instant a pair closes
    // and a sample holds until the next one — a diagonal would draw a gain or a
    // loss accruing where none did.
    lineType: 1,
    priceLineVisible: false,
    lastValueVisible: true,
    priceFormat: { type: "price", precision: 4, minMove: 0.0001 },
  });
  curve.createPriceLine({ price: 0, color: "rgba(174, 184, 199, 0.45)", lineWidth: 1, lineStyle: 2, axisLabelVisible: false, title: t("at_breakeven") });

  const bars = registerChart(LWC.createChart($("at-bars-chart"), chartOptions({
    rightPriceScale: { minimumWidth: 84 },
    timeScale: { secondsVisible: false },
  })));
  const histogram = bars.addHistogramSeries({
    priceLineVisible: false,
    lastValueVisible: false,
    base: 0,
    priceFormat: { type: "price", precision: 6, minMove: 0.000001 },
  });

  // The two charts share one clock: a zoom on either moves both. Two charts
  // rather than one with two price scales, because the curve is a running
  // total and a bar is one settlement's flow.
  let syncing = false;
  const follow = (from, to) => (range) => {
    if (syncing || !range) return;
    syncing = true;
    try {
      to.timeScale().setVisibleRange(range);
    } catch (_) {
      /* the other chart has no data yet */
    }
    syncing = false;
  };
  equity.timeScale().subscribeVisibleTimeRangeChange(follow(equity, bars));
  bars.timeScale().subscribeVisibleTimeRangeChange(follow(bars, equity));

  equity.subscribeCrosshairMove((param) => showEquityTip(param));
  bars.subscribeCrosshairMove((param) => showBarsTip(param));
  view.charts = { equity, curve, bars, histogram };
}

function tipRow(label, value, cls) {
  return el("div", { cls: "tip-row " + (cls || "") }, [el("span", { text: label }), el("span", { text: value })]);
}

function placeTip(tip, container, param) {
  const width = tip.offsetWidth || 220;
  const left = param.point.x + 18 + width > container.clientWidth ? param.point.x - width - 18 : param.point.x + 18;
  tip.style.left = `${Math.max(8, left)}px`;
}

function showEquityTip(param) {
  const tip = $("at-pnl-tip");
  const point = param && param.time !== undefined && param.point ? view.pointsByTime.get(param.time) : null;
  if (!point) {
    tip.hidden = true;
    return;
  }
  clear(tip);
  const peak = view.pnl ? view.pnl.peak_capital_quote : 0;
  const source = point.source === "closed" ? "từ file ý định: chỉ phần đã chốt" : point.source === "sample" ? "mẫu của portal" : "lần đọc này";
  tip.append(
    el("div", { cls: "tip-time", text: `${fmt.time(point.at_ms)} · ${source}` }),
    tipRow("Đã chốt", usdt(point.closed_quote)),
    tipRow("Tạm tính", isNum(point.open_quote) ? usdt(point.open_quote) + (point.open_complete ? "" : " (chưa đủ)") : "chưa biết"),
    tipRow("Tổng", `${usdt(point.total_quote)} · ${resultWord(point.total_quote, point.open_positions > 0)}`, "total " + signCls(point.total_quote)),
    tipRow("ROI trên vốn cao nhất", peak > 0 ? fmt.pct((point.total_quote / peak) * 100, 3, true) : "—"),
  );
  if (point.source !== "closed") tip.append(tipRow("Cặp đang giữ", `${point.open_positions} · vốn ${fmt.quote(point.capital_deployed_quote, 2)}`));
  tip.hidden = false;
  placeTip(tip, $("at-pnl-chart"), param);
}

function showBarsTip(param) {
  const tip = $("at-bars-tip");
  const bar = param && param.time !== undefined && param.point ? view.barsByTime.get(param.time) : null;
  if (!bar) {
    tip.hidden = true;
    return;
  }
  clear(tip);
  tip.append(el("div", { cls: "tip-time", text: `mốc settle ${fmt.time(bar.settled_at_ms)}` }));
  for (const row of bar.rows || []) tip.append(tipRow(row.symbol, usdt(row.income_quote, 6)));
  tip.append(tipRow(bar.income_quote >= 0 ? "Nhận" : "Trả", usdt(bar.income_quote, 6), "total " + signCls(bar.income_quote)));
  tip.hidden = false;
  placeTip(tip, $("at-bars-chart"), param);
}

function renderCharts(v) {
  ensureCharts();
  const points = v.points || [];
  const bars = v.bars || [];
  const hasHistory = (v.trades || []).some((t) => t.status !== "unwound" && t.status !== "alarm") || points.some((p) => p.source !== "now");
  $("at-pnl-empty").hidden = hasHistory;
  $("at-bars-empty").hidden = bars.length > 0;
  setText("at-pnl-headline", v.open_complete
    ? `Tổng ${usdt(v.total_quote)} · ${resultWord(v.total_quote, v.open_positions > 0)} · đã chốt + tạm tính`
    : `Đã chốt ${usdt(v.closed_cash_result_quote)} · ${v.open_positions - v.open_priced} cặp đang mở chưa định giá — đường vốn dừng ở mẫu đầy đủ gần nhất`,
  "hint " + (v.open_complete ? signCls(v.total_quote) : "warn"));
  const received = bars.reduce((sum, b) => sum + b.income_quote, 0);
  setText("at-bars-headline", bars.length ? `${bars.length} mốc · cộng ${usdt(received, 6)} · 7 ngày gần nhất` : "7 ngày gần nhất");
  if (!view.charts) return;

  // One value per second on the axis: the newest point of that second wins.
  view.pointsByTime = new Map();
  for (const p of points) view.pointsByTime.set(chartTime(p.at_ms), p);
  const curveData = [...view.pointsByTime.entries()].sort((a, b) => a[0] - b[0]).map(([time, p]) => ({ time, value: p.total_quote }));
  view.charts.curve.setData(hasHistory ? curveData : []);

  view.barsByTime = new Map();
  for (const b of bars) view.barsByTime.set(chartTime(b.settled_at_ms), b);
  const barData = [...view.barsByTime.entries()].sort((a, b) => a[0] - b[0])
    .map(([time, b]) => ({ time, value: b.income_quote, color: b.income_quote >= 0 ? GAIN : LOSS }));
  view.charts.histogram.setData(barData);

  if (curveData.length !== view.lastPointCount || barData.length !== view.lastBarCount) {
    view.lastPointCount = curveData.length;
    view.lastBarCount = barData.length;
    fitCharts();
  }
}

// fitCharts shows every point. It runs on the next frame, after the chart has
// measured its container: fitted while the tab was still hidden or before a
// resize, a chart keeps the old spacing and shows only its newest points.
function fitCharts() {
  if (!view.charts) return;
  requestAnimationFrame(() => {
    view.charts.equity.timeScale().fitContent();
    view.charts.bars.timeScale().fitContent();
  });
}

function renderLabels(v) {
  setText("at-label-closed", v.closed_label_vi);
  setText("at-label-open", v.open_label_vi);
  setText("at-label-total", v.total_label_vi);
  setText("at-label-bars", v.bars_label_vi);
  setText("at-label-points", v.points_label_vi);
  const list = $("at-pnl-problems");
  clear(list);
  for (const p of v.problems_vi || []) list.append(el("li", { text: p }));
  list.hidden = !(v.problems_vi || []).length;
}

// ------------------------------------------------------ capital allocation

function statTile(label, value, cls, sub) {
  return el("div", { cls: "stat" }, [
    el("div", { cls: "stat-k", text: label }),
    el("div", { cls: "stat-v " + (cls || ""), text: value }),
    sub ? el("div", { cls: "stat-s", text: sub }) : null,
  ]);
}

// renderCapital shows how one slot's size was arrived at: the buffer held
// back, what each slot gets, and when the account is read again. Every figure
// is the SERVER's — the page never re-derives a size, because the size the bot
// trades is the only one worth showing.
function renderCapital(s) {
  const pf = s.portfolio || {};
  const cfg = pf.default_pair_config || {};
  const on = Boolean(pf.auto_rebalance);
  setText("at-rebalance-state", on ? "TỰ ĐỘNG CÂN BẰNG: BẬT" : "TỰ ĐỘNG CÂN BẰNG: TẮT", "pill " + (on ? "ok" : ""));

  let next = "quy mô cố định theo giá trị người vận hành nhập";
  if (on && pf.next_rebalance_at_ms > 0) {
    const left = pf.next_rebalance_at_ms - (s.now_ms || Date.now());
    next = `lần cân bằng kế tiếp ${fmt.time(pf.next_rebalance_at_ms)}${left > 0 ? ` · còn ${fmt.duration(left / 1000)}` : " · đã tới hạn"}`;
  } else if (on) {
    next = "chưa cân bằng lần nào — lượt quét đầu sẽ đọc số dư và cấp quy mô";
  }
  setText("at-rebalance-next", next, "hint");

  const slots = pf.max_concurrent_positions || 0;
  const perNotional = s.capital_per_notional || 0;
  const tiles = $("at-capital-tiles");
  clear(tiles);
  tiles.append(
    statTile("Notional mỗi chân", `${fmt.quote(cfg.notional_quote, 2)} USDT`,
      "", slots ? `vốn mỗi slot ${fmt.quote(cfg.notional_quote * perNotional, 2)} USDT (×${perNotional})` : ""),
    statTile("Số slot", String(slots), "sm", `hạn mức vốn ${fmt.quote(pf.total_capital_cap_quote, 0)} USDT`),
    statTile("Đệm ký quỹ", fmt.pct((pf.margin_buffer_pct || 0) * 100, 0), "sm", "giữ lại, không chia cho slot nào"),
    statTile("Chu kỳ cân bằng", on ? `${pf.rebalance_interval_hours || 0} giờ` : "—", "sm",
      pf.last_rebalanced_at_ms > 0 ? `lần gần nhất ${fmt.time(pf.last_rebalanced_at_ms)}` : "chưa cân bằng lần nào"),
    statTile("Vốn đang dùng", `${fmt.quote(s.capital_deployed_quote, 2)} USDT`, "sm",
      `${s.open_positions || 0}/${slots} slot đang giữ`),
  );
  setText("at-capital-note", on
    ? "Quy mô mỗi slot = (tổng vốn hai ví × (1 − đệm) ÷ số slot) ÷ vốn trên mỗi notional, rồi lấy giá trị NHỎ NHẤT giữa nó và cái mỗi ví tự gánh nổi, hạn mức vốn và trần notional của portal. Hai ví spot và futures là hai đăng ký riêng trên testnet này, không chuyển tiền qua lại được. Cân bằng chỉ đổi quy mô các lệnh MỞ MỚI — vị thế đang mở giữ nguyên tới khi tự thoát."
    : "Tự động cân bằng đang TẮT: quy mô giữ đúng giá trị người vận hành nhập, lãi không được tái đầu tư.");
}

// -------------------------------------------------------- the positions matrix

function coin(symbol) {
  // Decorative only: the first letters of the symbol string, never read as an
  // asset (the venue's declared base asset is what every computation uses).
  return el("span", { cls: "at-coin", text: symbol.slice(0, 3), attrs: { "aria-hidden": "true" } });
}

// cell sets the label a narrow screen shows above the value, when the table's
// rows become cards.
function cell(td, label) {
  td.dataset.label = label;
  return td;
}

function symbolCell(symbol, sub) {
  return el("td", { cls: "at-sym-cell" }, [
    el("div", { cls: "at-sym" }, [coin(symbol), el("div", { cls: "at-sym-text" }, [el("strong", { text: symbol }), el("span", { cls: "mono", text: sub })])]),
  ]);
}

function two(a, b, label) {
  return cell(el("td", { cls: "r num" }, [el("span", { text: a }), el("span", { cls: "cell-sub", text: b })]), label);
}

// basisCell is the pair's thesis in one cell. A cash-and-carry position earns
// the basis it took on minus the basis it gives back, so the entry figure is
// what the venue paid it to hedge and the current one is what leaving would
// cost. Which way the number has moved since is what the take-profit and the
// widening stop each watch, so the sub-line names the direction in words as
// well as in sign — the two exits read the SAME figure in opposite directions.
function basisCell(pair, pos, sig) {
  const entry = isNum(pos.entry_basis_bps) ? pos.entry_basis_bps : sig.entry_basis_bps;
  const now = sig.basis_bps;
  const td = el("td", { cls: "r num" });
  if (!isNum(entry) || !isNum(now)) {
    td.append(el("span", { cls: "warn", text: "chưa đọc" }));
    td.title = "Chưa có giá giữa của cả hai chân trong lượt quét gần nhất — basis hiện tại không đo được.";
    return cell(td, "Basis: vào → hiện tại");
  }
  // widen is the server's own figure where it has one, so the cell and the
  // engine's stop can never disagree by a rounding.
  const widen = isNum(sig.basis_widen_bps) ? sig.basis_widen_bps : now - entry;
  const cap = (pair.config || {}).max_basis_widen_bps || 0;
  const blown = cap > 0 && widen > cap;
  td.append(el("span", { text: `${fmt.bps(entry, 1)} → ${fmt.bps(now, 1)}` }));
  td.append(el("span", {
    cls: "cell-sub " + (blown ? "neg" : widen <= 0 ? "pos" : ""),
    text: widen <= 0
      ? `${fmt.bps(widen, 1)} bps — đang CO, về phía chốt lời`
      : `${fmt.bps(widen, 1)} bps — đang GIÃN${cap > 0 ? `, cắt ở ${fmt.bps(cap, 0)}` : ""}`,
  }));
  td.title = [
    "Basis = (giá giữa perp − giá giữa spot) ÷ giá giữa spot, tính bằng bps.",
    `Lúc vào ${fmt.bps(entry, 1)} bps, hiện tại ${fmt.bps(now, 1)} bps, dịch ${fmt.bps(widen, 1)} bps.`,
    "Basis CO lại là phần lãi mà chốt lời hội tụ chờ; basis GIÃN ra là cái mà lệnh cắt lỗ canh.",
    blown ? `ĐÃ VƯỢT TRẦN ${fmt.bps(cap, 0)} bps — bot cắt ở lượt quét này.` : "",
  ].filter(Boolean).join("\n");
  return cell(td, "Basis: vào → hiện tại");
}

// takeProfitCell is how far a held pair is from leaving on its own: the running
// result against the take-profit target, and the settlements left of the
// amortization floor under it. Both are the ENGINE's own figures — the same
// ones its exit checks read — and both are estimates, so the cell carries the
// server's label rather than a word of its own.
function takeProfitCell(pair, pos, sig) {
  const cfg = pair.config || {};
  const target = isNum(sig.take_profit_target_pct) ? sig.take_profit_target_pct : 0;
  const got = sig.holding_return_on_capital_pct;
  const td = el("td", { cls: "r num" });
  if (target <= 0) {
    td.append(el("span", { cls: "hint", text: "tắt" }));
  } else if (!isNum(got)) {
    td.append(el("span", { cls: "warn", text: "chưa định giá" }));
    td.title = sig.holding_result_reason_vi || "";
  } else {
    const hit = got >= target;
    td.append(el("span", { cls: signCls(got), text: `${fmt.pct(got, 3, true)} / ${fmt.pct(target, 2, true)}` }));
    td.title = [
      sig.holding_result_label_vi || "",
      isNum(sig.holding_cash_result_quote) ? `Tạm tính ${usdt(sig.holding_cash_result_quote)} = funding ${usdt(sig.holding_funding_quote)} + trôi giá ${usdt(sig.holding_drift_quote)} − phí vào ${usdt(sig.holding_entry_fee_quote)} − phí đóng ước ${usdt(sig.holding_exit_cost_quote)}` : "",
      hit ? "ĐẠT NGƯỠNG — bot đóng ở lượt quét này" : "",
    ].filter(Boolean).join("\n");
  }
  const floor = cfg.min_hold_epochs || 0;
  const done = pos.settlements_since_open || 0;
  const sub = floor > 0 && done < floor
    ? `sàn giữ ${done}/${floor} mốc — khoá thoát funding`
    : floor > 0 ? `đã qua sàn giữ ${floor} mốc` : "không có sàn giữ";
  td.append(el("span", { cls: "cell-sub", text: sub }));
  return cell(td, "Chốt lời / Sàn giữ");
}

function renderPositions(s) {
  const body = $("at-pos-body");
  const positions = (s.positions || []).slice();
  setText("at-pos-count", `${positions.length} cặp${s.state !== "running" && positions.length ? " · bot không quét" : ""}`, "pill " + (positions.length ? "ok" : ""));
  $("at-pos-flat").hidden = positions.length > 0;
  $("at-pos-wrap").hidden = positions.length === 0;
  setText("at-pos-flat-text", s.state === "running"
    ? t("at_pos_flat_msg")
    : s.state === "emergency_halted" ? "Bot DỪNG BẢO VỆ và không giữ cặp nào mà nó nhận ra — đọc lý do phía trên"
      : "Bot đang tắt — không giữ cặp nào");
  clear(body);
  for (const pos of positions) {
    const pair = pairsOf(s).find((p) => p.symbol === pos.symbol) || {};
    const sig = pair.signal || {};
    const trade = tradeFor(pos.intent_id);
    const halted = pair.state === "emergency_halted";
    const residual = isNum(pair.residual_qty_coin) ? fmt.coin(pair.residual_qty_coin, true) : "—";
    const hedge = pair.hedge_status === "both_open" ? "HEDGED" : pair.hedge_status ? pair.hedge_status.toUpperCase() : "chưa đọc";
    const pairResult = trade && isNum(trade.cash_result_quote) ? trade.cash_result_quote : null;

    const close = el("button", { cls: "btn danger small", text: "[ĐÓNG CẶP NÀY]", attrs: { type: "button" } });
    close.disabled = !view.writable;
    close.title = `đóng hai chân của ${pos.intent_id} bằng MARKET qua portal; các cặp khác chạy tiếp`;
    close.addEventListener("click", () => view.handlers.closePair(pos.symbol));

    const row = el("tr", { cls: halted ? "at-row-halted" : "" }, [
      symbolCell(pos.symbol, (pos.adopted ? "tiếp nhận · " : "") + pos.intent_id),
      two(`${fmt.coin(pos.qty_coin)}`, `${fmt.quote(pos.notional_quote, 2)} USDT · vốn ${fmt.quote(pos.capital_quote, 2)}`, "Khối lượng · Notional"),
      cell(el("td", { cls: "r num", title: pos.marked_at_ms ? `giá giữa của lượt quét ${fmt.time(pos.marked_at_ms)}` : "" }, [
        el("span", { text: `${fmt.price(pos.spot_entry_avg_quote)} → ${fmt.price(sig.spot_mid_quote)}` }),
        el("span", { cls: "cell-sub", text: `${fmt.price(pos.perp_entry_avg_quote)} → ${fmt.price(sig.perp_mid_quote)}` }),
      ]), "Spot / perp: vào → giữa"),
      basisCell(pair, pos, sig),
      cell(el("td", { cls: "r num" }, [
        el("span", { cls: pair.hedge_status === "both_open" ? "" : "neg", text: residual }),
        el("span", { cls: "cell-sub " + (pair.hedge_status === "both_open" ? "pos" : "neg"), text: `${hedge}${isNum(pair.tolerance_qty_coin) && pair.tolerance_qty_coin > 0 ? " · ±" + fmt.coin(pair.tolerance_qty_coin) : ""}` }),
      ]), "Lệch delta"),
      cell(el("td", { cls: "r num" }, [
        el("span", { cls: signCls(pos.spot_leg_drift_quote), text: usdt(pos.spot_leg_drift_quote) }),
        el("span", { cls: "cell-sub " + signCls(pos.perp_leg_drift_quote), text: usdt(pos.perp_leg_drift_quote) }),
      ]), "Tạm tính spot / perp"),
      cell(el("td", { cls: "r num " + signCls(trade && trade.funding_rows_quote), text: trade ? usdt(trade.funding_rows_quote, 6) : "—", title: trade && trade.funding_incomplete_vi ? trade.funding_incomplete_vi : "" }), "Funding đã nhận"),
      cell(el("td", { cls: "r num" }, [
        el("span", { cls: "at-pair-result " + signCls(pairResult), text: pairResult === null ? "chưa định giá" : `${usdt(pairResult)} · ${resultWord(pairResult, true)}` }),
        el("span", { cls: "cell-sub", text: trade && isNum(trade.cash_result_on_capital_pct) ? `${fmt.pct(trade.cash_result_on_capital_pct, 3, true)} trên vốn` : "" }),
      ]), "Tạm tính cặp"),
      takeProfitCell(pair, pos, sig),
      cell(el("td", { cls: "r num", text: String(pos.settlements_since_open) }), "Mốc settle"),
      el("td", { cls: "at-action-cell" }, [close]),
    ]);
    row.title = pos.drift_label_vi || "";
    body.append(row);
  }
}

// ------------------------------------------------------------------ the radar

function check(key, label, sig) {
  const list = (sig && sig.entry_checks) || [];
  const found = list.filter((c) => (Array.isArray(key) ? key.includes(c.key) : c.key === key));
  const node = el("span", { cls: "at-chk", text: label });
  if (found.length === 0) {
    node.dataset.state = "none";
    node.title = "chưa đánh giá";
    return node;
  }
  const passed = found.every((c) => c.evaluated && c.passed);
  node.dataset.state = passed ? "ok" : "bad";
  node.title = found.map((c) => `${passed ? "ĐẠT" : c.evaluated ? "CHƯA" : "KHÔNG ĐO"} · ${c.name_vi}: ${c.detail_vi}`).join("\n");
  node.setAttribute("aria-label", `${label}: ${passed ? "đạt" : "chưa đạt"}`);
  return node;
}

function countdown(ms) {
  const node = el("span", { cls: "cell-sub" });
  view.countdowns.push({ node, ms });
  return node;
}

function renderRadar(s) {
  const body = $("at-radar-body");
  view.countdowns = [];
  clear(body);
  const pairs = pairsOf(s).slice().sort((a, b) => {
    const x = a.signal && isNum(a.signal.net_apr_pct) ? a.signal.net_apr_pct : -Infinity;
    const y = b.signal && isNum(b.signal.net_apr_pct) ? b.signal.net_apr_pct : -Infinity;
    return x !== y ? y - x : a.symbol.localeCompare(b.symbol);
  });
  if (pairs.length === 0) {
    emptyRow(body, 8, "portal không có cặp nào");
    return;
  }
  for (const p of pairs) {
    const sig = p.signal;
    const hours = sig && sig.interval_sec > 0 ? sig.interval_sec / 3600 : null;
    const aprCell = cell(el("td", { cls: "r num" }), "Net APR");
    if (sig && isNum(sig.net_apr_pct)) {
      aprCell.append(el("span", { cls: signCls(sig.net_apr_pct), text: fmt.pct(sig.net_apr_pct, 2, true) }),
        el("span", { cls: "cell-sub", text: `vốn ${fmt.pct(sig.net_apr_on_capital_pct, 2, true)} · ngưỡng ${p.config.min_net_apr_pct}%` }));
    } else {
      aprCell.append(el("span", { cls: "warn", text: sig ? "không tính" : "—" }));
      if (sig) aprCell.title = sig.net_apr_reason_vi || "";
    }

    const statusCell = cell(el("td", null, [el("span", { cls: "at-radar-pill", text: p.radar_vi, data: { radar: p.radar } })]), "Trạng thái");
    const why = p.state === "emergency_halted" ? p.halt_reason_vi
      : p.radar === "unproven" ? p.slot_reason_vi
        : p.radar === "paused" ? "tạm dừng vào lệnh mới — bật lại bằng công tắc"
          : p.radar === "off" ? (s.state === "running" ? "không nằm trong các cặp được chọn" : "bot đang tắt")
            : p.radar === "cooldown" ? `hồi phục tới ${fmt.time(p.cooldown_until_ms)}`
              : p.skip_vi || (sig ? sig.verdict_vi : "");
    if (why) {
      statusCell.title = why;
      statusCell.append(el("span", { cls: "cell-sub clip", text: why }));
    }

    const actions = el("div", { cls: "at-row-actions" });
    if (p.state === "emergency_halted") {
      const ack = el("button", { cls: "btn secondary small", text: "[XÁC NHẬN]", attrs: { type: "button" } });
      ack.disabled = !view.writable;
      ack.title = `xác nhận DỪNG BẢO VỆ #${p.halt_seq}; cặp chuyển sang TẠM DỪNG`;
      ack.addEventListener("click", () => view.handlers.ackPair(p));
      actions.append(ack);
    } else {
      const on = p.in_run && !p.paused;
      const sw = el("button", { cls: "at-switch row", attrs: { type: "button", role: "switch", "aria-checked": on ? "true" : "false", "aria-label": `cho phép bot vào lệnh ${p.symbol}` } }, [
        el("span", { cls: "at-switch-track", attrs: { "aria-hidden": "true" } }, [el("span", { cls: "at-switch-thumb" })]),
        el("span", { text: on ? "BẬT" : "TẮT" }),
      ]);
      sw.disabled = !view.writable || s.state !== "running";
      sw.title = s.state !== "running" ? "bot đang tắt — chọn cặp trong form rồi BẬT" : on ? "tạm dừng vào lệnh mới trên cặp này (vị thế đang giữ vẫn được quản lý thoát)" : "cho bot vào lệnh cặp này lại (có hộp xác nhận)";
      sw.addEventListener("click", () => view.handlers.togglePair(p));
      actions.append(sw);
    }

    body.append(el("tr", { cls: p.state === "emergency_halted" ? "at-row-halted" : "" }, [
      cell(el("td", { cls: "r at-rank-cell" + (p.rank > 0 ? "" : " no-rank") }, [el("span", { cls: "at-rank " + (p.rank === 1 ? "top" : ""), text: p.rank > 0 ? String(p.rank) : "—" })]), "Hạng"),
      symbolCell(p.symbol, p.overridden ? `${fmt.quote(p.config.notional_quote, 2)} USDT · cấu hình riêng` : `${fmt.quote(p.config.notional_quote, 2)} USDT/chân`),
      cell(el("td", { cls: "r num" }, [
        el("span", { cls: signCls(sig && sig.forecast_rate_per_interval_bps), text: sig && isNum(sig.forecast_rate_per_interval_bps) ? `${fmt.bps(sig.forecast_rate_per_interval_bps)} bps` : "—" }),
        el("span", { cls: "cell-sub", text: sig && isNum(sig.forecast_rate_per_8h_bps) ? `${fmt.bps(sig.forecast_rate_per_8h_bps)} bps/8h · TB ${fmt.bps(sig.trailing_mean_rate_per_8h_bps)}` : "" }),
      ]), "Funding"),
      cell(el("td", { cls: "num" }, [el("span", { text: hours ? `${hours}h/kỳ` : "chưa đo" }), sig && sig.next_funding_time_ms ? countdown(sig.next_funding_time_ms) : null]), "Chu kỳ · settle"),
      aprCell,
      cell(el("td", null, [el("span", { cls: "at-checks" }, [
        check("forming_positive", "F", sig),
        check("entry_basis", "B", sig),
        check("size_fits", "Q", sig),
        check("net_apr", "A", sig),
        check("depth", "D", sig),
        check(["time_to_settle", "clock"], "T", sig),
      ])]), "Điều kiện"),
      statusCell,
      el("td", { cls: "at-action-cell" }, [actions]),
    ]));
  }
  tick();
  const scanned = s.last_scan_at_ms ? `quét lúc ${fmt.time(s.last_scan_at_ms)}` : "chưa quét";
  setText("at-age", s.next_scan_at_ms ? `${scanned} · lần sau ${fmt.time(s.next_scan_at_ms)}` : scanned, "hint");
  const any = pairs.find((p) => p.signal && (p.signal.applied_vi || []).length);
  setText("at-cost-basis", any
    ? [`Đã tính: ${(any.signal.applied_vi || []).join(" ")}`, `Chưa trừ: ${(any.signal.excluded_vi || []).join(" ")}`, any.signal.fee_source_vi ? `Nguồn phí: ${any.signal.fee_source_vi}` : ""].filter(Boolean).join(" · ")
    : "");
}

// ------------------------------------------------------------------ history

function holdTime(t, nowMs) {
  const end = t.closed_at_ms > 0 ? t.closed_at_ms : nowMs;
  return t.opened_at_ms > 0 ? fmt.duration((end - t.opened_at_ms) / 1000) : "—";
}

function statusPill(t) {
  if (t.status === "closed") {
    const reason = t.close_reason_vi || t.note_vi || "Lịch sử cũ (chưa ghi vết)";
    return el("div", { cls: "at-status-cell", title: reason }, [
      el("span", { cls: "pill ok", text: "ĐÃ ĐÓNG" }),
      el("span", { cls: "cell-sub clip", text: reason }),
    ]);
  }
  if (t.status === "unwound") return el("span", { cls: "pill", text: "ĐÃ GỠ", title: t.note_vi });
  if (t.status === "alarm") return el("span", { cls: "pill bad", text: "BÁO ĐỘNG", title: t.note_vi });
  if (t.status === "untracked") return el("span", { cls: "pill bad", text: "CHƯA RÕ", title: t.note_vi });
  return el("span", { cls: "pill", text: "ĐANG MỞ" });
}

function renderFilter(v) {
  const select = $("at-history-filter");
  const symbols = [...new Set((v.trades || []).map((t) => t.symbol).concat(pairsOf(view.status).map((p) => p.symbol)))].sort();
  const have = [...select.options].map((o) => o.value).filter(Boolean);
  if (have.join(",") === symbols.join(",")) return;
  const keep = select.value;
  clear(select);
  const all = el("option", { text: "Tất cả các cặp" });
  all.value = "";
  select.append(all);
  for (const s of symbols) select.append(el("option", { text: s }));
  select.value = symbols.includes(keep) ? keep : "";
}

function renderTrades(v) {
  renderFilter(v);
  const body = $("at-trades-body");
  const foot = $("at-trades-foot");
  const list = (v.trades || []).filter((t) => !view.filter || t.symbol === view.filter);
  clear(foot);
  if (list.length === 0) {
    emptyRow(body, 11, view.filter ? `Auto-Trader chưa có lệnh nào trên ${view.filter}` : "Auto-Trader chưa có lệnh nào");
    return;
  }
  clear(body);
  const sum = { n: 0, notional: 0, funding: 0, fees: 0, slippage: 0, drift: 0, result: 0, capital: 0, openN: 0, openResult: 0 };
  for (const t of list) {
    const closed = t.status === "closed";
    if (t.status === "unwound" || t.status === "alarm") {
      body.append(el("tr", { title: t.note_vi || "" }, [
        el("td", { text: fmt.time(t.opened_at_ms) }),
        el("td", { text: t.symbol }),
        el("td", { cls: "mono", text: t.intent_id }),
        el("td", { cls: "r num", text: fmt.quote(t.notional_quote, 2) }),
        el("td", { cls: "wrap", attrs: { colspan: "6" }, text: t.note_vi || "" }),
        el("td", null, [statusPill(t)]),
      ]));
      continue;
    }
    if (closed) {
      sum.n++;
      sum.notional += t.notional_quote;
      sum.funding += t.funding_received_quote;
      sum.fees += t.commission_quote;
      sum.slippage += t.slippage_quote;
      sum.drift += t.pair_price_drift_quote;
      sum.capital += t.capital_quote;
      if (isNum(t.cash_result_quote)) sum.result += t.cash_result_quote;
    } else if (isNum(t.cash_result_quote)) {
      sum.openN++;
      sum.openResult += t.cash_result_quote;
    }
    const resultCell = el("td", { cls: "r num" }, isNum(t.cash_result_quote)
      ? [el("span", { cls: signCls(t.cash_result_quote), text: fmt.quote(t.cash_result_quote, 4, true) }),
        el("span", { cls: "cell-sub", text: `${fmt.pct(t.cash_result_on_capital_pct, 3, true)}${closed ? "" : " · tạm tính"}` })]
      : [el("span", { text: "—" })]);
    const drift = closed ? t.pair_price_drift_quote : t.pair_drift_quote;
    body.append(el("tr", { title: t.note_vi || "" }, [
      el("td", { text: fmt.time(t.opened_at_ms) }),
      el("td", { text: t.symbol }),
      el("td", { cls: "mono at-intent", text: t.intent_id, title: t.intent_id }),
      el("td", { cls: "r num", text: fmt.quote(t.notional_quote, 2) }),
      el("td", { cls: "r num" }, [el("span", { text: fmt.price(t.spot_entry_avg_quote) }), el("span", { cls: "cell-sub", text: fmt.price(t.perp_entry_avg_quote) })]),
      el("td", { text: holdTime(t, view.pnl.read_at_ms) }),
      el("td", { cls: "r num " + signCls(closed ? t.funding_received_quote : t.funding_rows_quote), text: fmt.quote(closed ? t.funding_received_quote : t.funding_rows_quote, 6, true) }),
      el("td", { cls: "r num", text: closed ? fmt.quote(-t.commission_quote, 6, true) : "khi đóng" }),
      el("td", { cls: "r num " + signCls(drift), text: isNum(drift) ? fmt.quote(drift, 6, true) : "—", title: closed ? `trong đó trượt giá so với mid: ${fmt.quote(t.slippage_quote, 6)} (đã nằm trong trôi giá theo giá khớp)` : "theo giá giữa, chưa trừ phí và trượt khi đóng" }),
      resultCell,
      el("td", null, [statusPill(t)]),
    ]));
  }
  const onCapital = sum.capital > 0 ? ` · ${fmt.pct((sum.result / sum.capital) * 100, 3, true)} trên tổng vốn các lượt` : "";
  foot.append(el("tr", null, [
    el("td", { text: "TỔNG ĐÃ CHỐT" }),
    el("td", { text: `${sum.n} cặp` }),
    el("td", { text: "" }),
    el("td", { cls: "r num", text: fmt.quote(sum.notional, 2) }),
    el("td", { text: "" }),
    el("td", { text: "" }),
    el("td", { cls: "r num " + signCls(sum.funding), text: fmt.quote(sum.funding, 6, true) }),
    el("td", { cls: "r num", text: fmt.quote(-sum.fees, 6, true) }),
    el("td", { cls: "r num " + signCls(sum.drift), text: fmt.quote(sum.drift, 6, true), title: `trong đó trượt giá ${fmt.quote(sum.slippage, 6)}` }),
    el("td", { cls: "r num " + signCls(sum.result), text: `${fmt.quote(sum.result, 4, true)} USDT${onCapital}` }),
    el("td", { text: resultWord(sum.result) }),
  ]));
  if (sum.openN > 0) {
    foot.append(el("tr", null, [
      el("td", { text: "TẠM TÍNH ĐANG MỞ" }),
      el("td", { text: `${sum.openN} cặp` }),
      ...Array.from({ length: 7 }, () => el("td", { text: "" })),
      el("td", { cls: "r num " + signCls(sum.openResult), text: `${fmt.quote(sum.openResult, 4, true)} USDT` }),
      el("td", { text: "chưa chốt" }),
    ]));
  }
}

// ------------------------------------------------------------------ console

function renderLog(s) {
  const list = $("at-log");
  clear(list);
  const lines = (s.log || []).slice(0, LOG_LINES);
  if (lines.length === 0) {
    list.append(el("li", { cls: "at-empty", text: "chưa có sự kiện" }));
    return;
  }
  for (const line of lines) {
    list.append(el("li", null, [
      el("span", { text: fmt.time(line.at_ms) }),
      el("span", { cls: "at-kind", text: `[${line.kind}${line.symbol ? " " + line.symbol : ""}]`, data: { kind: line.kind } }),
      el("span", { cls: "at-msg", text: line.message_vi }),
    ]));
  }
}

// --------------------------------------------------------------------- API

// tick runs every second: the settlement countdowns move between polls.
export function tick() {
  const now = Date.now();
  for (const c of view.countdowns) {
    const left = (c.ms - now) / 1000;
    setText(c.node, left > 0 ? `settle sau ${fmt.duration(left)}` : "đang settle");
  }
}

export const autotradeView = {
  // init takes the write handlers execution.js owns: closePair(symbol),
  // togglePair(pair), ackPair(pair), ackAll(halts).
  init(handlers) {
    view.handlers = handlers;
    $("at-history-filter").addEventListener("change", (ev) => {
      view.filter = ev.target.value;
      if (view.pnl) renderTrades(view.pnl);
    });
    shell.onTab("autotrade", {
      enter: () => {
        if (view.pnl) renderCharts(view.pnl);
        fitCharts();
      },
    });
    let resizeTimer = null;
    window.addEventListener("resize", () => {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(fitCharts, 150);
    });
  },

  // setWritable enables the row buttons; the server still refuses a write
  // without its own header.
  setWritable(on) {
    if (view.writable === on) return;
    view.writable = on;
    if (view.status) {
      renderStateTile(view.status);
      renderPositions(view.status);
      renderRadar(view.status);
    }
  },

  renderStatus(s) {
    view.status = s;
    renderStateTile(s);
    renderCapitalTile(s);
    renderCapital(s);
    renderPositions(s);
    renderRadar(s);
    renderLog(s);
  },

  renderPnL(v) {
    view.pnl = v;
    setText("at-pnl-age", `đọc ${fmt.time(v.read_at_ms)}`, "hint");
    renderResultTiles(v, view.status);
    renderCharts(v);
    renderLabels(v);
    renderTrades(v);
    if (view.status) renderPositions(view.status);
  },

  pnlFailed(message) {
    setText("at-pnl-age", "CŨ — " + message, "hint warn");
  },
};

onLanguageChange(() => {
  if (view.status) autotrade.renderStatus(view.status);
  if (view.pnl) autotrade.renderPnL(view.pnl);
});
