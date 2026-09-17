// Command bybitcheck is the step-4.5i connection acceptance against Bybit V5 on
// a NON-PRODUCTION host — the testnet or the demo-trading service — in the
// manner of cmd/brokercheck.
//
//	go run ./cmd/bybitcheck
//
// It PLACES NO ORDER. Every call is a GET: the venue clock, the unified wallet,
// one position, and the key's own permissions.
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
// permission names. No key, no secret, no signature, and no balance amount —
// this output is pasted into reports.
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
	symbol := flag.String("symbol", "BTCUSDT", "symbol whose position is read")
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
	mode, err := resolveMode(os.Getenv("BYBIT_MODE"), os.Getenv("BYBIT_TESTNET"))
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
	client, err := bybit.New(mode, cfg)
	if err != nil {
		fmt.Printf("[HỎNG] dựng client — %v\n", err)
		os.Exit(exitFailed)
	}
	fmt.Printf("── bybit %s · %s\n   credential: %s\n", mode, client.HTTP().BaseURL(), creds.SourceVI)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	checks := run(ctx, client, *symbol)

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
		fmt.Printf("Bước 4.5i (kết nối Bybit): %d/%d mục hỏng — CHƯA NGHIỆM THU.\n", failed, len(checks))
		os.Exit(exitFailed)
	}
	fmt.Printf("Bước 4.5i (kết nối Bybit): %d/%d mục đạt trên %s. Không lệnh nào được đặt.\n", len(checks), len(checks), mode)
	os.Exit(exitOK)
}

// resolveMode reads BYBIT_MODE and refuses a BYBIT_TESTNET that contradicts
// it. Neither set is an error rather than a default: sending a demo key to the
// testnet host answers 10003, which names neither variable.
func resolveMode(modeVar, testnetVar string) (bybit.Mode, error) {
	testnetVar = strings.ToLower(strings.TrimSpace(testnetVar))
	switch testnetVar {
	case "", "true", "false", "1", "0":
	default:
		return "", fmt.Errorf("BYBIT_TESTNET=%q không phải true/false/1/0 — không đoán", testnetVar)
	}
	if strings.TrimSpace(modeVar) == "" {
		switch testnetVar {
		case "true", "1":
			return bybit.ModeTestnet, nil
		case "":
			return "", errors.New("BYBIT_MODE chưa đặt (testnet | demo)")
		}
		return "", fmt.Errorf("BYBIT_MODE chưa đặt và BYBIT_TESTNET=%q không chỉ ra testnet — không tự chọn host", testnetVar)
	}
	mode, err := bybit.ParseMode(modeVar)
	if err != nil {
		return "", err
	}
	switch {
	case mode == bybit.ModeTestnet && (testnetVar == "false" || testnetVar == "0"):
		return "", errors.New("BYBIT_MODE=testnet nhưng BYBIT_TESTNET=false — hai biến mâu thuẫn, sửa .env")
	case mode == bybit.ModeDemo && (testnetVar == "true" || testnetVar == "1"):
		return "", errors.New("BYBIT_MODE=demo nhưng BYBIT_TESTNET=true — key demo không dùng được trên testnet, sửa .env")
	}
	return mode, nil
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
	if info.CanWithdraw() {
		fmt.Println("   \033[31m⚠ KEY NÀY CÓ QUYỀN RÚT TIỀN (Wallet: Withdraw). Tắt quyền rút ở trang quản lý API.\033[0m")
		out = append(out, check{"quyền key: KHÔNG rút tiền", false, "Wallet chứa Withdraw — tắt trước khi dùng key này cho bất cứ lệnh nào"})
	} else {
		out = append(out, check{"quyền key: KHÔNG rút tiền", true, "Wallet không chứa Withdraw"})
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
