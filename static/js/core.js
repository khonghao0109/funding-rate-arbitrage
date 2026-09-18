// Shared helpers for every tab. Vanilla ES modules, no framework (CLAUDE.md
// rule 11).
//
// Rules every module keeps:
//   1. Text that came from a venue, the scanner, the paper ledger or a cache
//      file is written with textContent (el()), never as HTML. The server
//      tests read these files for innerHTML and fail on it.
//   2. Nothing says "net" or "profit" unless the figure's own producer said
//      so and carries what it deducted.
//   3. No absolute URL: the page talks to its own origin only.

export const $ = (id) => document.getElementById(id);

// Longer than the server's own read limits (20 s), so a slow read that would
// have succeeded is not reported as a portal that did not answer.
const READ_TIMEOUT_MS = 25000;

// el builds an element. opts: cls, text, title, attrs, data. Children may be
// nodes, strings or null.
export function el(tag, opts, children) {
  const node = document.createElement(tag);
  if (opts) {
    if (opts.cls) node.className = opts.cls;
    if (opts.text !== undefined && opts.text !== null) node.textContent = String(opts.text);
    if (opts.title) node.title = String(opts.title);
    if (opts.attrs) for (const [k, v] of Object.entries(opts.attrs)) node.setAttribute(k, String(v));
    if (opts.data) for (const [k, v] of Object.entries(opts.data)) node.dataset[k] = String(v);
  }
  for (const child of children || []) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

export function clear(node) {
  while (node && node.firstChild) node.removeChild(node.firstChild);
}

export function setText(id, text, cls) {
  const node = typeof id === "string" ? $(id) : id;
  if (!node) return;
  const value = text === null || text === undefined ? "" : String(text);
  if (node.textContent !== value) node.textContent = value;
  if (cls !== undefined && node.className !== cls) node.className = cls;
}

export function isNum(x) {
  return typeof x === "number" && Number.isFinite(x);
}

// A table body holding one explanatory row.
export function emptyRow(tbody, colSpan, text, cls) {
  clear(tbody);
  const td = el("td", { cls: "empty " + (cls || ""), text });
  td.colSpan = colSpan;
  tbody.append(el("tr", null, [td]));
}

// --------------------------------------------------------------- formatting

// Float noise on a quantity that is really zero prints as -0.00000000.
export function tidy(x) {
  return Math.abs(x) < 5e-13 ? 0 : x;
}

export const fmt = {
  coin(x, signed) {
    if (!isNum(x)) return "—";
    const v = tidy(x);
    const s = v.toFixed(8);
    return signed && v > 0 ? "+" + s : s;
  },
  quote(x, digits, signed) {
    if (!isNum(x)) return "—";
    const v = tidy(x);
    const s = v.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
    return signed && v > 0 ? "+" + s : s;
  },
  // A fill AT the touch computes as -1.9e-12 bps; that is zero, not "-0.00".
  bps(x, digits) {
    if (!isNum(x)) return "—";
    const d = digits === undefined ? 2 : digits;
    const v = Math.abs(x) < 0.5 * Math.pow(10, -d) ? 0 : x;
    return (v > 0 ? "+" : "") + v.toFixed(d);
  },
  pct(x, digits, signed) {
    if (!isNum(x)) return "—";
    const d = digits === undefined ? 2 : digits;
    const v = Math.abs(x) < 0.5 * Math.pow(10, -d) ? 0 : x;
    return (signed && v > 0 ? "+" : "") + v.toFixed(d) + "%";
  },
  price(x) {
    if (!isNum(x)) return "—";
    if (x >= 1000) return x.toFixed(2);
    if (x >= 100) return x.toFixed(3);
    if (x >= 10) return x.toFixed(4);
    if (x >= 1) return x.toFixed(5);
    return x.toFixed(6);
  },
  notional(x) {
    if (!isNum(x) || x <= 0) return "—";
    if (x >= 1e9) return (x / 1e9).toFixed(2) + "B";
    if (x >= 1e6) return (x / 1e6).toFixed(2) + "M";
    if (x >= 1e3) return (x / 1e3).toFixed(0) + "k";
    return x.toFixed(0);
  },
  time(ms) {
    if (!isNum(ms) || ms <= 0) return "—";
    const d = new Date(ms);
    const pad = (n) => String(n).padStart(2, "0");
    return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  },
  utc(ms) {
    if (!isNum(ms) || ms <= 0) return "—";
    return new Date(ms).toISOString().replace("T", " ").slice(0, 16) + "Z";
  },
  age(ms, nowMs) {
    const lang = document.documentElement.lang || "vi";
    if (!isNum(ms) || ms <= 0) {
      return lang === "en" ? "unread" : lang === "zh" ? "未读取" : "chưa đọc";
    }
    const sec = Math.max(0, Math.round(((nowMs || Date.now()) - ms) / 1000));
    if (sec < 1) {
      return lang === "en" ? "just now" : lang === "zh" ? "刚刚" : "vừa đọc";
    }
    return lang === "en" ? `${sec}s ago` : lang === "zh" ? `${sec}秒前` : `đọc ${sec}s trước`;
  },
  duration(seconds) {
    const lang = document.documentElement.lang || "vi";
    if (!isNum(seconds) || seconds < 0) return "—";
    if (seconds < 60) return `${Math.round(seconds)}s`;
    if (seconds < 3600) {
      const min = Math.floor(seconds / 60);
      return lang === "en" ? `${min}m` : lang === "zh" ? `${min}分钟` : `${min} phút`;
    }
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    if (lang === "en") return m > 0 ? `${h}h ${m}m` : `${h}h`;
    if (lang === "zh") return m > 0 ? `${h}小时 ${m}分` : `${h}小时`;
    return m > 0 ? `${h}g ${m}p` : `${h}g`;
  },
};

export function signCls(x) {
  if (!isNum(x) || tidy(x) === 0) return "";
  return x > 0 ? "pos" : "neg";
}

// Accepts a colour only in the forms the scanner's meta actually uses; it is
// applied through the CSSOM, never through markup.
export function safeColor(value) {
  const col = /^(#[0-9a-fA-F]{3,8}|[a-zA-Z]{3,20})$/.test(String(value ?? "")) ? String(value) : "#808a99";
  if (typeof document !== "undefined" && document.documentElement && document.documentElement.dataset.theme === "light" && col.startsWith("#") && col.length >= 7) {
    const r = parseInt(col.slice(1, 3), 16);
    const g = parseInt(col.slice(3, 5), 16);
    const b = parseInt(col.slice(5, 7), 16);
    const lum = (0.299 * r + 0.587 * g + 0.114 * b) / 255;
    if (lum > 0.52) {
      const dr = Math.max(0, Math.round(r * 0.5)).toString(16).padStart(2, "0");
      const dg = Math.max(0, Math.round(g * 0.5)).toString(16).padStart(2, "0");
      const db = Math.max(0, Math.round(b * 0.5)).toString(16).padStart(2, "0");
      return `#${dr}${dg}${db}`;
    }
  }
  return col;
}

// flash marks a value that just changed. One animation, restarted.
export function flash(node, up) {
  if (!node) return;
  node.classList.remove("flash-up", "flash-down");
  void node.offsetWidth;
  node.classList.add(up ? "flash-up" : "flash-down");
}

// --------------------------------------------------------------------- API

// Every /api/ call carries X-Execportal-Action — "read" for a GET — because the
// server refuses any /api/ request without it: that header is what a page on
// another site cannot send without a preflight.
export async function api(path, options) {
  try {
    const opts = Object.assign(
      { cache: "no-store", credentials: "same-origin", headers: { "X-Execportal-Action": "read" } },
      options || {}
    );
    // A read that hangs must not freeze a green banner behind it. Writes get no
    // timeout: an open can run for minutes, and aborting it would only turn a
    // known answer into an unknown one.
    if (!opts.method && typeof AbortSignal !== "undefined" && AbortSignal.timeout) opts.signal = AbortSignal.timeout(READ_TIMEOUT_MS);
    const res = await fetch(path, opts);
    let body = null;
    let unreadable = false;
    try {
      body = await res.json();
    } catch (_) {
      // A 200 whose body did not arrive whole says nothing about what the
      // server did — callers of a write must treat it as an unknown outcome.
      unreadable = true;
      body = { error_vi: `phản hồi HTTP ${res.status} không đọc được (không phải JSON trọn vẹn)` };
    }
    return { ok: res.ok && !unreadable, status: res.status, unreadable, body, headers: res.headers };
  } catch (err) {
    return { ok: false, status: 0, body: { error_vi: "không gọi được portal: " + (err && err.message ? err.message : err) } };
  }
}

// post sends a write. Called only after the operator confirmed the dialog —
// except the reconcile dry run, which sends no order and fills that dialog.
export function post(action, path, payload) {
  return api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Execportal-Action": action },
    body: JSON.stringify(payload),
  });
}

// schedule runs fn repeatedly, never overlapping itself, waiting everyMs()
// between runs (so a tab can poll fast while visible and slowly otherwise).
// kick() runs it now — or right after the run in flight — and there is only
// ever one timer, so kicking never doubles the polling rate.
export function schedule(fn, everyMs) {
  let running = false;
  let again = false;
  let timer = null;
  const run = async () => {
    clearTimeout(timer);
    timer = null;
    if (running) {
      again = true;
      return;
    }
    running = true;
    try {
      await fn();
    } catch (err) {
      console.warn("poll", err);
    } finally {
      running = false;
    }
    if (again) {
      again = false;
      run();
      return;
    }
    const next = everyMs();
    if (next > 0) timer = setTimeout(run, next);
  };
  run();
  return { kick: run };
}

// ------------------------------------------------------------------ charts

const activeCharts = new Set();

export function registerChart(chart) {
  if (chart) activeCharts.add(chart);
  return chart;
}

export function updateChartsTheme(theme) {
  const isLight = theme === "light";
  for (const chart of activeCharts) {
    try {
      chart.applyOptions({
        layout: {
          textColor: isLight ? "#334155" : "#aeb8c7",
        },
        grid: {
          vertLines: { color: isLight ? "rgba(0,0,0,0.06)" : "rgba(255,255,255,0.04)" },
          horzLines: { color: isLight ? "rgba(0,0,0,0.06)" : "rgba(255,255,255,0.05)" },
        },
        rightPriceScale: { borderColor: isLight ? "rgba(0,0,0,0.12)" : "rgba(255,255,255,0.1)" },
        timeScale: { borderColor: isLight ? "rgba(0,0,0,0.12)" : "rgba(255,255,255,0.1)" },
      });
    } catch (_) {}
  }
}

// Lightweight Charts theme shared by every chart on the page.
export function chartOptions(extra) {
  const isLight = document.documentElement.dataset.theme === "light";
  const base = {
    autoSize: true,
    layout: {
      background: { type: "solid", color: "rgba(0,0,0,0)" },
      textColor: isLight ? "#334155" : "#aeb8c7",
      fontSize: 11,
      fontFamily: "JetBrains Mono, ui-monospace, Menlo, monospace",
      attributionLogo: false,
    },
    grid: {
      vertLines: { color: isLight ? "rgba(0,0,0,0.06)" : "rgba(255,255,255,0.04)" },
      horzLines: { color: isLight ? "rgba(0,0,0,0.06)" : "rgba(255,255,255,0.05)" },
    },
    crosshair: { mode: 0 },
    rightPriceScale: { borderColor: isLight ? "rgba(0,0,0,0.12)" : "rgba(255,255,255,0.1)" },
    timeScale: { borderColor: isLight ? "rgba(0,0,0,0.12)" : "rgba(255,255,255,0.1)", timeVisible: true, secondsVisible: false },
    localization: { locale: "vi-VN" },
  };
  return deepMerge(base, extra || {});
}

function deepMerge(a, b) {
  const out = Array.isArray(a) ? a.slice() : Object.assign({}, a);
  for (const [k, v] of Object.entries(b)) {
    out[k] = v && typeof v === "object" && !Array.isArray(v) && a[k] && typeof a[k] === "object" ? deepMerge(a[k], v) : v;
  }
  return out;
}

export function chartsReady() {
  return typeof window.LightweightCharts !== "undefined";
}

export const NEON = { cyan: "#00f2fe", blue: "#4facfe", neg: "#ff4b4b", warn: "#ffb020", text2: "#aeb8c7", pos: "#10b981", live: "#10b981" };
