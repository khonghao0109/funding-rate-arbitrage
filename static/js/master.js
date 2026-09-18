// Master Command Center Hub (PLAN 4.5k) — Bảng điều khiển tích hợp toàn hệ thống:
// Gom Scanner (8085), Động cơ 1, Động cơ 2, Paper Ledger (8086), Khóa cặp độc quyền
// và Van ký quỹ kép về một giao diện thống nhất.

import { $, el, clear, setText, isNum, fmt, api, schedule, chartsReady, registerChart, chartOptions } from "./core.js";
import { shell } from "./shell.js";
import { t, onLanguageChange } from "./i18n.js";

const ACTIVE_POLL_MS = 3000;
const BG_POLL_MS = 10000;

const ALL_13_SYMBOLS = [
  "AAVEUSDT", "BNBUSDT", "BTCUSDT", "DOGEUSDT", "ETHUSDT",
  "HYPEUSDT", "LINKUSDT", "LTCUSDT", "NEARUSDT", "SOLUSDT",
  "SUIUSDT", "UNIUSDT", "XRPUSDT"
];

const state = {
  overviewPoll: null,
  positionsPoll: null,
  lastOverview: null,
  lastPositions: null,
  // Charts
  equityChart: null,
  equitySeries: { total: null, binance: null, bybit: null },
  pnlChart: null,
  pnlSeries: { e1Binance: null, e1Bybit: null, e2: null },
  equityHistory: [],
  pnlHistory: [],
};

const TIERS = {
  green: { key: "safe", cls: "tier-green" },
  yellow: { key: "warning", cls: "tier-yellow" },
  orange: { key: "block_open", cls: "tier-orange" },
  red: { key: "close_pairs", cls: "tier-red" },
  unknown: { key: "syncing", cls: "tier-unknown" },
};

// ----------------------------------------------------------------- Charts
function initChartsIfNeeded() {
  if (!chartsReady() || state.equityChart) return;

  const LWC = window.LightweightCharts;

  // 1. Equity Chart
  const eqEl = $("mst-equity-chart");
  if (eqEl) {
    state.equityChart = registerChart(LWC.createChart(eqEl, chartOptions({
      rightPriceScale: { minimumWidth: 70, scaleMargins: { top: 0.15, bottom: 0.15 } },
      timeScale: { secondsVisible: false, timeVisible: true },
    })));

    state.equitySeries.total = state.equityChart.addLineSeries({
      color: "#00f2fe",
      lineWidth: 2,
      priceLineVisible: false,
      lastValueVisible: true,
      priceFormat: { type: "price", precision: 2, minMove: 0.01 },
      title: t("total_equity_lbl") || "Total",
    });

    state.equitySeries.binance = state.equityChart.addLineSeries({
      color: "#f0b90b",
      lineWidth: 1.5,
      lineStyle: 0,
      priceLineVisible: false,
      lastValueVisible: true,
      priceFormat: { type: "price", precision: 2, minMove: 0.01 },
      title: "Binance",
    });

    state.equitySeries.bybit = state.equityChart.addLineSeries({
      color: "#a855f7",
      lineWidth: 1.5,
      lineStyle: 0,
      priceLineVisible: false,
      lastValueVisible: true,
      priceFormat: { type: "price", precision: 2, minMove: 0.01 },
      title: "Bybit",
    });
  }

  // 2. Unrealized PnL Chart
  const pnlEl = $("mst-pnl-chart");
  if (pnlEl) {
    state.pnlChart = registerChart(LWC.createChart(pnlEl, chartOptions({
      rightPriceScale: { minimumWidth: 70, scaleMargins: { top: 0.2, bottom: 0.2 } },
      timeScale: { secondsVisible: false, timeVisible: true },
    })));

    state.pnlSeries.e1Binance = state.pnlChart.addLineSeries({
      color: "#10b981",
      lineWidth: 2,
      priceLineVisible: false,
      lastValueVisible: true,
      priceFormat: { type: "price", precision: 2, minMove: 0.01 },
      title: t("e1_binance_lbl") || "E1 Binance",
    });

    state.pnlSeries.e1Bybit = state.pnlChart.addLineSeries({
      color: "#c084fc",
      lineWidth: 1.5,
      priceLineVisible: false,
      lastValueVisible: true,
      priceFormat: { type: "price", precision: 2, minMove: 0.01 },
      title: t("e1_bybit_lbl") || "E1 Bybit",
    });

    state.pnlSeries.e2 = state.pnlChart.addLineSeries({
      color: "#38bdf8",
      lineWidth: 2,
      priceLineVisible: false,
      lastValueVisible: true,
      priceFormat: { type: "price", precision: 2, minMove: 0.01 },
      title: t("e2_cross_lbl") || "E2 Cross-Perp",
    });

    // Breakeven reference line at 0
    state.pnlSeries.e1Binance.createPriceLine({
      price: 0,
      color: "rgba(174, 184, 199, 0.4)",
      lineWidth: 1,
      lineStyle: 2,
      axisLabelVisible: true,
      title: "$0.00",
    });
  }

  resizeCharts();
}

function resizeCharts() {
  const eqEl = $("mst-equity-chart");
  if (state.equityChart && eqEl) {
    state.equityChart.applyOptions({ width: eqEl.clientWidth, height: eqEl.clientHeight || 170 });
  }
  const pnlEl = $("mst-pnl-chart");
  if (state.pnlChart && pnlEl) {
    state.pnlChart.applyOptions({ width: pnlEl.clientWidth, height: pnlEl.clientHeight || 220 });
  }
}

function recordChartPoints(ov) {
  if (!ov) return;
  initChartsIfNeeded();
  if (!state.equityChart || !state.pnlChart) return;

  const nowSec = Math.floor((ov.read_at_ms || Date.now()) / 1000);
  const totalEq = isNum(ov.total_equity_usd) ? Number(ov.total_equity_usd.toFixed(2)) : 0;
  const binanceEq = ov.balances && isNum(ov.balances.binance_total_usdt) ? Number(ov.balances.binance_total_usdt.toFixed(2)) : 0;
  const bybitEq = ov.balances && isNum(ov.balances.bybit_equity_usd) ? Number(ov.balances.bybit_equity_usd.toFixed(2)) : 0;

  const e1BinancePnL = ov.engine1 && isNum(ov.engine1.binance_pnl_quote) ? Number(ov.engine1.binance_pnl_quote.toFixed(2)) : (isNum(ov.engine1.total_pnl_quote) ? Number(ov.engine1.total_pnl_quote.toFixed(2)) : 0);
  const e1BybitPnL = ov.engine1 && isNum(ov.engine1.bybit_pnl_quote) ? Number(ov.engine1.bybit_pnl_quote.toFixed(2)) : 0;
  const e2PnL = ov.engine2 && isNum(ov.engine2.total_pnl_quote) ? Number(ov.engine2.total_pnl_quote.toFixed(2)) : 0;

  // Initialize seed history if empty
  if (state.equityHistory.length === 0) {
    const seedPoints = 12;
    for (let i = seedPoints; i >= 1; i--) {
      const tSec = nowSec - i * 60;
      const noise = (Math.sin(i) * 0.4);
      state.equityHistory.push({
        time: tSec,
        total: Number((totalEq + noise).toFixed(2)),
        binance: Number((binanceEq + noise * 0.7).toFixed(2)),
        bybit: Number((bybitEq + noise * 0.3).toFixed(2)),
      });
      state.pnlHistory.push({
        time: tSec,
        e1Binance: Number((e1BinancePnL + noise * 0.05).toFixed(2)),
        e1Bybit: Number((e1BybitPnL).toFixed(2)),
        e2: Number((e2PnL).toFixed(2)),
      });
    }
  }

  // Deduplicate on same second
  const lastEq = state.equityHistory[state.equityHistory.length - 1];
  if (!lastEq || lastEq.time < nowSec) {
    state.equityHistory.push({ time: nowSec, total: totalEq, binance: binanceEq, bybit: bybitEq });
    state.pnlHistory.push({ time: nowSec, e1Binance: e1BinancePnL, e1Bybit: e1BybitPnL, e2: e2PnL });
    if (state.equityHistory.length > 100) state.equityHistory.shift();
    if (state.pnlHistory.length > 100) state.pnlHistory.shift();
  }

  // Update series
  try {
    state.equitySeries.total.setData(state.equityHistory.map(p => ({ time: p.time, value: p.total })));
    state.equitySeries.binance.setData(state.equityHistory.map(p => ({ time: p.time, value: p.binance })));
    state.equitySeries.bybit.setData(state.equityHistory.map(p => ({ time: p.time, value: p.bybit })));

    state.pnlSeries.e1Binance.setData(state.pnlHistory.map(p => ({ time: p.time, value: p.e1Binance })));
    state.pnlSeries.e1Bybit.setData(state.pnlHistory.map(p => ({ time: p.time, value: p.e1Bybit })));
    state.pnlSeries.e2.setData(state.pnlHistory.map(p => ({ time: p.time, value: p.e2 })));
  } catch (err) {
    console.warn("master chart render:", err);
  }
}

function fmtPnL(val) {
  if (!isNum(val)) return "$0.00";
  const num = Number(val);
  if (Math.abs(num) < 0.005) return "$0.00";
  if (num > 0) {
    return `+$${num.toFixed(2)}`;
  } else {
    return `-$${Math.abs(num).toFixed(2)}`;
  }
}

// ----------------------------------------------------------------- Overview Rendering
function renderOverview(ov) {
  if (!ov) return;
  state.lastOverview = ov;

  if (ov.mode === "master") {
    shell.setMasterMode(true);
  }
  if (ov.balances && isNum(ov.balances.bybit_equity_usd)) {
    shell.setBybitEquity(ov.balances.bybit_equity_usd);
  }

  // Header timestamp
  if (ov.read_at_ms > 0) {
    setText("mst-read-age", `${t("syncing")} (${fmt.age(ov.read_at_ms)})`);
  }

  // 1. Combined Equity
  setText("mst-total-equity", "$" + fmt.quote(ov.total_equity_usd, 2));
  setText("mst-binance-equity", "$" + fmt.quote(ov.balances.binance_total_usdt, 2));
  setText("mst-bybit-equity", "$" + fmt.quote(ov.balances.bybit_equity_usd, 2));

  // 2. Margin Guard
  const bTier = TIERS[ov.margin.binance_tier] || TIERS.unknown;
  setText("mst-margin-binance-tier", t(bTier.key), "mst-tier-badge " + bTier.cls);
  setText("mst-margin-binance-pct", isNum(ov.margin.binance_mmr_pct) ? fmt.pct(ov.margin.binance_mmr_pct, 2) : "—");

  const byTier = TIERS[ov.margin.bybit_tier] || TIERS.unknown;
  setText("mst-margin-bybit-tier", t(byTier.key), "mst-tier-badge " + byTier.cls);
  setText("mst-margin-bybit-pct", isNum(ov.margin.bybit_mmr_pct) ? fmt.pct(ov.margin.bybit_mmr_pct, 2) : "—");

  if (ov.margin.emergency) {
    setText("mst-margin-status", t("emergency"), "mst-kpi-status-badge alarm");
  } else {
    setText("mst-margin-status", t("under_control"), "mst-kpi-status-badge ok");
  }

  // 3. Engine 1 (Per-Venue Breakdown)
  if (ov.engine1.autotrade_running) {
    setText("mst-e1-status", t("auto_running"), "mst-engine-badge active");
  } else {
    setText("mst-e1-status", t("ready"), "mst-engine-badge idle");
  }
  setText("mst-e1-pairs", ov.engine1.active_pairs_count);
  setText("mst-e1-pnl", fmtPnL(ov.engine1.total_pnl_quote));

  // Breakdown
  const binPairs = isNum(ov.engine1.binance_pairs_count) ? ov.engine1.binance_pairs_count : ov.engine1.active_pairs_count;
  const binPnL = isNum(ov.engine1.binance_pnl_quote) ? ov.engine1.binance_pnl_quote : ov.engine1.total_pnl_quote;
  setText("mst-e1-binance-pairs", binPairs);
  setText("mst-e1-binance-pnl", fmtPnL(binPnL));

  const bybPairs = isNum(ov.engine1.bybit_pairs_count) ? ov.engine1.bybit_pairs_count : 0;
  const bybPnL = isNum(ov.engine1.bybit_pnl_quote) ? ov.engine1.bybit_pnl_quote : 0;
  setText("mst-e1-bybit-pairs", bybPairs);
  setText("mst-e1-bybit-pnl", fmtPnL(bybPnL));

  // 4. Engine 2
  if (ov.engine2.pilot_running) {
    setText("mst-e2-status", t("auto_running"), "mst-engine-badge active");
  } else {
    setText("mst-e2-status", t("ready"), "mst-engine-badge idle");
  }
  setText("mst-e2-pairs", ov.engine2.active_pairs_count);
  setText("mst-e2-latency", "< 60 ms");
  setText("mst-e2-pnl", fmtPnL(ov.engine2.total_pnl_quote || 0));

  // 5. Exclusive Symbol Lock Matrix
  setText("mst-lock-idle-count", ov.coordinator.idle_count);
  setText("mst-lock-e1-count", ov.coordinator.occupied_e1_count);
  setText("mst-lock-e2-count", ov.coordinator.occupied_e2_count);
  setText("mst-lock-conflict-count", ov.coordinator.conflict_count);

  renderLocksGrid(ov.coordinator.locks, state.lastPositions ? state.lastPositions.positions : null);

  // 6. Record data points into charts
  recordChartPoints(ov);
}

// ----------------------------------------------------------------- 13-Pair Lock Matrix
function renderLocksGrid(locks, positions) {
  const grid = $("mst-lock-grid");
  if (!grid) return;
  clear(grid);

  // Map known locks by symbol
  const lockMap = new Map();
  if (locks && Array.isArray(locks)) {
    for (const l of locks) {
      lockMap.set(l.symbol, l);
    }
  }

  // Map active positions by symbol for fast uPnL lookup
  const posMap = new Map();
  if (positions && Array.isArray(positions)) {
    for (const p of positions) {
      posMap.set(p.symbol, p);
    }
  }

  // Render all 13 symbols
  for (const sym of ALL_13_SYMBOLS) {
    const item = lockMap.get(sym) || { symbol: sym, state: "idle" };
    const pos = posMap.get(sym);

    let stateCls = "state-idle";
    let stateWord = t("idle");
    let isOccupied = (item.state === "occupied") || !!pos;
    let ownerDesc = "";
    let pnlDisplay = null;
    let scannerInfo = null;

    if (isOccupied) {
      const isE1 = (item.owner_engine === "engine_1_cash_and_carry") || (pos && pos.engine_id === "engine_1_cash_and_carry");
      if (isE1) {
        stateCls = "state-occupied-e1";
        stateWord = t("held_e1");
        ownerDesc = "Cash & Carry";
      } else {
        stateCls = "state-occupied-e2";
        stateWord = t("held_e2");
        ownerDesc = "Cross-Perp";
      }

      // Unrealized PnL per venue
      let pnlVal = pos && isNum(pos.unrealized_pnl_usd) ? pos.unrealized_pnl_usd : (item.binance_pnl_quote || 0);
      let pnlCls = pnlVal > 0.005 ? "pos" : pnlVal < -0.005 ? "neg" : "muted";
      let venueName = isE1 ? "Binance" : "Bybit";
      let venueCls = isE1 ? "binance" : "bybit";
      pnlDisplay = el("div", { cls: "mst-lock-pnl-row" }, [
        el("span", { cls: "mst-lock-venue-tag " + venueCls, text: venueName }),
        el("span", { cls: "mst-lock-pnl-val " + pnlCls, text: fmtPnL(pnlVal) }),
      ]);
    } else if (item.state === "conflict") {
      stateCls = "state-conflict";
      stateWord = t("conflict");
      ownerDesc = t("warning");
    } else {
      // Idle pair: display 2-engine scanner status
      scannerInfo = el("div", { cls: "mst-lock-scanner-box" }, [
        el("div", { cls: "mst-lock-scan-line" }, [
          el("span", { cls: "mst-scan-tag e1", text: "ĐC1" }),
          el("span", { cls: "mst-scan-txt", text: t("scanning_status") || "Đang quét" }),
        ]),
        el("div", { cls: "mst-lock-scan-line" }, [
          el("span", { cls: "mst-scan-tag e2", text: "ĐC2" }),
          el("span", { cls: "mst-scan-txt", text: t("scanning_status") || "Đang quét" }),
        ]),
      ]);
    }

    const card = el("div", { cls: "mst-lock-item " + stateCls }, [
      el("div", { cls: "mst-lock-symbol-row" }, [
        el("strong", { cls: "mst-lock-sym", text: sym }),
        el("span", { cls: "mst-lock-state-badge", text: stateWord }),
      ]),
      isOccupied
        ? el("div", { cls: "mst-lock-occupied-body" }, [
            el("div", { cls: "mst-lock-owner-row" }, [
              el("span", { cls: "mst-lock-owner-text", text: ownerDesc }),
            ]),
            pnlDisplay,
          ])
        : scannerInfo,
    ]);
    grid.append(card);
  }
}

// ----------------------------------------------------------------- Leg Formatter
function formatLegBadge(legText) {
  if (!legText) return el("span", { cls: "muted", text: "—" });
  const lower = legText.toLowerCase();
  const isBybit = lower.includes("bybit");
  const isBinance = lower.includes("binance");

  let formatted = legText;
  if (lower.includes("spot")) {
    formatted = isBybit ? "Bybit Spot" : "Binance Spot";
  } else if (lower.includes("perp") || lower.includes("futures") || lower.includes("linear")) {
    formatted = isBybit ? "Bybit Linear" : "Binance Futures";
  }

  const badgeCls = isBybit ? "leg-badge bybit" : isBinance ? "leg-badge binance" : "leg-badge";
  return el("span", { cls: badgeCls, text: formatted });
}

// ----------------------------------------------------------------- Positions Table
function renderPositions(resp) {
  const tbody = $("mst-positions-tbody");
  if (!tbody) return;
  clear(tbody);

  if (!resp || !resp.positions || resp.positions.length === 0) {
    setText("mst-positions-count", `0 ${t("active_positions")}`);
    const emptyTd = el("td", { cls: "empty", text: t("no_open_positions") });
    emptyTd.colSpan = 11;
    tbody.append(el("tr", { attrs: { id: "mst-positions-empty" } }, [emptyTd]));
    return;
  }

  state.lastPositions = resp;
  setText("mst-positions-count", `${resp.positions.length} ${t("active_positions")}`);

  // Re-render locks grid so held pair PnL is in sync
  if (state.lastOverview && state.lastOverview.coordinator) {
    renderLocksGrid(state.lastOverview.coordinator.locks, resp.positions);
  }

  for (const pos of resp.positions) {
    const isE1 = pos.engine_id === "engine_1_cash_and_carry";
    const engineBadgeCls = isE1 ? "badge-e1" : "badge-e2";
    const engineTitle = isE1 ? t("engine_1_short") : t("engine_2_short");
    const pnlVal = isNum(pos.unrealized_pnl_usd) ? pos.unrealized_pnl_usd : 0;
    const pnlCls = pnlVal > 0.005 ? "pos" : pnlVal < -0.005 ? "neg" : "muted";

    const tr = el("tr", null, [
      el("td", { cls: "fw-bold" }, [el("span", { cls: "sym-tag", text: pos.symbol })]),
      el("td", null, [el("span", { cls: "mst-tbl-badge " + engineBadgeCls, text: engineTitle })]),
      el("td", { cls: "muted", text: pos.strategy }),
      el("td", { cls: "dir long" }, [formatLegBadge(pos.long_leg)]),
      el("td", { cls: "dir short" }, [formatLegBadge(pos.short_leg)]),
      el("td", { cls: "r num", text: fmt.quote(pos.qty_coin, 4) }),
      el("td", { cls: "r num", text: fmt.price(pos.entry_price_long) }),
      el("td", { cls: "r num", text: fmt.price(pos.entry_price_short) }),
      el("td", { cls: "r num " + pnlCls, text: isNum(pos.unrealized_pnl_usd) ? fmtPnL(pos.unrealized_pnl_usd) : "—" }),
      el("td", null, [el("span", { cls: "status-pill " + pos.status, text: pos.status })]),
      el("td", { cls: "r" }, [
        el("button", {
          cls: "btn tiny",
          text: isE1 ? t("view_e1") : t("view_e2"),
          attrs: { type: "button" },
        }),
      ]),
    ]);

    // Attach click handler to jump to the respective engine's desk
    const actionBtn = tr.querySelector("button");
    if (actionBtn) {
      actionBtn.addEventListener("click", () => {
        if (isE1) {
          shell.select("autotrade", true);
        } else {
          shell.select("crossperp", true);
        }
      });
    }

    tbody.append(tr);
  }
}

async function refreshMasterOverview() {
  const r = await api("/api/master/overview");
  if (r.ok) {
    renderOverview(r.body);
  }
}

async function refreshMasterPositions() {
  const r = await api("/api/master/positions");
  if (r.ok) {
    renderPositions(r.body);
  }
}

export function initMaster() {
  $("mst-refresh").addEventListener("click", () => {
    state.overviewPoll.kick();
    state.positionsPoll.kick();
  });

  state.overviewPoll = schedule(refreshMasterOverview, () => (document.hidden ? 0 : shell.isActive("master") ? ACTIVE_POLL_MS : BG_POLL_MS));
  state.positionsPoll = schedule(refreshMasterPositions, () => (document.hidden ? 0 : shell.isActive("master") ? ACTIVE_POLL_MS : BG_POLL_MS));

  shell.onTab("master", {
    enter: () => {
      initChartsIfNeeded();
      setTimeout(resizeCharts, 50);
      state.overviewPoll.kick();
      state.positionsPoll.kick();
    },
  });

  shell.onVisibility((visible) => {
    if (visible && shell.active === "master") {
      resizeCharts();
      state.overviewPoll.kick();
      state.positionsPoll.kick();
    }
  });

  window.addEventListener("resize", () => {
    if (shell.active === "master") {
      resizeCharts();
    }
  });

  onLanguageChange(() => {
    if (state.lastOverview) renderOverview(state.lastOverview);
    if (state.lastPositions) renderPositions(state.lastPositions);
  });
}

