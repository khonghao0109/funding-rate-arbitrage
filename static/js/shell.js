// The page shell: tab routing, and the header every tab shares — connection
// chips, balances, and the worst hedge state across every tradable symbol,
// which stays visible (and blinks) on every tab because a naked leg is not a
// thing to find only when looking at the Execution tab.

import { $, setText, fmt, isNum } from "./core.js";

const TABS = ["master", "scanner", "radar", "crossperp", "autotrade", "manual", "paper", "backtest", "crowding"];
const TAB_ALIASES = {
  execution: "manual",
};
const TAB_KEY = "portal.tab";
const handlers = new Map();
const BALANCE_STALE_MS = 30000;
// Other tabs read positions every 15 s; three missed reads is stale.
const HEDGE_STALE_MS = 45000;
const balances = { readAtMs: 0, failedVI: "" };
// unifiedWallet is set from /api/status: spot and futures share ONE wallet.
let unifiedWallet = false;
let bybitEquityUSD = null;
let isMasterMode = false;
let lastBinanceSpotUSDT = 0;
let lastBinanceFuturesUSDT = 0;

// The age under the balances is redrawn every second from the read stamp, so a
// figure that stopped refreshing says so instead of reading "vừa đọc" forever.
function renderBalanceAge() {
  const node = $("bal-spot-age");
  if (!node) return;
  const row = node.closest(".balances");
  const age = balances.readAtMs ? Date.now() - balances.readAtMs : Infinity;
  const stale = Boolean(balances.failedVI) || age > BALANCE_STALE_MS;
  row.classList.toggle("stale", stale && balances.readAtMs > 0);
  if (!balances.readAtMs) return setText(node, balances.failedVI ? "không đọc được" : "");
  const text = balances.failedVI ? `CŨ · ${fmt.age(balances.readAtMs)} · lần đọc mới lỗi` : stale ? `CŨ · ${fmt.age(balances.readAtMs)}` : fmt.age(balances.readAtMs);
  setText(node, text);
  node.title = balances.failedVI || "";
}
const visibilityHandlers = [];
let active = null;

function remembered() {
  try {
    const r = window.localStorage.getItem(TAB_KEY);
    return TAB_ALIASES[r] || r;
  } catch (_) {
    return null;
  }
}

function remember(name) {
  try {
    window.localStorage.setItem(TAB_KEY, name);
  } catch (_) {
    /* per-viewer convenience only */
  }
}

function moveInk() {
  const ink = $("tab-ink");
  const button = active && $("tab-" + active);
  if (!ink || !button) return;
  ink.style.transform = `translateY(${button.offsetTop}px)`;
  ink.style.height = `${button.offsetHeight}px`;
}


function select(rawName, focus) {
  const name = TAB_ALIASES[rawName] || rawName;
  if (!TABS.includes(name) || name === active) return;
  const previous = active;
  active = name;
  for (const tab of TABS) {
    const button = $("tab-" + tab);
    const panel = $("panel-" + tab);
    const on = tab === name;
    if (button) {
      button.setAttribute("aria-selected", on ? "true" : "false");
      button.tabIndex = on ? 0 : -1;
    }
    if (panel) {
      panel.hidden = !on;
    }
  }
  if (focus) {
    const btn = $("tab-" + name);
    if (btn) btn.focus();
  }
  moveInk();
  remember(name);
  if (location.hash !== "#" + name) history.replaceState(null, "", "#" + name);
  if (previous) for (const h of handlers.get(previous) || []) if (h.leave) h.leave();
  for (const h of handlers.get(name) || []) if (h.enter) h.enter();
}

export const shell = {
  get active() {
    return active;
  },

  select(name, focus) {
    select(name, focus);
  },

  // isActive is true when the tab is selected AND the page is visible.
  isActive(name) {
    const resolved = TAB_ALIASES[name] || name;
    return active === resolved && !document.hidden;
  },

  onTab(name, h) {
    const resolved = TAB_ALIASES[name] || name;
    if (!handlers.has(resolved)) handlers.set(resolved, []);
    handlers.get(resolved).push(h);
  },

  onVisibility(fn) {
    visibilityHandlers.push(fn);
  },

  init() {
    for (const tab of TABS) {
      const btn = $("tab-" + tab);
      if (btn) btn.addEventListener("click", () => select(tab, false));
    }
    $("tabs").addEventListener("keydown", (ev) => {
      const index = TABS.indexOf(active);
      let next = null;
      if (ev.key === "ArrowRight") next = TABS[(index + 1) % TABS.length];
      if (ev.key === "ArrowLeft") next = TABS[(index - 1 + TABS.length) % TABS.length];
      if (ev.key === "Home") next = TABS[0];
      if (ev.key === "End") next = TABS[TABS.length - 1];
      if (next) {
        ev.preventDefault();
        select(next, true);
      }
    });
    // Only a tab name is a route; any other fragment (the skip link's #main)
    // is left alone rather than bounced to the first tab.
    window.addEventListener("hashchange", () => select(location.hash.slice(1), false));
    setInterval(renderBalanceAge, 1000);
    window.addEventListener("resize", moveInk);
    document.addEventListener("visibilitychange", () => {
      for (const fn of visibilityHandlers) fn(!document.hidden);
    });
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(moveInk);
  },

  // start selects the first tab once every module has registered its handlers.
  start() {
    const fromHash = location.hash.slice(1);
    const resolvedHash = TAB_ALIASES[fromHash] || fromHash;
    const rem = remembered();
    select(TABS.includes(resolvedHash) ? resolvedHash : TABS.includes(rem) ? rem : "master", false);
  },

  setChip(id, state, value, title) {
    const chip = $(id);
    if (!chip) return;
    if (chip.dataset.state !== state) chip.dataset.state = state;
    setText(id + "-v", value);
    if (title !== undefined) chip.title = title;
  },

  renderStatus(s) {
    setText("footer-hosts", (s.testnet_hosts || []).join(", "));
    // The venue both legs trade on (-broker). An older portal sends no venue
    // field; the page then keeps the Binance labels it was written with.
    // One wallet (Bybit UTA): the spot and futures tiles are two views of the
    // SAME USDT, so the total must not add them.
    unifiedWallet = s.unified_wallet === true;
    if (unifiedWallet) {
      setText("bal-spot-usdt-k", "USDT · ví hợp nhất (góc spot)");
      setText("bal-futures-usdt-k", "USDT · ví hợp nhất (góc futures)");
      setText("bal-total-usdt-k", "USDT · MỘT ví hợp nhất");
    }
    if (s.venue === "bybit") {
      const label = s.venue_label_vi || "Bybit";
      setText("venue-badge", `[${label.toUpperCase()}]`);
      setText("conn-spot-name", "Bybit Spot");
      setText("conn-futures-name", s.unified_wallet ? "Bybit USDT Perp · CHUNG VÍ UTA" : "Bybit USDT Perp");
      setText("confirm-venue-line", `LỆNH THẬT trên ${label.toUpperCase()} — không tiền thật`);
      for (const id of ["chip-spot", "chip-futures"]) {
        const chip = $(id);
        if (chip) chip.title = `API ${label} ${id === "chip-spot" ? "Spot" : "USDT Perp"}`;
      }
    }
    for (const [id, m] of [["chip-spot", s.spot], ["chip-futures", s.futures]]) {
      if (!m.configured) {
        this.setChip(id, "bad", "CHƯA CẤU HÌNH", m.credential_vi || "thiếu credential");
      }
    }
  },

  renderPortalDown(message) {
    for (const id of ["chip-spot", "chip-futures", "chip-skew"]) this.setChip(id, "bad", "portal?", message);
  },

  setMasterMode(active) {
    isMasterMode = Boolean(active);
    if (isMasterMode) {
      setText("venue-badge", "[MASTER · BINANCE + BYBIT TESTNET]");
      setText("brand-desc", "Hệ Thống Hợp Nhất · Động cơ 1 + Động cơ 2 + Scanner + Paper Ledger · Quản trị vốn liên sàn");
      this.updateHeaderLabels();
    }
  },

  setBybitEquity(usd) {
    if (isNum(usd)) {
      bybitEquityUSD = usd;
      this.setChip("chip-bybit", "ok", "OK", "API Bybit Linear · UTA Testnet");
      this.updateHeaderLabels();
      setText("bal-bybit-usdt", fmt.quote(bybitEquityUSD, 2));
      const total = lastBinanceSpotUSDT + lastBinanceFuturesUSDT + bybitEquityUSD;
      setText("bal-total-usdt", fmt.quote(total, 2));
    }
  },

  updateHeaderLabels() {
    if (bybitEquityUSD !== null || isMasterMode) {
      setText("bal-spot-usdt-k", "Binance Spot · USDT");
      setText("bal-futures-usdt-k", "Binance Futures · USDT");
      setText("bal-bybit-usdt-k", "Bybit Linear · USD");
      setText("bal-bybit-sub", "Hợp nhất UTA");
      setText("bal-total-usdt-k", "TỔNG VỐN LIÊN SÀN (USD)");
      setText("bal-total-sub", "Binance + Bybit");
    }
  },

  renderAccount(a, baseAsset) {
    const markets = [["chip-spot", a.spot], ["chip-futures", a.futures]];
    for (const [id, m] of markets) {
      if (!m.configured) {
        this.setChip(id, "bad", "CHƯA CẤU HÌNH", m.error_vi || "");
      } else if (m.error_vi || m.banned) {
        this.setChip(id, "bad", m.banned ? "IP BỊ CẤM" : "LỖI", [m.banned ? "418" : "", m.error_vi || ""].filter(Boolean).join(" · "));
      } else {
        this.setChip(id, "ok", isNum(m.ping_ms) ? `${m.ping_ms} ms` : "OK", `${m.host} · weight ${m.weight_used_1m}/${m.weight_limit_1m}`);
      }
    }
    const skews = markets.map(([, m]) => (m && isNum(m.clock_skew_ms) ? m.clock_skew_ms : null));
    if (skews.every((x) => x === null)) {
      this.setChip("chip-skew", "idle", "—", "chưa đo được");
    } else {
      const worst = Math.max(...skews.filter((x) => x !== null).map(Math.abs));
      const show = (x) => (x === null ? "—" : `${x > 0 ? "+" : ""}${x}`);
      this.setChip("chip-skew", worst < 1000 ? "ok" : "warn", `${show(skews[0])} / ${show(skews[1])} ms`,
        "spot / futures · dương = đồng hồ sàn đi trước; lệnh ký lùi 1000 ms so với sàn");
    }

    const find = (list, asset) => (list || []).find((b) => b.asset === asset);
    const spotUSDT = find(a.spot.balances, "USDT");
    const futUSDT = find(a.futures.balances, "USDT");
    const base = find(a.spot.balances, baseAsset);
    lastBinanceSpotUSDT = spotUSDT ? spotUSDT.total_qty_in_asset : 0;
    lastBinanceFuturesUSDT = futUSDT ? futUSDT.total_qty_in_asset : 0;
    setText("bal-spot-usdt", spotUSDT ? fmt.quote(spotUSDT.total_qty_in_asset, 2) : "—");
    setText("bal-futures-usdt", futUSDT ? fmt.quote(futUSDT.total_qty_in_asset, 2) : "—");
    if (bybitEquityUSD !== null || isMasterMode) {
      this.updateHeaderLabels();
      if (bybitEquityUSD !== null) {
        setText("bal-bybit-usdt", fmt.quote(bybitEquityUSD, 2));
        setText("bal-total-usdt", fmt.quote(lastBinanceSpotUSDT + lastBinanceFuturesUSDT + bybitEquityUSD, 2));
      }
    } else if (unifiedWallet) {
      // The futures view names USDT even at zero; the spot view lists only
      // non-zero coins, so it is not the one to read the wallet from.
      setText("bal-total-usdt", futUSDT ? fmt.quote(futUSDT.total_qty_in_asset, 2) : "—");
    } else {
      setText("bal-total-usdt", spotUSDT && futUSDT ? fmt.quote(spotUSDT.total_qty_in_asset + futUSDT.total_qty_in_asset, 2) : "—");
    }
    setText("bal-base-asset", baseAsset || "—");
    setText("bal-spot-base", base ? fmt.quote(base.total_qty_in_asset, 8) : "—");
    balances.readAtMs = a.read_at_ms;
    balances.failedVI = "";
    renderBalanceAge();
  },


  renderAccountFailed(messageVI) {
    balances.failedVI = messageVI || "lần đọc số dư lỗi";
    renderBalanceAge();
  },

  // renderHedges drives the chip from EVERY tradable symbol, worst first: a
  // naked ETH leg must not hide behind a green BTC chip because BTC is the one
  // selected on the Execution tab.
  renderHedges(bySymbol) {
    const rank = { unhedged: 0, evidence_conflict: 1, unknown: 2, both_open: 3, both_flat: 4 };
    const entries = Object.entries(bySymbol);
    if (entries.length === 0) {
      const chip = $("chip-hedge");
      chip.dataset.status = "unknown";
      this.setChip("chip-hedge", "idle", "—", "chưa đọc symbol nào");
      setText("chip-hedge-k", "Phòng hộ");
      $("tab-exec-alarm").hidden = true;
      return;
    }
    entries.sort(([, a], [, b]) => (rank[a.status] ?? 2) - (rank[b.status] ?? 2));
    const [worstSymbol, worst] = entries[0];
    const status = worst.status || "unknown";
    const chip = $("chip-hedge");
    chip.dataset.status = status;
    // A reading older than HEDGE_STALE_MS is not a statement about now: a
    // green chip turns amber and says CŨ; a red one stays red.
    const oldest = Math.max(...entries.map(([, p]) => (p.read_at_ms ? Date.now() - p.read_at_ms : Infinity)));
    const stale = oldest > HEDGE_STALE_MS || entries.some(([, p]) => p.stale);
    let state = status === "both_open" || status === "both_flat" ? "ok" : status === "unhedged" ? "bad" : "warn";
    if (stale && state === "ok") state = "warn";
    const title = entries.map(([sym, p]) => `${sym}: ${p.status_vi || p.status}${p.read_at_ms ? " · " + fmt.age(p.read_at_ms) : ""}${p.reason_vi ? " — " + p.reason_vi : ""}`).join("\n");
    const allGood = status === "both_open" || status === "both_flat";
    const word = allGood && entries.length > 1 ? `${entries.length}/${entries.length} ổn · ${worst.status_vi || status}` : worst.status_vi || status;
    this.setChip("chip-hedge", state, stale ? `CŨ · ${word}` : word, title);
    setText("chip-hedge-k", allGood && entries.length > 1 ? "Phòng hộ mọi symbol" : `Phòng hộ ${worstSymbol}`);
    $("tab-exec-alarm").hidden = !(status === "unhedged" || status === "evidence_conflict");
  },
};
