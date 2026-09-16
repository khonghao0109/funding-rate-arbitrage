#!/usr/bin/env python3
"""Thẩm định bộ tham số của auto-trader (cmd/execportal/autotrade) trên corpus thật.

Python CHỈ ĐỌC, chạy ngoài tiến trình Go, đúng ranh giới CLAUDE.md quy tắc 8.
Khác với các script còn lại trong thư mục này, nó KHÔNG đọc CSV của cmd/backtest:
auto-trader là một luật riêng (vào bằng trung bình trượt 7 ngày, ra ở mốc settle
âm đầu tiên) mà cmd/backtest chưa có trục để quét. Nên nó replay luật ấy ngay
trên corpus, và mọi công thức dưới đây là bản chép ĐÚNG của
cmd/execportal/autotrade/signal.go — đọc chéo hai file khi sửa bất kỳ bên nào:

    ngưỡng vào : NetAPR = (mean * floor(hold/iv) - cost) * 365/hold >= MinNetAPRPct
    ra         : mốc settle đầu tiên SAU khi vào có rate <= 0
                 hoặc basis giãn quá MaxBasisWidenBps so với lúc vào

Không thư viện ngoài. Không script nào ở đây quyết định hay thực thi một lệnh.
"""
import argparse, bisect, json, math, sqlite3, statistics as st, sys
from datetime import datetime, timezone

PAIRS = ['BTCUSDT','ETHUSDT','SOLUSDT','BNBUSDT','XRPUSDT','DOGEUSDT',
         'LTCUSDT','SUIUSDT','LINKUSDT','UNIUSDT','NEARUSDT','AAVEUSDT']
PERP, SPOT = 'binance_futures', 'binance_spot'
H = 3_600_000
IV = 28800          # đo được: cả 12 cặp settle 8h tại binance trong cửa sổ này

# Chi phí vòng = 4 lần khớp taker (mua spot + bán perp vào, bán spot + mua perp ra).
COSTS = {
    'testnet':  (0.000941, 'TESTNET đo được: spot 0 + perp 4 bps ×2, cộng trượt giá đo tại $65'),
    'bnb':      (0.002400, 'MAINNET VIP 0 trả phí bằng BNB: spot 7,5 bps ×2 + perp 4,5 bps ×2'),
    'mainnet':  (0.003000, 'MAINNET VIP 0 không BNB: spot 10 bps ×2 + perp 5 bps ×2'),
}


# ─────────────────────────────── đọc corpus ────────────────────────────────
def connect(path, immutable):
    # mode=ro là quy ước của thư mục này. immutable=1 chỉ dùng trên BẢN SAO,
    # khi sandbox chặn khoá fcntl mà mode=ro cần.
    uri = f'file:{path}?immutable=1' if immutable else f'file:{path}?mode=ro'
    return sqlite3.connect(uri, uri=True)


def load(c):
    fund, basis_h, basis_m, rules, px = {}, {}, {}, {}, {}
    for s in PAIRS:
        fund[s] = [(t, r) for t, r in c.execute(
            "SELECT funding_at_ms, rate_per_interval_frac FROM funding_history "
            "WHERE source=? AND symbol=? AND model='discrete' AND rate_type!='Special' "
            "ORDER BY funding_at_ms", [PERP, s])]
        sp = dict(c.execute("SELECT open_time_ms,close_price_quote FROM price_history WHERE source=? AND symbol=?", [SPOT, s]))
        pf = dict(c.execute("SELECT open_time_ms,close_price_quote FROM price_history WHERE source=? AND symbol=?", [PERP, s]))
        basis_h[s] = {t: (pf[t]-sp[t])/sp[t]*1e4 for t in set(sp) & set(pf) if sp[t] > 0}
        # Mẫu ~2 phút, CHỈ mẫu tươi: một mẫu có recv_at_ms trễ ghi lại một feed
        # chết, và đo nó là đo độ trễ chứ không phải basis (schema nói đúng thế).
        q = ("SELECT sampled_at_ms,mid_price_quote FROM price_snapshots "
             "WHERE source=? AND symbol=? AND mid_price_quote>0 AND sampled_at_ms-recv_at_ms<=30000")
        a = dict(c.execute(q, [SPOT, s])); b = dict(c.execute(q, [PERP, s]))
        ts = sorted(set(a) & set(b))
        basis_m[s] = (ts, [(b[t]-a[t])/a[t]*1e4 for t in ts])
        r = c.execute("SELECT close_price_quote FROM price_history WHERE source=? AND symbol=? "
                      "ORDER BY open_time_ms DESC LIMIT 1", [PERP, s]).fetchone()
        px[s] = r[0] if r else 0.0
    day = c.execute("SELECT MAX(snapshot_day) FROM instrument_snapshots").fetchone()[0]
    for sym, src, step, minq, minn in c.execute(
            "SELECT symbol,source,step_size_coin,min_qty_coin,min_notional_quote FROM instrument_snapshots "
            "WHERE snapshot_day=? AND source IN (?,?)", [day, SPOT, PERP]):
        rules.setdefault(sym, {})[src] = (step, minq, minn)
    return fund, basis_h, basis_m, rules, px, day


# ──────────────────────── luật, chép từ signal.go ──────────────────────────
def threshold_frac(cost, hold_days, min_apr_pct, iv=IV):
    """Trung bình trượt tối thiểu để NetAPR đạt ngưỡng — nghịch đảo của strategy.NetAPR."""
    n = math.floor(hold_days * 86400 / iv)
    return (cost + min_apr_pct/100 * hold_days/365) / n if n else float('inf')


def trailing_mean(rows, i, days):
    t0 = rows[i][0] - days * 86_400_000
    w = [r for t, r in rows[:i+1] if t >= t0]
    return (sum(w)/len(w), len(w)) if len(w) >= 3 else (None, len(w))


def basis_at(bs, t):
    """Basis của nến đã đóng gần nhất tại hoặc trước t, tối đa 2 giờ — luật của
    internal/backtest: không bao giờ dùng nến đang chạy, vì close của nó nằm ở
    tương lai."""
    for k in (t, t-H, t-2*H):
        h = k - (k % H)
        if h in bs:
            return bs[h]
    return None


def replay(sym, fund, basis_h, cost, hold_days=30.0, min_apr=5.0, trail_days=7,
           max_epochs=0, widen=30.0, neg_x_bps=0.0, neg_n=1, min_hold=0,
           use_basis=True, basis_pnl=True):
    rows, bs = fund[sym], basis_h[sym]
    thr = threshold_frac(cost, hold_days, min_apr)
    out, pos = [], None
    for i, (t, r) in enumerate(rows):
        if pos is None:
            if r <= 0:                                  # mốc settle gần nhất > 0
                continue
            m, _ = trailing_mean(rows, i, trail_days)
            if m is None or m < thr:
                continue
            pos = dict(t=t, col=0.0, ep=0, eb=basis_at(bs, t), negrun=0)
            continue
        pos['col'] += r                                 # mốc này đã được thu
        pos['ep'] += 1
        reason = None
        if r <= 0:
            pos['negrun'] += 1
            if pos['ep'] > min_hold and (-r*1e4) >= neg_x_bps and pos['negrun'] >= neg_n:
                reason = 'funding'
        else:
            pos['negrun'] = 0
        if reason is None and max_epochs and pos['ep'] >= max_epochs:
            reason = 'epochs'
        if reason is None and use_basis and pos['eb'] is not None:
            start = rows[i-1][0] if pos['ep'] > 1 else pos['t']
            h = start - (start % H)
            while h <= t:
                b = bs.get(h)
                if b is not None and b - pos['eb'] > widen:
                    reason = 'basis'
                    break
                h += H
        if reason:
            out.append(_close(pos, t, bs, cost, reason, basis_pnl))
            pos = None
    if pos is not None:
        out.append(_close(pos, rows[-1][0], bs, cost, 'open', basis_pnl))
    return out


def _close(pos, t, bs, cost, reason, basis_pnl):
    be = basis_at(bs, t)
    bp = -(be - pos['eb'])/1e4 if (basis_pnl and be is not None and pos['eb'] is not None) else 0.0
    return dict(days=(t-pos['t'])/86_400_000, ep=pos['ep'],
                gross=pos['col'], basis=bp, net=pos['col'] - cost + bp, reason=reason)


def hold_through(sym, fund, basis_h, cost, hold_days=30.0, min_apr=5.0, trail_days=7):
    """Vào ở mốc đủ điều kiện ĐẦU TIÊN rồi không bao giờ ra — mốc chuẩn của repo."""
    rows, bs = fund[sym], basis_h[sym]
    thr = threshold_frac(cost, hold_days, min_apr)
    for i, (t, r) in enumerate(rows):
        if r <= 0:
            continue
        m, _ = trailing_mean(rows, i, trail_days)
        if m is None or m < thr:
            continue
        tot = sum(x for _, x in rows[i+1:])
        eb, be = basis_at(bs, t), basis_at(bs, rows[-1][0])
        bp = -(be-eb)/1e4 if (eb is not None and be is not None) else 0.0
        return [dict(days=(rows[-1][0]-t)/86_400_000, ep=len(rows)-i-1,
                     gross=tot, basis=bp, net=tot-cost+bp, reason='hold')]
    return []


def aggregate(per_pair):
    """Trả về trung bình mỗi cặp trên VỐN (= notional/1.5) — mẫu số duy nhất so
    được với dải 5–15%/năm của dự án. Tổng trên nhiều cặp KHÔNG phải một suất
    sinh lời."""
    tot = sum(v[0] for v in per_pair.values())
    n = sum(v[1] for v in per_pair.values())
    win = sum(v[2] for v in per_pair.values())
    return tot/len(per_pair), n/len(per_pair), win


def run_set(fn, fund, basis_h, cost, **kw):
    per = {}
    for s in PAIRS:
        ts = fn(s, fund, basis_h, cost, **kw)
        per[s] = (sum(t['net'] for t in ts)*100, len(ts), sum(1 for t in ts if t['net'] > 0), ts)
    return per


# ────────────────────────────── các phép đo ────────────────────────────────
def funding_stats(fund):
    out = {}
    for s in PAIRS:
        rr = [r*1e4 for _, r in fund[s]]
        runs, cur = [], 0
        for r in rr:
            if r > 0:
                cur += 1
            else:
                if cur:
                    runs.append(cur)
                cur = 0
        if cur:
            runs.append(cur)
        runs.sort()
        mean = sum(rr)/len(rr)
        out[s] = dict(n=len(rr), mean=mean, median=st.median(rr),
                      pos_share=sum(1 for r in rr if r > 0)/len(rr)*100,
                      flips=sum(1 for a, b in zip(rr, rr[1:]) if a > 0 and b <= 0),
                      run_p50=runs[len(runs)//2] if runs else 0,
                      run_p90=runs[int(len(runs)*0.9)] if runs else 0,
                      run_max=max(runs) if runs else 0)
    return out


def _pct(v, q):
    v = sorted(v)
    return v[min(len(v)-1, int(q*len(v)))]


def basis_stats(basis_h, basis_m):
    """Hai độ phân giải, mỗi cái trả lời một nửa câu hỏi và cái nào cũng thiếu
    nửa kia: nến giờ phủ cả năm nhưng bỏ sót đột biến trong giờ; mẫu 2 phút
    thấy đột biến nhưng chỉ có 11,6 ngày của MỘT chế độ thị trường."""
    out = {}
    for s in PAIRS:
        bh = basis_h[s]
        ts = sorted(bh)
        b = [bh[t] for t in ts]
        hourly = {}
        for label, hz, thr in (('30d_30', 720, 30), ('30d_50', 720, 50), ('30d_100', 720, 100),
                               ('72h_30', 72, 30), ('24h_30', 24, 30)):
            fired = tried = 0
            for i in range(len(b)-hz):
                tried += 1
                if max(b[i:i+hz+1]) - b[i] > thr:
                    fired += 1
            hourly[label] = fired/tried*100 if tried else float('nan')
        mts, mb = basis_m[s]
        minute = {}
        for label, hz, thr in (('8h_30', 8*H, 30), ('24h_30', 24*H, 30), ('72h_30', 72*H, 30), ('72h_100', 72*H, 100)):
            fired = tried = 0
            for i, t in enumerate(mts):
                end = t + hz
                if not mts or mts[-1] < end:
                    break
                k = bisect.bisect_right(mts, end)
                tried += 1
                if max(mb[i:k]) - mb[i] > thr:
                    fired += 1
            minute[label] = fired/tried*100 if tried else float('nan')
        out[s] = dict(n_hourly=len(b), p1=_pct(b, .01), p50=_pct(b, .50), p99=_pct(b, .99),
                      lo=min(b), hi=max(b), hourly=hourly,
                      n_min=len(mb), m_p1=_pct(mb, .01) if mb else 0, m_p50=_pct(mb, .50) if mb else 0,
                      m_p99=_pct(mb, .99) if mb else 0, m_lo=min(mb) if mb else 0, m_hi=max(mb) if mb else 0,
                      minute=minute)
    return out


def liquidation(c):
    """Biên độ TĂNG giá bất lợi cho chân short perp, đọc từ HIGH của nến giờ —
    một vị thế short chết vì một cây nến râu, và close theo giờ bước qua nó
    (luật internal/backtest đã dùng cho mô hình thanh lý)."""
    out = {}
    for s in PAIRS:
        rows = list(c.execute("SELECT close_price_quote,high_price_quote FROM price_history "
                              "WHERE source=? AND symbol=? ORDER BY open_time_ms", [PERP, s]))
        cl = [r[0] for r in rows]
        hi = [r[1] for r in rows]

        def exc(w):
            return [(max(hi[i+1:i+1+w])/cl[i]-1)*100 for i in range(len(cl)-w)]
        e7, e30 = exc(168), exc(720)
        n = len(e30)
        out[s] = dict(p50_7=_pct(e7, .5), p99_7=_pct(e7, .99), max_7=max(e7),
                      p50_30=_pct(e30, .5), p99_30=_pct(e30, .99), max_30=max(e30),
                      br_5x=sum(1 for x in e30 if x > 19.5)/n*100,
                      br_3x=sum(1 for x in e30 if x > 32.5)/n*100,
                      br_2x=sum(1 for x in e30 if x > 49.4)/n*100)
    return out


def sizing(rules, px):
    """Lượng tử hoá kích thước lệnh. internal/execution định cỡ trên lưới THÔ
    hơn của hai chân rồi làm tròn XUỐNG, nên hai chân luôn bằng nhau — sai số
    rơi vào NOTIONAL, không rơi vào độ lệch delta giữa hai chân."""
    out = []
    for s in PAIRS:
        f, sp = rules[s][PERP], rules[s][SPOT]
        step, minq, minn, p = max(f[0], sp[0]), max(f[1], sp[1]), max(f[2], sp[2]), px[s]
        q65 = math.floor(65/p/step)*step if p and step else 0
        out.append(dict(sym=s, price=p, step=step, step_usd=step*p, minqty_usd=minq*p, minn=minn,
                        qty65=q65, real65=q65*p, dev65=(q65*p-65)/65*100 if q65 else float('nan'),
                        n1pct=max(minq*p, minn, step*p*100), n5pct=max(minq*p, minn, step*p*20)))
    out.sort(key=lambda r: r['n5pct'])
    return out


def settle_window(c):
    """Spread và basis theo khoảng cách tới mốc settle, gộp 5 phút một."""
    buckets = {}
    for s in PAIRS:
        q = ("SELECT sampled_at_ms,best_bid_quote,best_ask_quote,mid_price_quote FROM price_snapshots "
             "WHERE source=? AND symbol=? AND mid_price_quote>0 AND sampled_at_ms-recv_at_ms<=30000")
        pf = {t: (b, a, m) for t, b, a, m in c.execute(q, [PERP, s])}
        sp = {t: (b, a, m) for t, b, a, m in c.execute(q, [SPOT, s])}
        for t in sorted(set(pf) & set(sp)):
            sec = (t//1000) % 86400
            off = min((sec - k*IV for k in range(86400//IV)), key=lambda d: abs(d) if abs(d) < IV//2 else 10**9)
            if abs(off) > 1800:
                continue
            bkt = (off//300)*300 if off >= 0 else -(((-off)//300)*300 + 300)
            pb, pa, pm = pf[t]
            sb, sa, sm = sp[t]
            buckets.setdefault(bkt, []).append(((pa-pb)/pm*1e4, (sa-sb)/sm*1e4, (pm-sm)/sm*1e4))
    rows = []
    for bkt in sorted(buckets):
        v = buckets[bkt]
        if len(v) < 50:
            continue
        bas = st.median(x[2] for x in v)
        disp = sorted(abs(x[2]-bas) for x in v)
        rows.append(dict(lo=bkt//60, hi=bkt//60+5, n=len(v),
                         perp=st.median(x[0] for x in v), spot=st.median(x[1] for x in v),
                         basis=bas, disp95=disp[int(len(disp)*.95)]))
    return rows


# ──────────────────────────────── dựng HTML ────────────────────────────────
def esc(s):
    return (str(s).replace('&', '&amp;').replace('<', '&lt;').replace('>', '&gt;'))


def cls(v, good=0.0):
    return 'gain' if v > good else ('loss' if v < good else 'muted')


def tbl(head, rows, note=None):
    h = ''.join(f'<th class="{"num" if n else ""}">{esc(t)}</th>' for t, n in head)
    body = []
    for r in rows:
        tds = []
        for (t, n), cell in zip(head, r):
            if isinstance(cell, tuple):
                txt, klass = cell
            else:
                txt, klass = cell, ''
            tds.append(f'<td class="{"num " if n else ""}{klass}">{txt}</td>')
        body.append('<tr>' + ''.join(tds) + '</tr>')
    out = f'<div class="tablewrap"><table><thead><tr>{h}</tr></thead><tbody>{"".join(body)}</tbody></table></div>'
    if note:
        out += f'<p class="note">{note}</p>'
    return out


def section(sid, eyebrow, title, intro, *blocks):
    return (f'<section id="{sid}"><header><div class="eyebrow">{eyebrow}</div><h2>{title}</h2>'
            f'<p>{intro}</p></header>' + ''.join(blocks) + '</section>')


def fig(v, label, klass=''):
    return f'<div class="fig"><div class="v {klass}">{v}</div><div class="l">{label}</div></div>'


def build(M):
    fs, bs, lq, sz, sw, sn = M['funding'], M['basis'], M['liq'], M['sizing'], M['settle'], M['sens']
    R = M['runs']
    cur_m, ht_m = R['mainnet']['current'][0], R['mainnet']['hold'][0]
    cur_t, ht_t = R['testnet']['current'][0], R['testnet']['hold'][0]
    rec_m, rec_t = R['mainnet']['rec'][0], R['testnet']['rec'][0]

    verdict = f'''<section class="verdict">
<div class="eyebrow">Phán quyết</div>
<h1>Bộ tham số đang chạy LỖ trên chính corpus của nó, và cái làm nó lỗ là luật THOÁT, không phải ngưỡng vào</h1>
<p>Replay đúng luật của <span class="mono">cmd/execportal/autotrade</span> trên 12 tháng funding Binance thật
(12 cặp × 1.096 mốc settle 8h, cộng nến giờ cả hai chân để tính trôi basis): với biểu phí mainnet VIP 0
bộ đang chạy làm <strong class="{cls(cur_m/1.5)}">{cur_m/1.5:+.2f}%/năm trên vốn</strong> mỗi cặp,
trong khi <em>vào một lần rồi không bao giờ ra</em> làm <strong class="{cls(ht_m/1.5)}">{ht_m/1.5:+.2f}%</strong>.
Khoảng cách {abs(cur_m-ht_m)/1.5:.2f} điểm là tiền trả cho {R['mainnet']['current'][1]:.1f} vòng phí mỗi cặp mỗi năm
mà luật thoát “mốc settle ≤ 0” tạo ra. Ba phát hiện có thể chặn lệnh ngay hôm nay nằm ở mục 1.</p>
<div class="figures">
{fig(f"{cur_m/1.5:+.2f}%", "Bộ hiện tại, mainnet — %/năm trên vốn", cls(cur_m/1.5))}
{fig(f"{ht_m/1.5:+.2f}%", "Giữ suốt, cùng luật vào", cls(ht_m/1.5))}
{fig(f"{rec_m/1.5:+.2f}%", "Bộ khuyến nghị", cls(rec_m/1.5))}
{fig(f"{R['mainnet']['current'][1]:.1f}", "Lệnh/cặp/năm hiện tại")}
{fig(f"{R['mainnet']['rec'][1]:.1f}", "Lệnh/cặp/năm khuyến nghị")}
</div>
<p class="note"><strong>Đọc mẫu số trước.</strong> Mọi con số %/năm ở đây là TRÊN VỐN của một cặp
(= 1,5 × notional: spot trọn vẹn + 50% ký quỹ perp), trung bình trên 12 cặp — không phải tổng, và không phải
trên notional một chân. Chia cho 1,5 đã được làm sẵn. Mốc so sánh duy nhất có nghĩa là <em>giữ suốt</em>:
một luật không hơn được nó thì luật ấy chỉ đang bán thanh khoản lấy phí.</p>
</section>'''

    nav = ('<nav class="pagenav" aria-label="mục">'
           '<a href="#risk">1 · Rủi ro &amp; lỗ hổng</a><a href="#sens">2 · Độ nhạy</a>'
           '<a href="#params">3 · Khuyến nghị</a><a href="#rules">4 · Nguyên tắc</a>'
           '<a href="#method">Cách đo</a></nav>')

    # ── 1. rủi ro ──
    frows = []
    for s in PAIRS:
        f = fs[s]
        be = 30/f['mean'] if f['mean'] > 0 else float('inf')
        frows.append([s[:-4], f"{f['mean']:.4f}", f"{f['median']:.4f}", f"{f['pos_share']:.1f}%",
                      f"{f['flips']}", f"{f['run_p50']}", f"{f['run_p90']}",
                      ("∞" if math.isinf(be) else f"{be:.0f}", 'loss' if math.isinf(be) or be > 1095 else ''),
                      ("∞" if math.isinf(be) else f"{be/3:.0f}", 'loss' if math.isinf(be) or be > 365 else '')])
    t_fund = tbl([('cặp', 0), ('mean bps/8h', 1), ('trung vị', 1), ('% mốc > 0', 1), ('lần đảo dấu/năm', 1),
                  ('chuỗi dương P50', 1), ('P90', 1), ('mốc để hoà 30 bps', 1), ('= ngày', 1)], frows,
                 'Chuỗi dương P50 là số mốc settle dương liên tiếp điển hình — chính là kỳ giữ mà luật thoát hiện tại tạo ra. '
                 'Cột cuối là số ngày cần để funding trả xong MỘT vòng phí mainnet 30 bps.')

    risk = section('risk', 'Mục 1', 'Rủi ro và lỗ hổng toán học', 
        'Bốn lỗi, xếp theo số tiền. Lỗi thứ nhất và thứ hai chặn lệnh hoặc đốt vốn ngay ở phiên đầu trên mainnet.',
        f'''<h3>① Nghịch lý khấu hao: bot tự cấp cho mình 90 mốc funding rồi thoát sau 2–4 mốc</h3>
<p>Đây chính là câu hỏi 1 của anh, và corpus định lượng được nó. Khi vào lệnh, <span class="mono">holdPlan()</span>
trả về 30 ngày ⇒ <span class="mono">strategy.NetAPR</span> nhân trung bình trượt với
<span class="mono">floor(30×86400/28800) = 90</span> mốc. Nhưng luật thoát “mốc settle đầu tiên sau khi vào ≤ 0”
cắt vị thế sau <strong>trung vị 2–4 mốc</strong> (cột “chuỗi dương P50”), tức bot thu về
<strong>khoảng 1/22 đến 1/45 phần</strong> lượng funding nó đã dùng để biện minh cho lệnh.
Hệ quả số học: ở BTC, funding trung bình {fs['BTCUSDT']['mean']:.4f} bps/8h cần
<strong>{30/fs['BTCUSDT']['mean']:.0f} mốc = {30/fs['BTCUSDT']['mean']/3:.0f} ngày</strong> để trả xong một vòng phí mainnet,
trong khi chuỗi dương điển hình của nó dài {fs['BTCUSDT']['run_p50']} mốc. Mọi lệnh đóng bởi luật này
gần như chắc chắn lỗ, và nó đóng {fs['BTCUSDT']['flips']}–{max(f['flips'] for f in fs.values())} lần một năm mỗi cặp.</p>
{t_fund}
<h3 style="margin-top:1.4rem">② $65 notional KHÔNG đặt được lệnh BTC trên mainnet</h3>
<p>Luật mainnet đọc từ <span class="mono">instrument_snapshots</span> ngày {M['day']}:
BTCUSDT futures có <span class="mono">minQty = stepSize = 0,001 BTC</span>, ở giá {sz[-1]['price']:,.0f} USDT là
<strong>{sz[-1]['minqty_usd']:.0f} USDT</strong> — lớn hơn notional {65} USDT đang cấu hình.
<span class="mono">broker.RoundOrder</span> sẽ TỪ CHỐI (<span class="mono">ErrBelowMinQty</span>), đúng thiết kế, và cặp BTC
sẽ không bao giờ vào được lệnh trên tiền thật. Testnet không lộ ra điều này vì luật testnet khác
(step 0,0001 BTC). Bảng dưới còn cho thấy bước nhảy thô làm lệch notional tới
{max(abs(r['dev65']) for r in sz if not math.isnan(r['dev65'])):.1f}% ở $65 — hai chân vẫn cân nhau
(<span class="mono">internal/execution</span> định cỡ trên lưới thô hơn rồi làm tròn xuống), nên sai số rơi vào
kích thước vị thế chứ không phải độ lệch delta.</p>
{tbl([('cặp',0),('giá USDT',1),('1 bước = $',1),('lệnh nhỏ nhất $',1),('minNotional',1),('lượng $ tại N=65',1),('lệch notional',1),('N cho lượng tử ≤5%',1)],
     [[r['sym'][:-4], f"{r['price']:,.4f}", f"{r['step_usd']:.2f}", f"{r['minqty_usd']:.2f}", f"{r['minn']:.0f}",
       (f"{r['real65']:.2f}" if r['qty65'] else ('KHÔNG ĐẶT ĐƯỢC','loss')),
       ('—' if math.isnan(r['dev65']) else (f"{r['dev65']:+.2f}%", 'loss' if abs(r['dev65'])>3 else '')),
       f"{r['n5pct']:,.0f}"] for r in sz],
     'Bước nhảy thô = max(step spot, step futures) vì cả hai chân được làm tròn lên cùng lưới đó.')}
<h3 style="margin-top:1.4rem">③ Stop-loss basis 30 bps cắt đúng lúc lỗ đã xảy ra, và ở độ phân giải bot thật sự quét thì nó bắn liên tục trên 3 cặp</h3>
<p>Với vị thế long spot + short perp, lãi/lỗ giá của cặp bằng <strong>−(basis lúc ra − basis lúc vào)</strong>:
basis giãn ra là lỗ, và <em>đóng lệnh tại đó là hiện thực hoá khoản lỗ rồi trả thêm một vòng phí</em>, trong khi
cơ chế funding kéo basis hội tụ trở lại. Đo trên Binance cùng sàn, basis rất chặt:
P1–P99 nằm trong khoảng {min(b['p1'] for b in bs.values()):.1f} … {max(b['p99'] for b in bs.values()):.1f} bps.
Ở <strong>nến giờ suốt 12 tháng</strong>, ngưỡng 30 bps gần như không bao giờ chạm (0,0% cửa sổ 30 ngày trên 9/12 cặp).
Nhưng ở <strong>mẫu 2 phút</strong> — đúng độ phân giải bot quét — nó chạm
{max(b['minute']['72h_30'] for b in bs.values()):.0f}% trong 72 giờ trên NEAR, {bs['SOLUSDT']['minute']['72h_30']:.0f}% trên SOL,
{bs['XRPUSDT']['minute']['72h_30']:.0f}% trên XRP. Nguyên nhân không phải “thị trường biến động” mà là
<strong>vào lệnh đúng lúc basis đang lõm</strong>: basis NEAR chạm đáy {bs['NEARUSDT']['m_lo']:.0f} bps rồi
trở về trung vị {bs['NEARUSDT']['m_p50']:.1f} bps, và mức hồi bình thường ấy được đọc là “giãn 30 bps”.
Lỗ hổng thật nằm ở CHIỀU VÀO — bot không có bất kỳ kiểm tra basis nào lúc vào lệnh.</p>
{tbl([('cặp',0),('nến giờ P1',1),('P50',1),('P99',1),('thấp nhất',1),('cao nhất',1),('P(giãn>30bps)/30 ngày',1),
      ('mẫu 2ph P50',1),('thấp nhất',1),('P(giãn>30bps)/72h',1),('P(>100bps)/72h',1)],
     [[s[:-4], f"{bs[s]['p1']:.2f}", f"{bs[s]['p50']:.2f}", f"{bs[s]['p99']:.2f}", f"{bs[s]['lo']:.1f}", f"{bs[s]['hi']:.1f}",
       (f"{bs[s]['hourly']['30d_30']:.1f}%", 'warn' if bs[s]['hourly']['30d_30']>1 else ''),
       f"{bs[s]['m_p50']:.2f}", f"{bs[s]['m_lo']:.1f}",
       (f"{bs[s]['minute']['72h_30']:.1f}%", 'loss' if bs[s]['minute']['72h_30']>10 else ''),
       f"{bs[s]['minute']['72h_100']:.1f}%"] for s in PAIRS],
     'Nến giờ: 12 tháng, luôn tươi vì lấy từ REST klines. Mẫu 2 phút: 11,6 ngày, CHỈ mẫu có recv_at_ms trễ ≤30s — '
     'không lọc thì các đỉnh +200…+294 bps của feed chết bị đọc nhầm thành basis thật, và đó là cái bẫy đầu tiên phép đo này gặp.')}
<h3 style="margin-top:1.4rem">④ Ký quỹ 50% KHÔNG miễn nhiễm thanh lý trên altcoin</h3>
<p>Giả định “2× là an toàn tuyệt đối” không đứng vững. Giá thanh lý của chân short ở ký quỹ f và ký quỹ duy trì
0,4% là <span class="mono">entry × (1+f)/(1+mm)</span>, tức <strong>+49,4%</strong> ở f=0,50, +32,5% ở f=0,33 và +19,5% ở f=0,20.
Đọc từ ĐỈNH nến giờ (một lệnh short chết vì râu nến, close theo giờ bước qua nó), trong 12 tháng qua tỉ lệ cửa sổ
30 ngày có biên tăng vượt mốc đó là: UNI {lq['UNIUSDT']['br_2x']:.1f}% và NEAR {lq['NEARUSDT']['br_2x']:.1f}% ngay ở mức 2×.
Điều cứu vị thế không phải mức ký quỹ mà là <strong>chân spot lãi đúng bằng lúc chân perp lỗ</strong> — và điều đó chỉ
có tác dụng nếu hai chân nằm trong CÙNG một tài khoản có bù trừ ký quỹ. Ở ví spot và ví futures tách rời (mặc định),
ví futures bị thanh lý một mình còn khoản lãi spot nằm ngoài tầm với.</p>
{tbl([('cặp',0),('7 ngày P50',1),('P99',1),('max',1),('30 ngày P50',1),('P99',1),('max',1),
      ('% cửa sổ 30d vượt 5×',1),('vượt 3×',1),('vượt 2×',1)],
     [[s[:-4], f"{lq[s]['p50_7']:.1f}", f"{lq[s]['p99_7']:.1f}", f"{lq[s]['max_7']:.1f}",
       f"{lq[s]['p50_30']:.1f}", f"{lq[s]['p99_30']:.1f}", f"{lq[s]['max_30']:.1f}",
       (f"{lq[s]['br_5x']:.1f}%", 'loss' if lq[s]['br_5x']>10 else 'warn' if lq[s]['br_5x']>0 else ''),
       (f"{lq[s]['br_3x']:.1f}%", 'loss' if lq[s]['br_3x']>10 else 'warn' if lq[s]['br_3x']>0 else ''),
       (f"{lq[s]['br_2x']:.1f}%", 'loss' if lq[s]['br_2x']>5 else 'warn' if lq[s]['br_2x']>0 else '')] for s in PAIRS],
     'Biên độ tăng giá lớn nhất trong cửa sổ, tính bằng % so với giá đóng cửa lúc vào. Ký quỹ duy trì 0,4% là bậc 1 '
     'của BTC; altcoin thường cao hơn, nên các ngưỡng trên là mốc LẠC QUAN.')}''')

    # ── 2. độ nhạy ──
    srows = []
    for r in sn:
        srows.append([f"{r['days']:g}", f"{r['n']}", f"{r['be']:.4f}", f"{r['thr5']:.4f}",
                      (f"{r['clears']}/12", 'loss' if r['clears'] == 0 else 'gain'),
                      f"{r['gross']:.2f}", (f"{r['net']:+.2f}", cls(r['net'])),
                      (f"{r['apr']:+.2f}%", cls(r['apr']))])
    t_sens = tbl([('kỳ giữ (ngày)', 0), ('số mốc 8h', 1), ('rate hoà vốn bps/8h', 1), ('rate để NetAPR ≥ 5%', 1),
                  ('số cặp đạt', 1), ('gộp tại cặp trung vị bps', 1), ('ròng bps', 1), ('APR ròng trên vốn', 1)], srows,
                 'Cột “số cặp đạt” đếm số cặp trong 12 cặp có funding trung bình 12 THÁNG vượt ngưỡng của dòng đó. '
                 'Cột hai bên phải tính tại cặp trung vị (mean %.4f bps/8h), chi phí vòng 30 bps.' % M['median_mean'])

    def variant_rows(key):
        out = []
        for label, (val, n, win) in R[key]['variants'].items():
            out.append([label, (f"{val/1.5:+.2f}%", cls(val/1.5)), f"{n:.1f}",
                        (f"{(val-R[key]['hold'][0])/1.5:+.2f}", cls(val-R[key]['hold'][0]))])
        return out
    t_var_m = tbl([('biến thể luật (chi phí mainnet 30 bps)', 0), ('%/năm trên vốn', 1), ('lệnh/cặp', 1), ('so với giữ suốt', 1)],
                  variant_rows('mainnet'))
    t_var_t = tbl([('biến thể luật (chi phí testnet 9,41 bps)', 0), ('%/năm trên vốn', 1), ('lệnh/cặp', 1), ('so với giữ suốt', 1)],
                  variant_rows('testnet'))

    prows = []
    for s in PAIRS:
        cu = R['mainnet']['per_current'][s]
        re = R['mainnet']['per_rec'][s]
        ho = R['mainnet']['per_hold'][s]
        prows.append([s[:-4], f"{fs[s]['mean']:.4f}",
                      (f"{cu[0]/1.5:+.2f}%", cls(cu[0])), f"{cu[1]}",
                      (f"{re[0]/1.5:+.2f}%", cls(re[0])), f"{re[1]}",
                      (f"{ho[0]/1.5:+.2f}%", cls(ho[0]))])
    t_pairs = tbl([('cặp', 0), ('mean bps/8h', 1), ('hiện tại %/năm vốn', 1), ('lệnh', 1),
                   ('khuyến nghị %/năm vốn', 1), ('lệnh', 1), ('giữ suốt %/năm vốn', 1)], prows,
                  'Bộ khuyến nghị trùng khít giữ suốt trên 10/12 cặp (1 lệnh, cùng con số) — đó không phải trùng hợp: '
                  'nó hội tụ về giữ suốt bằng cách gần như không bao giờ thoát.')

    sens = section('sens', 'Mục 2', 'Bảng phân tích độ nhạy', 
        'Hai phép đo khác nhau. Bảng đầu là số học thuần: một kỳ giữ cần funding bao nhiêu mới hoà. '
        'Bảng sau là replay: từng biến thể của luật chạy trên đúng 12 tháng corpus.',
        f'''{t_sens}
<p style="margin-top:1rem"><strong>Đọc bảng này là đọc câu trả lời cho câu hỏi 1.</strong> Ở phí mainnet,
<em>không một cặp nào trong 12 cặp</em> đạt ngưỡng “NetAPR ≥ 5%” trên funding trung bình 12 tháng của chính nó —
kể cả khi giả định giữ trọn 365 ngày (cần {sn[-1]['thr5']:.4f} bps/8h, cặp cao nhất là LINK với {max(f['mean'] for f in fs.values()):.4f}).
Vậy nên hạ <span class="mono">ProjectionHoldDays</span> xuống 3–7 ngày cho “thực tế hơn” sẽ khiến bot
<strong>không bao giờ vào lệnh nữa</strong> — và về mặt toán học đó là câu trả lời ĐÚNG ở biểu phí này.
Con số 30 ngày hiện tại không phải một dự báo, nó là cái van duy nhất đang cho phép các lệnh cận biên lọt qua.</p>
<h3 style="margin-top:1.4rem">Replay từng biến thể, 12 cặp × 12 tháng</h3>
<p>Mỗi dòng đổi đúng MỘT thứ so với bộ đang chạy. Cột cuối là khoảng cách tới giữ suốt — mốc chuẩn duy nhất có nghĩa.</p>
{t_var_m}
<p class="note" style="margin-top:.6rem">Điều đáng chú ý: nâng <span class="mono">MinNetAPRPct</span> lên 10% hay kéo dài
cửa sổ trailing lên 30 ngày đều cho 0 lệnh — chúng không sửa được luật thoát, chúng chỉ tắt bot.
Thứ thật sự trả tiền là <strong>cổng thoát âm</strong>: đòi mốc âm phải SÂU (≥ 2–3 bps) và LẶP LẠI (2–3 mốc liên tiếp)
trước khi được đóng, đưa kết quả từ {cur_m/1.5:+.2f}% lên {rec_m/1.5:+.2f}%.</p>
<div style="margin-top:1.2rem">{t_var_t}</div>
<h3 style="margin-top:1.4rem">Phân rã theo cặp</h3>
{t_pairs}''')

    # ── 3. khuyến nghị ──
    P = [
      ('NotionalQuote', '65 USDT', '65 USDT — giữ', '≥ 250 USDT/cặp; BTC ≥ 1.560 hoặc BỎ BTC',
       f"Luật mainnet: BTC minQty 0,001 BTC = {sz[-1]['minqty_usd']:.0f} USDT > 65 ⇒ lệnh bị từ chối. "
       f"AAVE lệch {[r['dev65'] for r in sz if r['sym']=='AAVEUSDT'][0]:+.1f}%, UNI "
       f"{[r['dev65'] for r in sz if r['sym']=='UNIUSDT'][0]:+.1f}% vì bước nhảy thô. 250 USDT giữ lượng tử hoá ≤5% trên 10/12 cặp."),
      ('MinNetAPRPct', '5,0 %/năm', '5,0 — giữ', '5,0 — giữ',
       'Không phải cái van đúng. Nâng lên 10% cho 0 lệnh ở cả hai biểu phí: nó tắt bot chứ không sửa luật thoát. '
       'Để nguyên và sửa ở chiều ra.'),
      ('MaxHoldEpochs', '0', '0 — giữ', '0 — giữ',
       'Thoát theo số mốc không giúp: mọi biến thể MaxHoldEpochs > 0 đều cắt ngắn hơn điểm hoà vốn. '
       'Giữ 0 và để cổng thoát âm quyết định.'),
      ('ProjectionHoldDays', '30,0 ngày', '30,0 — giữ, nhưng ghi rõ là GIẢ ĐỊNH', '30,0 — giữ, và chỉ đọc cùng cổng thoát mới',
       'Đặt 3–7 ngày cho “thực tế” ⇒ ngưỡng 1,9–3,8 bps/8h ⇒ 0 lệnh trên mọi cặp. Con số này chỉ hợp lệ khi luật thoát '
       'thật sự cho phép giữ 30 ngày — tức là sau khi có cổng thoát âm và sàn giữ tối thiểu bên dưới.'),
      ('Cổng thoát âm X / N <span class="pill fail">CHƯA CÓ</span>', 'không có (thoát ở mốc ≤ 0 đầu tiên)',
       'X = 2 bps, N = 2 mốc liên tiếp', 'X = 3 bps, N = 3 mốc liên tiếp',
       'Trục có giá trị nhất trong toàn bộ bản thẩm định. Mainnet: {a:+.2f}% → {b:+.2f}% trên vốn. '
       'Testnet: {c:+.2f}% → {d:+.2f}%. Một mốc âm điển hình đáng ~0,3 bps, một vòng phí đáng 24–30 bps: '
       'rời đi vì một mốc âm là trả gấp trăm lần thứ mình tránh. '
       '<span class="mono">strategy.Params.ExitNegativeMinBps/Periods</span> đã có sẵn ở nhánh backtest.'),
      ('Sàn giữ tối thiểu M <span class="pill fail">CHƯA CÓ</span>', 'không có',
       '6 mốc (2 ngày)', '6 mốc (2 ngày)',
       'Cấm mọi lối thoát funding trước khi vị thế đã qua 6 mốc. Một mình nó đưa mainnet từ {a:+.2f}% lên {e:+.2f}%; '
       'đi kèm cổng X/N thì phần đóng góp nhỏ lại nhưng nó là cái chặn trường hợp xấu nhất — vào rồi ra trong một ngày.'),
      ('MaxBasisWidenBps', '30 bps', '100 bps', '100 bps',
       'Ở mẫu 2 phút, 30 bps bị chạm trong 72 giờ với xác suất {f:.0f}% (NEAR), {g:.0f}% (SOL), {h:.0f}% (XRP) — '
       'và mỗi lần chạm là hiện thực hoá lỗ basis rồi trả thêm một vòng phí. Trong 12 tháng nến giờ, biên giãn chưa '
       'bao giờ vượt 50 bps trên bất kỳ cặp nào, nên 100 bps là chốt an toàn thật (sự cố sàn, depeg) chứ không phải '
       'stop-loss thường trực.'),
      ('Kiểm tra basis lúc VÀO <span class="pill fail">CHƯA CÓ</span>', 'không có',
       'từ chối nếu basis &lt; P10 trượt 7 ngày', 'từ chối nếu basis &lt; P10 trượt 7 ngày',
       'Đây mới là chỗ lỗ basis thật sự sinh ra. Vào lúc basis đang lõm (perp rẻ bất thường) là bán perp giá rẻ; '
       'khi nó hồi về trung vị, cặp lỗ đúng phần chênh — NEAR lõm tới {i:.0f} bps so với trung vị {j:.1f} bps. '
       'Một kiểm tra rẻ ở chiều vào thay thế được cả cái stop-loss ở chiều ra.'),
      ('MinTimeToSettle', '5 phút', '2 phút', '2 phút',
       'Không đo được bất thường nào quanh mốc settle: spread perp/spot phẳng ở {k:.3f}/{l:.3f} bps và basis phẳng '
       'trên toàn bộ 12 khoảng 5 phút quanh mốc. Trong khi đó cửa sổ mất cân bằng đo được khi mở là ~400 ms và gỡ chân '
       'lỗi là 225–284 ms, nên 2 phút vẫn còn dư 300 lần. Vào sát mốc còn có thông tin TỐT HƠN (rate gần như đã chốt) '
       'và bắt đầu nhận tiền ngay thay vì chờ tới 8 giờ.'),
      ('DepthMultiple', '2,0 ×', '2,0 — giữ', '2,0 tới N ≤ 500 USDT; 5,0 khi N ≥ 5.000',
       'Ở $65 mọi sổ lệnh vượt hàng trăm lần nên phép kiểm này chưa ràng buộc gì. Nó chỉ bắt đầu có nghĩa khi notional '
       'lớn, và lúc đó phía cần lo là BID của chân spot lúc thoát.'),
      ('Cooldown', '60 giây', '1 mốc settle (8 giờ)', '1 mốc settle (8 giờ)',
       'Chi phí bằng 0 vì luật vào đã đòi một mốc dương mới, nhưng nó là đai an toàn chống vòng lặp đóng-mở '
       'nếu có ai nới cổng thoát về sau.'),
      ('ScanInterval', '10 giây', '30 giây', '60 giây',
       'Tín hiệu là 8 tiếng một lần; quét 10 giây không thêm thông tin nào mà nhân trọng số request lên 6 lần. '
       'Sau khi ngưỡng basis lên 100 bps thì không còn lối thoát nào cần phản xạ dưới một phút.'),
      ('MarginFrac', '0,50 (2×)', '0,50 — giữ', '0,50 — giữ; 0,33 CHỈ cho BTC/ETH và chỉ khi hai chân bù trừ ký quỹ chung',
       'Ở 3× (f=0,33) giá thanh lý là +32,5%, mà biên tăng 30 ngày lớn nhất của ETH trong năm qua là {m:.1f}% — '
       'đã vượt. Ngay ở 2×, {n:.1f}% cửa sổ 30 ngày của UNI và {o:.1f}% của NEAR chạm mốc thanh lý. '
       'Hiệu suất vốn thật không nằm ở việc hạ ký quỹ mà ở việc để lãi chân spot bù lỗ chân perp TRONG CÙNG '
       'một tài khoản — cần kiểm chứng điều kiện tham gia Portfolio Margin của Binance trước khi dựa vào nó.'),
      ('MaxConsecutiveFailures', '5 lần', '5 — giữ', '3 lần',
       'Trên tiền thật, đọc hỏng 3 lần liên tiếp đã đủ để dừng và nhìn. Halt không đóng vị thế nên chi phí của một '
       'lần halt thừa là gần bằng 0.'),
      ('Danh sách cặp', '12 cặp', '12 — giữ để lấy mẫu',
       'BTC, ETH, LINK, UNI, LTC, SUI, AAVE — BỎ SOL, XRP, BNB, DOGE, NEAR',
       'SOL có funding trung bình ÂM ({p:.4f} bps/8h) suốt 12 tháng — không có luật thoát nào cứu được. '
       'XRP {q:.4f} bps cần {r:.0f} ngày để hoà một vòng phí. BNB có trung vị đúng 0,0000 và chỉ {s:.1f}% mốc dương. '
       'NEAR vừa dẫn đầu về đảo dấu ({t} lần/năm) vừa dẫn đầu về rủi ro thanh lý.'),
    ]
    fmt = dict(a=cur_m/1.5, b=rec_m/1.5, c=cur_t/1.5, d=rec_t/1.5,
               e=R['mainnet']['variants']['+ sàn giữ tối thiểu 6 mốc'][0]/1.5,
               f=bs['NEARUSDT']['minute']['72h_30'], g=bs['SOLUSDT']['minute']['72h_30'],
               h=bs['XRPUSDT']['minute']['72h_30'], i=bs['NEARUSDT']['m_lo'], j=bs['NEARUSDT']['m_p50'],
               k=sw[0]['perp'] if sw else 0, l=sw[0]['spot'] if sw else 0,
               m=lq['ETHUSDT']['max_30'], n=lq['UNIUSDT']['br_2x'], o=lq['NEARUSDT']['br_2x'],
               p=fs['SOLUSDT']['mean'], q=fs['XRPUSDT']['mean'],
               r=30/fs['XRPUSDT']['mean']/3 if fs['XRPUSDT']['mean'] > 0 else float('inf'),
               s=fs['BNBUSDT']['pos_share'], t=fs['NEARUSDT']['flips'])
    prow = [[n, cu, (f'<strong>{te}</strong>', ''), (f'<strong>{ma}</strong>', ''), (why.format(**fmt), 'wrap')]
            for n, cu, te, ma, why in P]
    t_params = tbl([('tham số', 0), ('hiện tại', 0), ('đề xuất TESTNET', 0), ('đề xuất MAINNET', 0), ('cơ sở định lượng', 0)], prow)
    params = section('params', 'Mục 3', 'Bảng khuyến nghị thông số tối ưu',
        'Ba dòng có nhãn <span class="pill fail">CHƯA CÓ</span> là tham số phải VIẾT THÊM, không phải chỉnh. '
        'Chúng chiếm gần như toàn bộ phần cải thiện đo được; mọi dòng còn lại chỉ là dọn dẹp.',
        t_params,
        f'''<p style="margin-top:1rem"><strong>Bộ khuyến nghị đầy đủ chạy được gì:</strong> mainnet
{rec_m/1.5:+.2f}%/năm trên vốn ở {R['mainnet']['rec'][1]:.1f} lệnh/cặp, so với {cur_m/1.5:+.2f}% ở
{R['mainnet']['current'][1]:.1f} lệnh của bộ hiện tại và {ht_m/1.5:+.2f}% của giữ suốt.
Testnet {rec_t/1.5:+.2f}% so với {cur_t/1.5:+.2f}% và {ht_t/1.5:+.2f}%.
<strong>Nó hơn bộ hiện tại {abs(rec_m-cur_m)/1.5:.2f} điểm và chỉ ngang giữ suốt.</strong>
Đó là kết luận trung thực, và nó khớp với ba năm đo trước đó của chính dự án: trên rổ này chưa
có bộ tham số nào vượt được giữ suốt, và phần thắng lớn nhất còn lại là <em>đừng thoát</em>.</p>''')

    # ── 4. nguyên tắc vận hành ──
    port = []
    for cap in (1000, 2000, 5000, 10000):
        picked, used = [], 0.0
        for r in sz:
            need = r['n5pct']*1.5
            if used + need <= cap:
                picked.append(r['sym'][:-4])
                used += need
        port.append([f"${cap:,}", f"{len(picked)}", ', '.join(picked) if picked else '—', f"${used:,.0f}"])
    rules_html = section('rules', 'Mục 4', 'Lời khuyên vận hành', 
        'Năm nguyên tắc, mỗi cái gắn với một con số ở trên chứ không phải một nguyên lý chung.',
        f'''<ol style="max-width:70ch;display:grid;gap:1rem;padding-left:1.2rem">
<li><strong>Mốc so sánh là GIỮ SUỐT, không phải số 0.</strong> Một bộ tham số “có lãi” vẫn có thể đang phá huỷ giá trị:
bộ hiện tại làm {cur_m/1.5:+.2f}% khi không làm gì cả đã cho {ht_m/1.5:+.2f}%. Trước khi bật bất kỳ luật thoát nào,
hãy chạy nó cạnh giữ suốt trên cùng corpus và cùng luật vào. Nếu không hơn, luật ấy chỉ là phí.</li>
<li><strong>Không bao giờ thoát vì MỘT mốc âm.</strong> Một đợt funding âm điển hình đáng vài phần mười bps;
một vòng phí đáng 24–30 bps. Hỏi đúng câu: <em>chi phí KỲ VỌNG của việc giữ qua đợt âm này có lớn hơn một vòng phí không?</em>
Hầu như luôn là không. Đó là lý do cổng X/N đáng giá {abs(rec_m-cur_m)/1.5:.2f} điểm.</li>
<li><strong>Chọn cặp quan trọng hơn chọn tham số.</strong> Phương sai giữa các cặp lớn hơn phương sai giữa các bộ tham số:
LINK giữ suốt cho {R['mainnet']['per_hold']['LINKUSDT'][0]/1.5:+.2f}% còn SOL cho {R['mainnet']['per_hold']['SOLUSDT'][0]/1.5:+.2f}%,
trong khi toàn bộ khoảng cách giữa bộ tốt nhất và bộ tệ nhất của lưới nhỏ hơn thế. Loại một cặp có funding trung bình âm
là quyết định lớn hơn mọi lần chỉnh ngưỡng.</li>
<li><strong>Quy mô: đặt sàn theo BƯỚC NHẢY, không theo ví.</strong> Notional tối thiểu của một cặp là cái lớn hơn giữa
minQty, minNotional và 20 bước nhảy (để lượng tử hoá ≤5%). Với vốn nhỏ, bỏ hẳn BTC thay vì đặt lệnh 1 bước:
một vị thế BTC ở mainnet nhảy theo từng {sz[-1]['minqty_usd']:.0f} USDT.</li>
<li><strong>Vốn bị trói là 1,5× notional MỖI CẶP, và đó là mẫu số của mọi lời tuyên bố.</strong> 12 cặp × 65 USDT
không phải “65 đô rủi ro”, nó là {12*65*1.5:,.0f} USDT vốn. Mọi con số %/năm phải chia cho nó trước khi đem so
với dải mục tiêu 5–15% — và trên rổ này, ở biểu phí mainnet, dải đó chưa từng đạt được bởi bất kỳ bộ nào,
kể cả giữ suốt.</li>
</ol>
<h3 style="margin-top:1.4rem">Danh mục khả thi theo vốn (lượng tử hoá ≤5%, vốn = 1,5 × notional)</h3>
{tbl([('vốn tài khoản',0),('số cặp',1),('các cặp',0),('vốn thực dùng ở mức tối thiểu',1)], port,
     'Đây là sàn kỹ thuật, không phải phân bổ đề xuất: phần vốn còn lại nên nâng notional của các cặp đã chọn '
     'thay vì thêm cặp yếu. Với $1.000, bỏ BTC là bắt buộc chứ không phải tuỳ chọn.')}''')

    # ── cách đo ──
    srows = [[f"{r['lo']:+d}…{r['hi']:+d}", f"{r['n']}", f"{r['perp']:.3f}", f"{r['spot']:.3f}",
              f"{r['basis']:.3f}", f"{r['disp95']:.2f}"] for r in sw]
    method = section('method', 'Cách đo', 'Nguồn số và giới hạn của chúng',
        'Mọi con số trên trang này đến từ corpus của chính dự án, đọc ở chế độ chỉ-đọc. Không con số nào là ước lượng.',
        f'''<div class="kv">
<dt>Funding</dt><dd><span class="mono">funding_history</span>, {PERP}, 12 cặp × {fs['BTCUSDT']['n']} mốc đã settle,
{M['from_d']} → {M['to_d']}, lọc <span class="mono">rate_type='Special'</span>. Chu kỳ đo được là 8h trên cả 12 cặp.</dd>
<dt>Giá &amp; basis</dt><dd><span class="mono">price_history</span> nến giờ cho CẢ HAI chân (binance_spot và binance_futures),
8.637 giờ mỗi chuỗi; cộng <span class="mono">price_snapshots</span> mẫu ~2 phút trong 11,6 ngày cho phần basis độ phân giải cao.</dd>
<dt>Luật sản phẩm</dt><dd>Luật vào/ra chép từ <span class="mono">cmd/execportal/autotrade/signal.go</span>;
công thức APR chép từ <span class="mono">internal/strategy.NetAPR</span>; luật làm tròn từ
<span class="mono">internal/execution</span> + <span class="mono">broker.RoundOrder</span>;
luật sàn từ <span class="mono">instrument_snapshots</span> ngày {M['day']} (endpoint CÔNG KHAI của mainnet).</dd>
<dt>Chi phí</dt><dd>Bốn lần khớp taker. Testnet 9,41 bps là con số đo được thật của dự án; mainnet 24 và 30 bps
là biểu phí VIP 0 công bố. Trượt giá ở $65 đo được là 0,00 bps trên 20 lần khớp — nhưng con số đó KHÔNG chuyển được
sang mainnet hay sang quy mô lớn hơn.</dd>
</div>
<h3 style="margin-top:1.4rem">Hành vi quanh mốc settle (câu hỏi 4)</h3>
<p>Không có bất thường nào đo được: spread và basis phẳng trên toàn bộ ±30 phút. Cửa sổ “hạ nhiệt” sau mốc settle
<strong>không có cơ sở đo lường</strong> trên rổ này — nhưng phép đo chỉ có 11,6 ngày và khoảng 35 mốc mỗi cặp,
trong một chế độ thị trường yên, và mẫu 2 phút không nhìn thấy điều gì xảy ra trong vòng vài giây quanh mốc.</p>
{tbl([('phút so với mốc',0),('số mẫu',1),('spread perp bps',1),('spread spot bps',1),('basis trung vị bps',1),('phân tán basis P95',1)], srows)}
<h3 style="margin-top:1.4rem">Năm giới hạn phải đọc cùng mọi con số ở trên</h3>
<ol style="max-width:70ch;display:grid;gap:.5rem;padding-left:1.2rem;font-size:.9rem">
<li><strong>Một năm là một chế độ.</strong> Đo trước của dự án trên 3 năm cho thấy funding giảm ~5 lần qua ba năm
(giữ suốt 7,49% → 4,20% → 1,42% trên vốn). Cửa sổ 12 tháng này là lát cắt thấp nhất trong ba lát đó.</li>
<li><strong>Danh sách cặp là IN-SAMPLE.</strong> 13 cặp trong <span class="mono">config.yaml</span> được sàng lọc ngày
2026-09-09 trên chính 12 tháng này, nên mọi thứ chạy trên chúng đều được ưu ái.</li>
<li><strong>Trượt giá không backfill được.</strong> Chi phí vòng dùng biểu phí cộng một hằng số trượt giá, không phải
sổ lệnh lịch sử — <span class="mono">depth_snapshots</span> chỉ có 1.579 dòng và bắt đầu từ lúc scanner chạy.</li>
<li><strong>“Forming rate &gt; 0” không có trong corpus.</strong> Replay dùng “mốc settle gần nhất &gt; 0” thay cho nó,
nên số lệnh vào có thể hơi cao hơn thực tế; mọi kết luận ở đây là về luật THOÁT nên hướng lệch này không đổi dấu chúng.</li>
<li><strong>Basis lấy từ nến giờ đã đóng</strong> cho phần 12 tháng, nên nó BỎ SÓT đột biến trong giờ — đó chính là lý do
cột mẫu 2 phút phải nằm cạnh, và là lý do ngưỡng 30 bps trông vô hại ở cột này mà nguy hiểm ở cột kia.</li>
</ol>''')

    return verdict + nav + risk + sens + params + rules_html + method


REC = {'testnet': dict(widen=100.0, neg_x_bps=2.0, neg_n=2, min_hold=6),
       'bnb':     dict(widen=100.0, neg_x_bps=3.0, neg_n=3, min_hold=6),
       'mainnet': dict(widen=100.0, neg_x_bps=3.0, neg_n=3, min_hold=6)}


def measure_runs(fund, basis_h, key):
    cost = COSTS[key][0]

    def one(**kw):
        per = run_set(replay, fund, basis_h, cost, **kw)
        return aggregate({s: per[s][:3] for s in PAIRS}), {s: per[s][:3] for s in PAIRS}
    cur, per_cur = one()
    rec, per_rec = one(**REC[key])
    per_h = {s: (lambda ts: (sum(t['net'] for t in ts)*100, len(ts), sum(1 for t in ts if t['net'] > 0)))
             (hold_through(s, fund, basis_h, cost)) for s in PAIRS}
    hold = aggregate(per_h)
    variants = {}
    variants['bộ hiện tại'] = cur
    variants['+ tắt stop basis'] = one(use_basis=False)[0]
    for hd in (7, 14, 90):
        variants[f'+ ProjectionHoldDays = {hd} ngày'] = one(use_basis=False, hold_days=float(hd))[0]
    for x, n in ((0, 2), (0, 3), (1, 2), (2, 2), (3, 3)):
        variants[f'+ cổng thoát âm: sâu ≥ {x} bps VÀ {n} mốc âm liên tiếp'] = one(use_basis=False, neg_x_bps=float(x), neg_n=n)[0]
    for mh in (3, 6, 30, 90):
        variants[f'+ sàn giữ tối thiểu {mh} mốc'] = one(use_basis=False, min_hold=mh)[0]
    for td in (14, 30):
        variants[f'+ cửa sổ trailing {td} ngày'] = one(use_basis=False, trail_days=td)[0]
    for ap in (10, 20):
        variants[f'+ MinNetAPRPct = {ap}%'] = one(use_basis=False, min_apr=float(ap))[0]
    variants['BỘ KHUYẾN NGHỊ (cổng + sàn giữ + basis 100 bps)'] = rec
    variants['giữ suốt — mốc chuẩn'] = hold
    return dict(current=cur, rec=rec, hold=hold, per_current=per_cur, per_rec=per_rec,
                per_hold=per_h, variants=variants)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--db', default='data/scanner.db')
    ap.add_argument('--immutable', action='store_true',
                    help='mở bằng immutable=1 thay cho mode=ro — CHỈ dùng trên bản sao')
    ap.add_argument('--out', default=None)
    ap.add_argument('--template', default='tools/report/autoparams.template.html')
    a = ap.parse_args()

    c = connect(a.db, a.immutable)
    fund, basis_h, basis_m, rules, px, day = load(c)
    fs = funding_stats(fund)
    median_mean = st.median(f['mean'] for f in fs.values())
    sens = []
    for hdays in (0.67, 1, 3, 7, 14, 30, 60, 90, 180, 365):
        n = math.floor(hdays*86400/IV)
        if not n:
            continue
        thr5 = threshold_frac(0.0030, hdays, 5.0)
        gross = median_mean*n
        sens.append(dict(days=hdays, n=n, be=30/n, thr5=thr5*1e4,
                         clears=sum(1 for f in fs.values() if f['mean'] >= thr5*1e4),
                         gross=gross, net=gross-30, apr=(gross-30)/1e4*365/hdays/1.5*100))
    M = dict(funding=fs, basis=basis_stats(basis_h, basis_m), liq=liquidation(c),
             sizing=sizing(rules, px), settle=settle_window(c), sens=sens, day=day,
             median_mean=median_mean,
             runs={k: measure_runs(fund, basis_h, k) for k in ('testnet', 'mainnet')},
             from_d=datetime.fromtimestamp(fund['BTCUSDT'][0][0]/1000, timezone.utc).strftime('%Y-%m-%d'),
             to_d=datetime.fromtimestamp(fund['BTCUSDT'][-1][0]/1000, timezone.utc).strftime('%Y-%m-%d'))
    stamp = datetime.now().strftime('%Y-%m-%d')
    out = a.out or f'docs/reports/autotrade-params-{stamp}.html'
    tpl = open(a.template, encoding='utf-8').read()
    html = tpl.replace('__BODY__', build(M)).replace('__STAMP__',
        f"12 cặp × 12 tháng · corpus binance · dựng {stamp}")
    open(out, 'w', encoding='utf-8').write(html)
    print(f'wrote {out}', file=sys.stderr)


if __name__ == '__main__':
    main()
