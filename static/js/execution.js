// Execution Control — Strategy 1's two-leg position on Binance TESTNET.
//
// This is the only tab that writes. Nothing is sent to /api/open, /api/close or
// /api/reconcile before the operator confirms the dialog; the action header is
// set only on that path and the server refuses a write without it. The tab
// reads nothing from the Scanner or Paper tabs — no signal reaches a button.

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
  ];
  shell.onTab("execution", { enter: refreshAll });
  setInterval(() => {
    renderPositionsAge();
    shell.renderHedges(state.hedges);
  }, 1000);
}
