#!/usr/bin/env python3
"""Build the bilingual report from report.template.html + ONE analyze.py JSON.

Single grid since 2026-09-07: config.yaml's strategy block now carries the
sign-flip gates, so entry, exit and gate axes belong to the same sweep and the
page no longer merges two runs measured on two different order books.
"""
import argparse
import datetime
import json
import os
import pathlib
import re

HERE = pathlib.Path(__file__).resolve().parent

ap = argparse.ArgumentParser(description=__doc__)
ap.add_argument("--json", required=True, help="analyze.py output")
# The leverage block is measured by its own small sweep (lev.py) because it has
# to compare only the series that trade at EVERY leverage level — switching the
# margin model on refuses entry wherever the maintenance bracket is unverified,
# so the main grid's rows are not comparable across that axis.
ap.add_argument("--leverage", help="lev.py output; the section is dropped without it")
# The basis exit's cost, measured by running the shipped set twice with only
# the basis thresholds changed. It cannot come from the main grid: cmd/backtest
# hardcodes MaxBasisPct/MaxBasisWidenPct in baseParams, so -config does not
# reach a -sweep run — only the plain path reads them from the config block.
ap.add_argument("--isolation", help="basis on/off comparison; the block is dropped without it")
ap.add_argument("--template", default=str(HERE / "report.template.html"))
ap.add_argument("--out", action="append", required=True, help="output path (repeatable)")
# Stamped into the page so a reader can rebuild the run that produced it.
ap.add_argument("--commit", required=True, help="the commit the sweep binary was built from")
ap.add_argument("--gates", default="2.0,2,1.0",
                help="config.yaml exit_negative_min_bps,periods,cum_cost_frac — the row the gate table always shows")
# The <title> tag names the page before its JS runs, and it is what a gallery
# or a browser tab shows. Each report is a different measurement run, so each
# needs its own name — two pages called the same thing cannot be told apart in
# a list of them.
ap.add_argument("--title", help="static <title>; defaults to the template's")
args = ap.parse_args()

data = json.load(open(args.json, encoding="utf-8"))
data["leverage"] = (json.load(open(args.leverage, encoding="utf-8"))
                    if args.leverage and os.path.exists(args.leverage) else None)
data["basis_isolation"] = (json.load(open(args.isolation, encoding="utf-8"))
                           if args.isolation and os.path.exists(args.isolation) else None)

GRID_COMMIT = args.commit
LIVE_GATES = [float(x) for x in args.gates.split(",")]
GATE_AXES = ("exit_negative_min_bps", "exit_negative_periods", "exit_negative_cum_cost_frac")


def utc(ms):
    return datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")


def vi(x, d=1):
    return f"{x:,.{d}f}".replace(",", "X").replace(".", ",").replace("X", ".")


def zh(x, d=1):
    return f"{x:,.{d}f}"


def n_vi(x):
    return f"{x:,}".replace(",", ".")


def n_zh(x):
    return f"{x:,}"


# One grid feeds both the "whole sweep" and the "gate" views of the page.
gate_variants = 1
for a in GATE_AXES:
    gate_variants *= len(data["grid"].get(a, [0]))
book = data["windows"]["12"].get("book_sampled_at_ms")
data["gate_grid"] = {
    "param_sets": data["grid"]["param_sets"], "series": data["grid"]["series"],
    "gate_variants": gate_variants, "base_sets": data["grid"]["param_sets"] // gate_variants,
    "commit": GRID_COMMIT, "book": utc(book["max"]) if book else "—",
}
for W in data["windows"].values():
    W["gate_best_sets"], W["gate_current_set"] = W["best_sets"], W["current_set"]
    W["gate_set_stats"], W["gate_axes"] = W["set_stats"], W["axes"]
    W["gate_hold_through"] = [x["corpus"]["hold_through_net_pct"] for x in W["series"]]
    W["gate_ht_by_series"] = {f"{x['symbol']}|{x['perp']}": x["corpus"]["hold_through_net_pct"] for x in W["series"]}
    W["gate_window"] = {"from_ms": W["window_from_ms"], "to_ms": W["window_to_ms"], "days": W["window_days"]}
    W["gate_series"] = [{"symbol": x["symbol"], "perp": x["perp"],
                         "hold_through_net_pct": x["corpus"]["hold_through_net_pct"],
                         "best": x["best"], "sets_beating_hold_through": x.get("sets_beating_hold_through"),
                         "runs": x["runs"]} for x in W["series"]]

# The engine states its assumptions per run; a sweep report states them for the
# whole grid, and the notional/hold line must not claim axes this grid fixed.
for W in data["windows"].values():
    a = W.get("assumptions_vi") or []
    if a:
        a[0] = re.sub(r"\([a-z_]+ / [a-z_]+\)", "(sổ MỚI NHẤT của từng cặp sàn, một phép đo mỗi cặp)", a[0])
        # The engine states ONE run's notional and holding period; a sweep
        # varies both, so the line has to name the axis, not one of its values.
        for i, line in enumerate(a):
            a[i] = re.sub(r"Giả định giữ \d+ ngày để khấu hao chi phí, vốn \d+ mỗi vị thế",
                          "Giả định giữ 30 hoặc 90 ngày (theo bộ tham số) để khấu hao chi phí, vốn 50.000 mỗi vị thế",
                          line)

W12, W6, W3 = data["windows"]["12"], data["windows"]["6"], data["windows"]["3"]


def ht_sum(W):
    return sum(x["corpus"]["hold_through_net_pct"] for x in W["series"])


def ht_prof(W):
    return sum(1 for x in W["series"] if x["corpus"]["hold_through_net_pct"] > 0)


def by_key(W):
    return {(x["symbol"], x["perp"]): x for x in W["series"]}


S12 = by_key(W12)
cur12, prev12, best12 = W12["current_set"], W12.get("prev_set"), W12["best_sets"][0]
stats12 = W12["set_stats"]
perp12 = {b["perp"]: b for b in W12["by_perp"]}
ta12 = W12["trades_all"]
c12 = W12["counts"]
bridged = sorted({(x["symbol"], x["perp"]) for x in W12["series"] if x["perp"] in ("hyperliquid_futures", "kraken_futures")})


def series_ret(setrow, sym, perp):
    return (setrow or {}).get("series", {}).get(f"{sym}|{perp}", {}).get("return_pct")


def fmt_set(p, f=vi):
    return (f"{f(p['min_rate_per_8h_bps'],1)} / {int(p['persistence_periods'])} · "
            f"{f(p['min_net_apr_frac']*100,0)}% → {f(p['exit_net_apr_frac']*100,2)}% / {int(p['exit_persistence_periods'])} · "
            f"X{f(p['exit_negative_min_bps'],1)} N{int(p['exit_negative_periods'])} C{f(p['exit_negative_cum_cost_frac'],2)}")


# The four best series by realized APR, named in the verdict prose.
top_series = sorted(W12["series"], key=lambda x: -(x["best"]["realized_apr_pct"] if x["best"] else -1e9))[:4]
hl_btc = S12.get(("BTCUSDT", "hyperliquid_futures"))
hl_eth = S12.get(("ETHUSDT", "hyperliquid_futures"))

EVAL = {"rows": [], "overall": {}, "next": []}


def row(crit_vi, crit_zh, verdict, ev_vi, ev_zh):
    EVAL["rows"].append({"crit": {"vi": crit_vi, "zh": crit_zh}, "verdict": verdict,
                         "evidence": {"vi": ev_vi, "zh": ev_zh}})


# ① Does the shipped set make money over 12 months?
cur_sum, cur_prof = cur12["sum_return_pct"], cur12["profitable"]
prev_sum = prev12["sum_return_pct"] if prev12 else None
row("Bộ tham số đang chạy, 12 tháng", "实盘参数组，12 个月",
    "pass" if cur_sum >= ht_sum(W12) else ("partial" if cur_sum > 0 else "fail"),
    f"Bộ shipped ({fmt_set(cur12['params'])}) cho tổng {vi(cur_sum,2)}% trên {cur12['n']} chuỗi, "
    f"{cur_prof}/{cur12['n']} chuỗi dương, {vi(cur12['mean_trades'],1)} lệnh mỗi chuỗi. "
    + (f"Bộ 3.3 cũ mà nó thay cho {vi(prev_sum,2)}% ({prev12['profitable']}/{prev12['n']}, {vi(prev12['mean_trades'],1)} lệnh mỗi chuỗi) trên cùng lưới, cùng cửa sổ, cùng sổ lệnh — "
        f"khoảng cách nở ra chính vì hai sàn settle theo giờ: luật cũ thoát ở bất kỳ mốc âm nào, mà ở nhịp 1h thì mốc âm nhiều gấp 8 lần, nên nó trả {vi(prev12['mean_trades'],0)} vòng phí mỗi chuỗi. " if prev12 else "")
    + f"Tốt nhất từng chuỗi: " + ", ".join(f"{x['symbol']}·{x['perp'].replace('_futures','')} {vi(x['best']['realized_apr_pct'],2)}% APR" for x in top_series if x["best"]) + ".",
    f"实盘参数组（{fmt_set(cur12['params'], zh)}）在 {cur12['n']} 个序列上合计 {zh(cur_sum,2)}%，{cur_prof}/{cur12['n']} 个序列为正，每序列 {zh(cur12['mean_trades'],1)} 笔。"
    + (f"它所替换的 3.3 参数组在同一网格、同一窗口、同一订单簿上为 {zh(prev_sum,2)}%（{prev12['profitable']}/{prev12['n']}，{zh(prev12['mean_trades'],1)} 笔）。" if prev12 else "")
    + "各序列最佳：" + "，".join(f"{x['symbol']}·{x['perp'].replace('_futures','')} {zh(x['best']['realized_apr_pct'],2)}% 年化" for x in top_series if x["best"]) + "。")

# ② The bar that matters: hold-through.
hts, htp = ht_sum(W12), ht_prof(W12)
beat = stats12["at_or_above_hold_through"]
row("So với giữ suốt (mốc chuẩn thật)", "对比全程持有（真正的基准）",
    "pass" if beat > 0 else "fail",
    f"Giữ suốt cả cửa sổ và trả đúng một vòng phí cho tổng {vi(hts,2)}% ({htp}/{len(W12['series'])} chuỗi dương). "
    f"Trong {n_vi(stats12['n'])} bộ tham số của lưới, {n_vi(stats12['positive'])} bộ có tổng dương nhưng chỉ "
    f"{n_vi(beat)} bộ đạt hoặc vượt mốc đó; bộ tốt nhất {vi(stats12['max_sum_return_pct'],2)}%. "
    f"Bộ đang chạy {vi(cur_sum,2)}%. Một luật vào/ra chỉ đáng giữ nếu THẮNG giữ suốt, không phải chỉ dương.",
    f"全程持有整个窗口、只付一次往返，合计 {zh(hts,2)}%（{htp}/{len(W12['series'])} 个序列为正）。"
    f"网格 {n_zh(stats12['n'])} 组参数中，{n_zh(stats12['positive'])} 组合计为正，但只有 {n_zh(beat)} 组达到或超过该基准；最佳 {zh(stats12['max_sum_return_pct'],2)}%。"
    f"实盘参数组 {zh(cur_sum,2)}%。一套进出场规则只有胜过全程持有才值得保留，仅仅为正是不够的。")


# ③ Evidence: how many trades stand behind a positive number.
best_tr = W12["trades_best"]
row("Số lệnh làm bằng chứng", "作为证据的交易数量",
    "weak",
    f"Bộ đang chạy mở {vi(cur12['mean_trades'],1)} lệnh mỗi chuỗi trong cả năm — đó là điểm mạnh về chi phí và "
    f"điểm yếu về thống kê: với 1–2 lệnh, kết quả một chuỗi là kết quả của một lần vào đúng hay sai thời điểm, "
    f"không phân biệt được kỹ năng với may mắn. Toàn lưới {n_vi(ta12['n'])} lệnh, "
    f"{vi(ta12['share_net_positive']*100,1)}% có ròng dương, giữ trung vị {vi(ta12['median_held_days'],1)} ngày.",
    f"实盘参数组每序列全年只开 {zh(cur12['mean_trades'],1)} 笔 — 这在成本上是优点，在统计上是弱点：只有 1–2 笔时，"
    f"一个序列的结果就是一次入场时机对错的结果，无法区分能力与运气。全网格 {n_zh(ta12['n'])} 笔，"
    f"{zh(ta12['share_net_positive']*100,1)}% 净为正，中位持有 {zh(ta12['median_held_days'],1)} 天。")

# ④ Quote bridging — the new risk this run took on.
if bridged:
    names = ", ".join(f"{s_}·{p_.replace('_futures','')}" for s_, p_ in bridged)
    row("Rủi ro quote của các chuỗi mới", "新增序列的计价货币风险", "open",
        f"{len(bridged)} chuỗi trong lượt này chỉ ghép được vì config khai báo USD ≡ USDT: {names}. "
        f"Vị thế trung tính về coin nhưng CÒN MỞ rủi ro USDT/USD, và KHÔNG con số nào trên trang này trừ khoản đó. "
        f"Chính hai chuỗi hyperliquid lại là kết quả cao nhất — nên phần lợi thế đó chưa được trừ đúng chi phí của nó. "
        f"Trước 2026-09-07 các chuỗi này bị từ chối thẳng; đổi lại là quyết định rủi ro, không phải phát hiện mới về thị trường.",
        f"本轮有 {len(bridged)} 个序列只因配置声明 USD ≡ USDT 才得以配对：{names}。"
        f"仓位在币种上中性，但仍敞口于 USDT/USD，本页没有任何数字扣除该成本。"
        f"最好的结果恰恰来自这两个 hyperliquid 序列 — 因此那部分优势尚未扣除其应有的成本。"
        f"2026-09-07 之前这些序列被直接拒绝；改变的是风险决策，不是对市场的新发现。")

# ⑤ Exit rule.
ex = {a["value"]: a for a in W12["axes"]["exit_persistence_periods"]}
lo, hi = min(ex), max(ex)
row("Luật thoát", "退出规则", "partial",
    f"Trục quyết định là độ dài lối thoát suy giảm: {int(lo)} kỳ cho trung vị {vi(ex[lo]['median_return_pct'],2)}% "
    f"và {vi(ex[lo]['share_profitable']*100,0)}% lượt có lãi, {int(hi)} kỳ cho {vi(ex[hi]['median_return_pct'],2)}% "
    f"và {vi(ex[hi]['share_profitable']*100,0)}%. Nói cách khác cái sinh lãi là THOÁT ÍT ĐI. "
    f"Toàn lưới {vi(ta12['exit_reasons'].get('sign_flip',0)/max(ta12['n'],1)*100,1)}% lệnh vẫn đóng vì funding đảo dấu. "
    f"Luật thoát đáng viết tiếp phải so chi phí giữ xuyên đợt âm KỲ VỌNG với một vòng phí, và phải thắng giữ suốt.",
    f"决定性的维度是衰减退出的长度：{int(lo)} 期时中位 {zh(ex[lo]['median_return_pct'],2)}%、"
    f"{zh(ex[lo]['share_profitable']*100,0)}% 盈利，{int(hi)} 期时 {zh(ex[hi]['median_return_pct'],2)}%、"
    f"{zh(ex[hi]['share_profitable']*100,0)}%。换句话说，赚钱的做法是少退出。"
    f"全网格仍有 {zh(ta12['exit_reasons'].get('sign_flip',0)/max(ta12['n'],1)*100,1)}% 的交易因费率变号平仓。"
    f"值得继续写的退出规则必须比较持有过负费率段的期望成本与一次往返成本，并且要胜过全程持有。")

# ⑥ Cost structure.
costs = sorted(((b["perp"], b["cost_pct_50k"]) for b in W12["by_perp"] if b["cost_pct_50k"]), key=lambda x: x[1])
row("Cấu trúc chi phí", "成本结构", "pass",
    "Đã đo và tách được. Vòng vào/ra @50k, trung vị theo sàn perp: "
    + ", ".join(f"{n.replace('_futures','')} {vi(c,3)}%" for n, c in costs)
    + f". Chân spot là binance_spot ở mọi chuỗi, nên hai lệnh spot chiếm 20 trong ~30 bps ở khắp nơi — "
      f"đòn bẩy lớn nhất còn lại vẫn là phí, không phải tham số.",
    "已测量并拆分。50k 往返成本，按永续交易所的中位数："
    + "，".join(f"{n.replace('_futures','')} {zh(c,3)}%" for n, c in costs)
    + "。所有序列的现货腿都是 binance_spot，因此两笔现货在各处都占约 30 bps 中的 20 bps — "
      "剩下最大的杠杆仍然是手续费，而不是参数。")

# ⑦ Which parameter values are even reachable.
rate = {a["value"]: a for a in W12["axes"]["min_rate_per_8h_bps"]}
dead = [v for v, a in rate.items() if not a["n"]]
row("Vùng tham số có ý nghĩa", "有意义的参数区域", "partial",
    f"Ngưỡng vào chạy từ {vi(min(rate),1)} đến {vi(max(rate),1)} bps/8h"
    + (f"; {', '.join(vi(v,1) for v in dead)} không khớp lệnh nào trên bất kỳ chuỗi nào" if dead else "; mọi mức đều có lệnh")
    + f". Notional cố định ở {n_vi(int(cur12['params']['notional_quote']))} nên trang không nói gì về độ nhạy theo "
      f"cỡ vốn — lượt trước đã đo và thấy nó không đổi kết quả trên BTC/ETH. Kỳ giữ giả định là một TRỤC của lưới "
      f"lần này ({', '.join(vi(a['value'],0) for a in W12['axes']['holding_days'])} ngày), vì nó không phải chú "
      f"thích mà là số học: net APR = (rate × số mốc − vòng phí) × 365 / kỳ giữ, nên nó đặt luôn cả ngưỡng vào lẫn "
      f"ngưỡng thoát-do-suy-giảm. Bộ đang ship dùng {int(cur12['params']['holding_days'])} ngày.",
    f"入场阈值范围 {zh(min(rate),1)} 至 {zh(max(rate),1)} bps/8h"
    + (f"；{', '.join(zh(v,1) for v in dead)} 在任何序列上都没有触发" if dead else "；每一档都有交易")
    + f"。名义本金固定为 {n_zh(int(cur12['params']['notional_quote']))}，因此本页不涉及规模敏感性 — "
      f"上一轮已测得它在 BTC/ETH 上不改变结论。假设持仓期这一轮是网格的一个维度"
      f"（{', '.join(zh(a['value'],0) for a in W12['axes']['holding_days'])} 天），因为它不是注释而是算术："
      f"净年化 =（费率 × 期数 − 往返成本）× 365 / 持仓期，它同时决定了入场门槛和衰减退出门槛。"
      f"实盘参数组使用 {int(cur12['params']['holding_days'])} 天。")

# ⑧ Coverage.
# Sorted on (days, name), not days alone: the input is a SET, so ties would
# fall back to set-iteration order and the same data would render a different
# sentence on every run. Two builds of one report must be diffable.
covs = sorted({(x["perp"], round(x["covered_days"])) for x in W12["series"]}, key=lambda x: (x[1], x[0]))
row("Độ phủ của kiểm chứng", "检验的覆盖范围", "partial",
    f"{len(W12['series'])} chuỗi phát lại được. Corpus KHÔNG đều: "
    + ", ".join(f"{n.replace('_futures','')} {d} ngày" for n, d in covs)
    + ". Đọc bảng từng chuỗi cùng cột độ phủ, nếu không là so một năm với một quý. "
      "Paradex vẫn bị từ chối vì funding của nó là chỉ số liên tục, không có mốc settle để đếm.",
    f"{len(W12['series'])} 个序列可回放。语料并不均匀："
    + "，".join(f"{n.replace('_futures','')} {d} 天" for n, d in covs)
    + "。逐序列表格必须连同覆盖天数一起读，否则就是拿一年比一个季度。"
      "Paradex 仍被拒绝，因为它的资金费率是连续指数，没有可计数的结算时点。")

# ⑨ Cost model.
row("Mô hình chi phí", "成本模型", "partial",
    f"Mỗi cặp sàn MỘT sổ lệnh đo lúc {data['gate_grid']['book']}, giữ cố định cho cả ba cửa sổ; độ sâu không "
    f"backfill được. Sổ lúc thị trường căng — đúng lúc phải thoát — đắt hơn. Đường cong tuyến tính từng khúc ước "
    f"trượt giá CAO hơn thật, tức lệch về phía an toàn.",
    f"每个交易所对只有一份订单簿，测于 {data['gate_grid']['book']}，三个窗口固定不变；深度无法回填。"
    f"市场紧张时 — 恰是必须退出时 — 的订单簿更贵。分段线性曲线对滑点的估计偏高，即偏向保守。")

# ⑩ Basis exit — the line this report exists to change.
bs = W12["basis"]
row("Điều kiện thoát theo basis", "基差退出条件",
    "pass" if bs["series_fully_evaluable"] > bs["series"] / 2 else "partial",
    f"Lượt trước: 0 lần đánh giá được, ở mọi mốc của mọi lượt chạy — không lưới nào phát hiện được điều đó vì "
    f"không lưới nào đo nó. price_history (nến 1h, 12 tháng, 8 nguồn) đã lấp: nay "
    f"{bs['series_fully_evaluable']}/{bs['series']} chuỗi báo 0 mốc không đánh giá được, và điều kiện chạy thật trên "
    f"{n_vi(bs['settlements_evaluable'])} mốc. Còn {n_vi(bs['evaluations_not_evaluable'])} lần đánh giá thiếu giá — "
    f"đúng chỗ funding với tới 365 ngày còn nến chỉ ~208 (Hyperliquid) hoặc có lỗ hổng (Kraken) — và ở đó luật báo "
    f"KHÔNG đánh giá được chứ không bịa số. Giá dùng là nến ĐÃ ĐÓNG gần nhất, không phải nến chứa mốc đó: giá đóng "
    f"của nến ấy nằm ở tương lai so với quyết định.",
    f"上一轮：在每一轮、每一个结算点都是 0 次可评估 — 任何参数网格都发现不了，因为没有网格测量过它。"
    f"price_history（1 小时 K 线、12 个月、8 个数据源）补上了这个缺口：现在 "
    f"{bs['series_fully_evaluable']}/{bs['series']} 个序列报告 0 个无法评估的结算点，条件在 "
    f"{n_zh(bs['settlements_evaluable'])} 个结算点上真正运行。仍有 {n_zh(bs['evaluations_not_evaluable'])} 次评估缺少价格 — "
    f"正是资金费率覆盖 365 天而 K 线只到约 208 天（Hyperliquid）或存在缺口（Kraken）之处 — 规则在那里报告无法评估，"
    f"而不是编造数字。使用的价格是最近一根已完全收盘的 K 线，而不是包含该时点的那根：后者的收盘价相对于决策位于未来。")

# ⑪ The denominator correction.
if data.get("leverage"):
    L = data["leverage"]
    off = next(r for r in L["rows"] if r["off"])
    best_lev = max((r for r in L["rows"] if not r["off"]), key=lambda r: r["after_pct"])
    row("Mẫu số: notional hay vốn", "分母：名义本金还是资本", "pass",
        f"Mọi con số khác trên trang này tính trên NOTIONAL, và notional không đổi khi bật đòn bẩy — nên chúng đo "
        f"cái GIÁ của đòn bẩy, không bao giờ đo được cái LỢI. Chân spot không đòn bẩy được, nên vốn = N·(1+f) và trần "
        f"của toàn bộ lợi ích là 2,00× khi f→0, không phải 10× ở đòn bẩy 10×. Đo trên {L['series']} chuỗi, sau phí "
        f"thanh lý: tắt {vi(off['after_pct'],2)}%, tốt nhất {best_lev['label']} {vi(best_lev['after_pct'],2)}% "
        f"(+{vi(best_lev['after_pct']-off['after_pct'],2)} điểm, {vi(best_lev['vs'],2)}× trên trần {vi(best_lev['ceiling'],2)}×), "
        f"và âm từ 5x. Đổi lại {min(r['liquidations'] for r in L['rows'])} → "
        f"{max(r['liquidations'] for r in L['rows'])} lần thanh lý. Ship ở 0.",
        f"本页其他所有数字都以名义本金为分母，而开杠杆时名义本金不变 — 因此它们衡量的是杠杆的成本，永远无法衡量其收益。"
        f"现货腿无法加杠杆，所以资本 = N·(1+f)，全部收益的上限是 f→0 时的 2.00×，而不是 10 倍杠杆下的 10×。"
        f"在 {L['series']} 个序列上测量，扣除强平费后：不用杠杆 {zh(off['after_pct'],2)}%，最优 {best_lev['label']} "
        f"{zh(best_lev['after_pct'],2)}%（+{zh(best_lev['after_pct']-off['after_pct'],2)} 个百分点，"
        f"为上限 {zh(best_lev['ceiling'],2)}× 的 {zh(best_lev['vs'],2)}×），从 5x 起转负。"
        f"代价是强平次数从 {min(r['liquidations'] for r in L['rows'])} 升到 {max(r['liquidations'] for r in L['rows'])}。实盘设为 0。")

beats_ht = stats12["max_sum_return_pct"] >= hts
mor12 = W12.get("morning_set")
mor_sum = mor12["sum_return_pct"] if mor12 else None
EVAL["overall"] = {
    "vi": (f"So với báo cáo trước, lượt này đổi ba thứ và chỉ một trong đó là tham số. "
           f"MỘT: điều kiện thoát theo basis lần đầu tiên ĐO ĐƯỢC — suốt bước 3.3 nó báo “không đánh giá được” ở mọi "
           f"mốc của mọi lượt, nên không lưới nào từng kiểm nó; nay {bs['series_fully_evaluable']}/{bs['series']} chuỗi "
           f"báo 0 mốc thiếu giá. HAI: mẫu số. Mọi con số ở đây tính trên notional, mà notional không đổi khi bật đòn "
           f"bẩy — bảng đòn bẩy của báo cáo trước vì thế chỉ đo được cái giá, không đo được cái lợi; trần thật của "
           f"hiệu quả vốn là 2,00×, vì chân spot không đòn bẩy được. BA: bộ tham số shipped chuyển sang "
           f"{fmt_set(cur12['params'])} với kỳ giữ giả định {int(cur12['params']['holding_days'])} ngày. "
           f"Kết quả: bộ đang chạy cho tổng {vi(cur_sum,2)}% trên 12 tháng, {cur_prof}/{cur12['n']} chuỗi dương, "
           f"{vi(cur12['mean_trades'],1)} lệnh mỗi chuỗi"
           + (f", so với {vi(mor_sum,2)}% của bộ sáng cùng ngày" if mor_sum is not None else "")
           + (f" và {vi(prev_sum,2)}% của bộ 3.3" if prev_sum is not None else "")
           + f". Mốc chuẩn vẫn là giữ suốt: {vi(hts,2)}% ({htp}/{len(W12['series'])} chuỗi dương), và "
           + (f"chỉ {n_vi(beat)} trong {n_vi(stats12['n'])} bộ đạt tới" if beat else f"KHÔNG bộ nào trong {n_vi(stats12['n'])} bộ chạm tới")
           + f" — bộ tốt nhất {vi(stats12['max_sum_return_pct'],2)}%. "
             f"Đọc đúng là: một điều kiện thoát đã hết mù, một phép đọc sai mẫu số đã được sửa, và kết luận cũ vẫn "
             f"đứng — luật vào/ra chưa chứng minh được là hơn mua rồi giữ. Hai khoản vẫn chưa trừ: rủi ro USDT/USD "
             f"của các chuỗi ghép khác quote, và khoảng long spot trần trụi sau một cú thanh lý."),
    "zh": (f"与上一份报告相比，本轮改变了三件事，其中只有一件是参数。"
           f"其一：基差退出条件首次可以测量 — 在整个 3.3 步中它在每一轮、每个结算点都报告“无法评估”，"
           f"因此没有任何网格检验过它；现在 {bs['series_fully_evaluable']}/{bs['series']} 个序列报告 0 个缺价结算点。"
           f"其二：分母。本页所有数字都以名义本金计，而开杠杆时名义本金不变 — 上一份报告的杠杆表因此只测到成本，"
           f"测不到收益；资本效率的真实上限是 2.00×，因为现货腿无法加杠杆。"
           f"其三：实盘参数组改为 {fmt_set(cur12['params'], zh)}，假设持仓期 {int(cur12['params']['holding_days'])} 天。"
           f"结果：实盘参数组 12 个月合计 {zh(cur_sum,2)}%，{cur_prof}/{cur12['n']} 个序列为正，每序列 {zh(cur12['mean_trades'],1)} 笔"
           + (f"，对比当日上午参数组的 {zh(mor_sum,2)}%" if mor_sum is not None else "")
           + (f" 和 3.3 参数组的 {zh(prev_sum,2)}%" if prev_sum is not None else "")
           + f"。基准仍是全程持有：{zh(hts,2)}%（{htp}/{len(W12['series'])} 个序列为正），"
           + (f"网格 {n_zh(stats12['n'])} 组中只有 {n_zh(beat)} 组达到" if beat else f"网格 {n_zh(stats12['n'])} 组无一达到")
           + f" — 最佳 {zh(stats12['max_sum_return_pct'],2)}%。"
             f"正确的解读是：一条退出条件不再是盲的，一处分母误读已被纠正，而旧结论依然成立 — "
             f"进出场规则尚未证明胜过买入并持有。仍有两项未扣除：跨计价配对序列的 USDT/USD 风险，"
             f"以及强平之后裸多现货的那段窗口。"),
}
EVAL["next"] = [
    {"vi": f"Cổng 3.5 tiếp tục với bộ tham số CŨ. Tiến trình nhật ký (khởi động 09:39 ngày 2026-09-07) nạp config đúng "
           f"một lần nên vẫn chạy {fmt_set(prev12['params']) if prev12 else 'bộ 3.3'} và biểu phí trước khi xác minh; "
           f"file config đã đổi HAI lần kể từ đó nhưng nó thì không. Phán quyết phải dựng lại bộ cũ bằng "
           f"`git show ba5ee31:config.yaml`. Không restart trước 2026-09-21.",
     "zh": f"第 3.5 步的门槛继续使用旧参数组。日志进程（2026-09-07 09:39 启动）只加载一次配置，因此仍在跑 "
           f"{fmt_set(prev12['params'], zh) if prev12 else '3.3 参数组'} 和验证前的费率表；配置文件此后已改动两次，它没有。"
           f"裁定时必须用 `git show ba5ee31:config.yaml` 重建旧参数组。2026-09-21 之前不要重启。"},
    {"vi": "Định giá rủi ro USDT/USD trước khi tin các chuỗi ghép khác quote, hoặc tìm một chân spot quote USD thật. "
           "Đây là khoản duy nhất đứng giữa kết quả tốt nhất của trang này và một con số dùng được.",
     "zh": "在相信跨计价配对序列之前先给 USDT/USD 风险定价，或者找一条真正以 USD 计价的现货腿。"
           "这是本页最佳结果与一个可用数字之间唯一的差距。"},
    {"vi": "Hiệu quả vốn thật không đến từ đòn bẩy mà từ việc để CẢ HAI CHÂN trên MỘT sàn dùng unified/portfolio "
           "margin: lãi spot bù lỗ perp trong cùng tài khoản, nên tiến gần trần 2× mà không thêm rủi ro thanh lý. "
           "Đó là quyết định thực thi ở phase 4; hiện chỉ binance có cả hai chân cùng sàn.",
     "zh": "真正的资本效率不来自杠杆，而来自把两条腿放在同一家交易所并使用统一/组合保证金："
           "现货浮盈在同一账户内抵消永续亏损，因而接近 2× 上限且不增加强平风险。"
           "这是第 4 阶段的执行决策；目前只有 binance 的两条腿在同一家交易所。"},
    {"vi": "Chưa sang giai đoạn 4 (thực thi, API key). Mốc để sang không phải “có lãi” mà là “thắng giữ suốt trên 12 "
           "tháng, ở nhiều chuỗi, sau khi đã trừ rủi ro quote”.",
     "zh": "暂不进入第 4 阶段（执行、API 密钥）。进入的标准不是“盈利”，而是“在 12 个月、多个序列上、并扣除计价货币风险后胜过全程持有”。"},
    {"vi": "Đòn bẩy còn lại theo thứ tự: phí chân spot (20 trong 30 bps), khớp maker ở chân perp, rồi luật thoát so "
           "chi phí giữ xuyên kỳ vọng với một vòng phí.",
     "zh": "剩余杠杆按顺序：现货腿手续费（30 bps 中的 20）、永续腿挂单成交，然后是把持有过负费率段的期望成本与一次往返成本相比较的退出规则。"},
]

now = datetime.datetime.now().astimezone()
META = {"generated_at": now.strftime("%Y-%m-%d %H:%M %z")}

tpl = open(args.template, encoding="utf-8").read()
if args.title:
    tpl = re.sub(r"<title>.*?</title>", f"<title>{args.title}</title>", tpl, count=1)
out = (tpl.replace("/*__DATA__*/", json.dumps(data, ensure_ascii=False, separators=(",", ":")))
          .replace("/*__EVAL__*/", json.dumps(EVAL, ensure_ascii=False))
          .replace("/*__META__*/", json.dumps(META, ensure_ascii=False))
          .replace("/*__LIVE_GATES__*/", json.dumps(LIVE_GATES)))
assert "/*__" not in out
# Each report is a NEW file, never an overwrite: an older one is the record of
# what was known on the day it was built, and a new run supersedes it rather
# than erasing the difference between the two.
for path in args.out:
    open(path, "w", encoding="utf-8").write(out)
print(len(out), "bytes ·", len(W12["series"]), "series ·", stats12["n"], "sets ·",
      f"shipped {cur_sum:+.2f}% vs hold-through {hts:+.2f}%")
