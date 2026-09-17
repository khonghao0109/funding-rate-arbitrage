// Command bybitcheck is the step-4.5i connection acceptance against Bybit V5 on
// a NON-PRODUCTION host — the testnet or the demo-trading service — in the
// manner of cmd/brokercheck.
//
//	go run ./cmd/bybitcheck
//
// It PLACES NO ORDER. Every call is a GET: the venue clock, the unified wallet,
// one position, the key's own permissions, and — for BOTH markets Strategy 1
// trades on this venue, spot and linear (PLAN 4.5j) — the symbol's trading rules,
// its order book, this account's taker fee, and the key permission that market
// needs.
//
// Configuration, from .env or the shell:
//
//	BYBIT_API_KEY / BYBIT_API_SECRET   the key pair (never printed)
//	BYBIT_MODE                         testnet | demo — which host the key belongs to
//	BYBIT_TESTNET                      legacy; must not contradict BYBIT_MODE
//
// A key works on exactly one of Bybit's four environments (error 10003), so a
// mode that does not match the key is a configuration error, reported as one.
//
// What it prints: the host, HTTP outcome, round-trip time and clock skew, the
// NAMES of the wallet's coins and how many are non-zero, whether USDT has a
// non-zero equity, the position's signed size on one symbol, and the key's
// permission names; per market the rules (step, tick, minimum), the touch
// (best bid/ask, spread in bps, levels returned) and the taker fee in bps. No
// key, no secret, no signature, and no balance amount — this output is pasted
// into reports.
//
// Exit codes: 0 every check passed · 1 a check failed · 2 no credential.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/bybit"

	"github.com/joho/godotenv"
)

const (
	exitOK           = 0
	exitFailed       = 1
	exitNoCredential = 2
)

// maxAcceptedSkewMs is the acceptance line of PLAN 4.5i's check: tighter than
// the 5000 ms recv_window the client refuses at, because a skew that large
// works today and fails the next time the machine drifts.
const maxAcceptedSkewMs = 500

type check struct {
	NameVI   string
	Pass     bool
	DetailVI string
}

func main() {
	symbol := flag.String("symbol", "BTCUSDT", "symbol whose position, rules and books are read — the same string on spot and linear")
	timeout := flag.Duration("timeout", 60*time.Second, "overall deadline for the run")
	flag.Parse()
	_ = godotenv.Load()

	fmt.Println("BYBITCHECK — nghiệm thu kết nối Bước 4.5i, Bybit V5 KHÔNG PHẢI mainnet, KHÔNG đặt lệnh")
	fmt.Printf("host cho phép (Bybit): %s\n\n", strings.Join(broker.TestnetHostsFor(broker.SchemeBybitV5Header), ", "))

	// The credential first, so "nothing configured" exits 2 as documented.
	creds, err := broker.CredentialsFromEnv("BYBIT_API_KEY", "BYBIT_API_SECRET")
	switch {
	case errors.Is(err, broker.ErrNoCredentials):
		fmt.Printf("CHƯA NGHIỆM THU — không có key: %v\n", err)
		os.Exit(exitNoCredential)
	case err != nil:
		fmt.Printf("[HỎNG] credential — %v\n", err)
		os.Exit(exitFailed)
	}
	mode, err := bybit.ResolveMode(os.Getenv("BYBIT_MODE"), os.Getenv("BYBIT_TESTNET"))
	if err != nil {
		fmt.Printf("[HỎNG] cấu hình chế độ — %v\n", err)
		os.Exit(exitFailed)
	}

	cfg, err := bybit.DefaultConfig(mode, creds)
	if err != nil {
		fmt.Printf("[HỎNG] cấu hình — %v\n", err)
		os.Exit(exitFailed)
	}
	cfg.UserAgentVI = "funding-rate-arbitrage/bybitcheck"
	client, err := bybit.New(mode, broker.MarketFuturesUSDM, cfg)
	if err != nil {
		fmt.Printf("[HỎNG] dựng client — %v\n", err)
		os.Exit(exitFailed)
	}
	// The spot client is the linear one's sibling over the SAME transport: one
	// host, one IP limit, one clock, one unified wallet.
	spot, err := client.WithMarket(broker.MarketSpot)
	if err != nil {
		fmt.Printf("[HỎNG] dựng client spot — %v\n", err)
		os.Exit(exitFailed)
	}
	fmt.Printf("── bybit %s · %s\n   credential: %s\n", mode, client.HTTP().BaseURL(), creds.SourceVI)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	checks := run(ctx, client, *symbol)
	checks = append(checks, runMarkets(ctx, spot, client, *symbol)...)

	fmt.Println()
	failed := 0
	for _, c := range checks {
		mark := "ĐẠT "
		if !c.Pass {
			mark, failed = "HỎNG", failed+1
		}
		fmt.Printf("[%s] %s — %s\n", mark, c.NameVI, c.DetailVI)
	}
	fmt.Println()
	if failed > 0 {
		fmt.Printf("Bước 4.5i/4.5j (kết nối Bybit, spot + linear): %d/%d mục hỏng — CHƯA NGHIỆM THU.\n", failed, len(checks))
		os.Exit(exitFailed)
	}
	fmt.Printf("Bước 4.5i/4.5j (kết nối Bybit, spot + linear): %d/%d mục đạt trên %s. Không lệnh nào được đặt.\n", len(checks), len(checks), mode)
	os.Exit(exitOK)
}

func run(ctx context.Context, client *bybit.Client, symbol string) []check {
	var out []check

	// 1. Clock: unsigned, and the correction every signed call rides on.
	start := time.Now()
	skewMs, err := client.SyncClock(ctx)
	rtt := time.Since(start)
	if err != nil {
		fmt.Printf("   giờ server: %v\n", err)
		return append(out, check{"giờ server " + broker.BybitTimePath, false, err.Error()})
	}
	fmt.Printf("   giờ server · vòng %s · lệch %d ms\n", rtt.Round(time.Millisecond), skewMs)
	out = append(out, check{"giờ server " + broker.BybitTimePath, abs(skewMs) < maxAcceptedSkewMs,
		fmt.Sprintf("vòng %s, lệch %d ms (ngưỡng nghiệm thu |lệch| < %d ms; client từ chối ký từ %d ms)",
			rtt.Round(time.Millisecond), skewMs, maxAcceptedSkewMs, client.HTTP().RecvWindowMs())})

	// 2. Wallet (signed).
	start = time.Now()
	wallet, err := client.FetchWallet(ctx, "USDT")
	rtt = time.Since(start)
	if err != nil {
		fmt.Printf("   ví: %v\n", err)
		out = append(out, check{"ví Unified /v5/account/wallet-balance", false, explain(err)})
	} else {
		names, nonZero, usdtEquity := []string{}, 0, false
		for _, c := range wallet.Coins {
			names = append(names, c.Coin)
			if c.EquityCoin != 0 || c.WalletBalanceCoin != 0 {
				nonZero++
			}
			if c.Coin == "USDT" && c.EquityCoin > 0 {
				usdtEquity = true
			}
		}
		sort.Strings(names)
		fmt.Printf("   ví · vòng %s · %d coin (%d khác 0): %s · USDT có số dư: %v · tỷ lệ MM được công bố: %v\n",
			rtt.Round(time.Millisecond), len(names), nonZero, strings.Join(names, ", "), usdtEquity, wallet.RatesPublished)
		detail := fmt.Sprintf("HTTP 200 retCode 0, %d coin, USDT equity > 0: %v (số dư không in)", len(names), usdtEquity)
		if !usdtEquity {
			detail += " — tài khoản chưa có USDT: nạp ở testnet.bybit.com (Faucet) hoặc demo (Demo Trading → nạp tiền demo)"
		}
		out = append(out, check{"ví Unified /v5/account/wallet-balance", usdtEquity, detail})
	}

	// 3. One position (signed), read from the venue.
	pos, err := client.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		fmt.Printf("   vị thế %s: %v\n", symbol, err)
		out = append(out, check{"vị thế " + symbol + " /v5/position/list", false, explain(err)})
	} else {
		fmt.Printf("   vị thế %s · khối lượng có dấu %v coin · đòn bẩy %v×\n", symbol, pos.QtyCoin, pos.LeverageX)
		out = append(out, check{"vị thế " + symbol + " /v5/position/list", true,
			fmt.Sprintf("HTTP 200 retCode 0, chế độ một chiều, khối lượng có dấu %v coin", pos.QtyCoin)})
	}

	// 4. The key's permissions.
	info, err := client.FetchAPIKeyInfo(ctx)
	if err != nil {
		fmt.Printf("   quyền key: %v\n", err)
		return append(out, check{"quyền key /v5/user/query-api", false, explain(err)})
	}
	keys := make([]string, 0, len(info.Permissions))
	for k, v := range info.Permissions {
		if len(v) > 0 {
			keys = append(keys, k+"["+strings.Join(v, ",")+"]")
		}
	}
	sort.Strings(keys)
	fmt.Printf("   quyền key · chỉ đọc: %v · UTA: %v · %s\n", info.ReadOnly, info.UTA, strings.Join(keys, " "))
	out = append(out, check{"quyền key: giao dịch hợp đồng", info.CanTradeContracts(),
		fmt.Sprintf("chỉ đọc %v, ContractTrade %v", info.ReadOnly, info.Permissions["ContractTrade"])})
	out = append(out, check{"quyền key: giao dịch spot", info.CanTradeSpot(),
		fmt.Sprintf("chỉ đọc %v, Spot %v", info.ReadOnly, info.Permissions["Spot"])})
	if info.CanWithdraw() {
		fmt.Println("   \033[31m⚠ KEY NÀY CÓ QUYỀN RÚT TIỀN (Wallet: Withdraw). Tắt quyền rút ở trang quản lý API.\033[0m")
		out = append(out, check{"quyền key: KHÔNG rút tiền", false, "Wallet chứa Withdraw — tắt trước khi dùng key này cho bất cứ lệnh nào"})
	} else {
		out = append(out, check{"quyền key: KHÔNG rút tiền", true, "Wallet không chứa Withdraw"})
	}
	return out
}

// runMarkets reads, for each of Strategy 1's two legs, what an order on that
// market is sized and priced by. Rules and books are PUBLIC; the fee rate and the
// wallet view are signed. A market passes when its symbol is Trading on a
// non-zero grid, its book has both sides, and this account's taker fee reads.
func runMarkets(ctx context.Context, spot, linear *bybit.Client, symbol string) []check {
	var out []check
	type readiness struct {
		c      *bybit.Client
		nameVI string
	}
	var bases []string
	for _, m := range []readiness{{spot, "SPOT"}, {linear, "LINEAR (perp)"}} {
		label := fmt.Sprintf("%s %s", m.nameVI, symbol)
		rules, err := m.c.FetchInstrument(ctx, symbol)
		if err != nil {
			fmt.Printf("   %s · luật: %v\n", label, err)
			out = append(out, check{label + ": luật /v5/market/instruments-info", false, explain(err)})
			continue
		}
		bases = append(bases, rules.BaseAsset+"/"+rules.QuoteAsset)
		gridOK := rules.Status == "trading" && rules.StepSizeCoin > 0 && rules.TickSizeQuote > 0
		fmt.Printf("   %s · luật · %s · bước khối lượng %g %s · bước giá %g · tối thiểu %g %s · tối thiểu khối lượng %g · trần %g\n",
			label, rules.Status, rules.StepSizeCoin, rules.BaseAsset, rules.TickSizeQuote, rules.MinNotionalQuote, rules.QuoteAsset,
			rules.MinQtyCoin, rules.MaxQtyCoin)
		out = append(out, check{label + ": luật /v5/market/instruments-info", gridOK,
			fmt.Sprintf("%s, stepSize %g, tickSize %g, minNotional %g %s (nguồn %s)", rules.Status, rules.StepSizeCoin,
				rules.TickSizeQuote, rules.MinNotionalQuote, rules.QuoteAsset, rules.Source)})

		book, err := m.c.FetchDepthBook(ctx, symbol)
		switch {
		case err != nil:
			fmt.Printf("   %s · sổ lệnh: %v\n", label, err)
			out = append(out, check{label + ": sổ lệnh /v5/market/orderbook", false, explain(err)})
		case len(book.Bids) == 0 || len(book.Asks) == 0:
			out = append(out, check{label + ": sổ lệnh /v5/market/orderbook", false, "một phía sổ lệnh trống"})
		default:
			bid, ask := book.Bids[0].PriceQuote, book.Asks[0].PriceQuote
			mid := (bid + ask) / 2
			spreadBps := (ask - bid) / mid * 10_000
			fmt.Printf("   %s · sổ lệnh · bid %g · ask %g · chênh chạm %.2f bps · %d/%d mức\n",
				label, bid, ask, spreadBps, len(book.Bids), len(book.Asks))
			out = append(out, check{label + ": sổ lệnh /v5/market/orderbook", ask > bid,
				fmt.Sprintf("bid %g / ask %g, chênh chạm %.2f bps, %d bid · %d ask mức", bid, ask, spreadBps, len(book.Bids), len(book.Asks))})
		}

		fee, err := m.c.CommissionRates(ctx, symbol)
		if err != nil {
			fmt.Printf("   %s · phí: %v\n", label, err)
			out = append(out, check{label + ": phí /v5/account/fee-rate", false, explain(err)})
		} else {
			fmt.Printf("   %s · phí taker của tài khoản %.2f bps\n", label, fee.TakerBps())
			out = append(out, check{label + ": phí /v5/account/fee-rate", true, fmt.Sprintf("taker %.2f bps", fee.TakerBps())})
		}
	}
	// What the portal's bot and close read on the perp leg, all GETs: the
	// ticker (mark, forming rate, next settlement), the settled history over a
	// window (walked backwards from its end), the risk-limit tier a small
	// position falls in, and the account's settlement rows.
	now := time.Now().UnixMilli()
	if mp, err := linear.MarkPrice(ctx, broker.MarketFuturesUSDM, symbol); err != nil {
		out = append(out, check{symbol + ": giá mark /v5/market/tickers", false, explain(err)})
	} else {
		nextIn := time.Duration(mp.NextFundingTimeMs-now) * time.Millisecond
		fmt.Printf("   LINEAR %s · mark %g · funding đang hình thành %.2f bps/kỳ · mốc settle kế tiếp sau %s\n",
			symbol, mp.MarkPriceQuote, mp.LastFundingRateFrac*10_000, nextIn.Round(time.Minute))
		out = append(out, check{symbol + ": giá mark /v5/market/tickers", mp.MarkPriceQuote > 0 && mp.NextFundingTimeMs > now,
			fmt.Sprintf("mark %g, mốc kế tiếp sau %s", mp.MarkPriceQuote, nextIn.Round(time.Minute))})
	}
	const historyDays = 7
	if rows, err := linear.FundingRateHistoryRange(ctx, symbol, now-historyDays*24*time.Hour.Milliseconds(), now); err != nil {
		out = append(out, check{symbol + ": funding đã settle /v5/market/funding/history", false, explain(err)})
	} else {
		fmt.Printf("   LINEAR %s · %d mốc funding đã settle trong %d ngày\n", symbol, len(rows), historyDays)
		out = append(out, check{symbol + ": funding đã settle /v5/market/funding/history", len(rows) > 0,
			fmt.Sprintf("%d mốc trong %d ngày, cũ nhất trước", len(rows), historyDays)})
	}
	if tier, err := linear.FetchMaintenanceBracket(ctx, symbol, 1_000); err != nil {
		out = append(out, check{symbol + ": bậc rủi ro /v5/market/risk-limit", false, explain(err)})
	} else {
		fmt.Printf("   LINEAR %s · bậc rủi ro cho 1000 USDT: bậc %d, duy trì %.4f%%, đòn bẩy tối đa %gx, trần %g\n",
			symbol, tier.Tier, tier.MaintMarginFrac*100, tier.MaxLeverage, tier.NotionalCapQuote)
		out = append(out, check{symbol + ": bậc rủi ro /v5/market/risk-limit", true,
			fmt.Sprintf("bậc %d, duy trì %.4f%% (phân số, đo trên linear)", tier.Tier, tier.MaintMarginFrac*100)})
	}
	if rows, err := linear.FundingIncome(ctx, broker.MarketFuturesUSDM, symbol, now-historyDays*24*time.Hour.Milliseconds(), now); err != nil {
		out = append(out, check{symbol + ": funding tài khoản /v5/account/transaction-log", false, explain(err)})
	} else {
		fmt.Printf("   LINEAR %s · %d dòng funding của tài khoản trong %d ngày (số tiền không in)\n", symbol, len(rows), historyDays)
		out = append(out, check{symbol + ": funding tài khoản /v5/account/transaction-log", true,
			fmt.Sprintf("HTTP 200 retCode 0, %d dòng SETTLEMENT", len(rows))})
	}

	if len(bases) == 2 && bases[0] != bases[1] {
		out = append(out, check{symbol + ": hai chân cùng tài sản", false,
			fmt.Sprintf("spot khai %s, linear khai %s — không phải một cặp hedge", bases[0], bases[1])})
	} else if len(bases) == 2 {
		out = append(out, check{symbol + ": hai chân cùng tài sản", true, "cả hai sàn khai " + bases[0]})
	}

	// One wallet, two views: which COINS each market lists — names only.
	for _, m := range []struct {
		c      *bybit.Client
		market broker.Market
	}{{spot, broker.MarketSpot}, {linear, broker.MarketFuturesUSDM}} {
		balances, err := m.c.GetBalance(ctx, m.market)
		if err != nil {
			out = append(out, check{fmt.Sprintf("số dư (%s)", m.market), false, explain(err)})
			continue
		}
		names := make([]string, 0, len(balances))
		for _, b := range balances {
			names = append(names, b.Asset)
		}
		sort.Strings(names)
		fmt.Printf("   ví UTA nhìn từ %s · %d dòng: %s (số dư không in; cùng MỘT ví, không cộng hai góc nhìn)\n",
			m.market, len(names), strings.Join(names, ", "))
		out = append(out, check{fmt.Sprintf("số dư (%s) /v5/account/wallet-balance", m.market), true,
			fmt.Sprintf("HTTP 200 retCode 0, %d dòng", len(names))})
	}
	return out
}

// explain names the likely cause of the two configuration errors an operator
// actually meets, in Vietnamese, beside the venue's own words.
func explain(err error) string {
	switch {
	case errors.Is(err, bybit.ErrKeyInvalid):
		return err.Error() + " — kiểm tra BYBIT_MODE: key testnet chỉ chạy trên api-testnet, key demo chỉ chạy trên api-demo"
	case errors.Is(err, bybit.ErrForbidden):
		return err.Error() + " — dừng ít nhất 10 phút, không chạy lại ngay"
	}
	return err.Error()
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
