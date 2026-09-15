// Market Scanner — the step-3.5 scanner's own WebSocket contract
// (docs/WS-CONTRACT.md, v1), received through the portal's READ-ONLY relay.
//
// It replaced the scanner's own dashboard (static/app.js, removed 2026-09-15)
// and keeps that dashboard's contract rules: sources, labels, colours and
// thresholds come from `meta`;
// freshness is the server's verdict and ages are measured on the server clock;
// a stale price breaks its line instead of drawing a flat one; every gross
// figure says it is gross.
//
// The socket is opened only while this tab is showing, and closed a minute
// after it stops showing: each session is one more client the gate process
// builds spread matrices for.

import { $, el, clear, setText, isNum, fmt, safeColor, flash, api, chartOptions, chartsReady, emptyRow, NEON } from "./core.js";
import { shell } from "./shell.js";

const WIRE_VERSION = 1;
const TOGGLE_KEY = "portal.scanner.sources.v1";
const SYMBOL_KEY = "portal.scanner.symbol";
const DISCONNECT_AFTER_MS = 60000;
const MAX_LINE_POINTS = 600;
const CANDLE_SEC = 60;
const MAX_CANDLES = 240;
const MAX_ALERTS = 30;
const FLASH_EVERY_MS = 1500;

const COST_NAMES = {
  taker_fee: "phí taker",
  maker_fee: "phí maker",
  taker_fee_entry: "phí taker lúc mở",
  taker_fee_exit: "phí taker lúc đóng",
  maker_rebate: "hoàn phí maker",
  withdrawal: "phí rút/chuyển",
  slippage: "trượt giá",
  funding: "phí funding",
};

class ScannerTab {
  constructor() {
    this.meta = null;
    this.sourceMeta = new Map();
    this.symbols = [];
    this.symbol = null;
    this.clockOffsetMs = 0;

    this.prices = new Map();
    this.sourceStatus = new Map();
    this.absent = new Set();
    this.lines = new Map();
    this.candles = new Map();
    this.spreads = null;
    this.alerts = [];
    this.funding = {};
    this.depth = {};
    this.fundingBasis = null;
    this.depthMeta = null;
    this.tick = null;
    this.toggles = this.loadToggles();
    this.minSpreadPct = 0.05;
    this.flashedAt = new Map();
    this.ticks = 0;
    this.closeSeq = 0;
    this.depthAtServerMs = 0;
    this.contractWarning = "";

    this.ws = null;
    this.wanted = false;
    this.gotMeta = false;
    this.backoffMs = 1000;
    this.reconnectTimer = null;
    this.disconnectTimer = null;
    this.frames = 0;

    this.queue = [];
    this.draining = false;
    this.dirty = { sources: false, spreads: false, alerts: false, funding: false, depth: false };
    this.flushTimer = null;

    this.chart = null;
    this.lineSeries = new Map();
    this.candleSeries = null;
    this.mode = "lines";
    this.candleSource = null;
    this.historyChart = null;
    this.historySeries = new Map();
    this.historyToken = 0;
    this.historyLoadedFor = "";
  }

  // ------------------------------------------------------------ lifecycle

  init() {
    $("sc-symbol").addEventListener("change", (ev) => this.changeSymbol(ev.target.value));
    $("sc-min-spread").addEventListener("input", (ev) => {
      ev.target.dataset.userEdited = "true";
      this.minSpreadPct = parseFloat(ev.target.value) || 0;
      this.mark("alerts", "spreads");
    });
    $("sc-alerts-clear").addEventListener("click", () => {
      this.alerts = [];
      this.mark("alerts");
    });
    for (const button of document.querySelectorAll("#panel-scanner .seg button[data-mode]")) {
      button.addEventListener("click", () => this.setMode(button.dataset.mode));
    }
    $("sc-candle-source").addEventListener("change", (ev) => {
      this.candleSource = ev.target.value;
      this.redrawChart();
    });
    $("sc-realtime").addEventListener("click", () => this.chart && this.chart.timeScale().scrollToRealTime());
    $("sc-history-days").addEventListener("change", () => this.loadHistory(true));
    $("sc-sources").addEventListener("change", (ev) => {
      const box = ev.target;
      if (box.type !== "checkbox" || !box.dataset.source) return;
      this.toggles[box.dataset.source] = box.checked;
      this.saveToggles();
      this.redrawChart();
      this.mark("sources", "spreads", "alerts");
    });

    shell.onTab("scanner", {
      enter: () => {
        this.ensureCharts();
        this.want(true);
        // Points stored while another tab showed were never drawn; drawing
        // only the next one would join the gap with a straight line.
        this.redrawChart();
        this.mark("sources", "spreads", "alerts", "funding", "depth");
        this.flush();
        this.loadHistory(false);
      },
      leave: () => this.want(false),
    });
    shell.onVisibility((visible) => {
      if (shell.active !== "scanner") return;
      this.want(visible);
    });
    setInterval(() => this.updateCountdowns(), 1000);
    this.setFeed("idle", "chưa nối — mở tab này để nối relay chỉ đọc tới scanner");
  }

  want(on) {
    clearTimeout(this.disconnectTimer);
    if (!on) {
      // A backoff timer armed while the tab showed must not reconnect to the
      // gate after the tab is gone.
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (on) {
      this.wanted = true;
      if (!this.ws && !this.reconnectTimer) this.connect();
      return;
    }
    this.disconnectTimer = setTimeout(
      () => this.disconnect("tạm ngắt — tab Scanner đóng quá 60 s; nối lại khi mở tab"),
      DISCONNECT_AFTER_MS
    );
  }

  connect() {
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    if (!this.wanted || shell.active !== "scanner" || document.hidden) {
      this.setFeed("idle", "tạm ngắt — nối lại khi mở tab");
      return;
    }
    this.gotMeta = false;
    this.setFeed("connecting", "đang nối relay tới scanner…");
    let ws;
    try {
      ws = new WebSocket(`ws://${location.host}/api/scanner/ws`);
    } catch (err) {
      this.onClosed(null, 0, String(err));
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      if (this.ws === ws) this.setFeed("connecting", "portal đã nhận, chờ message meta từ scanner…");
    };
    ws.onmessage = (ev) => {
      if (this.ws !== ws) return;
      let data;
      try {
        data = JSON.parse(ev.data);
      } catch (_) {
        return;
      }
      this.frames++;
      if (!this.gotMeta) {
        if (data.type !== "meta") {
          this.setFeed("bad", `frame đầu là "${String(data.type)}", không phải meta — tiến trình ở cổng scanner có đúng là cmd/scanner?`);
          return;
        }
        this.gotMeta = true;
        this.backoffMs = 1000;
      }
      this.enqueue(data);
    };
    ws.onclose = (ev) => this.onClosed(ws, ev.code, ev.reason);
  }

  disconnect(reason) {
    this.wanted = false;
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    const ws = this.ws;
    this.ws = null;
    if (ws) {
      ws.onclose = null;
      ws.close(1000, "tab đóng");
    }
    this.invalidateFreshness();
    this.setFeed("idle", reason);
  }

  onClosed(ws, code, reason) {
    if (ws && this.ws !== ws) return;
    this.ws = null;
    this.invalidateFreshness();
    // A relay that drops while nobody is looking stays down until the tab is
    // opened again; reconnecting it would add a client for no reader.
    if (!this.wanted || shell.active !== "scanner" || document.hidden) {
      this.wanted = false;
      this.setFeed("idle", "tạm ngắt — nối lại khi mở tab");
      return;
    }
    const waitSec = Math.round(this.backoffMs / 1000);
    const show = (why) => this.setFeed("bad", `relay đóng: ${why} — nối lại sau ${waitSec} s`);
    show(reason || (code === 1006 ? "mất kết nối (1006)" : `mã ${code}`));
    if (!reason) {
      // A refused handshake reaches the page as a bare 1006; the portal keeps
      // the reason in its status. The answer belongs to THIS close only.
      const closeSeq = ++this.closeSeq;
      api("/api/status").then((r) => {
        if (closeSeq !== this.closeSeq) return;
        const feed = r.ok && r.body.feeds && r.body.feeds.scanner;
        const recent = feed && feed.last_error_vi && Date.now() - feed.last_error_at_ms < 15000 && feed.last_error_at_ms > (feed.last_ok_at_ms || 0);
        if (recent && !this.ws && this.reconnectTimer) show(feed.last_error_vi);
      });
    }
    this.reconnectTimer = setTimeout(() => this.connect(), this.backoffMs);
    this.backoffMs = Math.min(this.backoffMs * 2, 30000);
  }

  setFeed(stateName, text) {
    const line = $("sc-feed");
    line.dataset.state = stateName;
    $("sc-feed-dot").className = "status-dot " + (stateName === "live" ? "live" : stateName === "bad" ? "disconnected" : stateName === "connecting" || stateName === "warn" ? "stale" : "");
    setText("sc-feed-text", text);
    const chipState = stateName;
    const value = stateName === "live" || stateName === "warn" ? text.replace(/^⚠[^·]*· /, "⚠ ") : stateName === "idle" ? "tạm ngắt" : stateName === "connecting" ? "đang nối" : "LỖI";
    shell.setChip("chip-scanner", chipState, value, text);
  }

  // -------------------------------------------------------------- messages

  serverNowMs() {
    return Date.now() + this.clockOffsetMs;
  }

  enqueue(data) {
    if (typeof data.server_time_ms === "number") this.clockOffsetMs = data.server_time_ms - Date.now();
    this.queue.push(data);
    if (!this.draining) {
      this.draining = true;
      setTimeout(() => this.drain(), 50);
    }
  }

  drain() {
    const batch = this.queue.splice(0);
    this.draining = false;
    const spreads = [];
    for (const data of batch) {
      try {
        switch (data.type) {
          case "meta":
            this.applyMeta(data);
            break;
          case "prices":
            this.applyPrices(data);
            break;
          case "spreads":
            if (data.symbol === this.symbol) spreads.push(data);
            break;
          case "arbitrage":
            this.applyAlert(data.opportunity);
            break;
          case "funding":
            this.funding = data.funding || {};
            this.mark("funding");
            break;
          case "depth":
            this.depth = data.depth || {};
            this.depthAtServerMs = typeof data.server_time_ms === "number" ? data.server_time_ms : this.serverNowMs();
            this.mark("funding", "depth");
            break;
          default:
            break;
        }
      } catch (err) {
        console.warn("scanner message", data && data.type, err);
      }
    }
    // Only the newest matrix for the symbol on screen matters, and "newest" is
    // the server's clock: two ingestion goroutines broadcast concurrently.
    if (spreads.length) {
      const newest = spreads.reduce((a, b) => ((b.server_time_ms ?? 0) >= (a.server_time_ms ?? 0) ? b : a));
      if (!this.spreads || (newest.server_time_ms ?? 0) >= (this.spreads.server_time_ms ?? 0)) {
        this.spreads = newest;
        this.mark("spreads");
      }
    }
  }

  mark(...parts) {
    for (const p of parts) this.dirty[p] = true;
    if (this.flushTimer) return;
    this.flushTimer = setTimeout(() => {
      this.flushTimer = null;
      this.flush();
    }, 250);
  }

  // flush redraws what changed — only while the tab is showing. Data keeps
  // arriving in the background of the minute before the socket closes.
  flush() {
    if (shell.active !== "scanner") return;
    const d = this.dirty;
    this.dirty = { sources: false, spreads: false, alerts: false, funding: false, depth: false };
    // One renderer that throws must not cost the others their redraw — depth
    // is pushed once an hour, and a lost flag is an hour-old table.
    const run = (flag, fn) => {
      if (!flag) return;
      try {
        fn.call(this);
      } catch (err) {
        console.warn("scanner render", err);
      }
    };
    run(d.sources, this.renderSources);
    run(d.spreads, this.renderSpreads);
    run(d.alerts, this.renderAlerts);
    run(d.funding, this.renderFunding);
    run(d.depth || d.funding, this.renderDepth);
  }

  applyMeta(meta) {
    // Kept, and repeated on every live line, so the 200 ms prices stream does
    // not overwrite it.
    this.contractWarning = meta.v !== WIRE_VERSION ? `⚠ scanner nói hợp đồng v${meta.v}, trang đọc v${WIRE_VERSION} — số có thể sai · ` : "";
    this.meta = meta;
    this.symbols = meta.symbols || [];
    this.fundingBasis = meta.funding_basis || null;
    this.depthMeta = meta.depth || null;
    this.sourceMeta = new Map((meta.sources || []).map((s) => [s.source, s]));

    const minInput = $("sc-min-spread");
    if (typeof meta.alert_min_spread_pct === "number" && !minInput.dataset.userEdited) {
      this.minSpreadPct = meta.alert_min_spread_pct;
      minInput.value = String(meta.alert_min_spread_pct);
    }

    const select = $("sc-symbol");
    const previous = this.symbol;
    clear(select);
    for (const s of this.symbols) select.append(el("option", { text: s }));
    let wantedSymbol = previous;
    if (!wantedSymbol) {
      try {
        wantedSymbol = window.localStorage.getItem(SYMBOL_KEY);
      } catch (_) {
        wantedSymbol = null;
      }
    }
    const nextSymbol = this.symbols.includes(wantedSymbol) ? wantedSymbol : meta.default_symbol || this.symbols[0] || null;
    this.symbol = nextSymbol;
    if (previous && nextSymbol !== previous) {
      // The server stopped serving the symbol on screen: everything drawn for
      // it belongs to it, not to its replacement. The operator's saved choice
      // is left alone — the server picked this one, not the operator.
      this.resetSymbolState();
    }
    select.value = this.symbol || "";

    this.renderCostNote(meta.cost_basis);
    this.renderFundingBasisNote();
    this.renderDepthNote();
    this.buildSourceRows();
    this.buildCandleSourceChoices();
    this.ensureCharts();
    this.syncLineSeries();
    setText("sc-detail-symbol", this.symbol || "—");
    setText("sc-depth-symbol", this.symbol || "—");
    if (this.symbol !== previous) this.loadHistory(true);
    this.mark("sources", "spreads", "alerts", "funding", "depth");
  }

  changeSymbol(symbol) {
    if (!symbol || symbol === this.symbol) return;
    this.symbol = symbol;
    try {
      window.localStorage.setItem(SYMBOL_KEY, symbol);
    } catch (_) {
      /* per-viewer convenience */
    }
    this.resetSymbolState();
    setText("sc-detail-symbol", symbol);
    setText("sc-depth-symbol", symbol);
    this.buildSourceRows();
    this.loadHistory(true);
    this.mark("sources", "spreads", "alerts", "funding", "depth");
  }

  // resetSymbolState drops everything drawn for the previous symbol.
  resetSymbolState() {
    this.prices.clear();
    this.absent.clear();
    this.lines.clear();
    this.candles.clear();
    this.spreads = null;
    this.alerts = [];
    for (const series of this.lineSeries.values()) series.setData([]);
    if (this.candleSeries) this.candleSeries.setData([]);
    // Series hidden while a source was absent for the old symbol must come
    // back: the absent set was just cleared, so no later diff would notice.
    this.redrawChart();
  }

  applyPrices(msg) {
    if (msg.source_status) this.sourceStatus = new Map(Object.entries(msg.source_status));
    this.renderTickStatus(msg.tick_status);
    const bySource = (msg.prices || {})[this.symbol];
    if (bySource) {
      for (const [source, point] of Object.entries(bySource)) {
        const before = this.prices.get(source);
        this.prices.set(source, {
          price: point.price,
          previous: before ? before.price : point.price,
          status: point.status,
          ageMs: point.age_ms,
        });
        if (point.status === "stale") {
          this.breakLine(source);
        } else {
          this.addPoint(source, point.price);
        }
      }
      const wasAbsent = this.absent;
      this.absent = new Set();
      for (const source of this.sourceMeta.keys()) if (!(source in bySource)) this.absent.add(source);
      // A source that dropped out keeps no line and no last-value label: its
      // line gets a gap, and its series is hidden until it returns.
      let changed = wasAbsent.size !== this.absent.size;
      for (const source of this.absent) {
        if (!wasAbsent.has(source)) {
          this.breakLine(source);
          changed = true;
        }
      }
      if (changed) this.redrawChart();
    }
    let live = 0;
    let tradable = 0;
    for (const [source, meta] of this.sourceMeta) {
      if (!meta.tradable) continue;
      tradable++;
      const p = this.prices.get(source);
      if (p && p.status === "live" && !this.absent.has(source)) live++;
    }
    if (this.gotMeta) this.setFeed(this.contractWarning ? "warn" : "live", `${this.contractWarning}LIVE · ${live}/${tradable} nguồn · ${this.frames.toLocaleString("en-US")} frame`);
    this.mark("sources");
  }

  applyAlert(opportunity) {
    if (!opportunity) return;
    this.alerts.unshift(opportunity);
    if (this.alerts.length > MAX_ALERTS) this.alerts.length = MAX_ALERTS;
    this.mark("alerts");
  }

  // Our own socket is down: every "live" claim on screen was justified by a
  // message that is no longer arriving. Prices stay for context; freshness
  // claims are retracted, and every line gets a gap.
  invalidateFreshness() {
    for (const p of this.prices.values()) {
      p.status = "unknown";
      p.ageMs = -1;
    }
    for (const status of this.sourceStatus.values()) {
      status.state = "unknown";
      status.uptime_sec = 0;
      status.reconnect_count = 0;
    }
    for (const bySource of Object.values(this.funding)) {
      for (const point of Object.values(bySource)) {
        point.status = "unknown";
        point.age_ms = -1;
      }
    }
    for (const bySource of Object.values(this.depth)) {
      for (const point of Object.values(bySource)) {
        point.status = "unknown";
        point.age_ms = -1;
      }
    }
    this.absent.clear();
    for (const source of this.sourceMeta.keys()) this.breakLine(source);
    this.redrawChart();
    this.mark("sources", "funding", "depth");
  }

  // --------------------------------------------------------------- sources

  loadToggles() {
    try {
      return JSON.parse(window.localStorage.getItem(TOGGLE_KEY) || "{}") || {};
    } catch (_) {
      return {};
    }
  }

  saveToggles() {
    try {
      window.localStorage.setItem(TOGGLE_KEY, JSON.stringify(this.toggles));
    } catch (_) {
      /* per-viewer convenience */
    }
  }

  enabled(source) {
    if (Object.prototype.hasOwnProperty.call(this.toggles, source)) return this.toggles[source] !== false;
    const meta = this.sourceMeta.get(source);
    return meta ? meta.enabled_by_default !== false : true;
  }

  label(source) {
    const meta = this.sourceMeta.get(source);
    return (meta && meta.label) || source;
  }

  shortLabel(source) {
    const meta = this.sourceMeta.get(source);
    return (meta && meta.short_label) || source.slice(0, 3).toUpperCase();
  }

  buildSourceRows() {
    const list = $("sc-sources");
    clear(list);
    if (this.sourceMeta.size === 0) {
      list.append(el("div", { cls: "empty-state", text: "Scanner chưa gửi danh sách nguồn." }));
      return;
    }
    for (const [source, meta] of this.sourceMeta) {
      const box = el("input", { attrs: { type: "checkbox", "aria-label": `Hiện ${meta.label || source}` }, data: { source } });
      box.checked = this.enabled(source);
      const swatch = el("span", { cls: "swatch" });
      swatch.style.background = safeColor(meta.color);
      const fee = meta.tradable && !meta.fee_verified
        ? el("span", { cls: "badge fee", text: "chưa có phí", title: "Chưa xác minh được biểu phí của sàn này, nên mọi cặp có nó đều không có số sau phí. Không có nghĩa là miễn phí." })
        : null;
      const row = el("div", { cls: "source-row", data: { source } }, [
        box,
        el("span", { cls: "status-dot", data: { role: "dot" } }),
        el("div", { cls: "source-name" }, [swatch, el("span", { text: meta.label || source }), fee, el("span", { data: { role: "badge" } })]),
        el("div", { cls: "source-price" }, [el("span", { data: { role: "price" }, text: "—" }), el("span", { cls: "chg", data: { role: "chg" } })]),
      ]);
      list.append(row);
    }
  }

  statusClass(source) {
    const conn = this.sourceStatus.get(source);
    if (conn && conn.state === "disconnected") return "disconnected";
    if (conn && conn.state === "reconnecting") return "reconnecting";
    if (this.absent.has(source) && this.prices.has(source)) return "stale";
    const p = this.prices.get(source);
    if (!p) return "";
    return p.status === "live" ? "live" : p.status === "stale" ? "stale" : "";
  }

  statusTitle(source) {
    const conn = this.sourceStatus.get(source);
    const parts = [];
    const p = this.prices.get(source);
    if (!p) {
      parts.push(conn && conn.state === "disconnected" ? "Sàn chưa gửi dữ liệu nào — coi như mất kết nối" : "Đang chờ dữ liệu đầu tiên");
    } else {
      parts.push(p.status === "stale" ? "Dữ liệu CŨ — đã loại khỏi so sánh" : p.status === "live" ? "Dữ liệu mới" : "Chưa đo được độ mới của dữ liệu");
      if (p.ageMs >= 0) parts.push(`nhận cách đây ${p.ageMs < 1000 ? p.ageMs + "ms" : (p.ageMs / 1000).toFixed(1) + "s"}`);
    }
    if (conn && conn.uptime_sec > 0) parts.push(`kết nối liên tục ${fmt.duration(conn.uptime_sec)}`);
    if (conn && conn.reconnect_count > 0) parts.push(`đã nối lại ${conn.reconnect_count} lần`);
    return parts.join(" · ");
  }

  statusBadge(source) {
    if ((!this.ws || !this.gotMeta) && this.prices.has(source)) return this.wanted ? ["bad", "MẤT KẾT NỐI RELAY"] : ["wait", "RELAY NGẮT"];
    const conn = this.sourceStatus.get(source);
    if (conn && conn.state === "disconnected") return ["bad", "MẤT KẾT NỐI"];
    const p = this.prices.get(source);
    if (p && p.status === "stale") return ["stale", "CŨ"];
    if (!p) return ["wait", "CHỜ"];
    if (this.absent.has(source)) return ["stale", "KHÔNG GIÁ"];
    return null;
  }

  renderSources() {
    const now = Date.now();
    for (const row of document.querySelectorAll("#sc-sources .source-row")) {
      const source = row.dataset.source;
      row.classList.toggle("off", !this.enabled(source));
      const dot = row.querySelector('[data-role="dot"]');
      const cls = "status-dot " + this.statusClass(source);
      if (dot.className !== cls) dot.className = cls;
      dot.title = this.statusTitle(source);

      const badgeHolder = row.querySelector('[data-role="badge"]');
      const badge = this.statusBadge(source);
      const want = badge ? `badge ${badge[0]}|${badge[1]}` : "";
      if (badgeHolder.dataset.want !== want) {
        badgeHolder.dataset.want = want;
        clear(badgeHolder);
        if (badge) badgeHolder.append(el("span", { cls: "badge " + badge[0], text: badge[1] }));
      }

      const p = this.prices.get(source);
      const priceNode = row.querySelector('[data-role="price"]');
      const chgNode = row.querySelector('[data-role="chg"]');
      if (!p) {
        setText(priceNode, "—");
        setText(chgNode, "");
        continue;
      }
      const text = fmt.price(p.price);
      if (priceNode.textContent !== text) {
        const up = p.price >= (p.previous || p.price);
        priceNode.textContent = text;
        if ((this.flashedAt.get(source) || 0) + FLASH_EVERY_MS < now && p.previous !== p.price) {
          this.flashedAt.set(source, now);
          flash(priceNode.parentElement, up);
        }
      }
      const changePct = p.previous ? ((p.price - p.previous) / p.previous) * 100 : 0;
      const flat = Math.abs(changePct) < 0.0005;
      setText(chgNode, `${flat ? "·" : changePct > 0 ? "↑" : "↓"} ${Math.abs(changePct).toFixed(3)}%`, "chg " + (flat ? "faint" : changePct > 0 ? "pos" : "neg"));
    }
  }

  renderTickStatus(tick) {
    const note = $("sc-tick-note");
    const late = tick && tick.late_ticks ? tick.late_ticks : 0;
    // An absent field and a real zero render the same way — as nothing — so a
    // server that cannot count late ticks never appears to report none.
    if (late <= 0) {
      if (!note.hidden) note.hidden = true;
      return;
    }
    const parts = [`${late} tick trễ kể từ lúc scanner khởi động`];
    if (tick.last_late_job) {
      const by = tick.last_late_by_sec > 0 ? ` trễ ${fmt.duration(tick.last_late_by_sec)}` : "";
      let at = "";
      if (tick.last_late_at_ms > 0) {
        at = tick.last_late_by_sec > 0
          ? ` — không ghi gì từ ${new Date(tick.last_late_at_ms - tick.last_late_by_sec * 1000).toLocaleString()} đến ${new Date(tick.last_late_at_ms).toLocaleString()}`
          : ` lúc ${new Date(tick.last_late_at_ms).toLocaleString()}`;
      }
      parts.push(`gần nhất: ${tick.last_late_job}${by}${at}`);
    }
    parts.push("Trong khoảng trễ KHÔNG có gì được lấy mẫu, đánh giá hay ghi nhật ký — thường là máy ngủ.");
    setText(note, parts.join(" · "));
    note.hidden = false;
  }

  renderCostNote(costBasis) {
    const node = $("sc-cost-note");
    if (!costBasis) {
      setText(node, "");
      return;
    }
    const label = (list) => (list || []).map((k) => COST_NAMES[k] || k).join(", ");
    const applied = costBasis.applied || [];
    const headline = costBasis.note_vi || (applied.length ? `Số hiển thị đã trừ: ${label(applied)}.` : "Số hiển thị là chênh lệch THÔ, chưa trừ bất kỳ chi phí nào.");
    const excluded = (costBasis.excluded || []).length ? ` Chưa trừ: ${label(costBasis.excluded)}.` : "";
    clear(node);
    node.append(el("strong", { text: headline }), excluded, " Ô có dấu * là số THÔ (một sàn chưa có biểu phí).");
  }

  renderFundingBasisNote() {
    const node = $("sc-funding-basis");
    clear(node);
    if (!this.fundingBasis) return;
    const excluded = (this.fundingBasis.excluded || []).join(", ");
    node.append(el("strong", { text: `THÔ (${this.fundingBasis.model || "gross"})` }), ` — ${this.fundingBasis.note_vi || ""}`);
    if (excluded) node.append(" ", el("strong", { text: "Chưa trừ:" }), ` ${excluded}.`);
  }

  renderDepthNote() {
    const node = $("sc-depth-note");
    clear(node);
    if (!this.depthMeta) return;
    const windows = (this.depthMeta.windows_pct || []).map((w) => `${w}%`).join(" và ");
    const everyMin = Math.round((this.depthMeta.refresh_every_sec || 0) / 60);
    node.append(el("strong", { text: "Độ sâu" }), ` — cửa sổ ${windows} quanh mid, lấy lại mỗi ${everyMin} phút. ${this.depthMeta.note_vi || ""}`);
  }

  // --------------------------------------------------------------- spreads

  renderSpreads() {
    const root = $("sc-spreads");
    clear(root);
    const data = this.spreads;
    if (!data) {
      root.append(el("div", { cls: "empty-state", text: this.ws ? "Đang chờ ma trận chênh lệch cho cặp này…" : "Chưa nối scanner." }));
      return;
    }
    const groups = data.cross_venue_groups || [];
    let drawn = 0;
    for (const group of groups) {
      const block = this.renderGroup(group);
      if (block) {
        root.append(block);
        drawn++;
      }
    }
    if (drawn === 0) {
      const message = groups.length > 0
        ? "Mọi nguồn trong các nhóm đang bị tắt ở danh sách nguồn."
        : "Không có nhóm nào so sánh được: mỗi nhóm cần ít nhất hai nguồn cùng loại thị trường và cùng đồng quote. Lý do từng nguồn ở dưới.";
      root.append(el("div", { cls: "spread-group note", text: message }));
    }
    if ((data.basis || []).length) {
      const block = el("div", { cls: "spread-group" }, [el("div", { cls: "spread-title", text: "Basis (spot ↔ perp cùng sàn)" })]);
      for (const b of data.basis) {
        block.append(el("div", { cls: "basis-row" }, [
          el("span", { text: `${this.label(b.perp_source)} vs ${this.label(b.spot_source)}` }),
          el("span", { cls: b.basis_pct >= 0 ? "pos" : "neg", text: fmt.pct(b.basis_pct, 3, true) }),
        ]));
      }
      root.append(block);
    }
    if ((data.oracle_deviation || []).length) {
      const block = el("div", { cls: "spread-group" }, [
        el("div", { cls: "spread-title" }, ["Lệch so với oracle ", el("span", { cls: "badge wait", text: "tham chiếu" })]),
      ]);
      for (const d of data.oracle_deviation) {
        block.append(el("div", { cls: "basis-row" }, [
          el("span", null, [this.label(d.source), d.quote_asset_mismatch ? el("span", { cls: "badge stale", text: " lệch quote" }) : null]),
          el("span", { cls: d.deviation_pct >= 0 ? "pos" : "neg", text: fmt.pct(d.deviation_pct, 3, true) }),
        ]));
      }
      root.append(block);
    }
    if ((data.excluded_sources || []).length) {
      const list = el("ul", { cls: "excluded" });
      for (const e of data.excluded_sources) list.append(el("li", { text: `${this.label(e.source)}: ${e.note_vi || e.reason}` }));
      root.append(el("div", { cls: "spread-group" }, [el("div", { cls: "spread-title", text: "Nguồn không vào so sánh" }), list]));
    }
  }

  renderGroup(group) {
    const sources = (group.sources || []).filter((s) => this.enabled(s));
    if (sources.length === 0) return null;
    const block = el("div", { cls: "spread-group" });
    block.append(el("div", { cls: "spread-title" }, [group.label_vi || group.group_id, group.tradable ? null : el("span", { cls: "badge wait", text: "tham chiếu" })]));
    if (group.note_vi) block.append(el("p", { cls: "note", text: group.note_vi }));
    const grid = el("div", { cls: "matrix-grid mt8", attrs: { role: "table", "aria-label": `Ma trận ${group.label_vi || group.group_id}: hàng mua, cột bán` } });
    grid.style.gridTemplateColumns = `56px repeat(${sources.length}, minmax(0, 1fr))`;
    grid.append(el("div", { cls: "mx-head", text: "mua\\bán" }));
    for (const sell of sources) grid.append(el("div", { cls: "mx-head", text: this.shortLabel(sell), title: this.label(sell) }));
    for (const buy of sources) {
      grid.append(el("div", { cls: "mx-head", text: this.shortLabel(buy), title: this.label(buy) }));
      for (const sell of sources) {
        const cell = buy === sell ? null : group.matrix && group.matrix[buy] && group.matrix[buy][sell];
        if (!cell) {
          grid.append(el("div", { cls: "mx-cell", text: "·" }));
          continue;
        }
        const hasNet = typeof cell.spread_after_fees_pct === "number";
        const shown = hasNet ? cell.spread_after_fees_pct : cell.spread_gross_pct;
        let cls = shown >= this.minSpreadPct ? "opportunity" : shown > 0 ? "positive" : "negative";
        // The highlight is a claim that this is actionable: never on a group
        // that is only a reference, never on a GROSS figure.
        if ((!group.tradable || !hasNet) && cls === "opportunity") cls = "positive";
        const text = `${shown >= 0 ? "+" : ""}${shown.toFixed(2)}${hasNet ? "" : "*"}`;
        const basis = hasNet ? "đã trừ phí giao dịch (taker cả bốn lượt khớp)" : "THÔ, chưa trừ phí — chưa xác minh được biểu phí của một trong hai sàn";
        grid.append(el("div", {
          cls: `mx-cell ${cls}${hasNet ? "" : " gross-only"}`,
          text,
          title: `Mua ${this.label(buy)} → Bán ${this.label(sell)}: ${text}% (${basis})`,
        }));
      }
    }
    block.append(grid);
    return block;
  }

  renderAlerts() {
    const body = $("sc-alerts");
    const rows = this.alerts.filter((o) => o.spread_gross_pct >= this.minSpreadPct && this.enabled(o.buy_source) && this.enabled(o.sell_source));
    setText("sc-alerts-count", `${rows.length} cảnh báo · ngưỡng lọc trên chênh lệch THÔ — phần lớn âm sau một vòng phí`);
    if (rows.length === 0) {
      emptyRow(body, 6, this.alerts.length ? "Không cảnh báo nào qua bộ lọc hiện tại." : "Chưa có cảnh báo nào kể từ lúc nối.");
      return;
    }
    clear(body);
    const now = this.serverNowMs();
    for (const o of rows) {
      const hasNet = typeof o.spread_after_fees_pct === "number";
      body.append(el("tr", { cls: now - o.detected_at_ms < 5000 ? "fresh" : "" }, [
        el("td", { text: o.symbol }),
        el("td", { cls: "r", text: fmt.pct(o.spread_gross_pct, 3) }),
        el("td", {
          cls: "r " + (hasNet ? (o.spread_after_fees_pct >= 0 ? "pos" : "neg") : "faint"),
          text: hasNet ? fmt.pct(o.spread_after_fees_pct, 3, true) : "—",
          title: hasNet ? "Đã trừ phí taker cả bốn lượt khớp. Chưa trừ trượt giá và funding." : "Chưa xác minh được biểu phí của một trong hai sàn.",
        }),
        el("td", { text: `${this.shortLabel(o.buy_source)} ${fmt.price(o.buy_price)}` }),
        el("td", { text: `${this.shortLabel(o.sell_source)} ${fmt.price(o.sell_price)}` }),
        el("td", { text: new Date(o.detected_at_ms).toLocaleTimeString() }),
      ]));
    }
  }

  // --------------------------------------------------------------- funding

  fundingSources() {
    return [...this.sourceMeta.values()].filter((m) => m.market_type === "perp" && m.funding_publish_mode).map((m) => m.source);
  }

  renderFunding() {
    const table = $("sc-funding-matrix");
    clear(table);
    const sources = this.fundingSources();
    if (sources.length === 0 || this.symbols.length === 0) {
      table.append(el("tbody", null, [el("tr", null, [el("td", { cls: "empty", text: "Đang chờ dữ liệu funding…" })])]));
    } else {
      const head = el("tr", null, [el("th", { text: "Sàn" }), ...this.symbols.map((s) => el("th", { cls: "r", text: s }))]);
      const body = el("tbody");
      for (const source of sources) {
        const meta = this.sourceMeta.get(source) || {};
        const nameCell = el("td", { text: meta.short_label || source, title: meta.label || source });
        nameCell.style.color = safeColor(meta.color);
        const cells = this.symbols.map((symbol) => {
          const point = (this.funding[symbol] || {})[source];
          if (!point) return el("td", { cls: "f-zero", text: "—" });
          const cls = [signClass(point.rate_per_8h_bps), point.status === "live" ? "" : "f-stale"].join(" ");
          return el("td", { cls, title: this.fundingTooltip(symbol, source, point) }, [
            fmt.bps(point.rate_per_8h_bps, 4),
            el("span", { cls: "f-sub", text: `APR thô ${fmt.pct(point.apr_gross_pct, 2, true)}` }),
            point.hedge_spot_source ? null : el("span", { cls: "f-nohedge", text: "∅ hedge" }),
          ]);
        });
        body.append(el("tr", null, [nameCell, ...cells]));
      }
      table.append(el("thead", null, [head]), body);
    }
    this.renderFundingDetail();
  }

  fundingTooltip(symbol, source, point) {
    const meta = this.sourceMeta.get(source) || {};
    return `${symbol} · ${meta.label || source}\n${fmt.bps(point.rate_per_8h_bps, 4)} bps/8h · APR thô ${fmt.pct(point.apr_gross_pct, 2, true)}\n`
      + `Chu kỳ ${formatInterval(point)} · ${point.raw_rate_field || "?"} = ${point.raw_rate}\n`
      + (point.hedge_spot_source ? `Hedge: ${point.hedge_spot_source}` : `Hedge: không có — ${point.hedge_note_vi || ""}`);
  }

  renderFundingDetail() {
    const body = $("sc-funding-detail");
    const bySource = this.funding[this.symbol] || {};
    const sources = this.fundingSources().filter((s) => bySource[s]);
    if (sources.length === 0) {
      emptyRow(body, 10, "Chưa có reading funding nào cho cặp này.");
      return;
    }
    clear(body);
    for (const source of sources) {
      const meta = this.sourceMeta.get(source) || {};
      const point = bySource[source];
      const name = el("td", { text: meta.short_label || source, title: meta.label || source });
      name.style.color = safeColor(meta.color);
      const breakeven = point.breakeven_days_fees_only === null || point.breakeven_days_fees_only === undefined ? "—" : point.breakeven_days_fees_only.toFixed(1);
      const perpDepth = this.depthCell(source, "ask");
      const spotDepth = point.hedge_spot_source ? this.depthCell(point.hedge_spot_source, "bid") : { text: "—", title: "Không có chân spot để hedge." };
      body.append(el("tr", { cls: point.status === "live" ? "" : "f-stale" }, [
        name,
        el("td", { cls: "r " + signClass(point.rate_per_8h_bps), text: fmt.bps(point.rate_per_8h_bps, 4) }),
        el("td", { cls: "r " + signClass(point.apr_gross_pct), text: fmt.pct(point.apr_gross_pct, 2, true) }),
        el("td", { text: formatInterval(point) }),
        el("td", { cls: "mono", data: { next: Number(point.next_funding_at_ms) || 0, model: point.model || "", countdown: "1" }, text: this.countdown(point.next_funding_at_ms, point.model) }),
        el("td", { text: this.freshness(point), title: this.freshnessTooltip(source) }),
        el("td", { text: point.hedge_spot_source || "không có", cls: point.hedge_spot_source ? "" : "warn", title: point.hedge_note_vi || "" }),
        el("td", { cls: "r", text: breakeven, title: "Chỉ trừ phí taker bốn lượt khớp; chưa có slippage, chưa có chi phí vay." }),
        el("td", { cls: "r", text: perpDepth.text, title: perpDepth.title }),
        el("td", { cls: "r", text: spotDepth.text, title: spotDepth.title }),
      ]));
    }
  }

  countdown(nextMs, model) {
    if (model === "continuous") return "liên tục";
    const next = Number(nextMs) || 0;
    if (next <= 0) return "—";
    const remaining = next - this.serverNowMs();
    if (remaining <= 0) {
      // Bounded like the backend's 2-minute grace: a stamp hours gone is a
      // period that ENDED, not a settlement in progress.
      const overdue = -remaining;
      if (overdue <= 120000) return "đang settle…";
      const min = Math.floor(overdue / 60000);
      return `qua mốc ${min >= 60 ? `${Math.floor(min / 60)}g${String(min % 60).padStart(2, "0")}` : `${min}m`}`;
    }
    const total = Math.floor(remaining / 1000);
    const pad = (n) => String(n).padStart(2, "0");
    return `${pad(Math.floor(total / 3600))}:${pad(Math.floor((total % 3600) / 60))}:${pad(total % 60)}`;
  }

  updateCountdowns() {
    if (shell.active !== "scanner" || document.hidden) return;
    if (++this.ticks % 30 === 0) this.mark("depth");
    for (const cell of document.querySelectorAll('#sc-funding-detail [data-countdown="1"]')) {
      setText(cell, this.countdown(Number(cell.dataset.next) || 0, cell.dataset.model));
    }
  }

  freshness(point) {
    const ageMs = Number(point.age_ms);
    const age = isNum(ageMs) && ageMs >= 0 ? `${Math.round(ageMs / 1000)}s` : "—";
    if (point.status === "live") return age;
    if (point.status === "unknown") return "— · mất kết nối scanner";
    return `${age} · ${point.stale_reason === "settled" ? "đã qua mốc settle" : "im lặng"}`;
  }

  freshnessTooltip(source) {
    const meta = this.sourceMeta.get(source) || {};
    const mode = meta.funding_publish_mode === "on_change"
      ? "Sàn chỉ phát khi số funding đổi, nên im lặng lâu là BÌNH THƯỜNG — tuổi ở đây không đo được sức sống."
      : "Sàn phát theo nhịp cố định, nên tuổi ở đây đo được sức sống của feed.";
    return meta.funding_stale_after_sec ? `${mode} Ngưỡng cũ: ${meta.funding_stale_after_sec}s.` : mode;
  }

  // depthCell renders one side within ±0.5%; "≥" when the venue's book does
  // not reach the window, because then the figure is a lower bound.
  depthCell(source, side) {
    const point = (this.depth[this.symbol] || {})[source];
    const label = this.label(source);
    if (!point) return { text: "—", title: `Chưa có số đo độ sâu cho ${label}.` };
    if (point.error_vi) return { text: "lỗi", title: point.error_vi };
    const quote = point[`${side}_depth_within_0_5pct_quote`];
    const covered = point.covers_0_5pct;
    const ageMs = this.depthAge(point);
    return {
      text: `${covered ? "" : "≥"}${fmt.notional(quote)}`,
      title: `${label} · ${side === "bid" ? "phía mua (bid)" : "phía bán (ask)"} trong ±0,5% quanh mid · ${point[`${side}_levels`]} mức`
        + (covered ? "" : " · sàn KHÔNG trả đủ mức tới 0,5% — cận dưới")
        + (ageMs >= 0 ? ` · đo cách đây ${Math.round(ageMs / 60000)} phút` : ""),
    };
  }

  // depthAge is the measurement's age NOW: age_ms as of the depth message plus
  // the time since that message, on the server clock. -1 when unknown.
  depthAge(p) {
    if (!p || !isNum(p.age_ms) || p.age_ms < 0) return -1;
    return p.age_ms + Math.max(0, this.serverNowMs() - (this.depthAtServerMs || this.serverNowMs()));
  }

  renderDepth() {
    const body = $("sc-depth");
    const bySource = this.depth[this.symbol] || {};
    const sources = [...this.sourceMeta.keys()].filter((s) => bySource[s]);
    if (sources.length === 0) {
      emptyRow(body, 9, "Chưa có số đo độ sâu cho cặp này (scanner quét định kỳ, mặc định mỗi giờ).");
      return;
    }
    clear(body);
    for (const source of sources) {
      const p = bySource[source];
      const meta = this.sourceMeta.get(source) || {};
      const name = el("td", { text: meta.label || source });
      name.style.color = safeColor(meta.color);
      const cell = (key, covered) => el("td", { cls: "r", text: p.error_vi ? "—" : `${covered ? "" : "≥"}${fmt.notional(p[key])}` });
      const notes = [];
      if (p.error_vi) notes.push(p.error_vi);
      if (p.is_contract_book) notes.push("sổ theo contract, đã quy đổi sang coin");
      if (meta.quote_asset) notes.push(meta.quote_asset);
      const ageMs = this.depthAge(p);
      const everySec = (this.depthMeta && this.depthMeta.refresh_every_sec) || 0;
      // The table is pushed once per sweep; its age and status were true at
      // that message. Both are carried forward on the page clock.
      const status = ageMs >= 0 && everySec > 0 && ageMs > 2.5 * everySec * 1000 ? "stale" : p.status;
      const age = ageMs >= 0 ? `${Math.round(ageMs / 60000)} phút` : "—";
      body.append(el("tr", { cls: status === "live" ? "" : "f-stale" }, [
        name,
        cell("bid_depth_within_0_1pct_quote", p.covers_0_1pct),
        cell("bid_depth_within_0_5pct_quote", p.covers_0_5pct),
        cell("ask_depth_within_0_1pct_quote", p.covers_0_1pct),
        cell("ask_depth_within_0_5pct_quote", p.covers_0_5pct),
        el("td", { cls: "r", text: p.error_vi ? "—" : fmt.pct(p.spread_pct, 4) }),
        el("td", { cls: "r", text: `${p.bid_levels || 0}/${p.ask_levels || 0}` }),
        el("td", { text: `${age}${status === "live" ? "" : " · " + (status || "?")}` }),
        el("td", { cls: p.error_vi ? "neg wrap" : "wrap", text: notes.join(" · ") }),
      ]));
    }
  }

  // ---------------------------------------------------------------- charts

  ensureCharts() {
    if (!chartsReady() || shell.active !== "scanner") return;
    if (!this.chart) {
      this.chart = window.LightweightCharts.createChart($("sc-chart"), chartOptions({
        timeScale: { secondsVisible: true },
        handleScale: { axisPressedMouseMove: true, mouseWheel: true, pinch: true },
      }));
      this.syncLineSeries();
    }
    if (!this.historyChart) {
      this.historyChart = window.LightweightCharts.createChart($("sc-history-chart"), chartOptions({ timeScale: { secondsVisible: false } }));
    }
  }

  syncLineSeries() {
    if (!this.chart) return;
    const styles = { solid: 0, dotted: 1, dashed: 2 };
    for (const [source, series] of [...this.lineSeries.entries()]) {
      if (!this.sourceMeta.has(source)) {
        this.chart.removeSeries(series);
        this.lineSeries.delete(source);
      }
    }
    for (const [source, meta] of this.sourceMeta) {
      const options = {
        color: safeColor(meta.color),
        lineWidth: 2,
        lineStyle: styles[meta.line_style] ?? 0,
        title: meta.short_label || source,
        priceLineVisible: false,
        lastValueVisible: true,
        crosshairMarkerRadius: 3,
      };
      const existing = this.lineSeries.get(source);
      if (existing) existing.applyOptions(options);
      else this.lineSeries.set(source, this.chart.addLineSeries(options));
    }
    this.redrawChart();
  }

  buildCandleSourceChoices() {
    const select = $("sc-candle-source");
    clear(select);
    const tradable = [...this.sourceMeta.values()].filter((m) => m.tradable);
    for (const m of tradable) {
      const option = el("option", { text: m.label || m.source });
      option.value = m.source;
      select.append(option);
    }
    if (!tradable.some((m) => m.source === this.candleSource)) this.candleSource = tradable.length ? tradable[0].source : null;
    select.value = this.candleSource || "";
  }

  setMode(mode) {
    this.mode = mode;
    for (const button of document.querySelectorAll("#panel-scanner .seg button[data-mode]")) {
      button.setAttribute("aria-pressed", button.dataset.mode === mode ? "true" : "false");
    }
    $("sc-candle-source").hidden = mode !== "candles";
    setText("sc-chart-title", mode === "candles" ? "Nến 1 phút" : "Giá theo sàn");
    setText("sc-chart-note", mode === "candles"
      ? "Nến 1 phút DỰNG TẠI TRANG từ các snapshot giá 200 ms của một nguồn kể từ lúc nối — không phải nến của sàn; khoảng mất kết nối không có nến."
      : "Đường giá: giá mỗi sàn từ message prices (200 ms). Một nguồn CŨ bị ngắt đường, không nối thẳng qua khoảng trống.");
    this.redrawChart();
  }

  addPoint(source, price) {
    if (!isNum(price)) return;
    const t = this.serverNowMs() / 1000;
    let history = this.lines.get(source);
    if (!history) {
      history = [];
      this.lines.set(source, history);
    }
    // Series times must strictly increase; the server-clock estimate can step
    // back when the offset is re-read.
    if (history.length && t <= history[history.length - 1][0]) return;
    history.push([t, price]);
    if (history.length > MAX_LINE_POINTS) history.shift();

    const bucket = Math.floor(t / CANDLE_SEC) * CANDLE_SEC;
    let candles = this.candles.get(source);
    if (!candles) {
      candles = [];
      this.candles.set(source, candles);
    }
    const last = candles[candles.length - 1];
    let candle;
    if (last && last.time === bucket) {
      last.high = Math.max(last.high, price);
      last.low = Math.min(last.low, price);
      last.close = price;
      candle = last;
    } else if (!last || bucket > last.time) {
      candle = { time: bucket, open: price, high: price, low: price, close: price };
      candles.push(candle);
      if (candles.length > MAX_CANDLES) candles.shift();
    }

    if (!this.chart || shell.active !== "scanner") return;
    if (this.mode === "lines") {
      const series = this.lineSeries.get(source);
      if (series && this.enabled(source) && !this.absent.has(source)) series.update({ time: t, value: price });
    } else if (source === this.candleSource && candle && this.candleSeries) {
      this.candleSeries.update(Object.assign({}, candle));
    }
    this.updateChartEmpty();
  }

  breakLine(source) {
    const history = this.lines.get(source);
    if (!history || history.length === 0) return;
    const last = history[history.length - 1];
    if (last[1] === null) return;
    const t = this.serverNowMs() / 1000;
    if (t <= last[0]) return;
    history.push([t, null]);
    const series = this.lineSeries.get(source);
    if (series && this.chart && this.mode === "lines" && this.enabled(source)) series.update({ time: t });
  }

  redrawChart() {
    if (!this.chart) return;
    const lines = this.mode === "lines";
    for (const [source, series] of this.lineSeries) {
      const on = lines && this.enabled(source) && !this.absent.has(source);
      series.applyOptions({ visible: on });
      const history = this.lines.get(source) || [];
      series.setData(on ? history.map(([time, value]) => (value === null ? { time } : { time, value })) : []);
    }
    if (!lines) {
      if (!this.candleSeries) {
        this.candleSeries = this.chart.addCandlestickSeries({
          upColor: NEON.cyan,
          downColor: NEON.neg,
          borderUpColor: NEON.cyan,
          borderDownColor: NEON.neg,
          wickUpColor: NEON.cyan,
          wickDownColor: NEON.neg,
          priceLineVisible: false,
        });
      }
      this.candleSeries.applyOptions({ visible: true });
      this.candleSeries.setData((this.candles.get(this.candleSource) || []).map((c) => Object.assign({}, c)));
    } else if (this.candleSeries) {
      this.candleSeries.applyOptions({ visible: false });
    }
    this.updateChartEmpty();
  }

  updateChartEmpty() {
    const node = $("sc-chart-empty");
    const hasData = this.mode === "lines"
      ? [...this.lines.values()].some((h) => h.length > 1)
      : (this.candles.get(this.candleSource) || []).length > 0;
    node.hidden = hasData;
  }

  // Settled history is asked over HTTP (WS-CONTRACT §10), through the portal,
  // which caches it for a minute so the gate's database is not read per tab.
  async loadHistory(force) {
    if (!this.symbol || shell.active !== "scanner") return;
    const days = $("sc-history-days").value;
    const key = `${this.symbol}|${days}`;
    if (!force && this.historyLoadedFor === key) return;
    this.historyLoadedFor = key;
    this.ensureCharts();
    const token = ++this.historyToken;
    // The previous symbol's lines do not stay up under the new symbol's title.
    this.clearHistory();
    setText("sc-history-coverage", "");
    setText("sc-history-note", "Đang tải…");
    const r = await api(`/api/scanner/funding-history?symbol=${encodeURIComponent(this.symbol)}&days=${encodeURIComponent(days)}`);
    if (token !== this.historyToken) return;
    if (!r.ok) {
      this.historyLoadedFor = "";
      setText("sc-history-note", r.body.error_vi || `Lỗi HTTP ${r.status}`);
      this.clearHistory();
      setText("sc-history-coverage", "");
      return;
    }
    this.renderHistory(r.body);
    setText("sc-history-note", r.body.note_vi || "");
  }

  clearHistory() {
    if (!this.historyChart) return;
    for (const series of this.historySeries.values()) this.historyChart.removeSeries(series);
    this.historySeries.clear();
  }

  renderHistory(body) {
    if (!this.historyChart) return;
    this.clearHistory();
    for (const series of body.series || []) {
      const meta = this.sourceMeta.get(series.source) || {};
      const line = this.historyChart.addLineSeries({
        color: safeColor(meta.color),
        lineWidth: 1,
        title: meta.short_label || series.source,
        priceLineVisible: false,
        lastValueVisible: false,
      });
      // Whole, strictly increasing seconds: a venue can stamp two settlements
      // inside one second; keep the first.
      const points = [];
      let previous = 0;
      for (const point of series.points || []) {
        const time = Math.floor(point.funding_at_ms / 1000);
        if (time <= previous) continue;
        previous = time;
        points.push({ time, value: point.rate_per_8h_bps });
      }
      line.setData(points);
      this.historySeries.set(series.source, line);
    }
    this.historyChart.timeScale().fitContent();
    const coverage = $("sc-history-coverage");
    clear(coverage);
    const rows = body.coverage || [];
    if (rows.length === 0) {
      coverage.append("Kho dữ liệu chưa có mốc nào cho cặp này.");
      return;
    }
    coverage.append("Độ phủ (bps/8h, THÔ, đã settle): ");
    rows.forEach((row, i) => {
      const meta = this.sourceMeta.get(row.source) || {};
      const name = el("strong", { text: meta.short_label || row.source });
      name.style.color = safeColor(meta.color);
      const days = (row.newest_at_ms - row.oldest_at_ms) / 86400000;
      coverage.append(i ? " · " : "", name, ` ${row.rows} mốc / ${days.toFixed(1)} ngày`);
    });
  }
}

function signClass(value) {
  if (!isNum(value) || value === 0) return "f-zero";
  return value > 0 ? "f-pos" : "f-neg";
}

function formatInterval(point) {
  if (point.model === "continuous") return "liên tục";
  const sec = Number(point.interval_sec) || 0;
  if (sec <= 0) return "—";
  return sec % 3600 === 0 ? `${sec / 3600}h` : `${Math.round(sec / 60)}m`;
}

export function initScanner() {
  const tab = new ScannerTab();
  tab.init();
  return tab;
}
