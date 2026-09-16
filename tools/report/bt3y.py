#!/usr/bin/env python3
"""Replay the auto-trader's CONVERGENCE-AND-AMORTIZATION rule set (PLAN 4.5f)
across every hedgeable venue in the corpus, and write one JSON for the portal's
Backtest tab.

Python CHỈ ĐỌC, chạy ngoài tiến trình Go, đúng ranh giới CLAUDE.md quy tắc 8.

WHY THIS EXISTS, and what it is NOT
-----------------------------------
`cmd/backtest` replays `internal/strategy` — the rule set the step-3.5 gate
runs. The auto-trader in `cmd/execportal/autotrade` is a SEPARATE engine by
decision Q18, and two of its pillars have no analogue in `internal/strategy` at
all: the entry basis floor and the convergence take-profit. So `cmd/backtest`
cannot answer "how would the NEW mechanism have done", and adding those axes to
it would change what the 3.5 gate journals and compares.
`tools/report/autoparams.py` set the precedent for replaying the auto-trader's
own rule on the corpus; this file is the same idea at the 4.5f rule set, across
venues since 4.5k.

Every formula below is a copy of `cmd/execportal/autotrade/signal.go` —
`assessEntry`, `assessExit` and `priceHolding`. Change one side, read the other.

WHAT IT CANNOT SEE, stated once here and again in the JSON's assumptions
-----------------------------------------------------------------------
  * There is NO historical order book. `depth_snapshots` starts where the
    scanner started and holds ~1,500 rows; slippage cannot be measured per fill
    as `priceHolding` measures it live. It is taken as a STATED round-trip cost
    per venue, from that venue's VERIFIED taker schedule in config.yaml.
  * For the same reason the 4.5i exit spread brake CANNOT be replayed: there is
    no historical touch to measure. It only ever DELAYS a close by one scan
    (10 s), so on a window of years its absence is immaterial — but it is
    absent, and that is stated rather than modelled.
  * The live entry also requires a forming rate > 0, depth >= 2x notional on
    four sides, clock skew <= 1 s and > 5 min to settlement. None are in the
    corpus; they can only REFUSE entries, so the trade counts here are an
    UPPER bound.
  * The basis is read from hourly candle closes, so a widening that opened and
    closed inside one hour is invisible.
  * Venue corpora are NOT the same depth and never will be (CLAUDE.md's history
    and candle-depth traps). Every series therefore carries its own covered
    window, and an APR is annualized on the days that series ACTUALLY covered —
    comparing a venue's three years with another's quarter is the error this
    file works hardest to make impossible to commit by accident.

Không thư viện ngoài. Không quyết định, không thực thi một lệnh nào.
"""
import argparse, json, math, os, sqlite3
from datetime import datetime, timezone

H = 3_600_000
DAY = 86_400_000

PAIRS = ['BTCUSDT', 'ETHUSDT', 'SOLUSDT', 'BNBUSDT', 'XRPUSDT', 'DOGEUSDT',
         'LTCUSDT', 'SUIUSDT', 'LINKUSDT', 'UNIUSDT', 'NEARUSDT', 'AAVEUSDT']

# One row per hedgeable venue: the perp, the spot leg it is hedged with, each
# leg's VERIFIED taker schedule from config.yaml, and whether the pairing spans
# two quote assets.
#
# quote_bridged is not a footnote. A USD-quoted perp against a USDT spot is
# delta-neutral in the COIN and OPEN in USDT/USD, and nothing in this project
# deducts a depeg — so those series carry an unpriced risk the others do not,
# and the label travels to the JSON and to the page.
VENUES = [
    # perp_source,          spot_source,    perp_bps, spot_bps, bridged, short label
    ('binance_futures',     'binance_spot', 5.0,      10.0,     False,   'BIN-F ← BIN-S'),
    ('bybit_futures',       'bybit_spot',   5.5,      10.0,     False,   'BYB-F ← BYB-S'),
    ('hyperliquid_futures', 'binance_spot', 4.5,      10.0,     True,    'HYP ← BIN-S'),
    ('kraken_futures',      'binance_spot', 5.0,      10.0,     True,    'KRK ← BIN-S'),
    ('gate_futures',        'binance_spot', 5.0,      10.0,     False,   'GATE ← BIN-S'),
    ('okx_futures',         'binance_spot', 5.0,      10.0,     False,   'OKX ← BIN-S'),
]

# The shipped 4.5f/4.5i/4.5j defaults, from cmd/execportal/autotrade/state.go.
DEFAULTS = dict(
    min_entry_basis_bps=5.0,
    min_hold_epochs=6,
    target_take_profit_net_pct=1.50,
    exit_negative_funding_rate_bps=-2.0,
    exit_negative_consecutive_epochs=2,
    max_basis_widen_bps=100.0,
    max_exit_spread_bps=10.0,          # live only; see the note above
    min_net_apr_pct=5.0,
    projection_hold_days=30.0,
    trailing_days=7.0,
    capital_per_notional=1.5,
)


def round_trip(perp_bps, spot_bps):
    """Four taker fills: buy spot + sell perp in, sell spot + buy perp out."""
    return 2 * (perp_bps + spot_bps) / 1e4


def connect(path):
    # immutable=1 is for a COPY only, and is what this directory uses when the
    # sandbox refuses the fcntl lock that mode=ro needs (autoparams.py says the
    # same). Never point it at the file a live scanner is writing.
    return sqlite3.connect(f'file:{path}?immutable=1', uri=True)


# ─────────────────────────────── read the corpus ───────────────────────────
def load_candles(c, source, symbol, cache):
    key = (source, symbol)
    if key not in cache:
        cache[key] = dict(c.execute(
            "SELECT open_time_ms,close_price_quote FROM price_history WHERE source=? AND symbol=?",
            [source, symbol]))
    return cache[key]


def load_series(c, symbol, perp_source, spot_source, cache):
    """One series: its settlements, and its basis in bps on every hour both legs
    published a candle."""
    fund = [(t, r) for t, r in c.execute(
        "SELECT funding_at_ms, rate_per_interval_frac FROM funding_history "
        "WHERE source=? AND symbol=? AND model='discrete' AND rate_type!='Special' "
        "ORDER BY funding_at_ms", [perp_source, symbol])]
    sp = load_candles(c, spot_source, symbol, cache)
    pf = load_candles(c, perp_source, symbol, cache)
    basis = {t: (pf[t] - sp[t]) / sp[t] * 1e4 for t in set(sp) & set(pf) if sp[t] > 0}
    return fund, basis


def basis_at(bs, t):
    """The basis of the newest candle to have CLOSED at or before t, at most two
    hours back. internal/backtest's rule: never the running candle, whose close
    is stamped in the future."""
    for k in (t, t - H, t - 2 * H):
        h = k - (k % H)
        if h in bs:
            return bs[h]
    return None


def threshold_frac(cost, hold_days, min_apr_pct, interval_sec):
    """The trailing mean a series must clear for NetAPR to reach the entry floor
    — the inverse of strategy.NetAPR, as autoparams.py states it."""
    n = math.floor(hold_days * 86400 / interval_sec)
    return (cost + min_apr_pct / 100 * hold_days / 365) / n if n else float('inf')


def measure_interval_sec(rows):
    """Rule 3: never hardcode a funding interval. The cadence is the MEASURED
    modal spacing of this series' own settlement stamps — 1h at hyperliquid and
    kraken, 8h at the others, and the epoch-counted rules mean different wall
    times because of it."""
    if len(rows) < 3:
        return 0
    gaps = {}
    for a, b in zip(rows, rows[1:]):
        g = round((b[0] - a[0]) / 1000)
        if g > 0:
            gaps[g] = gaps.get(g, 0) + 1
    return max(gaps.items(), key=lambda kv: kv[1])[0] if gaps else 0


def trailing_mean(rows, i, days):
    t0 = rows[i][0] - days * DAY
    w = [r for t, r in rows[:i + 1] if t >= t0]
    return (sum(w) / len(w), len(w)) if len(w) >= 3 else (None, len(w))


# ────────────────────── the rules, copied from signal.go ───────────────────
def running_result_frac(collected_frac, entry_basis, now_basis, cost):
    """priceHolding, expressed per unit of NOTIONAL.

    signal.go computes, in quote:
        funding = sum(rates) x perp entry notional
        drift   = (spot_now - spot_in)*qty + (perp_in - perp_now)*qty
        entry   = spot notional x spot taker + perp notional x perp taker
        exit    = current value of each leg x (taker + slippage)
        cash    = funding + drift - entry - exit

    Divide by the notional and the drift term is exactly the basis move, with
    the sign reversed because the position is SHORT the perp:
        drift/notional = -(basis_now - basis_in)/1e4
    Entry and exit together are the stated round trip. Entry slippage is
    therefore counted ONCE, inside that round trip, and never again through the
    drift — the double count PLAN 4.5e records.
    """
    return collected_frac - (now_basis - entry_basis) / 1e4 - cost


def replay(sym, rows, bs, cost, p):
    """One series, walked settlement by settlement with the hours between them.

    The funding exit can only fire AT a settlement, because that is when a rate
    exists (rule 6). The basis stop and the take-profit are priced on every
    hourly candle, because the live bot re-prices them on every scan and both
    read the book, not the funding row.
    """
    out, marks = [], []
    refused = dict(negative_rate=0, net_apr=0, basis_unreadable=0, basis_floor=0, holding=0)
    thr = threshold_frac(cost, p['projection_hold_days'], p['min_net_apr_pct'], p['interval_sec'])
    tp_frac = p['target_take_profit_net_pct'] / 100 * p['capital_per_notional']
    neg_floor = p['exit_negative_funding_rate_bps'] / 1e4
    pos = None

    def close(pos, t, reason, reason_vi, now_basis):
        out.append(dict(
            symbol=sym, entry_ms=pos['t'], exit_ms=t,
            days=(t - pos['t']) / DAY, epochs=pos['ep'],
            entry_basis_bps=pos['eb'], exit_basis_bps=now_basis,
            funding_frac=pos['col'], drift_frac=-(now_basis - pos['eb']) / 1e4,
            cost_frac=cost, net_frac=running_result_frac(pos['col'], pos['eb'], now_basis, cost),
            reason=reason, reason_vi=reason_vi))

    def walk_hours(pos, a, b):
        """Price the two book-driven exits on each closed candle in (a, b]."""
        h = a - (a % H) + H
        while h <= b:
            nb = bs.get(h)
            if nb is not None:
                if nb - pos['eb'] > p['max_basis_widen_bps']:
                    return ('basis', 'Cắt lỗ basis nổ: Basis giãn %+.1f bps > %.0f bps so với lúc vào'
                            % (nb - pos['eb'], p['max_basis_widen_bps']), nb, h)
                net = running_result_frac(pos['col'], pos['eb'], nb, cost)
                if p['target_take_profit_net_pct'] > 0 and net >= tp_frac:
                    roc = net / p['capital_per_notional'] * 100
                    return ('take_profit', 'Chốt lời hội tụ Basis: Net PnL %+.2f%% trên vốn ≥ ngưỡng %+.2f%%'
                            % (roc, p['target_take_profit_net_pct']), nb, h)
                if h % DAY < H:
                    marks.append((h, net))
            h += H
        return None

    for i, (t, r) in enumerate(rows):
        if pos is None:
            # ── Trụ cột 1 and the NetAPR floor: assessEntry ──
            if r <= 0:                                   # "mốc settle gần nhất > 0"
                refused['negative_rate'] += 1
                continue
            m, _ = trailing_mean(rows, i, p['trailing_days'])
            if m is None or m < thr:                     # "Net APR dự phóng ≥ ngưỡng"
                refused['net_apr'] += 1
                continue
            eb = basis_at(bs, t)
            if eb is None:
                refused['basis_unreadable'] += 1
                continue
            if eb < p['min_entry_basis_bps']:            # "Basis lúc vào ≥ ngưỡng"
                refused['basis_floor'] += 1
                continue
            pos = dict(t=t, col=0.0, ep=0, eb=eb)
            continue
        refused['holding'] += 1

        prev = rows[i - 1][0] if pos['ep'] > 0 else pos['t']
        hit = walk_hours(pos, prev, t)
        if hit:
            reason, vi, nb, at = hit
            close(pos, at, reason, vi, nb)
            pos = None
            continue

        # ── the settlement itself ──
        pos['col'] += r
        pos['ep'] += 1
        nb = basis_at(bs, t)
        if nb is None:
            nb = pos['eb']

        # ── Trụ cột 2 + 4: the funding exit, floored then hysteretic ──
        if pos['ep'] >= p['min_hold_epochs']:
            run = 0
            for j in range(i, -1, -1):
                if rows[j][0] <= pos['t']:
                    break
                if rows[j][1] <= neg_floor:
                    run += 1
                else:
                    break
            need = p['exit_negative_consecutive_epochs']
            if need > 0 and run >= need:
                close(pos, t, 'funding', 'Thoát funding âm kéo dài: %d mốc liên tiếp ≤ %.1f bps'
                      % (run, p['exit_negative_funding_rate_bps']), nb)
                pos = None
                continue

        # ── Trụ cột 3 re-priced now that this settlement's funding has landed ──
        net = running_result_frac(pos['col'], pos['eb'], nb, cost)
        if p['target_take_profit_net_pct'] > 0 and net >= tp_frac:
            roc = net / p['capital_per_notional'] * 100
            close(pos, t, 'take_profit', 'Chốt lời hội tụ Basis: Net PnL %+.2f%% trên vốn ≥ ngưỡng %+.2f%%'
                  % (roc, p['target_take_profit_net_pct']), nb)
            pos = None

    if pos is not None:
        last = rows[-1][0]
        nb = basis_at(bs, last)
        if nb is None:
            nb = pos['eb']
        close(pos, last, 'open', 'Còn mở ở cuối cửa sổ — chưa hiện thực hoá', nb)
        out[-1]['still_open'] = True
    return out, marks, refused


def hold_through(rows, bs, cost, p):
    """Enter at the first qualifying settlement and never leave — the repo's
    standing benchmark. Any rule that cannot beat it is not worth running."""
    thr = threshold_frac(cost, p['projection_hold_days'], p['min_net_apr_pct'], p['interval_sec'])
    for i, (t, r) in enumerate(rows):
        if r <= 0:
            continue
        m, _ = trailing_mean(rows, i, p['trailing_days'])
        if m is None or m < thr:
            continue
        eb = basis_at(bs, t)
        if eb is None or eb < p['min_entry_basis_bps']:
            continue
        nb = basis_at(bs, rows[-1][0])
        if nb is None:
            nb = eb
        return running_result_frac(sum(x for _, x in rows[i + 1:]), eb, nb, cost)
    return 0.0


# ───────────────────────────────── reporting ───────────────────────────────
def iso(ms):
    return datetime.fromtimestamp(ms / 1000, timezone.utc).strftime('%Y-%m-%d %H:%M')


def day_str(ms):
    return datetime.fromtimestamp(ms / 1000, timezone.utc).strftime('%Y-%m-%d')


def day_ms(s):
    return int(datetime.strptime(s, '%Y-%m-%d').replace(tzinfo=timezone.utc).timestamp() * 1000)


def year_buckets(lo_ms, hi_ms):
    """Calendar slices anchored on the corpus END, so "2025-2026" means the most
    recent 365 days rather than a calendar year the corpus only half covers."""
    out, hi = [], hi_ms
    while hi - 365 * DAY >= lo_ms - DAY:
        lo = hi - 365 * DAY
        out.append((day_str(lo)[:4] + '-' + day_str(hi)[:4], lo, hi))
        hi = lo
    return list(reversed(out))


def build(args):
    c = connect(args.db)
    p = dict(DEFAULTS)
    if args.take_profit is not None:
        p['target_take_profit_net_pct'] = args.take_profit
    K = p['capital_per_notional']
    notional = args.notional
    capital_slot = notional * K

    cache, series, skipped = {}, [], []
    for perp_src, spot_src, perp_bps, spot_bps, bridged, label in VENUES:
        cost = round_trip(perp_bps, spot_bps)
        for sym in PAIRS:
            fund, basis = load_series(c, sym, perp_src, spot_src, cache)
            iv = measure_interval_sec(fund)
            why = ''
            if len(fund) < 10:
                why = 'chỉ %d mốc settle' % len(fund)
            elif not basis:
                why = 'không có giờ nào cả hai chân cùng có nến'
            elif iv <= 0:
                why = 'không đo được nhịp settle'
            if why:
                skipped.append(dict(symbol=sym, perp_venue=perp_src, spot_venue=spot_src, reason_vi=why))
                continue
            ps = dict(p)
            ps['interval_sec'] = iv
            trades, marks, refused = replay(sym, fund, basis, cost, ps)
            # A series' own covered window: where BOTH its funding and its basis
            # exist. Annualizing on anything wider would credit a venue with
            # years it never published.
            lo_s = max(fund[0][0], min(basis))
            hi_s = min(fund[-1][0], max(basis))
            series.append(dict(
                symbol=sym, perp_venue=perp_src, spot_venue=spot_src, label=label,
                quote_bridged=bridged, cost_frac=cost, interval_sec=iv,
                covered_from=day_str(lo_s), covered_to=day_str(hi_s),
                covered_days=round(max((hi_s - lo_s) / DAY, 0.0), 1),
                settlements=len(fund), candle_hours=len(basis),
                trades=trades, marks=marks, refused=refused,
                hold_through_frac=hold_through(fund, basis, cost, ps)))

    if not series:
        raise SystemExit('bt3y: no series could be replayed from ' + args.db)

    all_trades = [t for s in series for t in s['trades']]
    closed = sorted((t for t in all_trades if not t.get('still_open')), key=lambda x: x['exit_ms'])
    lo = min(day_ms(s['covered_from']) for s in series)
    hi = max(day_ms(s['covered_to']) for s in series)
    years = (hi - lo) / DAY / 365

    # ── equity on the PORTFOLIO's capital: one slot per replayable series ──
    total_capital = capital_slot * len(series)
    marks_by_day, realized_by_day = {}, {}
    for s in series:
        key = (s['symbol'], s['perp_venue'])
        for ms, net in s['marks']:
            marks_by_day.setdefault(day_str(ms), {})[key] = net
        for t in s['trades']:
            if not t.get('still_open'):
                realized_by_day.setdefault(day_str(t['exit_ms']), []).append((key, t))

    curve, run_real, peak, max_dd, max_dd_day = [], 0.0, total_capital, 0.0, None
    open_marks, d = {}, lo - (lo % DAY)
    while d <= hi:
        ds = day_str(d)
        # Marks first, realizations second: a position closing today may also
        # have left a mark earlier today, and popping before the update would
        # let that mark back in and count the same trade twice.
        open_marks.update(marks_by_day.get(ds, {}))
        for key, t in realized_by_day.get(ds, []):
            run_real += t['net_frac'] * notional
            open_marks.pop(key, None)
        eq = total_capital + run_real + sum(v * notional for v in open_marks.values())
        peak = max(peak, eq)
        dd = (peak - eq) / peak * 100 if peak > 0 else 0.0
        if dd > max_dd:
            max_dd, max_dd_day = dd, ds
        curve.append(dict(time=ds, value=round(eq, 2), drawdown=round(-dd, 4)))
        d += DAY
    net_quote = run_real + sum(v * notional for v in open_marks.values())

    def agg(rows):
        """Per-series-on-capital, the repo's only comparable denominator. A SUM
        over series is not a rate of return and is never reported as one. The
        APR divides by the mean days those series ACTUALLY covered, so a venue
        with a quarter of corpus is not credited with three years."""
        n = len(rows)
        tot = sum(sum(t['net_frac'] for t in s['trades']) for s in rows)
        ts = [t for s in rows for t in s['trades'] if not t.get('still_open')]
        days = sum(s['covered_days'] for s in rows) / n
        on_cap = tot / n / K * 100
        return dict(
            series=n, trades=len(ts),
            still_open=len([t for s2 in rows for t in s2['trades'] if t.get('still_open')]),
            pnl_quote=round(tot * notional, 2),
            return_on_capital_pct=round(on_cap, 4),
            apr_on_capital_pct=round(on_cap / max(days / 365, 1e-9), 4),
            covered_days=round(days, 1),
            win_rate_pct=round(len([t for t in ts if t['net_frac'] > 0]) / len(ts) * 100, 2) if ts else 0.0,
            avg_days=round(sum(t['days'] for t in ts) / len(ts), 2) if ts else 0.0,
            funding_quote=round(sum(t['funding_frac'] for s in rows for t in s['trades']) * notional, 2),
            drift_quote=round(sum(t['drift_frac'] for s in rows for t in s['trades']) * notional, 2),
            fees_quote=round(-sum(t['cost_frac'] for s in rows for t in s['trades']) * notional, 2),
            hold_through_on_capital_pct=round(sum(s['hold_through_frac'] for s in rows) / n / K * 100, 4))

    venues = []
    for perp_src, spot_src, perp_bps, spot_bps, bridged, label in VENUES:
        rows = [s for s in series if s['perp_venue'] == perp_src]
        if not rows:
            continue
        v = agg(rows)
        v.update(perp_venue=perp_src, spot_venue=spot_src, label=label, quote_bridged=bridged,
                 round_trip_cost_frac=round_trip(perp_bps, spot_bps),
                 perp_taker_bps=perp_bps, spot_taker_bps=spot_bps,
                 interval_sec=rows[0]['interval_sec'],
                 covered_from=min(s['covered_from'] for s in rows),
                 covered_to=max(s['covered_to'] for s in rows))
        v['hold_through_apr_on_capital_pct'] = round(
            v['hold_through_on_capital_pct'] / max(v['covered_days'] / 365, 1e-9), 4)
        venues.append(v)
    venues.sort(key=lambda x: -x['apr_on_capital_pct'])

    symbols = []
    for sym in PAIRS:
        rows = [s for s in series if s['symbol'] == sym]
        if not rows:
            continue
        v = agg(rows)
        v.update(symbol=sym, venues=len(rows))
        symbols.append(v)
    symbols.sort(key=lambda x: -x['apr_on_capital_pct'])

    yearly = []
    for lbl, a, b in year_buckets(lo, hi):
        ts = [t for t in closed if a <= t['exit_ms'] < b]
        active = {(s['symbol'], s['perp_venue']) for s in series
                  if day_ms(s['covered_from']) < b and day_ms(s['covered_to']) > a}
        tot = sum(t['net_frac'] for t in ts)
        yearly.append(dict(
            label=lbl, from_day=day_str(a), to_day=day_str(b),
            trades=len(ts), series_active=len(active),
            venues_active=len({v for _, v in active}),
            pnl_quote=round(tot * notional, 2),
            return_on_capital_pct=round(tot / max(len(active), 1) / K * 100, 4),
            win_rate_pct=round(len([t for t in ts if t['net_frac'] > 0]) / len(ts) * 100, 2) if ts else 0.0,
            funding_quote=round(sum(t['funding_frac'] for t in ts) * notional, 2),
            fees_quote=round(-sum(t['cost_frac'] for t in ts) * notional, 2)))

    reasons = {}
    for t in closed:
        reasons[t['reason']] = reasons.get(t['reason'], 0) + 1

    trades_out = []
    for s in series:
        for t in s['trades']:
            trades_out.append(dict(
                symbol=t['symbol'], perp_venue=s['perp_venue'], spot_venue=s['spot_venue'],
                venue_label=s['label'], quote_bridged=s['quote_bridged'],
                entry_time=iso(t['entry_ms']), exit_time=iso(t['exit_ms']),
                entry_ms=t['entry_ms'], exit_ms=t['exit_ms'],
                duration_days=round(t['days'], 3), epochs=t['epochs'],
                entry_basis_bps=round(t['entry_basis_bps'], 2),
                exit_basis_bps=round(t['exit_basis_bps'], 2),
                funding_received=round(t['funding_frac'] * notional, 4),
                drift_quote=round(t['drift_frac'] * notional, 4),
                fees_paid=round(-t['cost_frac'] * notional, 4),
                pnl_quote=round(t['net_frac'] * notional, 4),
                pnl_pct=round(t['net_frac'] / K * 100, 4),
                exit_reason=t['reason'], exit_reason_vi=t['reason_vi'],
                still_open=bool(t.get('still_open'))))
    trades_out.sort(key=lambda x: x['entry_ms'])

    w = agg(series)
    doc = dict(
        built_at_ms=int(datetime.now(timezone.utc).timestamp() * 1000),
        label_vi='BACKTEST 3 NĂM ĐA SÀN — luật 4.5f (Hội tụ Basis & Khấu hao Phí) replay trên corpus thật',
        mode='backtest',
        engine='tools/report/bt3y.py — bản chép luật của cmd/execportal/autotrade/signal.go',
        window=dict(from_ms=lo, to_ms=hi, from_day=day_str(lo), to_day=day_str(hi),
                    days=round((hi - lo) / DAY, 1), years=round(years, 3)),
        params=dict({k: v for k, v in p.items() if k != 'interval_sec'},
                    notional_quote=notional, capital_per_slot_quote=capital_slot),
        summary=dict(
            capital_start_quote=round(total_capital, 2),
            slots=len(series), venues=len(venues),
            net_profit_quote=round(net_quote, 2),
            net_profit_pct_on_capital=round(net_quote / total_capital * 100, 4),
            return_per_series_on_capital_pct=w['return_on_capital_pct'],
            apr_on_capital_pct=w['apr_on_capital_pct'],
            hold_through_per_series_on_capital_pct=w['hold_through_on_capital_pct'],
            hold_through_apr_on_capital_pct=round(
                w['hold_through_on_capital_pct'] / max(w['covered_days'] / 365, 1e-9), 4),
            beats_hold_through=bool(w['return_on_capital_pct'] > w['hold_through_on_capital_pct']),
            mean_covered_days=w['covered_days'],
            max_drawdown_pct=round(max_dd, 4), max_drawdown_day=max_dd_day,
            total_trades=len(closed),
            still_open=len([t for t in all_trades if t.get('still_open')]),
            time_in_market_pct=round(
                sum(t['days'] for t in all_trades) / sum(s['covered_days'] for s in series) * 100, 2),
            win_rate_pct=w['win_rate_pct'], avg_hold_days=w['avg_days'],
            total_funding_quote=w['funding_quote'], total_drift_quote=w['drift_quote'],
            total_fees_quote=w['fees_quote'],
            funding_covers_fees_pct=round(w['funding_quote'] / -w['fees_quote'] * 100, 2) if w['fees_quote'] else 0.0,
            exit_reasons=reasons),
        equity_curve=curve,
        yearly_breakdown=yearly,
        venues_breakdown=venues,
        symbols_breakdown=symbols,
        skipped_series=skipped,
        trades=trades_out,
        assumptions_vi=[
            'Luật replay là 4.5f của cmd/execportal/autotrade, KHÔNG phải internal/strategy — cmd/backtest không có trục cho lọc basis lúc vào và chốt lời hội tụ.',
            'CÁC SÀN KHÔNG CÙNG ĐỘ SÂU CORPUS. Mỗi chuỗi mang cửa sổ ĐÃ PHỦ của riêng nó và APR được annualize trên số ngày đó, không phải trên 3 năm. So một sàn có 3 năm với một sàn có 3 tháng trên "cùng cửa sổ" là sai — đọc cột "ngày phủ" trước mọi so sánh.',
            'Chi phí vòng là THAM SỐ nêu tên theo biểu phí taker ĐÃ XÁC MINH của từng sàn trong config.yaml (2× (spot + perp)); dự án không có depth lịch sử nên không định giá được trượt giá theo từng lệnh như priceHolding làm khi chạy thật.',
            'Van chặn trượt giá theo spread (4.5i, 10 bps) KHÔNG replay được: corpus không có sổ lệnh lịch sử. Nó chỉ hoãn lệnh đóng một lượt quét (10 giây) nên trên cửa sổ nhiều năm ảnh hưởng không đáng kể — nhưng nó vắng mặt, và đây là ghi nhận chứ không phải mô hình hoá.',
            'Trượt giá lúc vào được trừ ĐÚNG MỘT LẦN, nằm trong chi phí vòng; phần trôi giá chỉ mang dịch chuyển basis — đúng như PLAN 4.5e ghi về phép đếm hai lần.',
            'Nhịp settle được ĐO từ chính chuỗi đó (quy tắc 3): hyperliquid và kraken settle 1 GIỜ, các sàn còn lại 8 giờ. Vì các luật đếm theo MỐC, sàn giữ 6 mốc là 48 giờ ở sàn 8h nhưng chỉ 6 GIỜ ở sàn 1h — cùng tham số, hai ý nghĩa thời gian.',
            'Chuỗi ghép perp USD với spot USDT (hyperliquid, kraken) mang nhãn quote_bridged: delta-neutral theo COIN nhưng MỞ rủi ro USDT/USD, và không con số nào ở đây trừ khoản đó.',
            'Basis đọc từ nến GIỜ đã đóng: một cú giãn mở và đóng trong cùng một giờ là vô hình ở đây.',
            'Lối vào khi chạy thật còn cần forming rate > 0, độ sâu ≥ 2× notional bốn phía, lệch đồng hồ ≤ 1s và > 5 phút tới mốc settle. Corpus không có các dữ kiện đó; chúng chỉ có thể TỪ CHỐI thêm, nên số lệnh ở đây là CẬN TRÊN.',
            'Funding là mốc rời rạc (quy tắc 6): chỉ các mốc settle SAU khi vào mới được tính, không bao giờ nhân APR với thời gian giữ.',
            'Lợi nhuận quy về VỐN = notional × %.2f. Con số mỗi chuỗi là TRUNG BÌNH trên %d chuỗi, KHÔNG phải tổng — tổng trên nhiều chuỗi không phải một suất sinh lời.' % (K, len(series)),
            'Chưa trừ: phí vay/margin spot, phí chuyển tiền, rủi ro thanh lý, và sổ lệnh TẠI LÚC ĐÓNG. Đây không phải lợi nhuận đã thực hiện.',
        ],
    )

    os.makedirs(os.path.dirname(args.out) or '.', exist_ok=True)
    with open(args.out, 'w', encoding='utf-8') as f:
        json.dump(doc, f, ensure_ascii=False, separators=(',', ':'))
    return doc


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--db', required=True, help='a COPY of the corpus; never the file a live scanner writes')
    ap.add_argument('--out', default='docs/reports/backtest-3y-latest.json')
    ap.add_argument('--csv', default='', help='also write one row per trade')
    ap.add_argument('--notional', type=float, default=50_000.0)
    ap.add_argument('--take-profit', type=float, default=None,
                    help='override target_take_profit_net_pct (%% on capital) to sweep the threshold')
    args = ap.parse_args()

    doc = build(args)
    s, w = doc['summary'], doc['window']
    print('cửa sổ      %s → %s · %d chuỗi trên %d sàn · phủ TB %.0f ngày/chuỗi'
          % (w['from_day'], w['to_day'], s['slots'], s['venues'], s['mean_covered_days']))
    print('lệnh        %d đóng, %d còn mở · thắng %.1f%% · giữ TB %.1f ngày · trong thị trường %.1f%%'
          % (s['total_trades'], s['still_open'], s['win_rate_pct'], s['avg_hold_days'], s['time_in_market_pct']))
    print('mỗi chuỗi   %+.3f%% trên vốn (%.3f%%/năm) · giữ suốt %+.3f%% (%.3f%%/năm) · %s'
          % (s['return_per_series_on_capital_pct'], s['apr_on_capital_pct'],
             s['hold_through_per_series_on_capital_pct'], s['hold_through_apr_on_capital_pct'],
             'VƯỢT giữ suốt' if s['beats_hold_through'] else 'THUA giữ suốt'))
    print('sụt vốn     %.3f%% (%s) · lý do ra %s' % (s['max_drawdown_pct'], s['max_drawdown_day'], s['exit_reasons']))
    print()
    print('%-16s %-6s %-6s %-10s %-10s %-10s %-7s %s'
          % ('sàn', 'chuỗi', 'lệnh', 'trên vốn', '%/năm', 'giữ suốt/n', 'thắng', 'ngày phủ'))
    for v in doc['venues_breakdown']:
        print('%-16s %-6d %-6d %-10.3f %-10.3f %-10.3f %-7.0f %.0f%s'
              % (v['label'], v['series'], v['trades'], v['return_on_capital_pct'],
                 v['apr_on_capital_pct'], v['hold_through_apr_on_capital_pct'],
                 v['win_rate_pct'], v['covered_days'],
                 '  [USD/USDT bridged]' if v['quote_bridged'] else ''))
    if doc['skipped_series']:
        print()
        print('bỏ qua      %d chuỗi thiếu dữ liệu' % len(doc['skipped_series']))

    if args.csv and doc['trades']:
        import csv as _csv
        with open(args.csv, 'w', newline='', encoding='utf-8') as f:
            wtr = _csv.DictWriter(f, fieldnames=list(doc['trades'][0].keys()))
            wtr.writeheader()
            wtr.writerows(doc['trades'])
        print('csv         %s (%d dòng)' % (args.csv, len(doc['trades'])))
    print('json        %s (%.1f KB)' % (args.out, os.path.getsize(args.out) / 1024))


if __name__ == '__main__':
    main()
