#!/usr/bin/env python3
"""Replay the auto-trader's CONVERGENCE-AND-AMORTIZATION rule set (PLAN 4.5f)
over the three-year corpus and write one JSON for the portal's Backtest tab.

Python CHỈ ĐỌC, chạy ngoài tiến trình Go, đúng ranh giới CLAUDE.md quy tắc 8.

WHY THIS EXISTS, and what it is NOT
-----------------------------------
`cmd/backtest` replays `internal/strategy` — the rule set the step-3.5 gate
runs. The auto-trader in `cmd/execportal/autotrade` is a SEPARATE engine by
decision Q18, and two of its four quantitative pillars have no analogue in
`internal/strategy` at all: the entry basis floor and the convergence
take-profit. So `cmd/backtest` cannot answer "how would the NEW mechanism have
done", and adding those axes to it would change what the 3.5 gate journals and
compares. `tools/report/autoparams.py` already set the precedent for replaying
the auto-trader's own rule on the corpus; this file is the same idea moved to
the 4.5f rule set.

Every formula below is a copy of `cmd/execportal/autotrade/signal.go` —
`assessEntry`, `assessExit` and `priceHolding`. Change one side, read the other.

WHAT IT CANNOT SEE, stated once here and again in the JSON's assumptions
-----------------------------------------------------------------------
  * There is NO historical order book. `depth_snapshots` starts where the
    scanner started and holds ~1,500 rows; slippage therefore cannot be
    measured per fill as `priceHolding` measures it live. It is taken as a
    STATED round-trip cost (see COSTS) and named in the output. This is the
    same limitation every backtest in this repo carries.
  * The live entry also requires a forming rate > 0, depth ≥ 2× notional on
    four sides, clock skew ≤ 1s and > 5 min to settlement. None of those are
    in the corpus; they can only REFUSE entries, so what is replayed here is
    an upper bound on how often the bot would have entered.
  * The basis is read from hourly candle closes, so a widening that opened and
    closed inside one hour is invisible.

Không thư viện ngoài. Không quyết định, không thực thi một lệnh nào.
"""
import argparse, json, math, os, sqlite3, sys
from datetime import datetime, timezone, timedelta

H = 3_600_000
DAY = 86_400_000

PAIRS = ['BTCUSDT', 'ETHUSDT', 'SOLUSDT', 'BNBUSDT', 'XRPUSDT', 'DOGEUSDT',
         'LTCUSDT', 'SUIUSDT', 'LINKUSDT', 'UNIUSDT', 'NEARUSDT', 'AAVEUSDT']
PERP, SPOT = 'binance_futures', 'binance_spot'

# Round trip = four taker fills (buy spot + sell perp in, sell spot + buy perp
# out). Keyed exactly as tools/report/autoparams.py keys them so the two tools
# quote the same cost for the same word.
COSTS = {
    'testnet': (0.000941, 'TESTNET đo được: spot 0 + perp 4 bps ×2, cộng trượt giá đo tại $65'),
    'bnb':     (0.002400, 'MAINNET VIP 0 trả phí bằng BNB: spot 7,5 bps ×2 + perp 4,5 bps ×2'),
    'mainnet': (0.003000, 'MAINNET VIP 0 không BNB: spot 10 bps ×2 + perp 5 bps ×2'),
}

# The shipped 4.5f/4.5g defaults, from cmd/execportal/autotrade/state.go.
DEFAULTS = dict(
    min_entry_basis_bps=5.0,
    min_hold_epochs=6,
    target_take_profit_net_pct=0.50,
    exit_negative_funding_rate_bps=-2.0,
    exit_negative_consecutive_epochs=2,
    max_basis_widen_bps=100.0,
    min_net_apr_pct=5.0,
    projection_hold_days=30.0,
    trailing_days=7.0,
    capital_per_notional=1.5,
)


def connect(path):
    # immutable=1 is for a COPY only, and is what this directory uses when the
    # sandbox refuses the fcntl lock that mode=ro needs (autoparams.py says the
    # same). Never point it at the file a live scanner is writing.
    return sqlite3.connect(f'file:{path}?immutable=1', uri=True)


# ─────────────────────────────── read the corpus ───────────────────────────
def load(c, pairs):
    """funding settlements, hourly basis in bps, and the hourly spot close."""
    fund, basis, spot_px = {}, {}, {}
    for s in pairs:
        fund[s] = [(t, r) for t, r in c.execute(
            "SELECT funding_at_ms, rate_per_interval_frac FROM funding_history "
            "WHERE source=? AND symbol=? AND model='discrete' AND rate_type!='Special' "
            "ORDER BY funding_at_ms", [PERP, s])]
        sp = dict(c.execute(
            "SELECT open_time_ms,close_price_quote FROM price_history WHERE source=? AND symbol=?", [SPOT, s]))
        pf = dict(c.execute(
            "SELECT open_time_ms,close_price_quote FROM price_history WHERE source=? AND symbol=?", [PERP, s]))
        basis[s] = {t: (pf[t] - sp[t]) / sp[t] * 1e4 for t in set(sp) & set(pf) if sp[t] > 0}
        spot_px[s] = sp
    return fund, basis, spot_px


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
    modal spacing of this series' own settlement stamps."""
    if len(rows) < 3:
        return 28800
    gaps = {}
    for a, b in zip(rows, rows[1:]):
        g = round((b[0] - a[0]) / 1000)
        if g > 0:
            gaps[g] = gaps.get(g, 0) + 1
    return max(gaps.items(), key=lambda kv: kv[1])[0] if gaps else 28800


def trailing_mean(rows, i, days):
    t0 = rows[i][0] - days * DAY
    w = [r for t, r in rows[:i + 1] if t >= t0]
    return (sum(w) / len(w), len(w)) if len(w) >= 3 else (None, len(w))


# ────────────────────── the rules, copied from signal.go ───────────────────
def running_result_frac(collected_frac, entry_basis, now_basis, cost):
    """priceHolding, expressed per unit of NOTIONAL.

    signal.go computes, in quote:
        funding = Σ rates × perp entry notional
        drift   = (spot_now − spot_in)·qty + (perp_in − perp_now)·qty
        entry   = spot notional × spot taker + perp notional × perp taker
        exit    = current value of each leg × (taker + slippage)
        cash    = funding + drift − entry − exit

    Divide by the notional and the drift term is exactly the basis move, with
    the sign reversed because the position is SHORT the perp:
        drift/notional = −(basis_now − basis_in)/1e4
    Entry and exit together are the stated round trip. Entry slippage is
    therefore counted ONCE, inside that round trip, and never again through the
    drift — the double count PLAN 4.5e records.
    """
    drift = -(now_basis - entry_basis) / 1e4
    return collected_frac + drift - cost


def replay(sym, rows, bs, cost, p):
    """One pair, walked settlement by settlement with the hours between them.

    The funding exit can only fire AT a settlement, because that is when a rate
    exists (rule 6). The basis stop and the take-profit are priced on every
    hourly candle, because the live bot re-prices them on every scan and both
    read the book, not the funding row.
    """
    out, marks = [], []
    # Why a settlement did NOT become an entry. The live bot runs every check
    # even after one fails so the log names all the problems at once; here the
    # checks are ordered, so a refusal is attributed to the FIRST one that bit.
    refused = dict(negative_rate=0, net_apr=0, basis_unreadable=0, basis_floor=0, holding=0)
    thr = threshold_frac(cost, p['projection_hold_days'], p['min_net_apr_pct'], p['interval_sec'])
    tp_frac = p['target_take_profit_net_pct'] / 100 * p['capital_per_notional']
    neg_floor = p['exit_negative_funding_rate_bps'] / 1e4
    pos = None

    def close(pos, t, reason, reason_vi, now_basis):
        net = running_result_frac(pos['col'], pos['eb'], now_basis, cost)
        out.append(dict(
            symbol=sym, entry_ms=pos['t'], exit_ms=t,
            days=(t - pos['t']) / DAY, epochs=pos['ep'],
            entry_basis_bps=pos['eb'], exit_basis_bps=now_basis,
            funding_frac=pos['col'], drift_frac=-(now_basis - pos['eb']) / 1e4,
            cost_frac=cost, net_frac=net, reason=reason, reason_vi=reason_vi))

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
                    marks.append((h, sym, net, False))
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
        reason = None
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
                reason = ('funding', 'Thoát funding âm kéo dài: %d mốc liên tiếp ≤ %.1f bps'
                          % (run, p['exit_negative_funding_rate_bps']))
        if reason:
            close(pos, t, reason[0], reason[1], nb)
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


def hold_through(sym, rows, bs, cost, p):
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
        tot = sum(x for _, x in rows[i + 1:])
        nb = basis_at(bs, rows[-1][0])
        if nb is None:
            nb = eb
        return dict(net_frac=running_result_frac(tot, eb, nb, cost),
                    days=(rows[-1][0] - t) / DAY, trades=1)
    return dict(net_frac=0.0, days=0.0, trades=0)


# ───────────────────────────────── reporting ───────────────────────────────
def iso(ms):
    return datetime.fromtimestamp(ms / 1000, timezone.utc).strftime('%Y-%m-%d %H:%M')


def day_str(ms):
    return datetime.fromtimestamp(ms / 1000, timezone.utc).strftime('%Y-%m-%d')


def year_buckets(lo_ms, hi_ms):
    """Calendar slices anchored on the corpus END, so "2025-2026" means the most
    recent 365 days rather than a calendar year the corpus only half covers."""
    out = []
    hi = hi_ms
    while hi - 365 * DAY >= lo_ms - DAY:
        lo = hi - 365 * DAY
        out.append((day_str(lo)[:4] + '-' + day_str(hi)[:4], lo, hi))
        hi = lo
    return list(reversed(out))


def build(args):
    c = connect(args.db)
    pairs = [s for s in PAIRS]
    fund, basis, spot_px = load(c, pairs)
    cost, cost_vi = COSTS[args.fees]

    p = dict(DEFAULTS)
    p['interval_sec'] = 28800

    notional = args.notional
    capital_slot = notional * p['capital_per_notional']

    usable = [s for s in pairs if len(fund[s]) > 10 and basis[s]]
    skipped = {s: ('không có settlement' if len(fund[s]) <= 10 else 'không có nến hai chân')
               for s in pairs if s not in usable}

    all_trades, all_marks, per_symbol, bench, refusals = [], [], {}, {}, {}
    for s in usable:
        rows = fund[s]
        p_s = dict(p)
        p_s['interval_sec'] = measure_interval_sec(rows)
        ts, marks, refused = replay(s, rows, basis[s], cost, p_s)
        refusals[s] = refused
        all_trades += ts
        all_marks += marks
        per_symbol[s] = ts
        bench[s] = hold_through(s, rows, basis[s], cost, p_s)

    lo = min(min(t for t, _ in fund[s]) for s in usable)
    hi = max(max(t for t, _ in fund[s]) for s in usable)

    # ── equity curve on the PORTFOLIO's capital: one slot per usable pair ──
    total_capital = capital_slot * len(usable)
    closed = sorted((t for t in all_trades if not t.get('still_open')), key=lambda x: x['exit_ms'])
    marks_by_day = {}
    for ms, sym, net, _ in all_marks:
        marks_by_day.setdefault(day_str(ms), {})[sym] = net
    realized_by_day = {}
    for t in closed:
        realized_by_day.setdefault(day_str(t['exit_ms']), []).append(t)

    curve, run_real, peak, max_dd, max_dd_day = [], 0.0, total_capital, 0.0, None
    d = lo - (lo % DAY)
    open_marks = {}
    while d <= hi:
        ds = day_str(d)
        # Marks first, realizations second: a position closing today may also
        # have left a mark earlier today, and popping before the update would
        # let that mark back in and count the same trade twice.
        open_marks.update(marks_by_day.get(ds, {}))
        for t in realized_by_day.get(ds, []):
            run_real += t['net_frac'] * notional
            open_marks.pop(t['symbol'], None)
        unreal = sum(v * notional for v in open_marks.values())
        eq = total_capital + run_real + unreal
        peak = max(peak, eq)
        dd = (peak - eq) / peak * 100 if peak > 0 else 0.0
        if dd > max_dd:
            max_dd, max_dd_day = dd, ds
        curve.append(dict(time=ds, value=round(eq, 2), drawdown=round(-dd, 4)))
        d += DAY

    net_quote = run_real + sum(v * notional for v in open_marks.values())
    years = (hi - lo) / DAY / 365
    wins = [t for t in closed if t['net_frac'] > 0]

    # ── per-year and per-symbol, both quoted PER SERIES ON CAPITAL ──
    yearly = []
    for label, a, b in year_buckets(lo, hi):
        ts = [t for t in closed if a <= t['exit_ms'] < b]
        act = sorted({t['symbol'] for t in ts})
        pnl = sum(t['net_frac'] for t in ts) * notional
        yearly.append(dict(
            label=label, from_day=day_str(a), to_day=day_str(b),
            trades=len(ts), symbols_traded=len(act),
            pnl_quote=round(pnl, 2),
            return_on_capital_pct=round(sum(t['net_frac'] for t in ts) / p['capital_per_notional'] * 100 / max(1, len(usable)), 4),
            win_rate_pct=round(len([t for t in ts if t['net_frac'] > 0]) / len(ts) * 100, 2) if ts else 0.0,
            funding_quote=round(sum(t['funding_frac'] for t in ts) * notional, 2),
            drift_quote=round(sum(t['drift_frac'] for t in ts) * notional, 2),
            fees_quote=round(-sum(t['cost_frac'] for t in ts) * notional, 2)))

    symbols = []
    for s in usable:
        ts = [t for t in per_symbol[s] if not t.get('still_open')]
        op = [t for t in per_symbol[s] if t.get('still_open')]
        tot = sum(t['net_frac'] for t in per_symbol[s])
        symbols.append(dict(
            symbol=s, trades=len(ts), still_open=len(op),
            net_frac=round(tot, 6),
            pnl_quote=round(tot * notional, 2),
            return_on_capital_pct=round(tot / p['capital_per_notional'] * 100, 4),
            apr_on_capital_pct=round(tot / p['capital_per_notional'] * 100 / years, 4),
            win_rate_pct=round(len([t for t in ts if t['net_frac'] > 0]) / len(ts) * 100, 2) if ts else 0.0,
            avg_days=round(sum(t['days'] for t in ts) / len(ts), 2) if ts else 0.0,
            funding_quote=round(sum(t['funding_frac'] for t in per_symbol[s]) * notional, 2),
            drift_quote=round(sum(t['drift_frac'] for t in per_symbol[s]) * notional, 2),
            fees_quote=round(-sum(t['cost_frac'] for t in per_symbol[s]) * notional, 2),
            hold_through_on_capital_pct=round(bench[s]['net_frac'] / p['capital_per_notional'] * 100, 4)))
    symbols.sort(key=lambda x: -x['return_on_capital_pct'])

    reasons = {}
    for t in closed:
        reasons[t['reason']] = reasons.get(t['reason'], 0) + 1

    trades_out = [dict(
        symbol=t['symbol'], venue='binance',
        entry_time=iso(t['entry_ms']), exit_time=iso(t['exit_ms']),
        entry_ms=t['entry_ms'], exit_ms=t['exit_ms'],
        duration_days=round(t['days'], 3), epochs=t['epochs'],
        entry_basis_bps=round(t['entry_basis_bps'], 2),
        exit_basis_bps=round(t['exit_basis_bps'], 2),
        funding_received=round(t['funding_frac'] * notional, 4),
        drift_quote=round(t['drift_frac'] * notional, 4),
        fees_paid=round(-t['cost_frac'] * notional, 4),
        pnl_quote=round(t['net_frac'] * notional, 4),
        pnl_pct=round(t['net_frac'] / p['capital_per_notional'] * 100, 4),
        exit_reason=t['reason'], exit_reason_vi=t['reason_vi'],
        still_open=bool(t.get('still_open'))) for t in sorted(all_trades, key=lambda x: x['entry_ms'])]

    hold_avg = sum(b['net_frac'] for b in bench.values()) / len(usable) / p['capital_per_notional'] * 100
    per_series_cap = sum(t['net_frac'] for t in all_trades) / len(usable) / p['capital_per_notional'] * 100

    doc = dict(
        built_at_ms=int(datetime.now(timezone.utc).timestamp() * 1000),
        label_vi='BACKTEST 3 NĂM — luật 4.5f (Hội tụ Basis & Khấu hao Phí) replay trên corpus thật',
        mode='backtest',
        engine='tools/report/bt3y.py — bản chép luật của cmd/execportal/autotrade/signal.go',
        window=dict(from_ms=lo, to_ms=hi, from_day=day_str(lo), to_day=day_str(hi),
                    days=round((hi - lo) / DAY, 1), years=round(years, 3)),
        params=dict(p, notional_quote=notional, capital_per_slot_quote=capital_slot,
                    round_trip_cost_frac=cost, round_trip_cost_vi=cost_vi,
                    fees_profile=args.fees, perp_source=PERP, spot_source=SPOT),
        summary=dict(
            capital_start_quote=round(total_capital, 2),
            slots=len(usable),
            net_profit_quote=round(net_quote, 2),
            net_profit_pct_on_capital=round(net_quote / total_capital * 100, 4),
            return_per_series_on_capital_pct=round(per_series_cap, 4),
            apr_on_capital_pct=round(per_series_cap / years, 4),
            hold_through_per_series_on_capital_pct=round(hold_avg, 4),
            hold_through_apr_on_capital_pct=round(hold_avg / years, 4),
            beats_hold_through=bool(per_series_cap > hold_avg),
            max_drawdown_pct=round(max_dd, 4),
            max_drawdown_day=max_dd_day,
            total_trades=len(closed),
            # The number that explains the rest: a rule that banks a small gain
            # and waits is a rule whose capital is idle most of the window, and
            # an idle slot earns nothing while hold-through keeps collecting.
            time_in_market_pct=round(
                sum(t['days'] for t in all_trades) / (len(usable) * (hi - lo) / DAY) * 100, 2),
            still_open=len([t for t in all_trades if t.get('still_open')]),
            win_rate_pct=round(len(wins) / len(closed) * 100, 2) if closed else 0.0,
            avg_hold_days=round(sum(t['days'] for t in closed) / len(closed), 2) if closed else 0.0,
            total_funding_quote=round(sum(t['funding_frac'] for t in all_trades) * notional, 2),
            total_drift_quote=round(sum(t['drift_frac'] for t in all_trades) * notional, 2),
            total_fees_quote=round(-sum(t['cost_frac'] for t in all_trades) * notional, 2),
            funding_covers_fees_pct=round(
                sum(t['funding_frac'] for t in all_trades) / sum(t['cost_frac'] for t in all_trades) * 100, 2)
            if sum(t['cost_frac'] for t in all_trades) else 0.0,
            exit_reasons=reasons),
        equity_curve=curve,
        yearly_breakdown=yearly,
        symbols_breakdown=symbols,
        exit_reason_stats={k: dict(
            trades=v,
            avg_pnl_quote=round(sum(t['net_frac'] for t in closed if t['reason'] == k) / v * notional, 2),
            avg_days=round(sum(t['days'] for t in closed if t['reason'] == k) / v, 2),
            wins=len([t for t in closed if t['reason'] == k and t['net_frac'] > 0]))
            for k, v in reasons.items()},
        entry_refusals=refusals,
        skipped_symbols=skipped,
        trades=trades_out,
        assumptions_vi=[
            'Luật replay là 4.5f của cmd/execportal/autotrade, KHÔNG phải internal/strategy — cmd/backtest không có trục cho hai trụ cột lọc basis lúc vào và chốt lời hội tụ.',
            'Chi phí vòng là THAM SỐ nêu tên (%s), không phải đo từ sổ lệnh: dự án không có depth lịch sử, nên không thể định giá trượt giá theo từng lệnh như priceHolding làm khi chạy thật.' % cost_vi,
            'Trượt giá lúc vào được trừ ĐÚNG MỘT LẦN, nằm trong chi phí vòng; phần trôi giá chỉ mang dịch chuyển basis — đúng như PLAN 4.5e ghi về phép đếm hai lần.',
            'Basis đọc từ nến GIỜ đã đóng: một cú giãn mở và đóng trong cùng một giờ là vô hình ở đây.',
            'Lối vào khi chạy thật còn cần forming rate > 0, độ sâu ≥ 2× notional bốn phía, lệch đồng hồ ≤ 1s và > 5 phút tới mốc settle. Corpus không có các dữ kiện đó; chúng chỉ có thể TỪ CHỐI thêm, nên số lệnh ở đây là CẬN TRÊN.',
            'Funding là mốc rời rạc (quy tắc 6): chỉ các mốc settle SAU khi vào mới được tính, không bao giờ nhân APR với thời gian giữ.',
            'Lợi nhuận quy về VỐN = notional × %.2f (spot trọn notional + ký quỹ perp). Con số mỗi chuỗi là TRUNG BÌNH trên %d cặp, không phải tổng — tổng trên nhiều cặp không phải một suất sinh lời.' % (p['capital_per_notional'], len(usable)),
            'Chưa trừ: phí vay/margin spot, phí chuyển tiền, rủi ro thanh lý, và sổ lệnh TẠI LÚC ĐÓNG. Đây không phải lợi nhuận đã thực hiện.',
        ],
        corpus=dict(
            perp_rows={s: len(fund[s]) for s in usable},
            candle_hours={s: len(basis[s]) for s in usable},
            interval_sec_measured={s: measure_interval_sec(fund[s]) for s in usable}),
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
    ap.add_argument('--fees', default='mainnet', choices=sorted(COSTS))
    args = ap.parse_args()

    doc = build(args)
    s = doc['summary']
    print('cửa sổ      %s → %s (%.1f ngày, %d cặp)' % (
        doc['window']['from_day'], doc['window']['to_day'], doc['window']['days'], s['slots']))
    print('lệnh        %d đóng, %d còn mở · thắng %.1f%% · giữ TB %.1f ngày' % (
        s['total_trades'], s['still_open'], s['win_rate_pct'], s['avg_hold_days']))
    print('mỗi chuỗi   %+.3f%% trên vốn (%.3f%%/năm) · giữ suốt %+.3f%% (%.3f%%/năm) · %s' % (
        s['return_per_series_on_capital_pct'], s['apr_on_capital_pct'],
        s['hold_through_per_series_on_capital_pct'], s['hold_through_apr_on_capital_pct'],
        'VƯỢT giữ suốt' if s['beats_hold_through'] else 'THUA giữ suốt'))
    print('sụt vốn     %.3f%% (%s)' % (s['max_drawdown_pct'], s['max_drawdown_day']))
    print('funding %.0f · trôi giá %.0f · phí %.0f quote · funding bù %.0f%% phí' % (
        s['total_funding_quote'], s['total_drift_quote'], s['total_fees_quote'], s['funding_covers_fees_pct']))
    print('lý do ra    %s' % s['exit_reasons'])

    if args.csv:
        import csv as _csv
        with open(args.csv, 'w', newline='', encoding='utf-8') as f:
            w = _csv.DictWriter(f, fieldnames=list(doc['trades'][0].keys())) if doc['trades'] else None
            if w:
                w.writeheader()
                w.writerows(doc['trades'])
        print('csv         %s (%d dòng)' % (args.csv, len(doc['trades'])))
    print('json        %s (%.1f KB)' % (args.out, os.path.getsize(args.out) / 1024))


if __name__ == '__main__':
    main()
