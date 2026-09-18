// Động cơ 2 — the cross-venue perp–perp desk (PLAN 4.5k, decision Q20): long
// perp on one venue, short perp on the other, one exclusive symbol lock between
// the two engines, and the dual margin valve above both.
//
// This module READS and DRAWS. Every write — open, close, reconcile, unblock,
// acknowledge the red latch — belongs to execution.js, which confirms it in a
// dialog and sends the action header; it hands this module those functions
// through setActions. The server tests hold that line by reading this file.
//
// What the operator is watching for is ONE thing: is anything stranded. So the
// panel is ranked by that and not by workflow — the red latch, then the list of
// stranded things, then the valve, then the pairs and the locks, and only at the
// bottom the routine controls.
//
// Rule 2: nothing here is called profit or "ròng". The one APR on the page is
// the figure a person typed to contest the symbol lock, and it is drawn beside
// the words that say what it has deducted (priority_apr_basis_vi) — the
// coordinator never computes it.

import { $, el, clear, setText, isNum, fmt, api, schedule, emptyRow } from "./core.js";
import { shell } from "./shell.js";
import { t, onLanguageChange } from "./i18n.js";

const ACTIVE_MS = 4000;
// The tab badge must be able to alarm while another tab is showing, so the two
// readings that drive it keep going in the background. Both are answered from
// the portal's memory and touch no venue.
const BACKGROUND_MS = 15000;

const view = {
  actions: null,
  busy: false,
  // enabled is the desk's own answer, and it OVERRIDES every other reading: a
  // portal with no Engine 2 must not be able to raise the tab's badge from a
  // margin or lock answer that arrived before it was switched off.
  enabled: false,
  canOpen: false,
  status: null,
  locks: null,
  margin: null,
  statusErrVI: "",
  polls: [],
};

// ------------------------------------------------------------------ wording

// Every tier is a WORD and a mark as well as a colour (UX: never state by
// colour alone), and "không đọc được" is its own state — never a green one.
const TIER = {
  green: { word: "AN TOÀN", key: "safe", mark: "●", whyVI: "dưới mọi ngưỡng của van" },
  yellow: { word: "VÀNG · CHẶN Đ.CƠ 2", key: "warning", mark: "▲", whyVI: "van chặn Động cơ 2 mở thêm trên sàn này" },
  orange: { word: "CAM · CHẶN MỌI LỆNH MỞ", key: "block_open", mark: "▲▲", whyVI: "van chặn mọi lệnh mở trên cả hai sàn" },
  red: { word: "ĐỎ · ĐANG ĐÓNG CẶP", key: "close_pairs", mark: "■", whyVI: "chốt đỏ: van tự đóng các cặp của Động cơ 2" },
  unknown: { word: "KHÔNG ĐỌC ĐƯỢC", key: "syncing", mark: "?", whyVI: "không có số đo — chặn mở mới, KHÔNG bao giờ tự đóng" },
};

const LOCK_STATE = {
  idle: { word: "RẢNH", cls: "idle" },
  occupied: { word: "ĐANG BẬN", cls: "occupied" },
  conflict: { word: "XUNG ĐỘT — CẦN NGƯỜI", cls: "conflict" },
};

const ENGINE = {
  engine_1_cash_and_carry: "Động cơ 1 · spot + perp một sàn",
  engine_2_cross_perp: "Động cơ 2 · perp chéo sàn",
};

const LOCK_SOURCE = {
  acquired: "động cơ giành được",
  reconciled_inferred: "suy ra từ vị thế khi đối soát",
};

const engineVI = (id) => ENGINE[id] || id || "—";
const tierOf = (name) => TIER[name] || TIER.unknown;
const clampPct = (x) => Math.max(0, Math.min(100, x));
const ms = (x) => (isNum(x) ? `${Math.round(x)} ms` : "—");

// ------------------------------------------------------------------- gauges

function gaugeCard(v, m) {
  const tier = tierOf(v.tier);
  const known = Boolean(v.has_reading) && v.tier !== "unknown" && isNum(v.ratio_pct);
  const reading = known ? fmt.pct(v.ratio_pct, 1) : "KHÔNG CÓ SỐ";

  const track = el("div", {
    cls: "cp-track",
    attrs: {
      role: "img",
      "aria-label": known
        ? `${v.venue}: ký quỹ ${reading} của mức thanh lý — ${tier.word}`
        : `${v.venue}: không đọc được tỷ lệ ký quỹ`,
    },
  });
  const fill = el("span", { cls: "cp-fill" });
  track.append(fill);
  if (known) fill.style.width = `${clampPct(v.ratio_pct).toFixed(1)}%`;

  // The ticks come from the API's own table; the page hardcodes no threshold.
  for (const [key, frac, labelVI] of [
    ["yellow", m.yellow_frac, "vàng"],
    ["orange", m.orange_frac, "cam"],
    ["red", m.red_frac, "đỏ"],
  ]) {
    if (!isNum(frac) || frac <= 0) continue;
    const tick = el("i", { cls: "cp-tick", data: { at: key }, title: `${labelVI} ${fmt.pct(frac * 100, 0)}` });
    tick.style.left = `${clampPct(frac * 100).toFixed(1)}%`;
    track.append(tick);
  }

  return el("div", { cls: "cp-gauge", data: { tier: v.tier || "unknown" } }, [
    el("div", { cls: "cp-gauge-head" }, [
      el("span", { cls: "cp-gauge-venue", text: v.venue || "—" }),
      el("span", { cls: "cp-tier", data: { tier: v.tier || "unknown" }, title: tier.whyVI }, [
        el("span", { cls: "cp-tier-mark", text: tier.mark, attrs: { "aria-hidden": "true" } }),
        el("span", { text: tier.word }),
      ]),
    ]),
    el("div", { cls: "cp-gauge-val num", text: reading }),
    el("div", { cls: "cp-gauge-unit", text: "ký quỹ duy trì / vốn chủ sở hữu · sàn thanh lý ở 100%" }),
    track,
    el("div", { cls: "cp-gauge-sub" }, [
      el("span", { text: isNum(v.age_ms) && v.has_reading ? `số đo cách đây ${ms(v.age_ms)}` : "chưa có số đo nào" }),
      el("span", { cls: "cp-dot-sep", text: " · ", attrs: { "aria-hidden": "true" } }),
      el("span", { text: v.source_vi || "không rõ nguồn" }),
    ]),
    v.problem_vi ? el("div", { cls: "cp-problem", text: "KHÔNG BIẾT: " + v.problem_vi }) : null,
  ]);
}

function renderMargin(m) {
  const box = $("cp-gauges");
  clear(box);
  const venues = m.venues || [];
  if (!venues.length) {
    box.append(el("div", { cls: "empty-state", text: "Van ký quỹ chưa trả về sàn nào — chưa thể nói sàn nào an toàn." }));
  } else {
    for (const v of venues) box.append(gaugeCard(v, m));
  }
  setText(
    "cp-margin-scale",
    `Ngưỡng van (đọc từ portal, trang không tự đặt số nào): vàng ${fmt.pct(m.yellow_frac * 100, 0)} chặn Động cơ 2 trên sàn đó · ` +
      `cam ${fmt.pct(m.orange_frac * 100, 0)} chặn mọi lệnh mở · đỏ ${fmt.pct(m.red_frac * 100, 0)} chốt lại và đóng các cặp của Động cơ 2 · ` +
      `nhả chốt khi xuống dưới ${fmt.pct(m.release_frac * 100, 0)}.`,
    "note"
  );
  setText("cp-margin-age", isNum(m.read_at_ms) ? fmt.age(m.read_at_ms) : "chưa đọc", "hint");

  const banner = $("cp-emergency");
  banner.hidden = !m.emergency;
  if (m.emergency) {
    setText(
      "cp-emergency-text",
      `CHỐT ĐỎ KÝ QUỸ #${m.emergency_seq} — van đã tự đóng (hoặc đang đóng) các cặp của Động cơ 2 và KHÔNG cấp khóa mới. ` +
        `Đọc nhật ký bên dưới trước khi xác nhận; xác nhận chỉ nhả chốt, nó không mở lại vị thế nào.`
    );
    const ack = $("cp-ack-margin");
    ack.textContent = `XÁC NHẬN ĐÃ ĐỌC CHỐT #${m.emergency_seq}`;
    ack.disabled = view.busy || !view.actions;
  }
}

// -------------------------------------------------------------------- pairs

function legLine(sideVI, venue, qtyCoin, priceQuote) {
  return el("div", { cls: "cp-leg" }, [
    el("span", { cls: "cp-side", data: { side: sideVI === "LONG" ? "long" : "short" }, text: sideVI }),
    el("span", { cls: "mono", text: venue || "—" }),
    el("span", { cls: "num", text: `${fmt.coin(qtyCoin)} coin` }),
    isNum(priceQuote) && priceQuote > 0 ? el("span", { cls: "faint num", text: `@ ${fmt.price(priceQuote)}` }) : null,
  ]);
}

function pendingCell(pending) {
  if (!pending || !pending.length) return el("td", { cls: "faint", text: "—" });
  const list = el("ul", { cls: "cp-pending" });
  for (const o of pending) {
    list.append(
      el("li", null, [
        el("span", { cls: "mono strong", text: `${o.venue} · ${o.leg}` }),
        el("span", { cls: "mono faint", text: " " + (o.client_order_id || "—") }),
        el("div", { cls: "cp-why", text: o.why_vi || "—" }),
      ])
    );
  }
  return el("td", { cls: "wrap" }, [list]);
}

function pairStateCell(p) {
  const td = el("td", { cls: "wrap" });
  const chips = el("div", { cls: "cp-chips" });
  let calm = true;
  if (p.unresolved) {
    calm = false;
    chips.append(el("span", { cls: "cp-flag", data: { level: "bad" }, text: "CHƯA GIẢI QUYẾT" }));
  }
  if (p.opening_orders_unproven) {
    calm = false;
    chips.append(el("span", { cls: "cp-flag", data: { level: "bad" }, text: "LỆNH MỞ CHƯA CHỨNG MINH XONG" }));
  }
  if (p.close_blocked_vi) {
    calm = false;
    chips.append(el("span", { cls: "cp-flag", data: { level: "bad" }, text: "BỊ CHẶN ĐÓNG" }));
  }
  if (p.adopted) chips.append(el("span", { cls: "cp-flag", data: { level: "info" }, text: "NHẬN LẠI SAU KHỞI ĐỘNG" }));
  if (calm) chips.append(el("span", { cls: "cp-flag", data: { level: "ok" }, text: "HAI CHÂN ĐANG MỞ" }));
  td.append(chips);
  if (p.close_blocked_vi) td.append(el("div", { cls: "cp-why neg", text: p.close_blocked_vi }));
  if (isNum(p.opened_at_ms) && p.opened_at_ms > 0) td.append(el("div", { cls: "cp-why faint", text: "mở lúc " + fmt.time(p.opened_at_ms) }));
  return td;
}

function pairActionsCell(p) {
  const td = el("td");
  const box = el("div", { cls: "at-row-actions" });

  const first = el("select", { cls: "cp-first", attrs: { "aria-label": `Đóng sàn nào trước cho ${p.symbol}` } });
  first.append(el("option", { attrs: { value: "" }, text: "để máy chọn chân lớn hơn" }));
  for (const venue of [p.long_venue, p.short_venue]) {
    if (venue) first.append(el("option", { attrs: { value: venue }, text: venue + " trước" }));
  }

  const closeBtn = el("button", { cls: "btn secondary small", attrs: { type: "button" }, text: "ĐÓNG CẶP" });
  closeBtn.disabled = view.busy || !view.actions;
  closeBtn.addEventListener("click", () => {
    if (view.actions) view.actions.closePair(p, first.value);
  });

  box.append(first, closeBtn);

  if (p.close_blocked_vi) {
    const b = el("button", {
      cls: "btn danger small",
      attrs: { type: "button" },
      title: "Máy KHÔNG tự hoà giải hai bằng chứng lệch nhau (Q21) — nút này là người vận hành nói đã đọc rồi.",
      text: "ĐÃ ĐỌC BẰNG CHỨNG — BỎ CHẶN ĐÓNG",
    });
    b.disabled = view.busy || !view.actions;
    b.addEventListener("click", () => {
      if (view.actions) view.actions.unblockClose(p);
    });
    box.append(b);
  }
  if (p.opening_orders_unproven) {
    const b = el("button", {
      cls: "btn danger small",
      attrs: { type: "button" },
      title: "Chỉ bấm khi ĐÃ tự kiểm tra trên sàn rằng những lệnh này không thể thực thi nữa.",
      text: "ĐÃ CHỨNG MINH LỆNH KẾT THÚC",
    });
    b.disabled = view.busy || !view.actions;
    b.addEventListener("click", () => {
      if (view.actions) view.actions.confirmOrders(p);
    });
    box.append(b);
  }
  td.append(box);
  return td;
}

function renderPairs(s) {
  const body = $("cp-pairs");
  const pairs = s.pairs || [];
  const stranded = pairs.filter(strandedPair).length;
  setText(
    "cp-pairs-count",
    pairs.length ? `${pairs.length} cặp · ${stranded} cần người` : "không giữ cặp nào",
    stranded ? "hint neg" : "hint"
  );
  if (!pairs.length) {
    emptyRow(body, 6, "Động cơ 2 không giữ cặp nào. Khóa symbol chỉ được cấp sau một lần đối soát đọc được CẢ HAI sàn.");
    return;
  }
  clear(body);
  for (const p of pairs) {
    const diff = isNum(p.long_qty_coin) && isNum(p.short_qty_coin) ? p.long_qty_coin - p.short_qty_coin : null;
    const tr = el("tr", { cls: strandedPair(p) ? "cp-row-alarm" : "" }, [
      el("td", null, [
        el("div", { cls: "strong", text: p.symbol }),
        el("div", { cls: "mono faint", text: p.intent_id || "chưa có ý định" }),
      ]),
      el("td", { cls: "wrap" }, [
        legLine("LONG", p.long_venue, p.long_qty_coin, p.long_avg_fill_price_quote),
        legLine("SHORT", p.short_venue, p.short_qty_coin, p.short_avg_fill_price_quote),
      ]),
      el("td", { cls: "r num" }, [
        el("span", { cls: diff === null ? "faint" : Math.abs(diff) > 5e-13 ? "neg" : "", text: diff === null ? "—" : fmt.coin(diff, true) }),
      ]),
      pairStateCell(p),
      pendingCell(p.pending_orders),
      pairActionsCell(p),
    ]);
    body.append(tr);
  }
}

const strandedPair = (p) => Boolean(p.unresolved || p.close_blocked_vi || p.opening_orders_unproven || (p.pending_orders || []).length);

// -------------------------------------------------------------------- locks

function renderLocks(l) {
  const body = $("cp-locks");
  const locks = l.locks || [];
  if (!locks.length) {
    emptyRow(body, 7, "Bảng khóa rỗng — chưa symbol nào được đối soát hoặc chưa động cơ nào giữ symbol.");
  } else {
    clear(body);
    for (const k of locks) {
      const st = LOCK_STATE[k.state] || { word: String(k.state || "?").toUpperCase(), cls: "unknown" };
      const tr = el("tr", { cls: k.state === "conflict" ? "cp-row-alarm" : "" }, [
        el("td", { cls: "strong", text: k.symbol }),
        el("td", null, [el("span", { cls: "cp-state", data: { state: st.cls }, text: st.word })]),
        el("td", { text: k.owner_engine ? engineVI(k.owner_engine) : "—" }),
        el("td", { cls: "mono faint", text: k.intent_id || "—" }),
        el("td", { cls: "mono", text: (k.venues || []).join(" + ") || "—" }),
        el("td", { cls: "r num" }, [
          el("span", { text: isNum(k.priority_apr_on_capital_frac) && k.priority_apr_on_capital_frac !== 0 ? fmt.pct(k.priority_apr_on_capital_frac * 100, 2, true) : "—" }),
          k.priority_apr_basis_vi ? el("div", { cls: "cp-why faint", text: "đã trừ: " + k.priority_apr_basis_vi }) : null,
        ]),
        el("td", { cls: "wrap" }, [
          k.details ? el("div", { text: k.details }) : null,
          k.evidence_vi ? el("div", { cls: "cp-why", text: `bằng chứng (${fmt.time(k.evidence_at_ms)}): ${k.evidence_vi}` }) : null,
          el("div", { cls: "cp-why faint", text: `${LOCK_SOURCE[k.source] || k.source || "không rõ nguồn"} · giữ từ ${fmt.time(k.locked_at_ms)}` }),
          el("div", {
            cls: "cp-why " + (k.orders_sent_at_ms ? "warn" : "faint"),
            text: k.orders_sent_at_ms
              ? `ĐÃ gửi lệnh dưới khóa này lúc ${fmt.time(k.orders_sent_at_ms)} — chỉ nhả được sau khi sàn đọc phẳng và im`
              : "chưa lệnh nào được gửi dưới khóa này",
          }),
        ]),
      ]);
      body.append(tr);
    }
  }

  const unverified = Object.entries(l.unverified || {});
  const parts = [];
  if (isNum(l.reconciled_at_ms) && l.reconciled_at_ms > 0) {
    parts.push(`Đối soát gần nhất: ${fmt.time(l.reconciled_at_ms)}.`);
  } else {
    parts.push("CHƯA ĐỐI SOÁT LẦN NÀO — bộ điều phối không cấp khóa nào cho tới khi đọc được mọi sàn.");
  }
  if (unverified.length) {
    parts.push(
      `${unverified.length} symbol CHƯA XÁC MINH (không cấp khóa): ` + unverified.map(([sym, why]) => `${sym} — ${why}`).join(" · ")
    );
  }
  const s = view.status;
  if (s && s.reconcile_err_vi) parts.push("Lần đối soát cuối LỖI: " + s.reconcile_err_vi);
  else if (s && s.reconcile_vi) parts.push(s.reconcile_vi + ".");
  setText("cp-locks-note", parts.join(" "), unverified.length || (s && s.reconcile_err_vi) ? "note warn" : "note");
  setText("cp-reconciled", isNum(l.read_at_ms) ? fmt.age(l.read_at_ms) : "chưa đọc", "hint");
}

// ---------------------------------------------------------- last open/close

function kv(rows) {
  const dl = el("dl", { cls: "kv" });
  for (const [k, v, cls] of rows) {
    if (v === null || v === undefined) continue;
    dl.append(el("dt", { text: k }), el("dd", { text: v, cls: cls || "" }));
  }
  return dl;
}

function eventsDetails(lines) {
  if (!lines || !lines.length) return null;
  const d = el("details", null, [el("summary", { cls: "hint", text: `dòng sự kiện của máy trạng thái (${lines.length})` })]);
  d.append(el("pre", { cls: "events", text: lines.join("\n") }));
  return d;
}

function legRows(nameVI, leg) {
  if (!leg || !leg.venue) return [];
  return [
    [`${nameVI} · sàn`, `${leg.venue} · ${leg.side || "—"} · ${leg.status || "chưa có trạng thái"}${leg.order_confirmed ? " (sàn đã xác nhận)" : " (sàn CHƯA xác nhận)"}`],
    [`${nameVI} · khớp`, `${fmt.coin(leg.filled_qty_coin)} coin @ ${fmt.price(leg.avg_fill_price_quote)}`],
    [`${nameVI} · vị thế sàn đọc về`, leg.venue_position_read ? `${fmt.coin(leg.venue_position_qty_coin, true)} coin` : "KHÔNG đọc được"],
    [`${nameVI} · id lệnh`, `${leg.client_order_id || "—"} / ${leg.venue_order_id || "—"}`],
  ];
}

function pendingBlock(pending) {
  if (!pending || !pending.length) return null;
  const ul = el("ul", { cls: "cp-pending" });
  for (const o of pending) {
    ul.append(el("li", null, [el("span", { cls: "mono strong", text: `${o.venue} · ${o.leg} · ${o.client_order_id}` }), el("div", { cls: "cp-why", text: o.why_vi || "—" })]));
  }
  return el("div", { cls: "callout bad" }, [el("div", { cls: "strong", text: `${pending.length} lệnh CHƯA chứng minh được là kết thúc — khóa vẫn được GIỮ` }), ul]);
}

function renderLastOpen(v) {
  const box = $("cp-last-open");
  clear(box);
  box.append(el("h3", { cls: "cp-sub-title", text: "Lần MỞ gần nhất" }));
  if (!v) {
    box.append(el("div", { cls: "empty-state", text: "Phiên này chưa mở cặp nào." }));
    return;
  }
  const headline = v.alarm
    ? "⚠ BÁO ĐỘNG — có lệnh còn có thể thực thi; ĐỌC SÀN trước khi làm gì tiếp"
    : v.hedged
      ? "ĐÃ MỞ · HAI CHÂN CÂN NHAU"
      : v.lock_refused
        ? "TỪ CHỐI VÌ KHÓA SYMBOL — không lệnh nào được gửi"
        : v.refused_before_placing
          ? "TỪ CHỐI TRƯỚC KHI GỬI — không có gì trên sàn"
          : "KHÔNG MỞ ĐƯỢC · đã kéo cả hai chân về phẳng";
  box.append(
    el("div", { cls: "callout " + (v.alarm ? "bad" : v.hedged ? "ok" : "warn"), text: headline }),
    kv([
      ["Symbol · ý định", `${v.symbol || "—"} · ${v.intent_id || "—"}`],
      ["Hai sàn", `LONG ${v.long_venue || "—"} / SHORT ${v.short_venue || "—"}`],
      ["Kết cục", `${v.outcome || "—"}${v.reduced_to_match ? " · đã thu nhỏ về chân ngắn" : ""}`],
      ["Cửa sổ hở (chỉ một chân trên sàn)", ms(v.unhedged_window_ms)],
      ["Lệch hai chân", `${fmt.coin(v.delta_imbalance_qty_coin, true)} coin (dung sai ${fmt.coin(v.tolerance_qty_coin)}, bước chung ${fmt.coin(v.common_step_coin)})`, Math.abs(v.delta_imbalance_qty_coin || 0) > 5e-13 ? "neg" : ""],
      ["Cỡ đích", `${fmt.coin(v.target_qty_coin)} coin · ${fmt.quote(v.notional_quote, 2)} quote mỗi chân`],
      ["Cỡ đó lấy từ đâu", v.size_basis_vi || "—"],
      ["Giá giữa tham chiếu · tuổi sổ", `${fmt.price(v.ref_mid_quote)} · ${ms(v.book_age_ms)}`],
      ["Chi phí vào ước tính · nới", `${fmt.pct(v.entry_cost_pct, 4)} · ${fmt.bps(v.widen_bps, 2)} bps`],
      ["Gỡ vị thế · tổng thời gian", `${v.unwind_duration_ms ? ms(v.unwind_duration_ms) : "không cần"} · ${ms(v.elapsed_ms)}`],
      ...legRows("Chân LONG", v.long),
      ...legRows("Chân SHORT", v.short),
    ]),
    pendingBlock(v.pending_orders),
    v.evidence_vi ? el("div", { cls: "callout", text: "Bằng chứng: " + v.evidence_vi }) : null,
    v.reason_vi ? el("div", { cls: "callout", text: "Lý do: " + v.reason_vi }) : null,
    v.error_vi ? el("div", { cls: "callout bad", text: "Lỗi: " + v.error_vi }) : null,
    eventsDetails(v.events_vi)
  );
}

function renderLastClose(v) {
  const box = $("cp-last-close");
  clear(box);
  box.append(el("h3", { cls: "cp-sub-title", text: "Lần ĐÓNG gần nhất" }));
  if (!v) {
    box.append(el("div", { cls: "empty-state", text: "Phiên này chưa đóng cặp nào." }));
    return;
  }
  const headline = v.alarm
    ? "⚠ BÁO ĐỘNG — hai chân có thể lệch; ĐỌC SÀN, ĐỪNG bấm đóng lại"
    : v.already_flat
      ? "CẶP ĐÃ PHẲNG SẴN — không lệnh nào cần gửi"
      : v.refused
        ? "TỪ CHỐI TRƯỚC KHI GỬI — không lệnh nào tới sàn"
        : v.lock_released
          ? "ĐÃ ĐÓNG · HAI CHÂN PHẲNG VÀ KHÓA ĐÃ NHẢ"
          : "ĐÓNG XONG NHƯNG KHÓA CHƯA NHẢ — còn thứ có thể thực thi";
  box.append(
    el("div", { cls: "callout " + (v.alarm ? "bad" : v.lock_released || v.already_flat ? "ok" : "warn"), text: headline }),
    kv([
      ["Symbol · ý định", `${v.symbol || "—"} · ${v.intent_id || "—"}`],
      ["Hai sàn", `LONG ${v.long_venue || "—"} / SHORT ${v.short_venue || "—"}`],
      ["Kết cục", v.outcome || "—"],
      ["Cửa sổ hở (chỉ một chân trên sàn)", ms(v.unhedged_window_ms)],
      ["Chân đóng trước", v.first_venue ? `${v.first_venue} (${v.first_leg || "—"})` : "máy chọn chân lớn hơn"],
      ["Giữ trước khi đóng", `LONG ${fmt.coin(v.long_before_qty_coin)} / SHORT ${fmt.coin(v.short_before_qty_coin)} coin`],
      ["Lệnh mở đã chứng minh xong", v.opening_orders_proven ? "rồi" : "CHƯA"],
      ["Lệnh còn nằm trên sàn", v.resting_orders < 0 ? "không đọc được" : String(v.resting_orders)],
      ["Số vòng · tổng thời gian", `${v.rounds} · ${ms(v.elapsed_ms)}`],
      ["Khóa symbol", v.lock_state_vi || (v.lock_released ? "đã nhả" : "còn giữ")],
      ...legRows("Chân LONG", v.long),
      ...legRows("Chân SHORT", v.short),
    ]),
    pendingBlock(v.pending_orders),
    v.reason_vi ? el("div", { cls: "callout", text: "Lý do: " + v.reason_vi }) : null,
    v.error_vi ? el("div", { cls: "callout bad", text: "Lỗi: " + v.error_vi }) : null,
    eventsDetails(v.events_vi)
  );
}

// ------------------------------------------------------------------- events

function renderEvents() {
  const box = $("cp-events");
  const lines = [];
  for (const e of (view.status && view.status.events) || []) lines.push(e);
  for (const e of (view.margin && view.margin.events) || []) lines.push(e);
  lines.sort((a, b) => (b.at_ms || 0) - (a.at_ms || 0));
  clear(box);
  if (!lines.length) {
    box.append(el("li", { cls: "at-empty", text: "chưa có sự kiện" }));
    return;
  }
  for (const e of lines.slice(0, 40)) {
    box.append(
      el("li", null, [
        el("span", { cls: "faint", text: fmt.time(e.at_ms) }),
        el("span", { cls: "at-kind", data: { kind: String(e.kind || "").toUpperCase() }, text: e.kind || "—" }),
        el("span", { cls: "at-msg", text: e.message_vi || "" }),
      ])
    );
  }
}

// -------------------------------------------------------------------- pilot

// The scanning loop that may open a cross-venue pair without a person pressing
// anything. It is off unless the process was started with -crossperp-pilot, and
// advisory by default; this strip is READ-ONLY and says which of the three it
// is, because an operator page that hides "something else can trade these
// symbols" is a lie by omission. Its own switch is not on this tab.
function renderPilot(p) {
  const box = $("cp-pilot");
  if (!p) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  const sends = p.enabled && !p.advisory_only && !p.halted;
  box.className = "callout cp-pilot " + (p.halted ? "bad" : sends ? "warn" : "");
  clear(box);
  const wordVI = p.halted
    ? `PHI CÔNG TỰ ĐỘNG ĐANG DỪNG BẢO VỆ — ${p.halted_vi || "không rõ lý do"}`
    : sends
      ? "PHI CÔNG TỰ ĐỘNG ĐANG BẬT VÀ ĐƯỢC GỬI LỆNH — nó có thể tự mở cặp trên các symbol này"
      : p.enabled
        ? "Phi công tự động BẬT nhưng CHỈ TƯ VẤN — nó tính và hiển thị, không được gửi lệnh"
        : t("cp_pilot_off");
  box.append(el("div", { cls: "strong", text: wordVI }));
  if (p.enabled) {
    box.append(
      el("div", { cls: "cp-why" }, [
        `quét mỗi ${fmt.duration(p.scan_every_s)} · đã quét ${p.scans} lượt · lần cuối ${fmt.time(p.last_scan_at_ms)} · ` +
          `${fmt.quote(p.notional_quote, 0)} quote mỗi chân · tối đa ${p.max_pairs} cặp · ` +
          `ngưỡng vào ${fmt.pct((p.min_after_cost_apr_on_capital_frac || 0) * 100, 2)}/năm trên vốn SAU CHI PHÍ (không phải "ròng") cho ${p.planned_hold_days} ngày giữ GIẢ ĐỊNH`,
      ])
    );
  }
  for (const note of p.notes_vi || []) box.append(el("div", { cls: "cp-why faint", text: note }));
}

// -------------------------------------------------------------- alarm rail

// The one question the tab answers. Anything in this list needs a person: the
// machine is forbidden from resolving it on its own (Q21, Q22).
function renderAlarm() {
  const head = $("cp-alarm-head");
  const list = $("cp-alarm-list");
  const rail = $("cp-alarm");
  if (!view.enabled) {
    $("tab-crossperp-alarm").hidden = true;
    return;
  }
  const s = view.status;
  const m = view.margin;
  const l = view.locks;
  const items = [];

  if (m && m.emergency) items.push(`CHỐT ĐỎ KÝ QUỸ #${m.emergency_seq} chưa được xác nhận.`);
  for (const v of (m && m.venues) || []) {
    if (v.tier === "unknown" || !v.has_reading) items.push(`${v.venue}: KHÔNG đọc được tỷ lệ ký quỹ — ${v.problem_vi || "không rõ lý do"}.`);
    else if (v.tier === "red" || v.tier === "orange") items.push(`${v.venue}: ký quỹ ${fmt.pct(v.ratio_pct, 1)} — ${tierOf(v.tier).whyVI}.`);
  }
  for (const k of (l && l.locks) || []) {
    if (k.state === "conflict") items.push(`Khóa ${k.symbol} XUNG ĐỘT: ${k.evidence_vi || k.details || "hai bằng chứng không khớp"}.`);
  }
  for (const p of (s && s.pairs) || []) {
    if (p.unresolved) items.push(`Cặp ${p.symbol} CHƯA GIẢI QUYẾT — hai chân không khớp nhau khi nhận lại.`);
    if (p.close_blocked_vi) items.push(`Cặp ${p.symbol} BỊ CHẶN ĐÓNG: ${p.close_blocked_vi}`);
    if (p.opening_orders_unproven) items.push(`Cặp ${p.symbol}: lệnh MỞ chưa chứng minh được là kết thúc.`);
    for (const o of p.pending_orders || []) items.push(`Cặp ${p.symbol} · ${o.venue}/${o.leg}: ${o.why_vi}`);
  }
  for (const [symbol, why] of Object.entries((s && s.held_locks) || {})) {
    items.push(`Khóa ${symbol} còn GIỮ dù sàn đọc phẳng — riêng việc nhả khóa hỏng: ${why}`);
  }
  for (const v of (s && s.venues) || []) {
    if (!v.ready) items.push(`Sàn ${v.name} chưa sẵn sàng: ${v.error_vi || "thiếu credential"}.`);
  }
  if (s && s.pilot && s.pilot.halted) items.push(`Phi công tự động DỪNG BẢO VỆ: ${s.pilot.halted_vi || "không rõ lý do"}`);
  if (s && s.reconcile_err_vi) items.push("Đối soát lỗi: " + s.reconcile_err_vi);
  if (s && !(s.reconciled_at_ms > 0)) items.push("Chưa đối soát lần nào — bộ điều phối sẽ không cấp khóa nào.");
  if (view.statusErrVI) items.push("Không đọc được trạng thái Động cơ 2: " + view.statusErrVI);

  clear(list);
  for (const text of items) list.append(el("li", { text }));
  rail.dataset.state = items.length ? "alarm" : "calm";
  setText(
    head,
    items.length
      ? `${items.length} việc CẦN NGƯỜI — máy không được phép tự xử lý bất kỳ mục nào bên dưới`
      : t("cp_alarm_calm")
  );
  $("tab-crossperp-alarm").hidden = items.length === 0;
  setText("tab-crossperp-alarm", items.length ? String(items.length) : "!");
}

// -------------------------------------------------------------------- form

function fillSelect(id, values, keepValue) {
  const sel = $(id);
  const before = keepValue === undefined ? sel.value : keepValue;
  clear(sel);
  for (const v of values) sel.append(el("option", { attrs: { value: v }, text: v }));
  if (values.includes(before)) sel.value = before;
}

function renderForm(s) {
  const venues = (s.venues || []).filter((v) => v.ready).map((v) => v.name);
  fillSelect("cp-symbol", s.symbols || []);
  fillSelect("cp-long", venues);
  fillSelect("cp-short", venues);
  if (venues.length > 1 && $("cp-long").value === $("cp-short").value) $("cp-short").value = venues[1];
  // A cross-venue pair is two venues or it is nothing: one leg is not a smaller
  // version of the strategy, so the button goes dead rather than letting the
  // portal answer the mistake.
  view.canOpen = venues.length >= 2 && (s.symbols || []).length > 0;

  setText(
    "cp-venues",
    (s.venues || []).map((v) => `${v.name}: ${v.ready ? v.source_vi || "sẵn sàng" : "KHÔNG sẵn sàng — " + (v.error_vi || "thiếu credential")}`).join(" · "),
    (s.venues || []).every((v) => v.ready) ? "hint" : "hint neg"
  );
  setText(
    "cp-limits",
    (view.canOpen ? "" : "KHÔNG MỞ ĐƯỢC: Động cơ 2 cần ĐÚNG HAI sàn sẵn sàng và ít nhất một symbol — xem dòng sàn ở đầu thẻ. ") +
      `Trần của bàn Động cơ 2: ${fmt.quote(s.max_notional_quote, 0)} quote MỖI CHÂN (thấp hơn Động cơ 1 vì cả hai chân đều dùng đòn bẩy), ` +
      `trần trượt giá ${fmt.bps(s.max_slippage_bps, 1)} bps. Khóa symbol chỉ được cấp sau khi đối soát đọc được cả hai sàn.`,
    view.canOpen ? "note" : "note warn"
  );
  setText("cp-size-hint", sizeHintVI(s), "hint");
  syncButtons();
}

function sizeHintVI(s) {
  return $("cp-size-qty").checked
    ? `Khối lượng theo COIN. Portal quy ra notional bằng giá giữa sổ của chân LONG rồi mới kiểm trần ${fmt.quote(s ? s.max_notional_quote : 0, 0)} quote.`
    : `Notional theo QUOTE cho MỘT chân, trần ${fmt.quote(s ? s.max_notional_quote : 0, 0)}. Chân kia cũng đúng cỡ đó.`;
}

function readPlan() {
  const s = view.status || {};
  const useQty = $("cp-size-qty").checked;
  const size = Number($("cp-size").value);
  const aprPct = Number($("cp-apr").value);
  const basis = $("cp-apr-basis").value.trim();
  const plan = {
    symbol: $("cp-symbol").value,
    longVenue: $("cp-long").value,
    shortVenue: $("cp-short").value,
    sizeKey: useQty ? "qty_coin" : "notional_quote",
    sizeValue: size,
    sizeUnitVI: useQty ? "coin" : "quote",
    aprOnCapitalFrac: isNum(aprPct) ? aprPct / 100 : 0,
    aprPct: aprPct,
    aprBasisVI: basis,
    maxNotionalQuote: s.max_notional_quote,
    maxSlippageBps: s.max_slippage_bps,
  };
  const problems = [];
  if (!plan.symbol) problems.push("chưa chọn symbol");
  if (!plan.longVenue || !plan.shortVenue) problems.push("chưa đủ hai sàn");
  if (plan.longVenue && plan.longVenue === plan.shortVenue) problems.push("sàn LONG và sàn SHORT phải KHÁC nhau — đây là cặp chéo sàn");
  if (!isNum(size) || size <= 0) problems.push("cỡ vị thế phải là số dương");
  if (!useQty && isNum(s.max_notional_quote) && size > s.max_notional_quote) problems.push(`notional vượt trần ${fmt.quote(s.max_notional_quote, 0)} quote mỗi chân`);
  if (!basis) problems.push("bắt buộc nêu con số APR đã trừ những gì — portal từ chối nếu để trống");
  plan.problemsVI = problems;
  return plan;
}

function syncButtons() {
  const ready = Boolean(view.actions) && view.enabled && !view.busy;
  $("cp-open").disabled = !ready || !view.canOpen;
  $("cp-reconcile").disabled = !ready;
  const ack = $("cp-ack-margin");
  if (ack) ack.disabled = !ready;
}

function submitOpen(ev) {
  ev.preventDefault();
  const plan = readPlan();
  if (plan.problemsVI.length) {
    setText("cp-busy", "KHÔNG gửi gì cả — " + plan.problemsVI.join("; "), "busy-line neg");
    return;
  }
  setText("cp-busy", "", "busy-line");
  if (view.actions) view.actions.openPair(plan);
}

// -------------------------------------------------------------------- reads

// showDisabled is the whole panel when Engine 2 is not wired: the message and
// nothing else. The readings are dropped with it, so a margin or lock answer
// that was in flight cannot raise the badge behind the message.
function showDisabled(messageVI) {
  view.enabled = false;
  view.status = null;
  view.locks = null;
  view.margin = null;
  $("cp-disabled").hidden = false;
  setText("cp-disabled", messageVI);
  $("cp-live").hidden = true;
  $("tab-crossperp-alarm").hidden = true;
  syncButtons();
}

async function refreshStatus() {
  if (!wanted()) return;
  const r = await api("/api/crossperp/status");
  if (!r.ok) {
    if (r.status === 404) {
      showDisabled(
        "Portal này chưa nối Động cơ 2: không có endpoint /api/crossperp/status. Bàn chéo sàn chỉ tồn tại khi cả hai sàn trả về credential và cờ -crossperp được bật."
      );
      return;
    }
    view.statusErrVI = r.body.error_vi || `HTTP ${r.status}`;
    setText("cp-pairs-count", view.statusErrVI, "hint neg");
    renderAlarm();
    return;
  }
  view.statusErrVI = "";
  const s = r.body;
  if (!s.enabled) {
    showDisabled(s.disabled_vi || "Động cơ 2 chưa được bật trên portal này.");
    return;
  }
  const wasOff = !view.enabled;
  $("cp-disabled").hidden = true;
  $("cp-live").hidden = false;
  view.enabled = true;
  view.status = s;
  renderPilot(s.pilot);
  renderPairs(s);
  renderForm(s);
  renderLastOpen(s.last_open);
  renderLastClose(s.last_close);
  setText("cp-lockfile", s.lock_file_vi ? "File khóa lúc khởi động: " + s.lock_file_vi : "", "note");
  renderEvents();
  renderAlarm();
  syncButtons();
  // The lock table is the first thing worth reading on a desk that just came
  // up, so it is kicked here rather than waited for.
  if (wasOff && view.locksPoll) view.locksPoll.kick();
}

// refreshLocks does NOT wait for the status read: the endpoint says for itself
// whether Engine 2 is wired, and gating it on the status answer left the lock
// table empty for a whole poll period on every load.
async function refreshLocks() {
  if (!shell.isActive("crossperp")) return;
  const r = await api("/api/coordinator/locks");
  if (!r.ok) {
    setText("cp-locks-note", r.body.error_vi || `HTTP ${r.status}`, "note neg");
    return;
  }
  if (!r.body.enabled) return;
  view.locks = r.body;
  renderLocks(r.body);
  renderAlarm();
}

async function refreshMargin() {
  if (!wanted()) return;
  const r = await api("/api/risk/margin");
  if (!r.ok) {
    setText("cp-margin-age", r.body.error_vi || `HTTP ${r.status}`, "hint neg");
    return;
  }
  if (!r.body.enabled) return;
  view.margin = r.body;
  renderMargin(r.body);
  renderEvents();
  renderAlarm();
}

// wanted is true while the tab is showing, and also while it is not: the badge
// has to be able to alarm from another tab. It stops only when the document is
// hidden, where a poll would buy nothing.
const wanted = () => !document.hidden;

// ---------------------------------------------------------------------- API

export const crossperpView = {
  // setActions takes the write handlers execution.js owns. Until it is called
  // every control on the tab stays disabled.
  setActions(actions) {
    view.actions = actions;
    syncButtons();
  },

  setBusy(on, labelVI) {
    view.busy = Boolean(on);
    setText("cp-busy", on ? labelVI || "đang gửi…" : "", "busy-line");
    syncButtons();
    if (view.status) renderPairs(view.status);
    if (view.margin) renderMargin(view.margin);
  },

  refresh() {
    for (const p of view.polls) p.kick();
  },
};

export function initCrossperp() {
  $("cp-form").addEventListener("submit", submitOpen);
  $("cp-reconcile").addEventListener("click", () => {
    if (view.actions) view.actions.reconcile();
  });
  $("cp-ack-margin").addEventListener("click", () => {
    if (view.actions && view.margin) view.actions.ackMargin(view.margin.emergency_seq);
  });
  for (const id of ["cp-size-notional", "cp-size-qty"]) {
    $(id).addEventListener("change", () => setText("cp-size-hint", sizeHintVI(view.status), "hint"));
  }
  syncButtons();

  view.locksPoll = schedule(refreshLocks, () => (shell.isActive("crossperp") ? ACTIVE_MS : 0));
  view.polls = [
    schedule(refreshStatus, () => (document.hidden ? 0 : shell.isActive("crossperp") ? ACTIVE_MS : BACKGROUND_MS)),
    schedule(refreshMargin, () => (document.hidden ? 0 : shell.isActive("crossperp") ? ACTIVE_MS : BACKGROUND_MS)),
    view.locksPoll,
  ];
  shell.onTab("crossperp", { enter: () => crossperpView.refresh() });
  shell.onVisibility((visible) => {
    if (visible) crossperpView.refresh();
  });
  onLanguageChange(() => {
    if (view.status) crossperpView.renderStatus(view.status);
    if (view.margin) crossperpView.renderMargin(view.margin);
    if (view.locks) crossperpView.renderLocks(view.locks);
  });
}
