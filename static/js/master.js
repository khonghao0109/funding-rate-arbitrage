// Master Command Center Hub (PLAN 4.5k) — Bảng điều khiển tích hợp toàn hệ thống:
// Gom Scanner (8085), Động cơ 1, Động cơ 2, Paper Ledger (8086), Khóa cặp độc quyền
// và Van ký quỹ kép về một giao diện thống nhất.

import { $, el, clear, setText, isNum, fmt, api, schedule } from "./core.js";
import { shell } from "./shell.js";
import { t, onLanguageChange } from "./i18n.js";

const ACTIVE_POLL_MS = 3000;
const BG_POLL_MS = 10000;

const state = {
  overviewPoll: null,
  positionsPoll: null,
  lastOverview: null,
  lastPositions: null,
};

const TIERS = {
  green: { key: "safe", cls: "tier-green" },
  yellow: { key: "warning", cls: "tier-yellow" },
  orange: { key: "block_open", cls: "tier-orange" },
  red: { key: "close_pairs", cls: "tier-red" },
  unknown: { key: "syncing", cls: "tier-unknown" },
};

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

  // Combined Equity
  setText("mst-total-equity", "$" + fmt.quote(ov.total_equity_usd, 2));
  setText("mst-binance-equity", "$" + fmt.quote(ov.balances.binance_total_usdt, 2));
  setText("mst-bybit-equity", "$" + fmt.quote(ov.balances.bybit_equity_usd, 2));

  // Margin Guard
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

  // Engine 1
  if (ov.engine1.autotrade_running) {
    setText("mst-e1-status", t("auto_running"), "mst-engine-badge active");
  } else {
    setText("mst-e1-status", t("ready"), "mst-engine-badge idle");
  }
  setText("mst-e1-pairs", ov.engine1.active_pairs_count);
  setText("mst-e1-pnl", "$" + fmt.quote(ov.engine1.total_pnl_quote, 2, true));

  // Engine 2
  if (ov.engine2.pilot_running) {
    setText("mst-e2-status", t("auto_running"), "mst-engine-badge active");
  } else {
    setText("mst-e2-status", t("ready"), "mst-engine-badge idle");
  }
  setText("mst-e2-pairs", ov.engine2.active_pairs_count);

  // Exclusive Symbol Lock Matrix
  setText("mst-lock-idle-count", ov.coordinator.idle_count);
  setText("mst-lock-e1-count", ov.coordinator.occupied_e1_count);
  setText("mst-lock-e2-count", ov.coordinator.occupied_e2_count);
  setText("mst-lock-conflict-count", ov.coordinator.conflict_count);

  renderLocksGrid(ov.coordinator.locks);
}

function renderLocksGrid(locks) {
  const grid = $("mst-lock-grid");
  if (!grid) return;
  clear(grid);

  if (!locks || locks.length === 0) {
    grid.append(el("div", { cls: "empty-state", text: t("syncing") }));
    return;
  }

  for (const item of locks) {
    let stateCls = "state-idle";
    let stateWord = t("idle");
    let ownerDesc = "Multi-Engine";

    if (item.state === "occupied") {
      if (item.owner_engine === "engine_1_cash_and_carry") {
        stateCls = "state-occupied-e1";
        stateWord = t("held_e1");
        ownerDesc = "Cash & Carry";
      } else {
        stateCls = "state-occupied-e2";
        stateWord = t("held_e2");
        ownerDesc = "Cross-Perp";
      }
    } else if (item.state === "conflict") {
      stateCls = "state-conflict";
      stateWord = t("conflict");
      ownerDesc = t("warning");
    }

    const card = el("div", { cls: "mst-lock-item " + stateCls }, [
      el("div", { cls: "mst-lock-symbol-row" }, [
        el("strong", { cls: "mst-lock-sym", text: item.symbol }),
        el("span", { cls: "mst-lock-state-badge", text: stateWord }),
      ]),
      el("div", { cls: "mst-lock-owner-row" }, [
        el("span", { cls: "mst-lock-owner-text", text: ownerDesc }),
      ]),
    ]);
    grid.append(card);
  }
}

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

  setText("mst-positions-count", `${resp.positions.length} ${t("active_positions")}`);

  for (const pos of resp.positions) {
    const isE1 = pos.engine_id === "engine_1_cash_and_carry";
    const engineBadgeCls = isE1 ? "badge-e1" : "badge-e2";
    const engineTitle = isE1 ? t("engine_1_short") : t("engine_2_short");
    const pnlVal = isNum(pos.unrealized_pnl_usd) ? pos.unrealized_pnl_usd : 0;
    const pnlCls = pnlVal > 0 ? "pos" : pnlVal < 0 ? "neg" : "muted";

    const tr = el("tr", null, [
      el("td", { cls: "fw-bold" }, [el("span", { cls: "sym-tag", text: pos.symbol })]),
      el("td", null, [el("span", { cls: "mst-tbl-badge " + engineBadgeCls, text: engineTitle })]),
      el("td", { cls: "muted", text: pos.strategy }),
      el("td", { cls: "dir long", text: pos.long_leg }),
      el("td", { cls: "dir short", text: pos.short_leg }),
      el("td", { cls: "r num", text: fmt.quote(pos.qty_coin, 4) }),
      el("td", { cls: "r num", text: fmt.price(pos.entry_price_long) }),
      el("td", { cls: "r num", text: fmt.price(pos.entry_price_short) }),
      el("td", { cls: "r num " + pnlCls, text: isNum(pos.unrealized_pnl_usd) ? "$" + fmt.quote(pos.unrealized_pnl_usd, 2, true) : "—" }),
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
  // Navigation shortcuts
  $("mst-nav-scanner").addEventListener("click", () => shell.select("scanner", true));
  $("mst-nav-radar").addEventListener("click", () => shell.select("radar", true));
  $("mst-nav-e1").addEventListener("click", () => shell.select("autotrade", true));
  $("mst-nav-e2").addEventListener("click", () => shell.select("crossperp", true));
  $("mst-nav-paper").addEventListener("click", () => shell.select("paper", true));

  $("mst-refresh").addEventListener("click", () => {
    state.overviewPoll.kick();
    state.positionsPoll.kick();
  });

  state.overviewPoll = schedule(refreshMasterOverview, () => (document.hidden ? 0 : shell.isActive("master") ? ACTIVE_POLL_MS : BG_POLL_MS));
  state.positionsPoll = schedule(refreshMasterPositions, () => (document.hidden ? 0 : shell.isActive("master") ? ACTIVE_POLL_MS : BG_POLL_MS));

  shell.onTab("master", {
    enter: () => {
      state.overviewPoll.kick();
      state.positionsPoll.kick();
    },
  });

  shell.onVisibility((visible) => {
    if (visible && shell.active === "master") {
      state.overviewPoll.kick();
      state.positionsPoll.kick();
    }
  });

  onLanguageChange(() => {
    if (state.lastOverview) renderOverview(state.lastOverview);
    if (state.lastPositions) renderPositions(state.lastPositions);
  });
}

