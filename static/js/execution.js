// Execution Control — Strategy 1's two-leg position on Binance TESTNET.
//
// This is the only tab that writes. Nothing is sent to /api/open, /api/close or
// /api/reconcile before the operator confirms the dialog; the action header is
// set only on that path and the server refuses a write without it. The tab
// reads nothing from the Scanner or Paper tabs — no signal reaches a button.
//
// The Auto-Trader card (PLAN Q18) switches the server's testnet bot on and off.
// The bot's own orders are placed by the server, through the same open and
// close these buttons use; this page only starts it (confirmed), stops it, or
// kills it (confirmed), and shows what it decided.

import { $, el, clear, setText, isNum, tidy, fmt, signCls, api, post, schedule, emptyRow } from "./core.js";
import { shell } from "./shell.js";

const FAST_MS = 3000;
const BACKGROUND_MS = 15000;
const SLOW_MS = 15000;
const FUNDING_MS = 60000;
const MARKET_MS = 30000;
const STALE_AFTER_MS = 12000;

const state = {
  symbol: null,
  status: null,
  account: null,
  positions: null,
  market: null,
  acting: false,
  portalOK: false,
  polls: [],
  hedges: {},
};

const execActive = () => shell.isActive("execution");
const q = (path) => `${path}?symbol=${encodeURIComponent(state.symbol)}`;

function baseAsset() {
  return (state.market && state.market.base_asset) || (state.positions && state.positions.base_asset) || (state.symbol || "BTCUSDT").replace(/USDT$|USDC$|USD$/, "");
}

// ------------------------------------------------------------------ status

export function onStatus(s) {
  state.status = s;
  state.portalOK = true;
  // A symbol the portal no longer trades must not keep an alarm alive.
  for (const symbol of Object.keys(state.hedges)) {
    if (!(s.symbols || []).includes(symbol)) delete state.hedges[symbol];
  }
  const creds = [s.spot.configured ? "spot ✓" : "spot ✗", s.futures.configured ? "futures ✓" : "futures ✗"].join(" · ");
  setText("portal-line", `${s.listen} · chạy ${Math.floor(s.uptime_sec / 60)} phút · ${creds}`);
  setText("portal-pill", "OK", "pill ok");
  if (s.busy) {
    setText("busy-line", `đang chạy: ${s.busy_action} từ ${fmt.time(s.busy_since_ms)}`);
  } else if (!state.acting) {
    setText("busy-line", "");
  }
  if (isNum(s.max_notional_quote)) $("notional").max = String(s.max_notional_quote);
  syncButtons();
}

export function onPortalDown(message) {
  state.portalOK = false;
  setText("portal-line", message);
  setText("portal-pill", "MẤT", "pill bad");
  syncButtons();
}

function fillSymbols(symbols) {
  const select = $("ex-symbol");
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
    state.positions = state.account = state.market = null;
    refreshAll();
  });
}

// ----------------------------------------------------------------- account

function renderConn(id, m) {
  const root = $(id);
  const pick = (role) => root.querySelector(`[data-role="${role}"]`);
  const pill = pick("pill");
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
  const err = pick("err");
  const message = m ? [m.banned ? "SÀN ĐANG CẤM IP (418)" : "", m.error_vi || ""].filter(Boolean).join(" · ") : "";
  err.textContent = message;
  err.hidden = !message;
}

function balanceLine(list, asset, digits) {
  const row = (list || []).find((b) => b.asset === asset);
  if (!row) return list ? `${asset}: sàn không liệt kê` : "chưa đọc được";
  return `${fmt.quote(row.free_qty_in_asset, digits)} ${asset} tự do · khoá ${fmt.quote(row.locked_qty_in_asset, digits)}`;
}

async function refreshAccount() {
  if (!state.symbol) return;
  const r = await api(q("/api/account"));
  if (!r.ok) {
    renderConn("conn-spot", null);
    renderConn("conn-futures", null);
    shell.renderPortalDown(r.body.error_vi || `HTTP ${r.status}`);
    shell.renderAccountFailed(r.body.error_vi || `HTTP ${r.status}`);
    return;
  }
  const a = r.body;
  if (a.symbol && a.symbol !== state.symbol) return;
  state.account = a;
  renderConn("conn-spot", a.spot);
  renderConn("conn-futures", a.futures);
  const quote = (state.market && state.market.quote_asset) || "USDT";
  setText("ex-bal-spot", balanceLine(a.spot.balances, quote, 2));
  setText("ex-bal-futures", balanceLine(a.futures.balances, quote, 2));
  setText("ex-bal-base", balanceLine(a.spot.balances, baseAsset(), 8));
  shell.renderAccount(a, baseAsset());
}

// --------------------------------------------------------------- positions

function unreadPositions(r) {
  return { status: "unknown", status_vi: "CHƯA XÁC ĐỊNH ĐƯỢC", reason_vi: r.body.error_vi || "không đọc được" };
}

// keepAlarm stores a symbol's newest reading for the header — except that a
// reading which could not decide ("unknown": a failed read, an exhausted read
// budget) does not erase an alarm: the last unhedged or conflicting state
// stays, marked stale, until a reading that decides replaces it.
function keepAlarm(symbol, next) {
  const prev = state.hedges[symbol];
  // A reading older than the one held (a slow background read for a symbol
  // just refreshed in front) is dropped.
  const held = prev && (prev.alarm || prev);
  const alarming = next.status === "unhedged" || next.status === "evidence_conflict";
  // An alarm is never dropped for its stamp: a server clock stepped back would
  // otherwise hide every fresh unhedged reading behind an older green one.
  if (!alarming && held && next.read_at_ms && held.read_at_ms && next.read_at_ms < held.read_at_ms) return;
  // The last DECIDED alarm is kept aside once, so a run of undecided readings
  // re-labels it instead of wrapping its text again on every poll.
  const alarm = prev && (prev.stale ? prev.alarm : prev);
  if (next.status === "unknown" && alarm && (alarm.status === "unhedged" || alarm.status === "evidence_conflict")) {
    state.hedges[symbol] = Object.assign({}, alarm, {
      stale: true,
      alarm,
      reason_vi: `CŨ — lần đọc mới không quyết được (${next.reason_vi || "?"}); lần cuối đọc được ${fmt.time(alarm.read_at_ms)}: ${alarm.reason_vi || ""}`,
    });
    return;
  }
  state.hedges[symbol] = next;
}

async function refreshPositions() {
  if (!state.symbol) return;
  const symbol = state.symbol;
  const r = await api(q("/api/positions"));
  // A slow answer for the symbol selected before must not land under the new
  // one's label.
  if (symbol !== state.symbol) return;
  if (!r.ok) {
    const p = unreadPositions(r);
    keepAlarm(symbol, p);
    renderHedge(p);
    shell.renderHedges(state.hedges);
    return;
  }
  state.positions = r.body;
  keepAlarm(symbol, r.body);
  renderPositions(r.body);
  renderCloseChoices();
  shell.renderHedges(state.hedges);
}

// The other tradable symbols, for the header chip only, at the background rate.
async function refreshOtherHedges() {
  const others = ((state.status && state.status.symbols) || []).filter((s) => s !== state.symbol);
  for (const symbol of others) {
    const r = await api(`/api/positions?symbol=${encodeURIComponent(symbol)}`);
    keepAlarm(symbol, r.ok ? r.body : unreadPositions(r));
  }
  if (others.length) shell.renderHedges(state.hedges);
}

// The position card's age runs every second, so a read that stopped coming
// back turns the card stale on its own.
function renderPositionsAge() {
  const at = state.positionsReadAtMs;
  const stale = !at || Date.now() - at > STALE_AFTER_MS;
  setText("pos-age", at ? (stale ? "CŨ — " : "") + fmt.age(at) : "chưa đọc", stale ? "hint warn" : "hint");
}

function renderHedge(p) {
  $("hedge-banner").dataset.status = p.status;
  setText("hedge-word", p.status_vi || p.status);
  const parts = [p.reason_vi, p.error_vi];
  const kept = state.hedges[state.symbol];
  if (p.status === "unknown" && kept && kept.stale && kept.alarm) {
    // The header chip still shows the last decided alarm; say so here too.
    parts.push(`Chip đầu trang giữ báo động cuối cùng đọc được (${kept.alarm.status_vi || kept.alarm.status}, ${fmt.time(kept.alarm.read_at_ms)}) cho tới khi một lần đọc quyết được.`);
  }
  setText("hedge-why", parts.filter(Boolean).join(" · "));
}

function renderPositions(p) {
  renderHedge(p);
  state.positionsReadAtMs = p.read_at_ms;
  renderPositionsAge();

  setText("spot-qty", fmt.coin(p.spot_qty_coin));
  setText("spot-unit", `${p.base_asset || "coin"} · theo lệnh của các ý định, đọc từ sàn`);
  setText("spot-intents", String(p.tracked_intents));
  setText("spot-balance", isNum(p.spot_base_balance_qty_coin) ? `${fmt.coin(p.spot_base_balance_qty_coin)} ${p.base_asset}` : "—");

  setText("perp-qty", fmt.coin(p.perp_qty_coin, true));
  setText("perp-intents", fmt.coin(p.intents_perp_qty_coin, true));
  setText("perp-upnl", fmt.quote(p.perp_unrealized_pnl_gross_quote, 4, true), signCls(p.perp_unrealized_pnl_gross_quote));
  setText("perp-updated", fmt.time(p.perp_updated_at_ms));

  const residual = p.delta_residual_coin;
  const tol = p.tolerance_qty_coin;
  setText("delta-value", fmt.coin(residual, true));
  setText("delta-tol", isNum(tol) && tol > 0 ? `dung sai ±${fmt.coin(tol)} coin (bước khối lượng thô hơn)` : "dung sai chưa đọc được");
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
  if (!p.intents || p.intents.length === 0) {
    emptyRow(body, 6, "không có ý định nào đang được theo dõi cho symbol này");
    return;
  }
  clear(body);
  for (const h of p.intents) {
    const seen = [].concat(h.spot.seen_vi || [], h.perp.seen_vi || []).join(" · ") || "—";
    body.append(el("tr", null, [
      el("td", { cls: "mono", text: h.intent_id }),
      el("td", { cls: "r", text: fmt.coin(h.spot.qty_coin, true) }),
      el("td", { cls: "r", text: fmt.coin(h.perp.qty_coin, true) }),
      el("td", { cls: "r " + (h.status === "unhedged" ? "neg" : ""), text: fmt.coin(h.residual_coin, true) }),
      el("td", { text: h.status }),
      el("td", { cls: "wrap", text: seen }),
    ]));
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
    const opt = el("option", { text: "— không có ý định đang giữ —" });
    opt.value = "";
    select.append(opt);
  }
  for (const h of held) {
    const opt = el("option", { text: `${h.intent_id} · spot ${fmt.coin(h.spot.qty_coin)} / perp ${fmt.coin(h.perp.qty_coin, true)}` });
    opt.value = h.intent_id;
    select.append(opt);
  }
  if (held.some((h) => h.intent_id === previous)) select.value = previous;
  syncButtons();
}

// ------------------------------------------------------------------ market

async function refreshMarket() {
  if (!state.symbol) return;
  const r = await api(q("/api/market"));
  if (!r.ok) {
    setText("market-hint", r.body.error_vi || "không đọc được luật thị trường");
    return;
  }
  const m = r.body;
  state.market = m;
  const parts = [];
  if (isNum(m.mark_price_quote) && m.mark_price_quote > 0) parts.push(`giá đánh dấu perp ${fmt.quote(m.mark_price_quote, 2)}`);
  if (isNum(m.last_funding_rate_per_period_bps)) parts.push(`funding gần nhất ${fmt.bps(m.last_funding_rate_per_period_bps)} bps mỗi chu kỳ của symbol (không quy đổi)`);
  if (m.next_funding_time_ms) parts.push(`settle kế tiếp ${fmt.time(m.next_funding_time_ms)}`);
  parts.push(`bước spot ${m.spot.step_size_coin} / perp ${m.futures.step_size_coin} coin`);
  if (m.error_vi) parts.push("⚠ " + m.error_vi);
  setText("market-hint", parts.join(" · "));
  if (isNum(m.smallest_workable_notional_quote) && m.smallest_workable_notional_quote > 0) {
    setText("notional-hint",
      `cỡ nhỏ nhất gợi ý ≈ ${fmt.quote(m.smallest_workable_notional_quote, 2)} quote (min notional spot ${m.spot.min_notional_quote} / perp ${m.futures.min_notional_quote}) · trần portal ${fmt.quote(m.max_notional_quote, 0)}`);
  }
}

// ----------------------------------------------------------------- intents

async function refreshIntents() {
  if (!state.symbol) return;
  const r = await api(q("/api/intents"));
  const body = $("intents-body");
  if (!r.ok) {
    emptyRow(body, 17, r.body.error_vi || "không đọc được", "neg");
    return;
  }
  const list = r.body.intents || [];
  if (list.length === 0) {
    emptyRow(body, 17, "chưa có ý định nào cho symbol này");
    return;
  }
  clear(body);
  for (const it of list) {
    const closed = it.closed_at_ms > 0;
    body.append(el("tr", null, [
      el("td", { text: fmt.time(it.opened_at_ms) }),
      el("td", { cls: "mono", text: it.intent_id }),
      el("td", { text: it.origin }),
      el("td", { cls: "r", text: fmt.quote(it.notional_quote, 2) }),
      el("td", { cls: "r", text: fmt.coin(it.spot_filled_qty_coin) }),
      el("td", { cls: "r", text: fmt.quote(it.spot_avg_fill_price_quote, 2) }),
      el("td", { cls: "r", text: fmt.quote(it.perp_avg_fill_price_quote, 2) }),
      el("td", { cls: "r", text: fmt.bps(it.spot_entry_slippage_bps) }),
      el("td", { cls: "r", text: fmt.bps(it.perp_entry_slippage_bps) }),
      el("td", { cls: "r", text: it.unhedged_window_ms ? String(it.unhedged_window_ms) : "—" }),
      el("td", { text: it.outcome + (it.tracked ? " · theo dõi" : "") }),
      el("td", { text: closed ? fmt.time(it.closed_at_ms) : "—" }),
      el("td", { cls: "r " + signCls(it.funding_received_quote), text: closed ? fmt.quote(it.funding_received_quote, 6, true) : "—" }),
      el("td", { cls: "r", text: closed ? fmt.quote(it.commission_quote, 6) : "—" }),
      el("td", { cls: "r", text: closed ? fmt.quote(it.slippage_quote, 6, true) : "—" }),
      el("td", { cls: "r " + signCls(it.realized_quote), text: closed ? fmt.quote(it.realized_quote, 6, true) : "—" }),
      el("td", { cls: "wrap", text: it.note_vi || "" }),
    ]));
  }
}

// ------------------------------------------------------------------ orders

async function refreshOrders() {
  if (!state.symbol) return;
  const r = await api(q("/api/orders"));
  const body = $("orders-body");
  if (!r.ok) {
    emptyRow(body, 10, r.body.error_vi || "không đọc được", "neg");
    setText("orders-age", "—");
    return;
  }
  const o = r.body;
  setText("orders-age", fmt.age(o.read_at_ms));
  clear(body);
  const rows = [].concat(o.spot || [], o.futures || []);
  for (const msg of [o.spot_error_vi, o.futures_error_vi].filter(Boolean)) {
    const td = el("td", { cls: "empty neg", text: msg });
    td.colSpan = 10;
    body.append(el("tr", null, [td]));
  }
  if (rows.length === 0) {
    const td = el("td", { cls: "empty", text: "không có lệnh nào đang mở trên hai sàn cho symbol này" });
    td.colSpan = 10;
    body.append(el("tr", null, [td]));
  }
  for (const x of rows) {
    body.append(el("tr", null, [
      el("td", { text: x.market }),
      el("td", { cls: x.side === "BUY" ? "pos" : "neg", text: x.side }),
      el("td", { text: x.type }),
      el("td", { text: x.status }),
      el("td", { cls: "r", text: fmt.coin(x.qty_coin) }),
      el("td", { cls: "r", text: fmt.quote(x.price_quote, 2) }),
      el("td", { cls: "r", text: fmt.coin(x.filled_qty_coin) }),
      el("td", { text: x.reduce_only ? "có" : "" }),
      el("td", { text: x.intent_id ? `${x.intent_id} (${x.kind_vi})` : "KHÔNG thuộc ý định nào", cls: x.intent_id ? "mono" : "warn" }),
      el("td", { text: fmt.time(x.updated_at_ms) }),
    ]));
  }
}

// ----------------------------------------------------------------- funding

async function refreshFunding() {
  if (!state.symbol) return;
  const r = await api(q("/api/funding"));
  const body = $("funding-body");
  const intents = $("funding-intents");
  if (!r.ok) {
    setText("funding-head", r.body.error_vi || "không đọc được funding");
    clear(body);
    clear(intents);
    return;
  }
  const f = r.body;
  setText("funding-age", fmt.age(f.read_at_ms));
  const totals = Object.entries(f.totals_by_asset || {}).map(([asset, v]) => `${fmt.quote(v, 8, true)} ${asset}`).join(" · ") || "0";
  const head = [
    `7 ngày ${fmt.time(f.window_start_ms)} → ${fmt.time(f.window_end_ms)}: ${(f.rows || []).length} mốc, tổng ${totals}`,
    isNum(f.last_funding_rate_per_period_bps) ? `mức gần nhất ${fmt.bps(f.last_funding_rate_per_period_bps)} bps mỗi chu kỳ` : "",
    f.next_funding_time_ms ? `settle kế tiếp ${fmt.time(f.next_funding_time_ms)}` : "",
    f.error_vi ? "⚠ " + f.error_vi : "",
  ].filter(Boolean).join(" · ");
  setText("funding-head", head + " — " + (f.note_vi || ""));

  if (!f.rows || f.rows.length === 0) {
    emptyRow(body, 5, "sàn không liệt kê mốc settle nào trong cửa sổ — chưa từng giữ perp qua mốc (quy tắc 6)");
  } else {
    clear(body);
    for (const row of f.rows) {
      body.append(el("tr", null, [
        el("td", { text: fmt.time(row.settled_at_ms) }),
        el("td", { cls: "r " + signCls(row.income_qty_in_asset), text: fmt.quote(row.income_qty_in_asset, 8, true) }),
        el("td", { text: row.asset }),
        el("td", { cls: "mono", text: row.tran_id }),
        el("td", { text: (row.intent_ids || []).join(", ") || "không ý định nào giữ qua mốc", cls: (row.intent_ids || []).length ? "" : "warn" }),
      ]));
    }
  }
  if (!f.intents || f.intents.length === 0) {
    emptyRow(intents, 8, "không có ý định nào đã mở được hai chân");
    return;
  }
  clear(intents);
  for (const it of f.intents) {
    const notes = [];
    if (it.outside_window) notes.push("cửa sổ giữ vượt 7 ngày đã đọc — số sàn chưa đầy đủ");
    if (it.shared_rows) notes.push(`${it.shared_rows} dòng chung với ý định khác, không chia`);
    if (it.other_asset_rows) notes.push(`${it.other_asset_rows} dòng bằng tài sản khác quote, không cộng vào`);
    if (!it.closed_at_ms) notes.push("chưa đóng — cache chưa có số funding");
    intents.append(el("tr", null, [
      el("td", { cls: "mono", text: it.intent_id }),
      el("td", { text: `${fmt.time(it.opened_at_ms)} → ${it.closed_at_ms ? fmt.time(it.closed_at_ms) : "đang giữ"}` }),
      el("td", { cls: "r", text: fmt.coin(it.perp_qty_coin) }),
      el("td", { cls: "r", text: it.closed_at_ms ? fmt.quote(it.cached_funding_received_quote, 8, true) : "—" }),
      el("td", { cls: "r", text: fmt.quote(it.venue_rows_quote, 8, true) }),
      el("td", { cls: "r", text: String(it.venue_rows) }),
      el("td", { cls: "r", text: String(it.shared_rows) }),
      el("td", { cls: "wrap", text: notes.join(" · ") }),
    ]));
  }
}

// ----------------------------------------------------------------- actions

function syncButtons() {
  const s = state.status;
  const ready = state.portalOK && s && s.spot.configured && s.futures.configured;
  const busy = state.acting || (s && s.busy);
  $("btn-open").disabled = !ready || busy;
  $("btn-reconcile").disabled = !ready || busy;
  $("btn-close").disabled = !ready || busy || !$("close-intent").value;
  syncAtButtons();
}

function setActing(on, label) {
  state.acting = on;
  setText("busy-line", on ? label : "");
  syncButtons();
}

// confirmDialog shows the modal and resolves true only on the confirm button;
// any other way the dialog closes is a "no".
function confirmDialog(title, rows, okLabel, okClass, extra) {
  return new Promise((resolve) => {
    const dialog = $("confirm");
    setText("confirm-title", title);
    const body = $("confirm-body");
    clear(body);
    body.append(kvList(rows));
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
    // Only a real press confirms. A scripted press is refused — that guards
    // against stray page code, not against a hostile script, which could call
    // the API directly (PLAN Q17, named debt).
    const onOK = (ev) => finish(Boolean(ev && ev.isTrusted));
    const onCancel = (ev) => {
      if (ev) ev.preventDefault();
      finish(false);
    };
    const onClose = () => finish(false);
    ok.addEventListener("click", onOK);
    cancel.addEventListener("click", onCancel);
    dialog.addEventListener("cancel", onCancel);
    dialog.addEventListener("close", onClose);
    dialog.showModal();
    cancel.focus();
  });
}

// unknownOutcome is what a write answers when the PORTAL did not answer. The
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
  const details = el("details", null, [el("summary", { text: `sự kiện của máy trạng thái (${lines.length})` })]);
  details.append(el("pre", { cls: "events", text: lines.join("\n") }));
  return details;
}

function legRows(name, leg, touchLabel, touch, slip) {
  return [
    [`${name} · trạng thái`, `${leg.status || "—"} · id ${leg.client_order_id || "—"} · orderId ${leg.venue_order_id || "—"}`],
    [`${name} · khớp`, `${fmt.coin(leg.filled_qty_coin)} coin @ ${fmt.quote(leg.avg_fill_price_quote, 2)}`],
    [`${name} · ${touchLabel}`, `${fmt.quote(touch, 2)} · trượt ${fmt.bps(slip)} bps (dương là tệ hơn)`],
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
  const s = state.status;
  const rows = [
    ["Symbol", state.symbol],
    ["Notional mỗi chân", `${fmt.quote(notional, 2)} quote`],
    ["Khối lượng ước tính", isNum(mark) && mark > 0 ? `≈ ${fmt.coin(notional / mark)} coin (làm tròn xuống theo bước sàn)` : "—"],
    ["Chân 1", "MUA spot — LIMIT khớp ngay, trần trượt " + (s ? s.max_slippage_bps : "?") + " bps"],
    ["Chân 2", "BÁN perp — LIMIT khớp ngay, ký quỹ " + (s ? s.perp_margin_frac * 100 : "?") + "% notional"],
    ["Thứ tự chân", legOrder === "parallel" ? "song song" : "tuần tự, spot trước"],
    ["Hạn mỗi chân", s ? `${s.leg_timeout_ms} ms` : "—"],
    ["Sàn", s ? `${s.spot.host} + ${s.futures.host}` : "—"],
  ];
  const warning = callout("Nếu một chân hỏng, máy trạng thái gỡ chân kia về phẳng. Không có trạng thái thứ ba: cả hai mở, hoặc cả hai phẳng.", "");
  if (!(await confirmDialog("MỞ VỊ THẾ 2 CHÂN?", rows, "XÁC NHẬN MỞ", "primary", warning))) return;

  setActing(true, "đang mở hai chân trên testnet…");
  const r = await post("open", "/api/open", { symbol: state.symbol, notional_quote: notional, leg_order: legOrder });
  setActing(false);
  const v = r.body || {};
  if (r.status === 0 || r.unreadable) {
    showResult("MỞ — KẾT CỤC CHƯA RÕ", unknownOutcome("mở", r));
  } else if (!r.ok) {
    showResult("MỞ — KHÔNG thực hiện", [callout(v.error_vi || `HTTP ${r.status}`, "bad")]);
  } else {
    const headline = v.alarm ? "⚠ CẢNH BÁO: CÓ THỂ KHÔNG PHÒNG HỘ — DỪNG VÀ KIỂM TRA"
      : v.hedged ? "ĐÃ MỞ · DELTA-NEUTRAL (HEDGED)"
      : v.refused_before_placing ? "TỪ CHỐI TRƯỚC KHI GỬI LỆNH — không có gì trên sàn"
      : "KHÔNG MỞ ĐƯỢC · đã gỡ về phẳng";
    const kind = v.alarm ? "bad" : v.hedged ? "ok" : "";
    showResult(`MỞ ${v.intent_id || ""}`, [
      callout(headline, kind),
      kvList([
        ["Kết cục", `${v.outcome}${v.reduced_to_match ? " · đã thu nhỏ về chân ngắn" : ""}`],
        ["Cỡ đích", `${fmt.coin(v.target_qty_coin)} coin`],
        ["Lệch hai chân", `${fmt.coin(v.residual_qty_coin)} coin (dung sai ${fmt.coin(v.tolerance_qty_coin)})`],
        ...legRows("Spot", v.spot || {}, "giá chào bán tốt nhất lúc chụp", v.spot_best_ask_quote, v.spot_entry_slippage_bps),
        ...legRows("Perp", v.perp || {}, "giá chào mua tốt nhất lúc chụp", v.perp_best_bid_quote, v.perp_entry_slippage_bps),
        ["Cửa sổ trần", `${v.unhedged_window_ms} ms`],
        ["Gỡ vị thế", v.unwind_duration_ms ? `${v.unwind_duration_ms} ms` : "không cần"],
        ["Sổ cũ lúc quyết định", `${v.book_age_ms} ms`],
        ["Tổng thời gian", `${v.elapsed_ms} ms`],
        ["Settle kế tiếp", fmt.time(v.next_funding_time_ms)],
        ["File ý định (cache)", v.cache_file || "—"],
      ]),
      v.error_vi ? callout("Lý do: " + v.error_vi, v.alarm ? "bad" : "") : null,
      v.cache_error_vi ? callout(v.cache_error_vi, "bad") : null,
      v.spot_flat_evidence_vi ? callout("Bằng chứng phẳng: " + v.spot_flat_evidence_vi, "") : null,
      v.bracket_vi ? el("p", { cls: "hint mt8", text: "Ký quỹ duy trì: " + v.bracket_vi }) : null,
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
    ["Sàn đang giữ (lệnh của ý định)", held ? `spot ${fmt.coin(held.spot.qty_coin, true)} · perp ${fmt.coin(held.perp.qty_coin, true)} coin` : "—"],
    ["Thứ tự", "MUA perp (reduce-only) trước, rồi BÁN spot đúng bằng phần perp đã đóng"],
    ["Loại lệnh", "MARKET, đọc lại từ sàn tới khi sàn báo xong"],
  ];
  if (!(await confirmDialog("ĐÓNG VỊ THẾ 2 CHÂN?", rows, "XÁC NHẬN ĐÓNG", "secondary"))) return;

  setActing(true, "đang đóng hai chân trên testnet…");
  const r = await post("close", "/api/close", { symbol: state.symbol, intent_id: intentID });
  setActing(false);
  const v = r.body || {};
  if (r.status === 0 || r.unreadable) {
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
        ["Ý định giữ trước khi đóng", `${fmt.coin(v.intent_qty_coin)} coin`],
        ["Đã đóng / còn lại", `${fmt.coin(v.closed_qty_coin)} / ${fmt.coin(v.remaining_qty_coin)} coin`],
        ["Vị thế perp sàn báo sau", `${fmt.coin(v.venue_perp_qty_coin, true)} coin`],
        ["Perp · đóng", `${(v.perp || {}).status || "—"} · ${fmt.coin((v.perp || {}).unwound_qty_coin)} coin @ ${fmt.quote((v.perp || {}).avg_fill_price_quote, 2)}`],
        ["Spot · đóng", `${(v.spot || {}).status || "—"} · ${fmt.coin((v.spot || {}).unwound_qty_coin)} coin @ ${fmt.quote((v.spot || {}).avg_fill_price_quote, 2)}`],
        ["Funding sàn đã trả", `${fmt.quote(v.funding_received_quote, 8, true)} · ${v.settlements_counted} mốc settle`, signCls(v.funding_received_quote)],
        ["Phí sàn thu (quote)", fmt.quote(v.commission_quote, 8)],
        ["Trượt giá (4 lần khớp)", fmt.quote(v.slippage_quote, 8, true)],
        ["RealizedQuote", fmt.quote(v.realized_quote, 8, true), signCls(v.realized_quote)],
        ["Trôi giá cặp (NGOÀI con số trên)", fmt.quote(v.pair_price_drift_quote, 8, true), signCls(v.pair_price_drift_quote)],
        ["Thời gian", `${v.elapsed_ms} ms`],
      ]),
      callout(v.realized_label_vi || "", ""),
      el("p", { cls: "hint mt8", text: `funding: ${v.funding_source_vi || "—"}` }),
      el("p", { cls: "hint", text: `phí: ${v.commission_source_vi || "—"}${v.commission_other_vi ? " · " + v.commission_other_vi : ""}` }),
      el("p", { cls: "hint", text: `trượt/trôi: ${v.price_drift_priced_vi || "—"}` }),
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
      el("td", { cls: "mono", text: p.intent_id }),
      el("td", { cls: "r", text: fmt.coin(p.spot_qty_coin, true) }),
      el("td", { cls: "r", text: fmt.coin(p.perp_qty_coin, true) }),
      el("td", { cls: "r neg", text: fmt.coin(p.residual_coin, true) }),
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
    ["Perp: sàn báo / ý định giải thích", `${fmt.coin(plan.venue_perp_qty_coin, true)} / ${fmt.coin(plan.intents_perp_qty_coin, true)} coin`],
  ];
  const extra = el("div");
  extra.append(callout(plan.note_vi || "", ""));
  if (plan.conflict_vi) extra.append(callout(plan.conflict_vi, "bad"));
  for (const u of plan.cache_unreadable_vi || []) extra.append(callout("file cache không đọc được: " + u, "bad"));
  if (plan.plans && plan.plans.length) extra.append(planTable(plan.plans));

  if (plan.to_send === 0 || plan.conflict_vi) {
    const title = plan.conflict_vi ? "LÀM PHẲNG — DỪNG: bằng chứng không khớp" : "LÀM PHẲNG — không có gì để gửi";
    showResult(title, [kvList(rows), extra]);
    return;
  }
  if (!(await confirmDialog("LÀM PHẲNG PHẦN LỆCH?", rows, `GỬI ${plan.to_send} LỆNH CÂN`, "danger", extra))) return;

  setActing(true, "đang gửi lệnh cân trên testnet…");
  const r = await post("reconcile", "/api/reconcile", { symbol: state.symbol, apply: true, plan_digest: plan.plan_digest });
  setActing(false);
  const v = r.body || {};
  if (r.status === 0 || r.unreadable) {
    showResult("LÀM PHẲNG — KẾT CỤC CHƯA RÕ", unknownOutcome("cân", r));
    refreshAll();
    return;
  }
  const nodes = [];
  if (!r.ok) nodes.push(callout(v.error_vi || `HTTP ${r.status}`, "bad"));
  for (const res of v.results || []) {
    nodes.push(kvList([
      ["Ý định", res.intent_id],
      ["Lệnh cân", `${res.client_order_id} · orderId ${res.venue_order_id || "—"} · ${res.status || "—"}`],
      ["Khớp", `${fmt.coin(res.filled_qty_coin)} coin @ ${fmt.quote(res.avg_fill_price_quote, 2)}`],
      ["Lệch sau khi cân", fmt.coin(res.residual_after_coin, true), res.balanced ? "pos" : "neg"],
    ]));
    if (res.error_vi) nodes.push(callout(res.error_vi, "bad"));
  }
  if (r.ok && (!v.results || v.results.length === 0)) nodes.push(callout("không có lệnh nào được gửi", ""));
  showResult("LÀM PHẲNG — kết quả", nodes);
  refreshAll();
}

// ------------------------------------------------------------- auto-trader

const AT_BADGE = {
  disabled: "[TẮT]",
  idle_scanning: "[ĐANG QUÉT]",
  evaluating: "[ĐANG QUÉT]",
  opening: "[ĐANG MỞ LỆNH]",
  in_position: "[ĐANG GIỮ VỊ THẾ - HEDGED]",
  closing: "[ĐANG ĐÓNG LỆNH]",
  cooldown: "[HỒI PHỤC]",
  emergency_halted: "[DỪNG BẢO VỆ]",
};
const AT_LOG_LINES = 10;

// acting is a start or stop this tab sent; killing is a kill it sent. A kill is
// never blocked behind a stop: a stop can wait minutes for an open in flight,
// and that is exactly when KILL must stay pressable.
const at = { status: null, acting: false, killing: false, stale: false };

const atHalted = (s) => Boolean(s && s.state === "emergency_halted");

async function refreshAutotrade() {
  const r = await api("/api/autotrade/status");
  if (!r.ok) {
    // The last status stays on screen, marked stale, and the buttons stop
    // trusting it until a read succeeds.
    at.stale = true;
    setText("at-age", "CŨ — không đọc được trạng thái bot: " + (r.body.error_vi || `HTTP ${r.status}`), "hint warn");
    syncAtButtons();
    return;
  }
  at.stale = false;
  at.status = r.body;
  renderAutotrade(r.body);
}

function renderAutotrade(s) {
  $("at-card").dataset.state = s.state;
  const badge = $("at-badge");
  badge.dataset.state = s.state;
  setText(badge, AT_BADGE[s.state] || `[${s.state_vi || s.state}]`);
  badge.title = `${s.state_vi || s.state} từ ${fmt.time(s.state_since_ms)}`;

  const toggle = $("at-toggle");
  toggle.setAttribute("aria-checked", s.enabled ? "true" : "false");
  toggle.dataset.halted = atHalted(s) ? "true" : "false";
  setText("at-toggle-label", atHalted(s) ? "XÁC NHẬN & TẮT" : s.enabled ? "TẮT AUTO-TRADER" : "BẬT AUTO-TRADER");
  setText("at-stop", atHalted(s) ? "[XÁC NHẬN DỪNG BẢO VỆ → TẮT]" : "[DỪNG & GIỮ VỊ THẾ]");
  if (s.notice_vi) setText("at-notice", s.notice_vi);

  const halt = $("at-halt");
  halt.hidden = !s.halt_reason_vi;
  setText(halt, s.halt_reason_vi ? `DỪNG BẢO VỆ — ${s.halt_reason_vi}. Xử lý nguyên nhân (đọc vị thế bên dưới, LÀM PHẲNG nếu cần), rồi XÁC NHẬN & TẮT trước khi bật lại.` : "");

  // While a run is on, the form shows the parameters it runs with; while it is
  // off, it keeps whatever the operator typed.
  if (s.enabled || atHalted(s)) {
    const c = s.config || {};
    if (c.symbol) $("at-symbol").value = c.symbol;
    if (isNum(c.notional_quote)) $("at-notional").value = String(c.notional_quote);
    if (isNum(c.min_net_apr_pct)) $("at-min-apr").value = String(c.min_net_apr_pct);
    if (isNum(c.max_hold_epochs)) $("at-max-epochs").value = String(c.max_hold_epochs);
  }

  const pos = s.position;
  setText("at-position-line", pos
    ? `bot giữ ${pos.intent_id} · ${fmt.coin(pos.qty_coin)} coin mỗi chân · qua ${pos.settlements_since_open} mốc settle${pos.adopted ? " · tiếp nhận sau khi bật lại" : ""}`
    : "bot không giữ vị thế nào");

  renderAtSignal(s);
  renderAtLog(s);
  syncAtButtons();
}

function atMark(text, cls) {
  return el("span", { cls: "at-mark " + cls, text });
}

function renderAtSignal(s) {
  const sig = s.signal;
  const scanned = s.last_scan_at_ms ? `quét lúc ${fmt.time(s.last_scan_at_ms)}` : "chưa quét";
  setText("at-age", s.next_scan_at_ms ? `${scanned} · lần sau ${fmt.time(s.next_scan_at_ms)}` : scanned, "hint");
  const verdict = $("at-verdict");
  const checks = $("at-checks");
  if (!sig) {
    verdict.dataset.state = "none";
    setText(verdict, s.enabled ? "Đang chờ lượt quét đầu tiên…" : "Chưa quét — bật bot để bắt đầu đánh giá");
    for (const id of ["at-funding", "at-funding-sub", "at-basis", "at-basis-sub", "at-apr", "at-apr-sub"]) setText(id, "—");
    clear(checks);
    setText("at-checks-summary", "Điều kiện vào / thoát");
    setText("at-cost-basis", "");
    renderAtCountdown();
    return;
  }
  const holding = (sig.exit_checks || []).length > 0;
  verdict.dataset.state = holding ? (sig.exit_due ? "exit" : "hold") : sig.entry_eligible ? "go" : "wait";
  setText(verdict, sig.verdict_vi || "—");

  const hours = sig.interval_sec > 0 ? sig.interval_sec / 3600 : null;
  setText("at-funding", isNum(sig.forecast_rate_per_interval_bps) ? `${fmt.bps(sig.forecast_rate_per_interval_bps)} bps` : "—",
    "stat-v sm " + signCls(sig.forecast_rate_per_interval_bps));
  setText("at-funding-sub", [
    isNum(sig.forecast_rate_per_8h_bps) ? `${fmt.bps(sig.forecast_rate_per_8h_bps)} bps/8h` : "",
    hours ? `chu kỳ đo được ${hours}h` : "chu kỳ chưa đo được",
    isNum(sig.last_settled_rate_per_interval_bps) ? `mốc gần nhất ${fmt.bps(sig.last_settled_rate_per_interval_bps)}` : "",
    isNum(sig.trailing_mean_rate_per_interval_bps) ? `TB ${sig.trailing_window_days} ngày ${fmt.bps(sig.trailing_mean_rate_per_interval_bps)} (${sig.trailing_settlements} mốc)` : "",
  ].filter(Boolean).join(" · "));

  setText("at-basis", isNum(sig.basis_bps) ? `${fmt.bps(sig.basis_bps)} bps` : "—");
  const limit = s.config ? s.config.max_basis_widen_bps : null;
  setText("at-basis-sub", isNum(sig.basis_widen_bps)
    ? `lúc vào ${fmt.bps(sig.entry_basis_bps)} · giãn ${fmt.bps(sig.basis_widen_bps)} / ngưỡng ${limit} bps`
    : `spot ${fmt.price(sig.spot_mid_quote)} · perp ${fmt.price(sig.perp_mid_quote)}`);

  if (isNum(sig.net_apr_pct)) {
    setText("at-apr", fmt.pct(sig.net_apr_pct, 2, true), "stat-v sm " + signCls(sig.net_apr_pct));
    setText("at-apr-sub",
      `trên notional một chân · trên vốn ${fmt.pct(sig.net_apr_on_capital_pct, 2, true)} (${sig.capital_per_notional}× notional) · ` +
      `giữ ${sig.holding_days < 2 ? sig.holding_days.toFixed(2) : Math.round(sig.holding_days)} ngày, ${sig.settlements_in_hold} mốc · ` +
      `chi phí vòng ${fmt.pct(sig.round_trip_cost_pct, 4)} (phí ${fmt.pct(sig.fees_pct, 4)} + trượt ${fmt.pct(sig.slippage_pct, 4)})`);
  } else {
    setText("at-apr", "không tính", "stat-v sm warn");
    setText("at-apr-sub", sig.net_apr_reason_vi || "—");
  }

  clear(checks);
  const group = (title, list, exit) => {
    if (!list || list.length === 0) return 0;
    checks.append(el("li", { cls: "at-group", text: title }));
    let good = 0;
    for (const c of list) {
      const mark = !c.evaluated ? atMark("KHÔNG ĐO", "na") : exit ? (c.passed ? atMark("GIỮ", "ok") : atMark("THOÁT", "bad")) : c.passed ? atMark("ĐẠT", "ok") : atMark("CHƯA", "bad");
      if (c.passed && c.evaluated) good++;
      checks.append(el("li", null, [mark, el("span", { cls: "at-detail" }, [el("strong", { text: c.name_vi }), " — " + (c.detail_vi || "")])]));
    }
    return good;
  };
  const entryGood = group("VÀO LỆNH", sig.entry_checks, false);
  const exitGood = group("THOÁT LỆNH", sig.exit_checks, true);
  setText("at-checks-summary", holding
    ? `Điều kiện thoát: ${exitGood}/${sig.exit_checks.length} nói GIỮ`
    : `Điều kiện vào: ${entryGood}/${(sig.entry_checks || []).length} đạt`);
  const applied = (sig.applied_vi || []).join(" ");
  const excluded = (sig.excluded_vi || []).join(" ");
  setText("at-cost-basis", [applied ? "Đã tính: " + applied : "", excluded ? "Chưa trừ: " + excluded : "", sig.fee_source_vi ? "Nguồn phí: " + sig.fee_source_vi : ""].filter(Boolean).join(" · "));
  renderAtCountdown();
}

// renderAtCountdown runs every second, so the countdown moves between polls.
function renderAtCountdown() {
  const sig = at.status && at.status.signal;
  if (!sig || !sig.next_funding_time_ms) {
    setText("at-countdown", "—");
    setText("at-settle-at", "—");
    return;
  }
  const left = (sig.next_funding_time_ms - Date.now()) / 1000;
  setText("at-countdown", left > 0 ? fmt.duration(left) : "đang settle");
  const minSec = at.status.config ? at.status.config.min_time_to_settle_sec : 0;
  setText("at-settle-at", `settle ${fmt.time(sig.next_funding_time_ms)}${left > 0 && left <= minSec ? " · quá gần để vào" : ""}`);
}

function renderAtLog(s) {
  const list = $("at-log");
  clear(list);
  const lines = (s.log || []).slice(0, AT_LOG_LINES);
  if (lines.length === 0) {
    list.append(el("li", { cls: "at-empty", text: "chưa có sự kiện" }));
    return;
  }
  for (const line of lines) {
    list.append(el("li", null, [
      el("span", { text: fmt.time(line.at_ms) }),
      el("span", { cls: "at-kind", text: `[${line.kind}]`, data: { kind: line.kind } }),
      el("span", { cls: "at-msg", text: line.message_vi }),
    ]));
  }
}

function syncAtButtons() {
  const s = at.status;
  const st = state.status;
  const ready = state.portalOK && st && st.spot.configured && st.futures.configured;
  const busy = at.acting || at.killing || Boolean(s && s.busy);
  const killBusy = at.killing || Boolean(s && s.busy === "kill");
  const halted = atHalted(s);
  const enabled = Boolean(s && s.enabled);
  $("at-start").disabled = !ready || busy || !s || at.stale || enabled || halted;
  $("at-stop").disabled = busy || !s || !(enabled || halted);
  $("at-kill").disabled = !ready || killBusy || !s;
  $("at-toggle").disabled = busy || !s || at.stale || (!ready && !enabled && !halted);
  for (const id of ["at-symbol", "at-notional", "at-min-apr", "at-max-epochs"]) $(id).disabled = enabled || halted || busy;
}

function atActionFailed(title, action, r) {
  if (r.status === 0 || r.unreadable) {
    showResult(`${title} — KẾT CỤC CHƯA RÕ`, unknownOutcome(action, r));
  } else {
    showResult(`${title} — KHÔNG thực hiện`, [callout((r.body && r.body.error_vi) || `HTTP ${r.status}`, "bad")]);
  }
}

async function atStart() {
  const symbol = $("at-symbol").value;
  const notional = Number($("at-notional").value);
  const minApr = Number($("at-min-apr").value);
  const epochs = Number($("at-max-epochs").value);
  const max = state.status ? state.status.max_notional_quote : 50000;
  const problems = [];
  if (!symbol) problems.push("chưa chọn symbol");
  if (!isNum(notional) || notional <= 0 || notional > max) problems.push(`notional phải trong (0, ${max}]`);
  if (!isNum(minApr) || Math.abs(minApr) > 1000) problems.push("Net APR tối thiểu phải là số trong ±1000");
  if (!Number.isInteger(epochs) || epochs < 0 || epochs > 1000) problems.push("số mốc settle tối đa phải là số nguyên trong [0, 1000]");
  if (problems.length) {
    showResult("Không bật — dữ liệu nhập sai", [callout(problems.join(" · "), "bad")]);
    return;
  }
  const st = state.status;
  // The run's other parameters are the server's shipped ones, read from the
  // status rather than repeated here.
  const c = (at.status && at.status.config) || {};
  const rows = [
    ["Symbol", symbol],
    ["Notional mỗi chân", `${fmt.quote(notional, 2)} quote`],
    ["Ngưỡng vào", `Net APR dự phóng ≥ ${minApr}%/năm trên notional một chân (strategy.NetAPR, phí đọc từ tài khoản testnet), độ sâu ±0,5% ≥ ${c.depth_multiple}× notional, còn > ${fmt.duration(c.min_time_to_settle_sec)} tới mốc settle`],
    ["Giữ", epochs > 0 ? `tối đa ${epochs} mốc settle` : `khi funding đã settle còn dương (dự phóng ${c.projection_hold_days} ngày)`],
    ["Thoát khi", `mốc settle sau lúc vào ≤ 0 · đủ số mốc · basis giãn > ${c.max_basis_widen_bps} bps · DỪNG/KILL`],
    ["Nhịp", `quét mỗi ${c.scan_interval_sec} giây · hồi phục ${c.cooldown_sec} giây sau mỗi lần đóng/mở hỏng · ${c.max_consecutive_failures} lỗi liên tiếp thì DỪNG BẢO VỆ`],
    ["Sàn", st ? `${st.spot.host} + ${st.futures.host}` : "—"],
  ];
  const warning = el("div");
  warning.append(callout("Bot sẽ TỰ ĐỘNG mở và đóng lệnh thật trên TESTNET, không hỏi lại từng lệnh, cho tới khi bạn DỪNG hoặc KILL.", "warn"));
  warning.append(callout("Một vị thế duy nhất. Hai chân lệch hay bằng chứng không khớp → bot DỪNG BẢO VỆ, không tự làm phẳng.", ""));
  if (!(await confirmDialog("BẬT AUTO-TRADER?", rows, "XÁC NHẬN BẬT", "primary", warning))) return;

  at.acting = true;
  syncAtButtons();
  const r = await post("autotrade-start", "/api/autotrade/start", { symbol, notional_quote: notional, min_net_apr_pct: minApr, max_hold_epochs: epochs });
  at.acting = false;
  if (!r.ok) {
    atActionFailed("BẬT AUTO-TRADER", "bật bot", r);
  } else {
    showResult("AUTO-TRADER ĐÃ BẬT", [callout(`Đang quét ${symbol} mỗi ${r.body.status.config.scan_interval_sec} giây. Nhật ký bot ở card phía trên.`, "ok")]);
  }
  refreshAll();
}

async function atStop() {
  const s = at.status;
  if (atHalted(s)) {
    const rows = [["Lý do dừng", s.halt_reason_vi || "—"], ["Sau khi xác nhận", "bot về TẮT; vị thế (nếu có) giữ nguyên, không lệnh nào được gửi"]];
    if (!(await confirmDialog("XÁC NHẬN ĐÃ XỬ LÝ DỪNG BẢO VỆ?", rows, "XÁC NHẬN & TẮT", "secondary"))) return;
  }
  // A stop that keeps the position sends no order, so it asks no confirmation.
  at.acting = true;
  syncAtButtons();
  const r = await post("autotrade-stop", "/api/autotrade/stop", { close_now: false });
  at.acting = false;
  if (!r.ok) {
    atActionFailed("DỪNG AUTO-TRADER", "dừng bot", r);
  } else {
    // Named by the server after the scan in flight returned — an open already
    // on its way when DỪNG was pressed is included. The title is the state the
    // server reports, never assumed.
    const after = r.body.status || {};
    const kept = r.body.kept_intent_id;
    showResult(after.state === "disabled" ? "AUTO-TRADER ĐÃ TẮT" : `AUTO-TRADER: ${after.state_vi || after.state}`, [
      callout(kept ? `Vị thế ${kept} GIỮ NGUYÊN trên sàn — đóng bằng ĐÓNG VỊ THẾ, hoặc BẬT lại để bot tiếp nhận.` : "Bot không giữ vị thế nào.", ""),
      after.halt_reason_vi ? callout("DỪNG BẢO VỆ: " + after.halt_reason_vi, "bad") : null,
    ]);
  }
  refreshAll();
}

async function atKill() {
  const s = at.status;
  const symbol = (s && s.config && s.config.symbol) || state.symbol;
  const pos = s && s.position;
  const held = state.symbol === symbol && state.positions ? `${state.positions.status_vi || state.positions.status} · spot ${fmt.coin(state.positions.spot_qty_coin)} / perp ${fmt.coin(state.positions.perp_qty_coin, true)} coin` : "đọc lại từ sàn khi bấm";
  const rows = [
    ["Symbol", symbol],
    ["Bot đang giữ", pos ? `${pos.intent_id} · ${fmt.coin(pos.qty_coin)} coin mỗi chân` : "không có vị thế của bot"],
    ["Sàn đang giữ", held],
    ["Sẽ làm", "dừng quét ngay; lệnh mở/đóng ĐÃ GỬI thì chờ nó xong (không huỷ giữa chừng); rồi ĐÓNG cả hai chân của bot bằng MARKET qua portal; bot về DỪNG BẢO VỆ"],
  ];
  const warning = callout("Kill chỉ đóng cặp CỦA BOT đang phòng hộ trên symbol này. Vị thế người vận hành tự mở, cặp lệch hoặc bằng chứng không khớp sẽ KHÔNG bị đụng — dùng ĐÓNG VỊ THẾ hoặc LÀM PHẲNG.", "bad");
  if (!(await confirmDialog("KILL SWITCH: DỪNG & ĐÓNG NGAY?", rows, "KILL — ĐÓNG NGAY", "danger", warning))) return;

  at.killing = true;
  syncAtButtons();
  setText("busy-line", "KILL SWITCH — đang dừng bot và đóng hai chân trên testnet…");
  const r = await post("autotrade-kill", "/api/autotrade/kill", {});
  at.killing = false;
  setText("busy-line", "");
  if (!r.ok) {
    atActionFailed("KILL SWITCH", "kill", r);
  } else {
    const oc = r.body.close || {};
    const res = oc.result || {};
    showResult("KILL SWITCH", [
      callout(oc.flat ? "ĐÃ DỪNG KHẨN CẤP · PHẲNG CẢ HAI CHÂN" : "ĐÃ DỪNG KHẨN CẤP — CHƯA PHẲNG, KIỂM TRA VỊ THẾ NGAY", oc.flat ? "ok" : "bad"),
      kvList([
        ["Kết cục", oc.detail_vi || "—", oc.flat ? "pos" : "neg"],
        ["Đã gửi lệnh đóng", oc.attempted ? "có" : "không"],
        ["Đã đóng / còn lại", oc.attempted ? `${fmt.coin(res.closed_qty_coin)} / ${fmt.coin(res.remaining_qty_coin)} coin` : "—"],
        ["Funding sàn đã trả", oc.attempted ? `${fmt.quote(res.funding_received_quote, 8, true)} · ${res.settlements_counted} mốc` : "—"],
        ["RealizedQuote (không phải lãi ròng)", oc.attempted ? fmt.quote(res.realized_quote, 8, true) : "—", signCls(res.realized_quote)],
        ["Trạng thái bot", r.body.status.state_vi],
      ]),
      res.error_vi ? callout("Lý do: " + res.error_vi, "bad") : null,
    ]);
  }
  refreshAll();
}

function atToggle() {
  const s = at.status;
  if (!s) return;
  if (s.enabled || atHalted(s)) {
    atStop();
  } else {
    atStart();
  }
}

// -------------------------------------------------------------------- boot

function refreshAll() {
  for (const p of state.polls) p.kick();
}

export function initExecution(status) {
  state.status = status;
  $("open-form").addEventListener("submit", doOpen);
  $("btn-close").addEventListener("click", doClose);
  $("btn-reconcile").addEventListener("click", doReconcile);
  $("close-intent").addEventListener("change", syncButtons);
  $("result-dismiss").addEventListener("click", () => {
    $("result").hidden = true;
  });
  fillSymbols(status.symbols || []);
  const atSymbol = $("at-symbol");
  for (const sym of status.symbols || []) atSymbol.append(el("option", { text: sym }));
  $("at-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    atStart();
  });
  $("at-stop").addEventListener("click", atStop);
  $("at-kill").addEventListener("click", atKill);
  $("at-toggle").addEventListener("click", atToggle);
  onStatus(status);

  // Account and positions feed the shared header, so they run on every tab —
  // fast here, slowly elsewhere. The rest only while this tab is open.
  const onlyHere = (fn) => async () => {
    if (execActive()) await fn();
  };
  state.polls = [
    schedule(refreshAccount, () => (execActive() ? FAST_MS : BACKGROUND_MS)),
    schedule(refreshPositions, () => (execActive() ? FAST_MS : BACKGROUND_MS)),
    schedule(refreshOtherHedges, () => BACKGROUND_MS),
    schedule(refreshMarket, () => MARKET_MS),
    schedule(onlyHere(refreshOrders), () => (execActive() ? SLOW_MS : 5000)),
    schedule(onlyHere(refreshIntents), () => (execActive() ? SLOW_MS : 5000)),
    schedule(onlyHere(refreshFunding), () => (execActive() ? FUNDING_MS : 5000)),
    schedule(refreshAutotrade, () => (execActive() ? FAST_MS : BACKGROUND_MS)),
  ];
  shell.onTab("execution", { enter: refreshAll });
  setInterval(() => {
    renderPositionsAge();
    renderAtCountdown();
    shell.renderHedges(state.hedges);
  }, 1000);
}
