// Paper Ledger — cmd/paperledger's /api/ledger, relayed READ-ONLY by the
// portal. Every figure is PAPER and says so; the ledger prices decisions the
// step-3.5 journal recorded and decides nothing (PLAN 4.3). The projected net
// APR is strategy.NetAPR's figure at entry, shown BESIDE the paper P&L, never
// converted into it.

import { $, el, clear, setText, isNum, fmt, api, schedule, chartOptions, chartsReady, emptyRow, NEON } from "./core.js";
import { shell } from "./shell.js";

const POLL_MS = 60000;

const state = { chart: null, series: null, capitalLine: null, poll: null, lastBuiltAtMs: 0 };

const paperTag = () => el("span", { cls: "tag paper", text: "PAPER" });
const sign = (v) => (isNum(v) && v !== 0 ? (v > 0 ? "pos" : "neg") : "");

function ensureChart() {
  if (state.chart || !chartsReady() || shell.active !== "paper") return;
  state.chart = window.LightweightCharts.createChart($("pp-equity"), chartOptions({ timeScale: { secondsVisible: false } }));
  state.series = state.chart.addAreaSeries({
    lineColor: NEON.cyan,
    topColor: "rgba(0, 242, 254, 0.28)",
    bottomColor: "rgba(79, 172, 254, 0.02)",
    lineWidth: 2,
    priceLineVisible: false,
  });
}

async function refresh() {
  if (!shell.isActive("paper")) return;
  const r = await api("/api/paper/ledger");
  if (!r.ok) {
    setText("pp-status", r.body.error_vi || `HTTP ${r.status}`, "note neg");
    setText("pp-source", "không đọc được");
    return;
  }
  const fetchedAt = Number(r.headers.get("X-Execportal-Fetched-At-Ms")) || 0;
  setText("pp-source", `cmd/paperledger · portal đọc lúc ${fmt.time(fetchedAt)}`);
  render(r.body);
}

function tile(label, value, cls, sub) {
  return el("div", { cls: "stat" }, [
    el("div", { cls: "stat-k" }, [paperTag(), label]),
    el("div", { cls: "stat-v " + (cls || ""), text: value }),
    sub ? el("div", { cls: "stat-s", text: sub }) : null,
  ]);
}

function render(r) {
  if (r.mode !== "paper") {
    setText("pp-status", `tiến trình trả mode "${String(r.mode)}", không phải paper — không hiển thị`, "note neg");
    return;
  }
  setText("pp-status",
    `dựng lúc ${fmt.utc(r.built_at_ms)} · cửa sổ ${fmt.utc(r.window_from_ms)} → ${fmt.utc(r.window_to_ms)} (${r.window_from_note_vi || "—"}) · ${r.journal_rows} hàng nhật ký: ${r.journal_enters} enter · ${r.journal_exits} exit · ${r.journal_holds} hold · ${r.journal_skips} skip`,
    "note");

  const tiles = $("pp-tiles");
  clear(tiles);
  tiles.append(
    tile("Vốn ảo khởi điểm", fmt.quote(r.capital_quote, 0), "", "quote"),
    tile("Equity", fmt.quote(r.equity_quote, 2), sign(r.equity_quote - r.capital_quote), `${fmt.quote(r.equity_quote - r.capital_quote, 2, true)} so với khởi điểm`),
    tile("Tiền mặt ảo", fmt.quote(r.cash_quote, 2)),
    tile("P&L thực hiện", fmt.quote(r.realized_quote, 2, true), sign(r.realized_quote)),
    tile("P&L vị thế mở", fmt.quote(r.open_pnl_quote, 2, true), sign(r.open_pnl_quote), "gồm funding đã ghi có"),
    tile("Funding ghi có", fmt.quote(r.funding_quote, 2, true), sign(r.funding_quote), r.funding_special ? `${r.funding_special} mốc Special` : "chỉ tại mốc settle"),
    tile("Phí taker đã trả", fmt.quote(r.fees_paid_quote, 2), "neg"),
    tile("Drawdown lớn nhất", fmt.quote(r.max_drawdown_quote, 2), r.max_drawdown_quote > 0 ? "neg" : ""),
    tile("Mở / đóng / từ chối / bất thường", `${(r.open || []).length} / ${(r.closed || []).length} / ${r.refusals} / ${r.anomalies}`, "sm " + (r.anomalies > 0 ? "neg" : "")),
  );

  renderEquity(r);
  renderOpen(r.open || []);
  renderClosed(r.closed || []);
  renderList("pp-real", r.real_vi);
  renderList("pp-fake", r.fake_vi);
  renderList("pp-assumptions", (r.assumptions_vi || []).concat(r.label_vi ? [r.label_vi] : []));
  renderEvents(r.events || []);
}

function renderEquity(r) {
  ensureChart();
  const eq = r.equity || [];
  $("pp-equity-empty").hidden = eq.length >= 2;
  if (!state.series) return;
  const points = [];
  const markers = [];
  let previous = 0;
  for (const p of eq) {
    const time = Math.floor(p.at_ms / 1000);
    if (time <= previous) continue;
    previous = time;
    points.push({ time, value: p.equity_quote });
    if (p.stale_marks > 0) markers.push({ time, position: "inBar", color: NEON.warn, shape: "circle", size: 0.5 });
  }
  state.series.setData(points);
  state.series.setMarkers(markers);
  if (state.capitalLine) state.series.removePriceLine(state.capitalLine);
  state.capitalLine = state.series.createPriceLine({ price: r.capital_quote, color: "rgba(174,184,199,0.55)", lineStyle: 2, lineWidth: 1, axisLabelVisible: true, title: "vốn khởi điểm" });
  if (state.lastBuiltAtMs !== r.built_at_ms) {
    state.chart.timeScale().fitContent();
    state.lastBuiltAtMs = r.built_at_ms;
  }
  if (eq.length >= 2) {
    const ys = eq.map((p) => p.equity_quote);
    setText("pp-equity-note", `${eq.length} điểm đánh dấu · min ${fmt.quote(Math.min(...ys), 2)} · max ${fmt.quote(Math.max(...ys), 2)} · nét đứt = vốn khởi điểm · chấm vàng = điểm có chân thiếu mẫu giá (giữ mark cũ)`);
  } else {
    setText("pp-equity-note", "Chưa đủ điểm đánh dấu để vẽ (cần ≥ 2).");
  }
}

function pairCell(p) {
  const td = el("td", null, [`${p.symbol} / ${p.perp_source} ← ${p.spot_source}`]);
  if (p.quote_bridged) td.append(" ", el("span", { cls: "badge stale", text: "KHÁC QUOTE" }));
  else if (!p.quote_assets_known) td.append(" ", el("span", { cls: "badge stale", text: "QUOTE CHƯA RÕ" }));
  return td;
}

function legCell(l) {
  if (!l) return el("td", { cls: "r", text: "—" });
  return el("td", { cls: "r" }, [
    fmt.quote(l.fill_price_quote, 4),
    el("span", { cls: "f-sub", text: `slip ${fmt.pct(l.slippage_pct, 4)} · phí ${fmt.quote(l.fee_quote, 2)}${l.depth_is_lower_bound ? " · ⚠ sổ cận dưới" : ""}` }),
  ]);
}

function fundingCell(p) {
  const parts = [`${p.funding_settlements} mốc`];
  if (p.funding_special) parts.push(`${p.funding_special} Special`);
  if (p.funding_unpriced) parts.push(`${p.funding_unpriced} không định giá`);
  return el("td", { cls: "r " + sign(p.funding_quote) }, [fmt.quote(p.funding_quote, 2, true), el("span", { cls: "f-sub", text: parts.join(", ") })]);
}

function aprCell(p) {
  return el("td", { cls: "r", text: p.projected_net_apr_ok ? fmt.pct(p.projected_net_apr_frac * 100, 2, true) : "không có số" });
}

function renderOpen(list) {
  const body = $("pp-open");
  if (list.length === 0) {
    emptyRow(body, 12, "không có vị thế mở");
    return;
  }
  clear(body);
  for (const p of list) {
    body.append(el("tr", null, [
      pairCell(p),
      el("td", { text: fmt.utc(p.opened_at_ms) }),
      el("td", { cls: "r", text: fmt.quote(p.qty_coin, 6) }),
      legCell(p.entry_spot),
      legCell(p.entry_perp),
      el("td", { cls: "r", text: fmt.quote(p.margin_quote, 2) }),
      fundingCell(p),
      el("td", { cls: "r " + sign(p.basis_pnl_quote), text: fmt.quote(p.basis_pnl_quote, 2, true) }),
      el("td", { cls: "r " + sign(p.open_pnl_quote), text: fmt.quote(p.open_pnl_quote, 2, true) }),
      aprCell(p),
      el("td", { cls: "r", text: `${fmt.pct(p.journal_cost_total_pct, 4)} / ${fmt.pct(p.paper_entry_cost_pct, 4)}` }),
      el("td", { cls: "clip " + (p.exit_refused_vi ? "warn" : "faint"), text: `${p.mark_is_stale ? "mark cũ · " : ""}${p.exit_refused_vi || ""}`, title: p.exit_refused_vi || "" }),
    ]));
  }
}

function renderClosed(list) {
  const body = $("pp-closed");
  if (list.length === 0) {
    emptyRow(body, 12, "chưa có vị thế đóng");
    return;
  }
  clear(body);
  for (const p of list) {
    const fees = [p.entry_spot, p.entry_perp, p.exit_spot, p.exit_perp].reduce((sum, l) => sum + (l && isNum(l.fee_quote) ? l.fee_quote : 0), 0);
    const days = (p.closed_at_ms - p.opened_at_ms) / 86400000;
    const capital = (p.entry_spot ? p.entry_spot.notional_quote : 0) + (p.margin_quote || 0);
    body.append(el("tr", null, [
      pairCell(p),
      el("td", { text: fmt.utc(p.opened_at_ms) }),
      el("td", { text: fmt.utc(p.closed_at_ms) }),
      el("td", { cls: "r", text: fmt.quote(days, 2) }),
      el("td", { cls: "r", text: fmt.quote(p.qty_coin, 6) }),
      fundingCell(p),
      el("td", { cls: "r neg", text: fmt.quote(fees, 2) }),
      el("td", { cls: "r " + sign(p.realized_pnl_quote), text: fmt.quote(p.realized_pnl_quote, 2, true) }),
      el("td", { cls: "r " + sign(p.realized_pnl_quote), text: capital > 0 ? fmt.pct((p.realized_pnl_quote / capital) * 100, 4, true) : "—", title: "trên vốn = notional spot + ký quỹ perp" }),
      el("td", { cls: "r " + sign(p.realized_pnl_quote), text: p.notional_quote > 0 ? fmt.pct((p.realized_pnl_quote / p.notional_quote) * 100, 4, true) : "—" }),
      aprCell(p),
      el("td", { cls: "r", text: `${fmt.pct(p.journal_cost_total_pct, 4)} / ${fmt.pct(p.paper_round_trip_cost_pct, 4)}` }),
    ]));
  }
}

function renderList(id, lines) {
  const list = $(id);
  clear(list);
  for (const line of lines || []) list.append(el("li", { text: line }));
  if (!lines || lines.length === 0) list.append(el("li", { text: "—" }));
}

function renderEvents(events) {
  const list = $("pp-events");
  clear(list);
  const recent = events.slice(-300).reverse();
  setText("pp-events-count", `${events.length} sự kiện · hiện ${recent.length} mới nhất`);
  if (recent.length === 0) {
    list.append(el("li", { text: "chưa có sự kiện" }));
    return;
  }
  for (const e of recent) {
    const kind = String(e.kind || "");
    list.append(el("li", {
      cls: kind.startsWith("refuse") ? "refuse" : kind === "anomaly" ? "anomaly" : "",
      text: `${fmt.utc(e.at_ms)} · ${kind} · ${e.symbol}/${e.perp_source} · ${fmt.quote(e.amount_quote, 2, true)} · ${e.detail_vi || ""}`,
    }));
  }
}

export function initPaper() {
  $("pp-refresh").addEventListener("click", () => state.poll && state.poll.kick());
  state.poll = schedule(refresh, () => (shell.isActive("paper") ? POLL_MS : 5000));
  shell.onTab("paper", {
    enter: () => {
      ensureChart();
      state.poll.kick();
    },
  });
}
