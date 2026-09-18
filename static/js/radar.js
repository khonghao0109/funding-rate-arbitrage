// Radar chéo sàn — cmd/scanner's cross-venue funding radar (PLAN 4.5i,
// direction 1), relayed READ-ONLY by the portal as bytes. It places nothing and
// the page sends nothing but reads.
//
// Every rate here is a FORMING rate and every spread is GROSS. The one figure
// with costs off it is "APR sau chi phí", and it names both of what it deducts
// (four taker fills, the touch spread of both books) and what it does not. It is
// never called "net" (core.js rule 2), and it is "if the spread held for the
// planned hold" — the three-year corpus says a ≥ 15% spread lasted a median of
// one day.

import { $, el, clear, setText, isNum, fmt, api, schedule, emptyRow, signCls } from "./core.js";
import { shell } from "./shell.js";
import { t, onLanguageChange } from "./i18n.js";

const POLL_MS = 5000;
const EVENTS_POLL_MS = 30000;
// The filter uses the lowest episode threshold and the alert the highest, both
// read from the radar's own config (config.yaml is the one source); these are
// only what the page shows before the first answer.
let filterGrossAPRPct = 15;
let alertGrossAPRPct = 25;
const FILTER_KEY = "portal.radar.filter";

const state = { radarPoll: null, eventsPoll: null, filter: "all", days: "7", last: null };

const STATUS_MAP = {
  good: { key: "rd_status_good", cls: "good" },
  wait_liquidity: { key: "rd_status_liquidity", cls: "stale" },
  watch: { key: "rd_status_watch", cls: "wait" },
  normal: { key: "rd_status_normal", cls: "wait" },
  unavailable: { key: "rd_status_nodata", cls: "bad" },
};

const REASON_KEYS = {
  below: "rd_reason_below",
  flip: "rd_reason_flip",
  stale: "rd_reason_stale",
  restart: "rd_reason_restart",
  "": "rd_reason_open",
};

function venueName(leg) {
  return (leg && leg.venue ? leg.venue : leg && leg.source ? leg.source : "?").toUpperCase();
}

function directionCell(p) {
  if (!p.short_source) return el("td", { cls: "muted", text: "—" });
  const shortLeg = p.a.source === p.short_source ? p.a : p.b;
  const longLeg = p.a.source === p.long_source ? p.a : p.b;
  return el("td", null, [
    el("span", { cls: "dir long", text: "LONG " + venueName(longLeg) }),
    el("span", { cls: "dir-sep", text: " · ", attrs: { "aria-hidden": "true" } }),
    el("span", { cls: "dir short", text: "SHORT " + venueName(shortLeg) }),
  ]);
}

function rateCell(leg) {
  const td = el("td", { cls: "r num" });
  if (leg.funding_status === "missing") {
    td.append(el("span", { cls: "muted", text: "không có" }));
    return td;
  }
  td.append(el("span", { cls: signCls(leg.rate_per_8h_bps), text: fmt.bps(leg.rate_per_8h_bps, 2) }));
  // Short on purpose: "đang hình thành" is on the card's tag, and the long form
  // of each bit is in the tooltip.
  const bits = [];
  if (leg.interval_sec > 0) bits.push(`${leg.interval_sec / 3600}h`);
  if (isNum(leg.funding_age_ms) && leg.funding_age_ms >= 0) bits.push(`${Math.round(leg.funding_age_ms / 1000)}s`);
  if (leg.funding_publish_mode === "on_change") bits.push("khi đổi");
  td.title = [
    leg.interval_sec > 0 ? `settle mỗi ${leg.interval_sec / 3600} giờ` : "",
    leg.is_estimated ? "rate đang hình thành, sàn còn đổi tới mốc" : "",
    isNum(leg.funding_age_ms) && leg.funding_age_ms >= 0 ? `nhận ${Math.round(leg.funding_age_ms / 1000)} giây trước` : "",
    leg.funding_publish_mode === "on_change" ? "sàn chỉ phát khi rate đổi — tuổi không chứng minh số còn đúng" : "",
  ].filter(Boolean).join("\n");
  td.append(el("div", { cls: "sub", text: bits.join(" · ") }));
  if (leg.funding_status !== "live") td.append(" ", el("span", { cls: "badge stale", text: "CŨ" }));
  return td;
}

function spreadCell(p) {
  if (p.a.funding_status === "missing" || p.b.funding_status === "missing") return el("td", { cls: "r muted", text: "—" });
  return el("td", { cls: "r num" }, [
    el("span", { text: fmt.bps(p.spread_per_8h_bps, 2) + " bps" }),
    el("div", { cls: "sub strong", text: fmt.pct(p.gross_apr_pct, 1) + "/năm gộp" }),
  ]);
}

function afterCostCell(p) {
  if (!isNum(p.after_cost_apr_capital_pct)) {
    return el("td", { cls: "r muted", text: "—", title: "Chi phí không tính được: sổ lệnh không live, funding cũ hoặc biểu phí chưa xác minh" });
  }
  return el("td", { cls: "r num" }, [
    el("span", { cls: "strong " + signCls(p.after_cost_apr_capital_pct), text: fmt.pct(p.after_cost_apr_capital_pct, 1, true) }),
    el("div", { cls: "sub", text: `vòng ${fmt.bps(p.cost_round_trip_bps, 1).replace("+", "")}` + (isNum(p.breakeven_hold_days) ? ` · hoà vốn ${p.breakeven_hold_days < 100 ? p.breakeven_hold_days.toFixed(1) : "> 99"} ng` : "") }),
  ]);
}

function bookCell(p) {
  const touch = (leg) => (isNum(leg.touch_spread_bps) ? leg.touch_spread_bps.toFixed(2) : "—");
  const td = el("td", { cls: "r num" }, [
    el("span", { text: `${touch(p.a)} / ${touch(p.b)} bps` }),
    el("div", { cls: "sub", text: isNum(p.cross_basis_bps) ? `basis ${fmt.bps(p.cross_basis_bps, 1)} bps` : "basis —" }),
  ]);
  if (p.a.price_status !== "live" || p.b.price_status !== "live") td.append(el("span", { cls: "badge stale", text: "SỔ CŨ" }));
  return td;
}

function statusCell(p) {
  const item = STATUS_MAP[p.status];
  const s = item ? { text: t(item.key), cls: item.cls } : { text: String(p.status || "?").toUpperCase(), cls: "wait" };
  const td = el("td", null, [el("span", { cls: "badge " + s.cls, text: s.text })]);
  if ((p.notes_vi || []).length) td.title = p.notes_vi.join("\n");
  return td;
}

function passesFilter(p) {
  const live = p.a.funding_status === "live" && p.b.funding_status === "live";
  return state.filter === "all" || (live && p.gross_apr_pct >= filterGrossAPRPct);
}

// An alert needs live FUNDING on both venues, not a live book: a wide spread is
// worth knowing about while a book is briefly stale.
function alerting(p) {
  return p.a.funding_status === "live" && p.b.funding_status === "live" && p.gross_apr_pct >= alertGrossAPRPct;
}

function render(snap) {
  state.last = snap;
  const th = snap.event_thresholds_gross_apr_pct || [];
  if (th.length) {
    filterGrossAPRPct = Math.min(...th);
    alertGrossAPRPct = Math.max(...th);
  }
  setText($("rd-filter").querySelector('[data-filter="hot"]'), `Chỉ ≥ ${filterGrossAPRPct}%/năm`);
  setText("tab-radar-alarm", `≥${alertGrossAPRPct}%`);
  const body = $("rd-rows");
  if (!snap.enabled) {
    emptyRow(body, 8, "Radar đang tắt trong config.yaml (cross_radar.enabled: false).");
    setText("rd-alert", "");
    return;
  }
  const venueA = snap.pairs.length ? venueName(snap.pairs[0].a) : snap.source_a;
  const venueB = snap.pairs.length ? venueName(snap.pairs[0].b) : snap.source_b;
  setText("rd-th-a", `${venueA} (bps/8h)`);
  setText("rd-th-b", `${venueB} (bps/8h)`);
  setText("rd-th-book", `Spread chạm ${venueA} / ${venueB}`);
  setText("rd-th-after", `APR sau chi phí / vốn (K=${snap.leverage_x_per_leg}×)`);
  setText("rd-assumptions",
    `Chênh = ${venueA} − ${venueB}, quy về 8h, ×1095 ra %/năm GỘP trên notional. APR sau chi phí trừ phí taker 4 lệnh và spread chạm hai sổ, ` +
    `rải trên ${snap.planned_hold_days} ngày giữ GIẢ ĐỊNH, rồi nhân K/2 = ${(snap.leverage_x_per_leg / 2).toFixed(2)} ` +
    `(vốn = 2N/K vì ký quỹ nằm ở hai sàn). CƠ HỘI TỐT khi ≥ ${snap.good_min_after_cost_apr_capital_pct}%, spread chạm cả hai ≤ ${snap.max_touch_spread_bps} bps ` +
    `và chênh hiện tại hoà vốn trong ≤ ${snap.good_max_breakeven_days} ngày; ` +
    `BÌNH THƯỜNG khi chênh gộp < ${snap.normal_below_gross_apr_pct}%.`,
    "note");
  const excluded = $("rd-excluded");
  clear(excluded);
  for (const text of snap.costs_excluded_vi || []) excluded.append(el("li", { text }));

  const rows = snap.pairs.filter(passesFilter);
  const alerts = snap.pairs.filter(alerting);
  // Symbols only, no live percentages: the region is role=status, and text that
  // changes every poll would be re-announced every five seconds. setText does
  // nothing when the text is unchanged, so it speaks only when the SET changes.
  setText("rd-alert",
    alerts.length
      ? `${alerts.length} cặp đang chênh ≥ ${alertGrossAPRPct}%/năm gộp: ${alerts.map((p) => p.symbol).join(", ")}`
      : `Không cặp nào chênh ≥ ${alertGrossAPRPct}%/năm gộp.`,
    alerts.length ? "radar-alert on" : "radar-alert");
  $("tab-radar-alarm").hidden = alerts.length === 0;

  const live = snap.pairs.filter((p) => p.status !== "unavailable").length;
  setText("rd-count", `${rows.length}/${snap.pairs.length} cặp hiển thị · ${live} có dữ liệu live`);
  if (!rows.length) {
    emptyRow(body, 8, state.filter === "all" ? "Chưa có cặp nào có funding ở hai sàn." : `Không cặp nào có dữ liệu live và chênh ≥ ${filterGrossAPRPct}%/năm gộp lúc này.`);
    return;
  }
  clear(body);
  for (const p of rows) {
    const hot = alerting(p);
    const tr = el("tr", { cls: hot ? "radar-hot" : "" }, [
      el("td", null, [
        el("span", { cls: "strong", text: p.symbol }),
        hot ? el("span", { cls: "badge stale ml4", text: `≥${alertGrossAPRPct}%` }) : null,
      ]),
      directionCell(p),
      rateCell(p.a),
      rateCell(p.b),
      spreadCell(p),
      afterCostCell(p),
      bookCell(p),
      statusCell(p),
    ]);
    body.append(tr);
  }
}

async function refreshRadar() {
  if (!shell.isActive("radar")) return;
  const r = await api("/api/scanner/cross-radar");
  if (!r.ok) {
    setText("rd-source", r.body.error_vi || `HTTP ${r.status}`, "hint neg");
    return;
  }
  setText("rd-source", `cmd/scanner · cập nhật ${fmt.time(r.body.updated_at_ms)}`, "hint");
  render(r.body);
}

function summaryTile(s) {
  const med = isNum(s.median_duration_sec) ? fmt.duration(s.median_duration_sec) : "—";
  const p90 = isNum(s.p90_duration_sec) ? fmt.duration(s.p90_duration_sec) : "—";
  const reasons = Object.entries(s.by_reason || {}).map(([k, v]) => `${REASON_VI[k] || k} ${v}`).join(" · ");
  return el("div", { cls: "stat" }, [
    el("div", { cls: "stat-k", text: `Đợt ≥ ${s.threshold_apr_pct}%/năm gộp` }),
    el("div", { cls: "stat-v", text: `${s.episodes} đợt` }),
    el("div", { cls: "stat-s", text: `trung vị ${med} · P90 ${p90} · đo được ${s.measured_count} · đang mở ${s.open}` }),
    s.censored_count > 0
      ? el("div", { cls: "stat-s", text: `bị cắt ${s.censored_count} · trung vị ≥ ${isNum(s.censored_median_lower_bound_sec) ? fmt.duration(s.censored_median_lower_bound_sec) : "—"} (cận dưới)` })
      : null,
    reasons ? el("div", { cls: "stat-s", text: reasons }) : null,
  ]);
}

async function refreshEvents() {
  if (!shell.isActive("radar")) return;
  const r = await api(`/api/scanner/cross-radar/events?days=${state.days}`);
  const body = $("rd-events");
  if (!r.ok) {
    setText("rd-events-note", r.body.error_vi || `HTTP ${r.status}`, "note neg");
    emptyRow(body, 7, "—");
    return;
  }
  setText("rd-events-note", r.body.note_vi, "note");
  const tiles = $("rd-summary");
  clear(tiles);
  for (const s of r.body.summary || []) tiles.append(summaryTile(s));
  const events = (r.body.events || []).slice(0, 100);
  if (!events.length) {
    emptyRow(body, 7, "Chưa ghi nhận đợt chênh nào trong cửa sổ này.");
    return;
  }
  clear(body);
  for (const e of events) {
    const reasonKey = REASON_KEYS[e.end_reason];
    const reasonText = reasonKey ? t(reasonKey) : (e.end_reason || "—");
    body.append(el("tr", null, [
      el("td", { cls: "strong", text: e.symbol }),
      el("td", { cls: "r num", text: `≥ ${e.threshold_apr_pct}%` }),
      el("td", { text: e.direction ? e.direction.replaceAll("_", " ") : "—" }),
      el("td", { cls: "num", text: fmt.time(e.started_at_ms) }),
      el("td", { cls: "r num", text: e.ended_at_ms ? fmt.duration(e.duration_sec) : t("rd_reason_open") }),
      el("td", { cls: "r num", text: fmt.pct(e.peak_gross_apr_pct, 1) }),
      el("td", null, [el("span", { cls: "badge " + (e.end_reason === "" ? "stale" : e.end_reason === "below" || e.end_reason === "flip" ? "wait" : "bad"), text: reasonText })]),
    ]));
  }
}

function setPressed(group, value, attr) {
  for (const b of $(group).querySelectorAll("button")) b.setAttribute("aria-pressed", b.dataset[attr] === value ? "true" : "false");
}

export function initRadar() {
  try {
    const saved = window.localStorage.getItem(FILTER_KEY);
    if (saved === "all" || saved === "hot") state.filter = saved;
  } catch (_) {
    /* per-viewer convenience only */
  }
  setPressed("rd-filter", state.filter, "filter");
  $("rd-filter").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-filter]");
    if (!b) return;
    state.filter = b.dataset.filter;
    setPressed("rd-filter", state.filter, "filter");
    try {
      window.localStorage.setItem(FILTER_KEY, state.filter);
    } catch (_) {
      /* ignore */
    }
    if (state.last) render(state.last);
  });
  $("rd-days").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-days]");
    if (!b) return;
    state.days = b.dataset.days;
    setPressed("rd-days", state.days, "days");
    state.eventsPoll.kick();
  });
  $("rd-refresh").addEventListener("click", () => {
    state.radarPoll.kick();
    state.eventsPoll.kick();
  });
  state.radarPoll = schedule(refreshRadar, () => (shell.isActive("radar") ? POLL_MS : 0));
  state.eventsPoll = schedule(refreshEvents, () => (shell.isActive("radar") ? EVENTS_POLL_MS : 0));
  shell.onTab("radar", {
    enter: () => {
      state.radarPoll.kick();
      state.eventsPoll.kick();
    },
  });
  shell.onVisibility((visible) => {
    if (visible && shell.active === "radar") {
      state.radarPoll.kick();
      state.eventsPoll.kick();
    }
  });
  onLanguageChange(() => {
    if (state.last) render(state.last);
  });
}
