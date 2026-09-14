// execportal — Strategy 1 operator page. Vanilla JS, no framework (PLAN Q10).
//
// Three rules this file keeps:
//   1. Every string that came from the venue or the cache is written with
//      textContent, never innerHTML. A venue error message is attacker-shaped
//      text as far as the DOM is concerned.
//   2. Nothing is sent to /api/open, /api/close or /api/reconcile before the
//      operator confirms the dialog. The X-Execportal-Action header is set only
//      on that path, and the server refuses a write without it.
//   3. Nothing here says "net" or "profit". Figures carry the label the server
//      gave them.
(() => {
  "use strict";

  const POLL_FAST_MS = 3000;
  const POLL_ORDERS_MS = 15000;
  const POLL_INTENTS_MS = 15000;
  const POLL_FUNDING_MS = 60000;
  const POLL_MARKET_MS = 30000;
  const POLL_STATUS_MS = 5000;
  const STALE_AFTER_MS = 12000;

  const $ = (id) => document.getElementById(id);
  const state = {
    symbol: null,
    status: null,
    account: null,
    positions: null,
    market: null,
    intents: null,
    acting: false,
    portalOK: false,
  };

  // ------------------------------------------------------------- helpers

  function el(tag, opts, children) {
    const node = document.createElement(tag);
    if (opts) {
      if (opts.cls) node.className = opts.cls;
      if (opts.text !== undefined && opts.text !== null) node.textContent = String(opts.text);
      if (opts.title) node.title = opts.title;
    }
    for (const child of children || []) {
      if (child === null || child === undefined) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function isNum(x) {
    return typeof x === "number" && Number.isFinite(x);
  }

  // Float noise on a quantity that is really zero prints as -0.00000000.
  function tidy(x) {
    return Math.abs(x) < 5e-13 ? 0 : x;
  }

  function fmtCoin(x, signed) {
    if (!isNum(x)) return "—";
    const v = tidy(x);
    const s = v.toFixed(8);
    return signed && v > 0 ? "+" + s : s;
  }

  function fmtQuote(x, digits, signed) {
    if (!isNum(x)) return "—";
    const v = tidy(x);
    const s = v.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
    return signed && v > 0 ? "+" + s : s;
  }

  function fmtBps(x) {
    if (!isNum(x)) return "—";
    // A fill AT the touch computes as -1.9e-12 bps; that is zero, not "-0.00".
    const v = Math.abs(x) < 0.005 ? 0 : x;
    return (v > 0 ? "+" : "") + v.toFixed(2);
  }

  function fmtTime(ms) {
    if (!isNum(ms) || ms <= 0) return "—";
    const d = new Date(ms);
    const pad = (n) => String(n).padStart(2, "0");
    return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  }

  function fmtAge(ms) {
    if (!isNum(ms) || ms <= 0) return "chưa đọc";
    const sec = Math.max(0, Math.round((Date.now() - ms) / 1000));
    return sec < 1 ? "vừa đọc" : `đọc ${sec}s trước`;
  }

  function signCls(x) {
    if (!isNum(x) || tidy(x) === 0) return "";
    return x > 0 ? "pos" : "neg";
  }

  function setText(id, text, cls) {
    const node = $(id);
    if (!node) return;
    node.textContent = text;
    if (cls !== undefined) node.className = cls;
  }

  // Every call carries X-Execportal-Action — "read" for a GET — because the
  // server refuses any /api/ request without it: that header is what a page on
  // another site cannot send without a preflight.
  async function api(path, options) {
    try {
      const opts = Object.assign({ cache: "no-store", credentials: "same-origin", headers: { "X-Execportal-Action": "read" } }, options || {});
      const res = await fetch(path, opts);
      let body = null;
      try {
        body = await res.json();
      } catch (_) {
        body = { error_vi: `phản hồi HTTP ${res.status} không phải JSON` };
      }
      return { ok: res.ok, status: res.status, body };
    } catch (err) {
      return { ok: false, status: 0, body: { error_vi: "không gọi được portal: " + (err && err.message ? err.message : err) } };
    }
  }

  function post(action, path, payload) {
    return api(path, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Execportal-Action": action },
      body: JSON.stringify(payload),
    });
  }

  function q(path) {
    return `${path}?symbol=${encodeURIComponent(state.symbol)}`;
  }

  // poll runs fn every `every` ms, never overlapping itself.
  function poll(fn, every) {
    let running = false;
    const tick = async () => {
      if (!running && state.symbol) {
        running = true;
        try {
          await fn();
        } catch (err) {
          console.warn("poll", err);
        } finally {
          running = false;
        }
      }
      setTimeout(tick, every);
    };
    tick();
    return tick;
  }

  function markPortal(ok, line) {
    state.portalOK = ok;
    $("portal-dot").className = "dot " + (ok ? "ok" : "bad");
    $("portal-line").textContent = line;
  }

  // -------------------------------------------------------------- status

  async function refreshStatus() {
    const r = await api("/api/status");
    if (!r.ok) {
      markPortal(false, r.body.error_vi || "mất kết nối portal");
      return;
    }
    const s = r.body;
    state.status = s;
    const creds = [s.spot.configured ? "spot ✓" : "spot ✗", s.futures.configured ? "futures ✓" : "futures ✗"].join(" · ");
    markPortal(true, `portal ${s.listen} · chạy ${Math.floor(s.uptime_sec / 60)} phút · ${creds}`);
    $("footer-hosts").textContent = (s.testnet_hosts || []).join(", ");
    if (s.busy) {
      setText("busy-line", `đang chạy: ${s.busy_action} từ ${fmtTime(s.busy_since_ms)}`);
    } else if (!state.acting) {
      setText("busy-line", "");
    }
    const notional = $("notional");
    if (isNum(s.max_notional_quote)) notional.max = String(s.max_notional_quote);
    syncButtons();
  }

  function fillSymbols(symbols) {
    const select = $("symbol");
    clear(select);
    let remembered = null;
    try {
      remembered = window.localStorage.getItem("execportal.symbol");
    } catch (_) {
      remembered = null;
    }
    for (const sym of symbols) select.append(el("option", { text: sym }));
    state.symbol = symbols.includes(remembered) ? remembered : symbols[0];
    select.value = state.symbol;
    select.addEventListener("change", () => {
      state.symbol = select.value;
      try {
        window.localStorage.setItem("execportal.symbol", state.symbol);
      } catch (_) {
        /* per-viewer convenience only */
      }
      state.positions = state.account = state.market = state.intents = null;
      refreshAll();
    });
  }

  // ------------------------------------------------------------- account

  function renderConn(id, m) {
    const root = $(id);
    const pick = (role) => root.querySelector(`[data-role="${role}"]`);
    const pill = pick("pill");
    const err = pick("err");
    pick("host").textContent = m ? m.host : "—";
    if (!m || !m.configured) {
      pill.textContent = "CHƯA CẤU HÌNH";
      pill.className = "pill bad";
    } else if (m.error_vi) {
      pill.textContent = "LỖI";
      pill.className = "pill bad";
    } else {
      pill.textContent = "OK";
      pill.className = "pill ok";
    }
    pick("ping").textContent = m && isNum(m.ping_ms) ? `${m.ping_ms} ms` : "—";
    pick("skew").textContent = m && isNum(m.clock_skew_ms) ? `${m.clock_skew_ms > 0 ? "+" : ""}${m.clock_skew_ms} ms` : "—";
    const used = m ? m.weight_used_1m : 0;
    const limit = m ? m.weight_limit_1m : 0;
    pick("weight").textContent = m && limit ? `${used} / ${limit}` : "—";
    const meter = pick("meter");
    const frac = limit ? Math.min(1, used / limit) : 0;
    meter.firstElementChild.style.width = `${(frac * 100).toFixed(1)}%`;
    meter.classList.toggle("hot", frac > 0.6);
    const message = m ? [m.banned ? "SÀN ĐANG CẤM IP (418)" : "", m.error_vi || ""].filter(Boolean).join(" · ") : "";
    err.textContent = message;
    err.hidden = !message;
  }

  function renderBalance(id, list, asset, note) {
    const root = $(id);
    const pick = (role) => root.querySelector(`[data-role="${role}"]`);
    pick("asset").textContent = asset || "—";
    const row = (list || []).find((b) => b.asset === asset);
    if (!row) {
      pick("value").textContent = "—";
      pick("meta").textContent = list ? "sàn không liệt kê tài sản này" : "chưa đọc được";
      return;
    }
    const digits = asset === "USDT" || asset === "USDC" ? 2 : 8;
    pick("value").textContent = fmtQuote(row.total_qty_in_asset, digits);
    pick("meta").textContent = `tự do ${fmtQuote(row.free_qty_in_asset, digits)} · khoá ${fmtQuote(row.locked_qty_in_asset, digits)}${note ? " · " + note : ""}`;
  }

  async function refreshAccount() {
    const r = await api(q("/api/account"));
    if (!r.ok) {
      renderConn("conn-spot", null);
      renderConn("conn-futures", null);
      return;
    }
    const a = r.body;
    state.account = a;
    renderConn("conn-spot", a.spot);
    renderConn("conn-futures", a.futures);
    const base = (state.market && state.market.base_asset) || (state.positions && state.positions.base_asset) || "BTC";
    const quote = (state.market && state.market.quote_asset) || "USDT";
    renderBalance("bal-spot-quote", a.spot.balances, quote);
    renderBalance("bal-spot-base", a.spot.balances, base, "gồm số dư testnet cấp sẵn");
    renderBalance("bal-futures-quote", a.futures.balances, quote);
  }

  // ----------------------------------------------------------- positions

  async function refreshPositions() {
    const r = await api(q("/api/positions"));
    if (!r.ok) {
      renderHedge({ status: "unknown", status_vi: "CHƯA XÁC ĐỊNH ĐƯỢC", reason_vi: r.body.error_vi || "không đọc được" });
      return;
    }
    state.positions = r.body;
    renderPositions(r.body);
    renderCloseChoices();
  }

  function renderHedge(p) {
    $("hedge-banner").dataset.status = p.status;
    setText("hedge-word", p.status_vi || p.status);
    const why = [p.reason_vi, p.error_vi].filter(Boolean).join(" · ");
    setText("hedge-why", why);
  }

  function renderPositions(p) {
    renderHedge(p);
    const stale = Date.now() - p.read_at_ms > STALE_AFTER_MS;
    setText("pos-age", (stale ? "CŨ — " : "") + fmtAge(p.read_at_ms), stale ? "small warn" : "small muted");

    setText("spot-qty", fmtCoin(p.spot_qty_coin));
    setText("spot-unit", `${p.base_asset || "coin"} · theo lệnh của các ý định, đọc từ sàn`);
    setText("spot-intents", String(p.tracked_intents));
    setText("spot-balance", isNum(p.spot_base_balance_qty_coin) ? `${fmtCoin(p.spot_base_balance_qty_coin)} ${p.base_asset}` : "—");

    setText("perp-qty", fmtCoin(p.perp_qty_coin, true));
    setText("perp-intents", fmtCoin(p.intents_perp_qty_coin, true));
    setText("perp-upnl", fmtQuote(p.perp_unrealized_pnl_gross_quote, 4, true), signCls(p.perp_unrealized_pnl_gross_quote));
    setText("perp-updated", fmtTime(p.perp_updated_at_ms));

    const residual = p.delta_residual_coin;
    const tol = p.tolerance_qty_coin;
    setText("delta-value", fmtCoin(residual, true));
    setText("delta-tol", isNum(tol) && tol > 0 ? `dung sai ±${fmtCoin(tol)} coin (bước khối lượng thô hơn)` : "dung sai chưa đọc được");
    const delta = $("delta");
    const good = p.status === "both_open" || p.status === "both_flat";
    delta.dataset.state = good ? "ok" : p.status === "unhedged" ? "bad" : "warn";
    const bar = $("delta-bar");
    const scale = isNum(tol) && tol > 0 ? tol * 10 : 1;
    const half = Math.min(50, (Math.abs(tidy(residual || 0)) / scale) * 50);
    const width = good ? Math.max(half, 1) : Math.max(half, 3);
    bar.style.width = `${width}%`;
    bar.style.left = residual < 0 ? `${50 - width}%` : "50%";

    const warnings = $("pos-warnings");
    clear(warnings);
    const lines = []
      .concat((p.unreadable_vi || []).map((s) => "không đọc được: " + s))
      .concat((p.working_vi || []).map((s) => "đang chạy: " + s))
      .concat((p.cache_unreadable_vi || []).map((s) => "file cache: " + s));
    for (const line of lines) warnings.append(el("li", { text: line }));
    warnings.hidden = lines.length === 0;
    if (lines.length) $("evidence").open = true;

    const body = $("pos-intents");
    clear(body);
    if (!p.intents || p.intents.length === 0) {
      body.append(el("tr", null, [el("td", { cls: "empty", text: "không có ý định nào đang được theo dõi cho symbol này" })]));
      body.firstChild.firstChild.colSpan = 6;
    } else {
      for (const h of p.intents) {
        const seen = [].concat(h.spot.seen_vi || [], h.perp.seen_vi || []).join(" · ") || "—";
        body.append(el("tr", null, [
          el("td", { text: h.intent_id }),
          el("td", { cls: "r", text: fmtCoin(h.spot.qty_coin, true) }),
          el("td", { cls: "r", text: fmtCoin(h.perp.qty_coin, true) }),
          el("td", { cls: "r " + (h.status === "unhedged" ? "neg" : ""), text: fmtCoin(h.residual_coin, true) }),
          el("td", { text: h.status }),
          el("td", { cls: "wrap", text: seen }),
        ]));
      }
    }
  }

  // Candidates for close: every tracked intent the venue says still holds
  // something, newest first.
  function renderCloseChoices() {
    const select = $("close-intent");
    const previous = select.value;
    clear(select);
    const held = ((state.positions && state.positions.intents) || []).filter(
      (h) => Math.abs(tidy(h.spot.qty_coin)) > 0 || Math.abs(tidy(h.perp.qty_coin)) > 0
    );
    if (held.length === 0) {
      select.append(el("option", { text: "— không có ý định đang giữ —" }));
      select.firstChild.value = "";
    }
    for (const h of held) {
      const opt = el("option", { text: `${h.intent_id} · spot ${fmtCoin(h.spot.qty_coin)} / perp ${fmtCoin(h.perp.qty_coin, true)}` });
      opt.value = h.intent_id;
      select.append(opt);
    }
    if (held.some((h) => h.intent_id === previous)) select.value = previous;
    syncButtons();
  }

  // -------------------------------------------------------------- market

  async function refreshMarket() {
    const r = await api(q("/api/market"));
    if (!r.ok) {
      setText("market-hint", r.body.error_vi || "không đọc được luật thị trường");
      return;
    }
    const m = r.body;
    state.market = m;
    const parts = [];
    if (isNum(m.mark_price_quote) && m.mark_price_quote > 0) parts.push(`giá đánh dấu perp ${fmtQuote(m.mark_price_quote, 2)}`);
    if (isNum(m.last_funding_rate_per_period_bps)) parts.push(`funding gần nhất ${fmtBps(m.last_funding_rate_per_period_bps)} bps mỗi chu kỳ của symbol (không quy đổi)`);
    if (m.next_funding_time_ms) parts.push(`settle kế tiếp ${fmtTime(m.next_funding_time_ms)}`);
    parts.push(`bước spot ${m.spot.step_size_coin} / perp ${m.futures.step_size_coin} coin`);
    if (m.error_vi) parts.push("⚠ " + m.error_vi);
    setText("market-hint", parts.join(" · "));
    if (isNum(m.smallest_workable_notional_quote) && m.smallest_workable_notional_quote > 0) {
      setText("notional-hint",
        `cỡ nhỏ nhất gợi ý ≈ ${fmtQuote(m.smallest_workable_notional_quote, 2)} quote (min notional spot ${m.spot.min_notional_quote} / perp ${m.futures.min_notional_quote}) · trần portal ${fmtQuote(m.max_notional_quote, 0)}`);
    }
  }

  // ------------------------------------------------------------- intents

  async function refreshIntents() {
    const r = await api(q("/api/intents"));
    const body = $("intents-body");
    if (!r.ok) {
      clear(body);
      body.append(el("tr", null, [el("td", { cls: "empty", text: r.body.error_vi || "không đọc được" })]));
      body.firstChild.firstChild.colSpan = 17;
      return;
    }
    state.intents = r.body;
    clear(body);
    const list = r.body.intents || [];
    if (list.length === 0) {
      body.append(el("tr", null, [el("td", { cls: "empty", text: "chưa có ý định nào cho symbol này" })]));
      body.firstChild.firstChild.colSpan = 17;
    }
    for (const it of list) {
      const closed = it.closed_at_ms > 0;
      body.append(el("tr", null, [
        el("td", { text: fmtTime(it.opened_at_ms) }),
        el("td", { text: it.intent_id }),
        el("td", { text: it.origin }),
        el("td", { cls: "r", text: fmtQuote(it.notional_quote, 2) }),
        el("td", { cls: "r", text: fmtCoin(it.spot_filled_qty_coin) }),
        el("td", { cls: "r", text: fmtQuote(it.spot_avg_fill_price_quote, 2) }),
        el("td", { cls: "r", text: fmtQuote(it.perp_avg_fill_price_quote, 2) }),
        el("td", { cls: "r", text: fmtBps(it.spot_entry_slippage_bps) }),
        el("td", { cls: "r", text: fmtBps(it.perp_entry_slippage_bps) }),
        el("td", { cls: "r", text: it.unhedged_window_ms ? String(it.unhedged_window_ms) : "—" }),
        el("td", { text: it.outcome + (it.tracked ? " · theo dõi" : "") }),
        el("td", { text: closed ? fmtTime(it.closed_at_ms) : "—" }),
        el("td", { cls: "r " + signCls(it.funding_received_quote), text: closed ? fmtQuote(it.funding_received_quote, 6, true) : "—" }),
        el("td", { cls: "r", text: closed ? fmtQuote(it.commission_quote, 6) : "—" }),
        el("td", { cls: "r", text: closed ? fmtQuote(it.slippage_quote, 6, true) : "—" }),
        el("td", { cls: "r " + signCls(it.realized_quote), text: closed ? fmtQuote(it.realized_quote, 6, true) : "—" }),
        el("td", { cls: "wrap", text: it.note_vi || "" }),
      ]));
    }
  }

  // -------------------------------------------------------------- orders

  async function refreshOrders() {
    const r = await api(q("/api/orders"));
    const body = $("orders-body");
    clear(body);
    if (!r.ok) {
      body.append(el("tr", null, [el("td", { cls: "empty", text: r.body.error_vi || "không đọc được" })]));
      body.firstChild.firstChild.colSpan = 10;
      setText("orders-age", "—");
      return;
    }
    const o = r.body;
    setText("orders-age", fmtAge(o.read_at_ms));
    const rows = [].concat(o.spot || [], o.futures || []);
    for (const msg of [o.spot_error_vi, o.futures_error_vi].filter(Boolean)) {
      body.append(el("tr", null, [el("td", { cls: "empty neg", text: msg })]));
      body.lastChild.firstChild.colSpan = 10;
    }
    if (rows.length === 0) {
      body.append(el("tr", null, [el("td", { cls: "empty", text: "không có lệnh nào đang mở trên hai sàn cho symbol này" })]));
      body.lastChild.firstChild.colSpan = 10;
    }
    for (const x of rows) {
      body.append(el("tr", null, [
        el("td", { text: x.market }),
        el("td", { cls: x.side === "BUY" ? "pos" : "neg", text: x.side }),
        el("td", { text: x.type }),
        el("td", { text: x.status }),
        el("td", { cls: "r", text: fmtCoin(x.qty_coin) }),
        el("td", { cls: "r", text: fmtQuote(x.price_quote, 2) }),
        el("td", { cls: "r", text: fmtCoin(x.filled_qty_coin) }),
        el("td", { text: x.reduce_only ? "có" : "" }),
        el("td", { text: x.intent_id ? `${x.intent_id} (${x.kind_vi})` : "KHÔNG thuộc ý định nào", cls: x.intent_id ? "" : "warn" }),
        el("td", { text: fmtTime(x.updated_at_ms) }),
      ]));
    }
  }

  // ------------------------------------------------------------- funding

  async function refreshFunding() {
    const r = await api(q("/api/funding"));
    const body = $("funding-body");
    const intents = $("funding-intents");
    clear(body);
    clear(intents);
    if (!r.ok) {
      setText("funding-head", r.body.error_vi || "không đọc được funding");
      return;
    }
    const f = r.body;
    setText("funding-age", fmtAge(f.read_at_ms));
    const totals = Object.entries(f.totals_by_asset || {}).map(([asset, v]) => `${fmtQuote(v, 8, true)} ${asset}`).join(" · ") || "0";
    const head = [
      `7 ngày ${fmtTime(f.window_start_ms)} → ${fmtTime(f.window_end_ms)}: ${(f.rows || []).length} mốc, tổng ${totals}`,
      isNum(f.last_funding_rate_per_period_bps) ? `mức gần nhất ${fmtBps(f.last_funding_rate_per_period_bps)} bps mỗi chu kỳ` : "",
      f.next_funding_time_ms ? `settle kế tiếp ${fmtTime(f.next_funding_time_ms)}` : "",
      f.error_vi ? "⚠ " + f.error_vi : "",
    ].filter(Boolean).join(" · ");
    setText("funding-head", head + " — " + (f.note_vi || ""));

    if (!f.rows || f.rows.length === 0) {
      body.append(el("tr", null, [el("td", { cls: "empty", text: "sàn không liệt kê mốc settle nào trong cửa sổ — chưa từng giữ perp qua mốc (quy tắc 6)" })]));
      body.firstChild.firstChild.colSpan = 5;
    }
    for (const row of f.rows || []) {
      body.append(el("tr", null, [
        el("td", { text: fmtTime(row.settled_at_ms) }),
        el("td", { cls: "r " + signCls(row.income_qty_in_asset), text: fmtQuote(row.income_qty_in_asset, 8, true) }),
        el("td", { text: row.asset }),
        el("td", { text: row.tran_id }),
        el("td", { text: (row.intent_ids || []).join(", ") || "không ý định nào giữ qua mốc", cls: (row.intent_ids || []).length ? "" : "warn" }),
      ]));
    }
    if (!f.intents || f.intents.length === 0) {
      intents.append(el("tr", null, [el("td", { cls: "empty", text: "không có ý định nào đã mở được hai chân" })]));
      intents.firstChild.firstChild.colSpan = 8;
    }
    for (const it of f.intents || []) {
      const notes = [];
      if (it.outside_window) notes.push("cửa sổ giữ vượt 7 ngày đã đọc — số sàn chưa đầy đủ");
      if (it.shared_rows) notes.push(`${it.shared_rows} dòng chung với ý định khác, không chia`);
      if (it.other_asset_rows) notes.push(`${it.other_asset_rows} dòng bằng tài sản khác quote, không cộng vào`);
      if (!it.closed_at_ms) notes.push("chưa đóng — cache chưa có số funding");
      intents.append(el("tr", null, [
        el("td", { text: it.intent_id }),
        el("td", { text: `${fmtTime(it.opened_at_ms)} → ${it.closed_at_ms ? fmtTime(it.closed_at_ms) : "đang giữ"}` }),
        el("td", { cls: "r", text: fmtCoin(it.perp_qty_coin) }),
        el("td", { cls: "r", text: it.closed_at_ms ? fmtQuote(it.cached_funding_received_quote, 8, true) : "—" }),
        el("td", { cls: "r", text: fmtQuote(it.venue_rows_quote, 8, true) }),
        el("td", { cls: "r", text: String(it.venue_rows) }),
        el("td", { cls: "r", text: String(it.shared_rows) }),
        el("td", { cls: "wrap", text: notes.join(" · ") }),
      ]));
    }
  }

  // ------------------------------------------------------------- actions

  function syncButtons() {
    const ready = state.portalOK && state.status && state.status.spot.configured && state.status.futures.configured;
    const busy = state.acting || (state.status && state.status.busy);
    $("btn-open").disabled = !ready || busy;
    $("btn-reconcile").disabled = !ready || busy;
    $("btn-close").disabled = !ready || busy || !$("close-intent").value;
  }

  function setActing(on, label) {
    state.acting = on;
    setText("busy-line", on ? label : "");
    syncButtons();
  }

  // confirmDialog shows the modal and resolves true only on the confirm button.
  function confirmDialog(title, rows, okLabel, okClass, extra) {
    return new Promise((resolve) => {
      const dialog = $("confirm");
      setText("confirm-title", title);
      const body = $("confirm-body");
      clear(body);
      const dl = el("dl", { cls: "kv" });
      for (const [k, v] of rows) {
        dl.append(el("dt", { text: k }), el("dd", { text: v }));
      }
      body.append(dl);
      if (extra) body.append(extra);
      const ok = $("confirm-ok");
      const cancel = $("confirm-cancel");
      ok.textContent = okLabel;
      ok.className = "btn " + okClass;
      ok.disabled = false;
      let settled = false;
      const finish = (answer) => {
        if (settled) return;
        settled = true;
        ok.removeEventListener("click", onOK);
        cancel.removeEventListener("click", onCancel);
        dialog.removeEventListener("cancel", onCancel);
        dialog.removeEventListener("close", onClose);
        if (dialog.open) dialog.close();
        resolve(answer);
      };
      const onOK = () => finish(true);
      const onCancel = (ev) => {
        if (ev) ev.preventDefault();
        finish(false);
      };
      // Any other way the dialog closes is a "no".
      const onClose = () => finish(false);
      ok.addEventListener("click", onOK);
      cancel.addEventListener("click", onCancel);
      dialog.addEventListener("cancel", onCancel);
      dialog.addEventListener("close", onClose);
      dialog.showModal();
      cancel.focus();
    });
  }

  // unknownOutcome is what a write answers when the PORTAL did not answer: the
  // laptop slept, the tab lost the connection, the write deadline passed. The
  // server runs every order on a context detached from the request, so the
  // action may well have happened — the only honest word is "unknown".
  function unknownOutcome(action, r) {
    return [
      callout(`KẾT CỤC CHƯA RÕ — portal không trả lời (${(r.body && r.body.error_vi) || "mất kết nối"}). Lệnh ${action} CÓ THỂ đã chạy trên sàn.`, "bad"),
      callout("Đừng bấm lại. Đọc lại vị thế và lệnh ở trên (đang làm mới), hoặc chạy `go run ./cmd/execcheck -status -intent <id>`.", ""),
    ];
  }

  function callout(text, kind) {
    return el("div", { cls: "callout " + (kind || ""), text });
  }

  function showResult(title, nodes) {
    setText("result-title", title);
    const body = $("result-body");
    clear(body);
    for (const n of nodes) if (n) body.append(n);
    $("result").hidden = false;
    $("result").scrollIntoView({ behavior: "smooth", block: "nearest" });
  }

  function kvList(rows) {
    const dl = el("dl", { cls: "kv" });
    for (const [k, v, cls] of rows) dl.append(el("dt", { text: k }), el("dd", { text: v, cls: cls || "" }));
    return dl;
  }

  function eventsBlock(lines) {
    if (!lines || lines.length === 0) return null;
    const details = el("details", null, [el("summary", { cls: "small muted", text: `sự kiện của máy trạng thái (${lines.length})` })]);
    details.append(el("pre", { cls: "events", text: lines.join("\n") }));
    return details;
  }

  function legRows(name, leg, touchLabel, touch, slip) {
    return [
      [`${name} · trạng thái`, `${leg.status || "—"} · id ${leg.client_order_id || "—"} · orderId ${leg.venue_order_id || "—"}`],
      [`${name} · khớp`, `${fmtCoin(leg.filled_qty_coin)} coin @ ${fmtQuote(leg.avg_fill_price_quote, 2)}`],
      [`${name} · ${touchLabel}`, `${fmtQuote(touch, 2)} · trượt ${fmtBps(slip)} bps (dương là tệ hơn)`],
    ];
  }

  async function doOpen(ev) {
    ev.preventDefault();
    const notional = Number($("notional").value);
    const legOrder = (document.querySelector('input[name="leg_order"]:checked') || {}).value || "sequential_spot_first";
    const max = state.status ? state.status.max_notional_quote : 50000;
    if (!isNum(notional) || notional <= 0 || notional > max) {
      showResult("Không mở — dữ liệu nhập sai", [callout(`Notional phải trong (0, ${max}] quote, nhận "${$("notional").value}"`, "bad")]);
      return;
    }
    const mark = state.market && state.market.mark_price_quote;
    const rows = [
      ["Symbol", state.symbol],
      ["Notional mỗi chân", `${fmtQuote(notional, 2)} quote`],
      ["Khối lượng ước tính", isNum(mark) && mark > 0 ? `≈ ${fmtCoin(notional / mark)} coin (làm tròn xuống theo bước sàn)` : "—"],
      ["Chân 1", "MUA spot — LIMIT khớp ngay, trần trượt " + (state.status ? state.status.max_slippage_bps : "?") + " bps"],
      ["Chân 2", "BÁN perp — LIMIT khớp ngay, ký quỹ " + (state.status ? state.status.perp_margin_frac * 100 : "?") + "% notional"],
      ["Thứ tự chân", legOrder === "parallel" ? "song song" : "tuần tự, spot trước"],
      ["Hạn mỗi chân", state.status ? `${state.status.leg_timeout_ms} ms` : "—"],
      ["Sàn", state.status ? `${state.status.spot.host} + ${state.status.futures.host}` : "—"],
    ];
    const warning = callout("Nếu một chân hỏng, máy trạng thái gỡ chân kia về phẳng. Không có trạng thái thứ ba: cả hai mở, hoặc cả hai phẳng.", "");
    if (!(await confirmDialog("MỞ VỊ THẾ 2 CHÂN?", rows, "XÁC NHẬN MỞ", "primary", warning))) return;

    setActing(true, "đang mở hai chân trên testnet…");
    const r = await post("open", "/api/open", { symbol: state.symbol, notional_quote: notional, leg_order: legOrder });
    setActing(false);
    const v = r.body || {};
    if (r.status === 0) {
      showResult("MỞ — KẾT CỤC CHƯA RÕ", unknownOutcome("mở", r));
    } else if (!r.ok) {
      showResult("MỞ — KHÔNG thực hiện", [callout(v.error_vi || `HTTP ${r.status}`, "bad")]);
    } else {
      const headline = v.alarm ? "⚠ CẢNH BÁO: CÓ THỂ KHÔNG PHÒNG HỘ — DỪNG VÀ KIỂM TRA" : v.hedged ? "ĐÃ MỞ · DELTA-NEUTRAL (HEDGED)" : v.refused_before_placing ? "TỪ CHỐI TRƯỚC KHI GỬI LỆNH — không có gì trên sàn" : "KHÔNG MỞ ĐƯỢC · đã gỡ về phẳng";
      const kind = v.alarm ? "bad" : v.hedged ? "ok" : "";
      showResult(`MỞ ${v.intent_id || ""}`, [
        callout(headline, kind),
        kvList([
          ["Kết cục", `${v.outcome}${v.reduced_to_match ? " · đã thu nhỏ về chân ngắn" : ""}`],
          ["Cỡ đích", `${fmtCoin(v.target_qty_coin)} coin`],
          ["Lệch hai chân", `${fmtCoin(v.residual_qty_coin)} coin (dung sai ${fmtCoin(v.tolerance_qty_coin)})`],
          ...legRows("Spot", v.spot || {}, "giá chào bán tốt nhất lúc chụp", v.spot_best_ask_quote, v.spot_entry_slippage_bps),
          ...legRows("Perp", v.perp || {}, "giá chào mua tốt nhất lúc chụp", v.perp_best_bid_quote, v.perp_entry_slippage_bps),
          ["Cửa sổ trần", `${v.unhedged_window_ms} ms`],
          ["Gỡ vị thế", v.unwind_duration_ms ? `${v.unwind_duration_ms} ms` : "không cần"],
          ["Sổ cũ lúc quyết định", `${v.book_age_ms} ms`],
          ["Tổng thời gian", `${v.elapsed_ms} ms`],
          ["Settle kế tiếp", fmtTime(v.next_funding_time_ms)],
          ["File ý định (cache)", v.cache_file || "—"],
        ]),
        v.error_vi ? callout("Lý do: " + v.error_vi, v.alarm ? "bad" : "") : null,
        v.cache_error_vi ? callout(v.cache_error_vi, "bad") : null,
        v.spot_flat_evidence_vi ? callout("Bằng chứng phẳng: " + v.spot_flat_evidence_vi, "") : null,
        v.bracket_vi ? el("p", { cls: "small muted", text: "Ký quỹ duy trì: " + v.bracket_vi }) : null,
        eventsBlock(v.events_vi),
      ]);
    }
    refreshAll();
  }

  async function doClose() {
    const intentID = $("close-intent").value;
    if (!intentID) return;
    const held = ((state.positions && state.positions.intents) || []).find((h) => h.intent_id === intentID);
    const rows = [
      ["Ý định", intentID],
      ["Symbol", state.symbol],
      ["Sàn đang giữ (lệnh của ý định)", held ? `spot ${fmtCoin(held.spot.qty_coin, true)} · perp ${fmtCoin(held.perp.qty_coin, true)} coin` : "—"],
      ["Thứ tự", "MUA perp (reduce-only) trước, rồi BÁN spot đúng bằng phần perp đã đóng"],
      ["Loại lệnh", "MARKET, đọc lại từ sàn tới khi sàn báo xong"],
    ];
    if (!(await confirmDialog("ĐÓNG VỊ THẾ 2 CHÂN?", rows, "XÁC NHẬN ĐÓNG", "secondary"))) return;

    setActing(true, "đang đóng hai chân trên testnet…");
    const r = await post("close", "/api/close", { symbol: state.symbol, intent_id: intentID });
    setActing(false);
    const v = r.body || {};
    if (r.status === 0) {
      showResult("ĐÓNG — KẾT CỤC CHƯA RÕ", unknownOutcome("đóng", r));
    } else if (!r.ok) {
      showResult("ĐÓNG — KHÔNG thực hiện", [callout(v.error_vi || `HTTP ${r.status}`, "bad")]);
    } else {
      const headline = v.alarm ? "⚠ CẢNH BÁO: HAI CHÂN CÓ THỂ LỆCH — DỪNG VÀ KIỂM TRA"
        : v.flat ? "ĐÃ ĐÓNG · PHẲNG CẢ HAI CHÂN"
        : v.sent_unconfirmed ? "⚠ ĐÃ GỬI LỆNH ĐÓNG nhưng sàn không báo khớp — đọc lại vị thế, KHÔNG bấm đóng lại"
        : v.refused ? "TỪ CHỐI TRƯỚC KHI GỬI — không lệnh nào tới sàn"
        : "ĐÓNG CHƯA XONG — cặp vẫn phòng hộ ở cỡ nhỏ hơn";
      showResult(`ĐÓNG ${v.intent_id || intentID}`, [
        callout(headline, v.alarm || v.sent_unconfirmed ? "bad" : v.flat ? "ok" : ""),
        kvList([
          ["Kết cục", v.outcome],
          ["Ý định giữ trước khi đóng", `${fmtCoin(v.intent_qty_coin)} coin`],
          ["Đã đóng / còn lại", `${fmtCoin(v.closed_qty_coin)} / ${fmtCoin(v.remaining_qty_coin)} coin`],
          ["Vị thế perp sàn báo sau", `${fmtCoin(v.venue_perp_qty_coin, true)} coin`],
          ["Perp · đóng", `${(v.perp || {}).status || "—"} · ${fmtCoin((v.perp || {}).unwound_qty_coin)} coin @ ${fmtQuote((v.perp || {}).avg_fill_price_quote, 2)}`],
          ["Spot · đóng", `${(v.spot || {}).status || "—"} · ${fmtCoin((v.spot || {}).unwound_qty_coin)} coin @ ${fmtQuote((v.spot || {}).avg_fill_price_quote, 2)}`],
          ["Funding sàn đã trả", `${fmtQuote(v.funding_received_quote, 8, true)} · ${v.settlements_counted} mốc settle`, signCls(v.funding_received_quote)],
          ["Phí sàn thu (quote)", fmtQuote(v.commission_quote, 8)],
          ["Trượt giá (4 lần khớp)", fmtQuote(v.slippage_quote, 8, true)],
          ["RealizedQuote", fmtQuote(v.realized_quote, 8, true), signCls(v.realized_quote)],
          ["Trôi giá cặp (NGOÀI con số trên)", fmtQuote(v.pair_price_drift_quote, 8, true), signCls(v.pair_price_drift_quote)],
          ["Thời gian", `${v.elapsed_ms} ms`],
        ]),
        callout(v.realized_label_vi || "", ""),
        el("p", { cls: "small muted", text: `funding: ${v.funding_source_vi || "—"}` }),
        el("p", { cls: "small muted", text: `phí: ${v.commission_source_vi || "—"}${v.commission_other_vi ? " · " + v.commission_other_vi : ""}` }),
        el("p", { cls: "small muted", text: `trượt/trôi: ${v.price_drift_priced_vi || "—"}` }),
        v.spot_flat_evidence_vi ? callout("Bằng chứng phẳng: " + v.spot_flat_evidence_vi, "") : null,
        v.error_vi ? callout("Lý do: " + v.error_vi, v.alarm ? "bad" : "") : null,
        v.cache_error_vi ? callout(v.cache_error_vi, "bad") : null,
        eventsBlock(v.events_vi),
      ]);
    }
    refreshAll();
  }

  function planTable(plans) {
    const wrap = el("div", { cls: "table-wrap mt8" });
    const table = el("table");
    const head = el("tr", null, ["Ý định", "Spot", "Perp", "Lệch", "Việc", "Lệnh / lý do"].map((h) => el("th", { text: h })));
    table.append(el("thead", null, [head]));
    const tbody = el("tbody");
    for (const p of plans) {
      tbody.append(el("tr", null, [
        el("td", { text: p.intent_id }),
        el("td", { cls: "r", text: fmtCoin(p.spot_qty_coin, true) }),
        el("td", { cls: "r", text: fmtCoin(p.perp_qty_coin, true) }),
        el("td", { cls: "r neg", text: fmtCoin(p.residual_coin, true) }),
        el("td", { cls: p.action === "send" ? "warn" : "neg", text: p.action === "send" ? "GỬI" : "TỪ CHỐI" }),
        el("td", { cls: "wrap", text: p.action === "send" ? `${p.reason_vi} · id ${p.client_order_id}${p.reduce_only ? " · reduce-only" : ""}` : p.reason_vi }),
      ]));
    }
    table.append(tbody);
    wrap.append(table);
    return wrap;
  }

  async function doReconcile() {
    setActing(true, "đang quét mọi ý định từ sàn (chưa gửi lệnh)…");
    const dry = await post("reconcile", "/api/reconcile", { symbol: state.symbol, apply: false });
    setActing(false);
    const plan = dry.body || {};
    if (!dry.ok) {
      showResult("LÀM PHẲNG — không lập được kế hoạch", [callout(plan.error_vi || `HTTP ${dry.status}`, "bad")]);
      return;
    }
    const rows = [
      ["Symbol", state.symbol],
      ["Ý định đã quét", String(plan.intents_scanned)],
      ["Đã cân / cần gửi / từ chối", `${plan.balanced} / ${plan.to_send} / ${plan.refused}`],
      ["Perp: sàn báo / ý định giải thích", `${fmtCoin(plan.venue_perp_qty_coin, true)} / ${fmtCoin(plan.intents_perp_qty_coin, true)} coin`],
    ];
    const extra = el("div");
    extra.append(callout(plan.note_vi || "", ""));
    if (plan.conflict_vi) extra.append(callout(plan.conflict_vi, "bad"));
    for (const u of plan.cache_unreadable_vi || []) extra.append(callout("file cache không đọc được: " + u, "bad"));
    if (plan.plans && plan.plans.length) extra.append(planTable(plan.plans));

    if (plan.to_send === 0 || plan.conflict_vi) {
      const title = plan.conflict_vi ? "LÀM PHẲNG — DỪNG: bằng chứng không khớp" : "LÀM PHẲNG — không có gì để gửi";
      showResult(title, [kvList(rows.map(([k, v]) => [k, v])), extra]);
      return;
    }
    if (!(await confirmDialog("LÀM PHẲNG PHẦN LỆCH?", rows, `GỬI ${plan.to_send} LỆNH CÂN`, "danger", extra))) return;

    setActing(true, "đang gửi lệnh cân trên testnet…");
    const r = await post("reconcile", "/api/reconcile", { symbol: state.symbol, apply: true, plan_digest: plan.plan_digest });
    setActing(false);
    const v = r.body || {};
    const nodes = [];
    if (r.status === 0) {
      showResult("LÀM PHẲNG — KẾT CỤC CHƯA RÕ", unknownOutcome("cân", r));
      refreshAll();
      return;
    }
    if (!r.ok) nodes.push(callout(v.error_vi || `HTTP ${r.status}`, "bad"));
    for (const res of v.results || []) {
      nodes.push(kvList([
        ["Ý định", res.intent_id],
        ["Lệnh cân", `${res.client_order_id} · orderId ${res.venue_order_id || "—"} · ${res.status || "—"}`],
        ["Khớp", `${fmtCoin(res.filled_qty_coin)} coin @ ${fmtQuote(res.avg_fill_price_quote, 2)}`],
        ["Lệch sau khi cân", fmtCoin(res.residual_after_coin, true), res.balanced ? "pos" : "neg"],
      ]));
      if (res.error_vi) nodes.push(callout(res.error_vi, "bad"));
    }
    if (r.ok && (!v.results || v.results.length === 0)) nodes.push(callout("không có lệnh nào được gửi", ""));
    showResult("LÀM PHẲNG — kết quả", nodes);
    refreshAll();
  }

  // ---------------------------------------------------------------- boot

  function refreshAll() {
    return Promise.all([refreshMarket(), refreshAccount(), refreshPositions(), refreshOrders(), refreshIntents(), refreshFunding()]);
  }

  async function boot() {
    $("open-form").addEventListener("submit", doOpen);
    $("btn-close").addEventListener("click", doClose);
    $("btn-reconcile").addEventListener("click", doReconcile);
    $("close-intent").addEventListener("change", syncButtons);
    $("result-dismiss").addEventListener("click", () => { $("result").hidden = true; });

    let first = await api("/api/status");
    while (!first.ok) {
      markPortal(false, first.body.error_vi || "không kết nối được portal — thử lại sau 3 giây");
      await new Promise((resolve) => setTimeout(resolve, 3000));
      first = await api("/api/status");
    }
    state.status = first.body;
    fillSymbols(first.body.symbols || []);
    await refreshStatus();

    poll(refreshStatus, POLL_STATUS_MS);
    poll(refreshMarket, POLL_MARKET_MS);
    poll(refreshAccount, POLL_FAST_MS);
    poll(refreshPositions, POLL_FAST_MS);
    poll(refreshOrders, POLL_ORDERS_MS);
    poll(refreshIntents, POLL_INTENTS_MS);
    poll(refreshFunding, POLL_FUNDING_MS);
  }

  boot();
})();
