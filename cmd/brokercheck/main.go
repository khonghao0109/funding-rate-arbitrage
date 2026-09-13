// Command brokercheck is the step-4.1 acceptance run: it proves signed REST
// access against Binance TESTNET and prints what it found.
//
//	go run ./cmd/brokercheck
//
// It is a diagnostic in the manner of cmd/fundingcheck — it opens sockets, it
// prints a table, and nothing in the trading path imports it. It is also the
// ONE command that links internal/broker; every other binary is held away from
// that package by a test (internal/broker/boundary_test.go).
//
// It PLACES NO ORDER. Step 4.1 is the transport: a clock, a signature, a
// budget, and two read-only account endpoints. Placing and cancelling an order
// on testnet is step 4.2, and 4.4–4.6 wait for the step-3.5 verdict.
//
// # Two venues, two accounts
//
// Binance's USDⓈ-M futures testnet and its spot testnet are SEPARATE systems
// with separate registrations, measured 2026-09-13: a futures key presented to
// testnet.binance.vision is refused with -2015. So each venue reads its own
// pair of variables, and a venue with no key is SKIPPED rather than failed —
// having only one of the two accounts is a normal state, not a defect.
//
//	futures  BINANCE_FUTURES_TESTNET_API_KEY / _SECRET
//	         falling back to BINANCE_TESTNET_API_KEY / _SECRET
//	spot     BINANCE_SPOT_TESTNET_API_KEY / _SECRET
//
// Half a pair is NOT a skip: it is a failure, because a typo reported as "no
// key" would leave a venue untested while the operator believes otherwise.
//
// Nothing here prints a key, a secret, a signature, or a balance amount: what
// it prints is the HTTP status, the measured clock skew in milliseconds, the
// weight the venue says this IP has spent, and the NUMBER and NAMES of the
// assets each account returned.
//
// Exit codes: 0 every venue that had a key passed · 1 a venue with a key failed
// a check · 2 no venue had a key, so nothing was attempted.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/broker"

	"github.com/joho/godotenv"
)

// The credential variables, per venue. The legacy pair stays readable as the
// FUTURES fallback so an existing .env keeps working without an edit.
var (
	futuresEnv = []broker.EnvPair{
		{KeyVar: "BINANCE_FUTURES_TESTNET_API_KEY", SecretVar: "BINANCE_FUTURES_TESTNET_API_SECRET"},
		{KeyVar: "BINANCE_TESTNET_API_KEY", SecretVar: "BINANCE_TESTNET_API_SECRET"},
	}
	spotEnv = []broker.EnvPair{
		{KeyVar: "BINANCE_SPOT_TESTNET_API_KEY", SecretVar: "BINANCE_SPOT_TESTNET_API_SECRET"},
	}
)

// exit codes
const (
	exitOK          = 0
	exitFailed      = 1
	exitNoCredentia = 2
)

type check struct {
	NameVI   string
	Pass     bool
	DetailVI string
}

type venue struct {
	NameVI       string
	BaseURL      string
	TimePath     string
	WeightPerMin int
	Account      broker.Endpoint
	Parse        func(json.RawMessage) (assets []string, nonZero int, err error)
	EnvPairs     []broker.EnvPair
}

func main() {
	recvWindowMs := flag.Int64("recv-window-ms", broker.DefaultRecvWindowMs,
		"recvWindow sent with every signed request, in milliseconds (documented default 5000, maximum 60000)")
	timeout := flag.Duration("timeout", 30*time.Second, "overall deadline for the run")
	flag.Parse()

	// .env is gitignored and is where an operator normally puts these. Its
	// absence is not an error: the variables may be exported in the shell.
	_ = godotenv.Load()

	fmt.Println("BROKERCHECK — nghiệm thu Bước 4.1, TESTNET, KHÔNG đặt lệnh")
	fmt.Printf("host cho phép: %s\n", strings.Join(broker.TestnetHosts(), ", "))
	fmt.Printf("recv_window_ms: %d (mặc định %d, tối đa %d)\n\n",
		*recvWindowMs, broker.DefaultRecvWindowMs, broker.MaxRecvWindowMs)

	venues := []venue{{
		NameVI:       "binance futures testnet",
		BaseURL:      broker.BinanceFuturesTestnetBaseURL,
		TimePath:     broker.BinanceFuturesTimePath,
		WeightPerMin: broker.BinanceFuturesWeightPerMin,
		Account:      broker.FuturesAccountBalance,
		Parse:        parseFuturesBalance,
		EnvPairs:     futuresEnv,
	}, {
		NameVI:       "binance spot testnet",
		BaseURL:      broker.BinanceSpotTestnetBaseURL,
		TimePath:     broker.BinanceSpotTimePath,
		WeightPerMin: broker.BinanceSpotWeightPerMin,
		Account:      broker.SpotAccount,
		Parse:        parseSpotAccount,
		EnvPairs:     spotEnv,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var all []check
	var skipped []string
	attempted := 0

	for _, v := range venues {
		creds, err := broker.CredentialsFromEnvAny(v.EnvPairs...)
		switch {
		case errors.Is(err, broker.ErrNoCredentials):
			// A normal state, not a defect: the two testnets are separate
			// registrations and an operator may hold only one.
			fmt.Printf("── %s · BỎ QUA — chưa có key\n   %v\n   Cách nạp: %s\n\n", v.NameVI, err, envHint(v.EnvPairs))
			skipped = append(skipped, v.NameVI)
			continue
		case err != nil:
			// Half a pair, or a caller bug. Loud, never skipped.
			fmt.Printf("── %s · LỖI CẤU HÌNH\n   %v\n\n", v.NameVI, err)
			all = append(all, check{v.NameVI + ": credential", false, err.Error()})
			attempted++
			continue
		}
		attempted++
		all = append(all, runVenue(ctx, v, creds, *recvWindowMs)...)
	}

	fmt.Println()
	failed := 0
	for _, c := range all {
		mark := "ĐẠT "
		if !c.Pass {
			mark, failed = "HỎNG", failed+1
		}
		fmt.Printf("[%s] %s — %s\n", mark, c.NameVI, c.DetailVI)
	}
	for _, s := range skipped {
		fmt.Printf("[BỎ QUA] %s — chưa có key, không tính là hỏng\n", s)
	}
	fmt.Println()

	if attempted == 0 {
		fmt.Println("Bước 4.1: KHÔNG sàn nào có key — không thử gì cả, CHƯA NGHIỆM THU.")
		fmt.Println("Không đánh dấu ✅ khi chưa gọi được số dư testnet thật.")
		os.Exit(exitNoCredentia)
	}
	if failed > 0 {
		fmt.Printf("Bước 4.1: %d/%d mục hỏng — CHƯA NGHIỆM THU.\n", failed, len(all))
		os.Exit(exitFailed)
	}
	fmt.Printf("Bước 4.1: %d/%d mục đạt trên TESTNET. Không lệnh nào được đặt.\n", len(all), len(all))
	if len(skipped) > 0 {
		fmt.Printf("Còn %s chưa nghiệm thu vì chưa có key riêng cho sàn đó.\n", strings.Join(skipped, ", "))
	}
	os.Exit(exitOK)
}

// envHint names the variables that would configure a venue. Names only.
func envHint(pairs []broker.EnvPair) string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.KeyVar+" + "+p.SecretVar)
	}
	return strings.Join(out, "  hoặc  ")
}

func runVenue(ctx context.Context, v venue, creds broker.Credentials, recvWindowMs int64) []check {
	fmt.Printf("── %s · %s\n", v.NameVI, v.BaseURL)
	fmt.Printf("   credential: %s\n", creds.SourceVI)

	client, err := broker.NewClient(broker.Config{
		BaseURL:           v.BaseURL,
		Credentials:       creds,
		RecvWindowMs:      recvWindowMs,
		TimePath:          v.TimePath,
		WeightLimitPerMin: v.WeightPerMin,
		UserAgentVI:       "funding-rate-arbitrage/brokercheck",
	})
	if err != nil {
		fmt.Printf("   client: %v\n\n", err)
		return []check{{v.NameVI + ": dựng client", false, err.Error()}}
	}

	var out []check

	// 1. The venue clock. Unsigned, weight 1, and the measurement every signed
	//    request is corrected by.
	startedAt := time.Now()
	skewMs, err := client.SyncClock(ctx)
	elapsed := time.Since(startedAt)
	if err != nil {
		fmt.Printf("   giờ server: %v\n\n", err)
		return append(out, check{v.NameVI + ": giờ server", false, err.Error()})
	}
	fmt.Printf("   giờ server %s · HTTP 200 · lệch %d ms · vòng %s · %s\n",
		v.TimePath, skewMs, elapsed.Round(time.Millisecond), client.Budget().ReportVI())
	out = append(out, check{
		v.NameVI + ": giờ server", true,
		fmt.Sprintf("HTTP 200, lệch %d ms so với đồng hồ máy (recv_window_ms %d)", skewMs, client.RecvWindowMs()),
	})

	// 2. The signed read. This is the acceptance criterion in PLAN 4.1: "gọi
	//    được endpoint đọc số dư trên testnet".
	var raw json.RawMessage
	startedAt = time.Now()
	err = client.GetSigned(ctx, v.Account, nil, &raw)
	elapsed = time.Since(startedAt)
	if err != nil {
		fmt.Printf("   %s: %v\n\n", v.Account.Path, err)
		return append(out, check{v.NameVI + ": " + v.Account.Path, false, err.Error()})
	}

	assets, nonZero, err := v.Parse(raw)
	if err != nil {
		fmt.Printf("   %s: %v\n\n", v.Account.Path, err)
		return append(out, check{v.NameVI + ": " + v.Account.Path, false, err.Error()})
	}
	sort.Strings(assets)
	shown := assets
	if len(shown) > 12 {
		shown = append(append([]string{}, assets[:12]...), fmt.Sprintf("… +%d", len(assets)-12))
	}
	// Amounts are deliberately NOT printed, only names and counts: this output
	// is pasted into reports and transcripts, and a balance is account
	// information even on testnet. The count of non-zero assets is what the
	// acceptance actually needs — it says the account answered with real state
	// and not with an empty envelope.
	fmt.Printf("   %s · HTTP 200 · weight %d · vòng %s · %d tài sản (%d khác 0): %s\n",
		v.Account.Path, v.Account.WeightIP, elapsed.Round(time.Millisecond),
		len(assets), nonZero, strings.Join(shown, ", "))
	fmt.Printf("   %s\n", client.Budget().ReportVI())
	fmt.Printf("   tài liệu: %s\n\n", v.Account.DocURL)

	out = append(out, check{
		v.NameVI + ": " + v.Account.Path, len(assets) > 0,
		fmt.Sprintf("HTTP 200, %d tài sản trả về (%d khác 0), weight %d, %s",
			len(assets), nonZero, v.Account.WeightIP, client.Budget().ReportVI()),
	})
	if banned, until := client.Budget().Banned(); banned {
		out = append(out, check{v.NameVI + ": rate limit", false,
			fmt.Sprintf("IP bị cấm (HTTP 418) tới %s — KHÔNG thử lại, thử lại làm lệnh cấm dài ra", until)})
	}
	return out
}

// parseFuturesBalance reads GET /fapi/v3/balance: an array of
// {accountAlias, asset, balance, crossWalletBalance, …}.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Futures-Account-Balance-V3
func parseFuturesBalance(raw json.RawMessage) ([]string, int, error) {
	var rows []struct {
		Asset   string `json:"asset"`
		Balance string `json:"balance"`
	}
	if err := decode(raw, &rows); err != nil {
		return nil, 0, err
	}
	assets, nonZero := make([]string, 0, len(rows)), 0
	for _, r := range rows {
		assets = append(assets, r.Asset)
		if isNonZero(r.Balance) {
			nonZero++
		}
	}
	return assets, nonZero, nil
}

// parseSpotAccount reads GET /api/v3/account: an object whose `balances` array
// holds {asset, free, locked}.
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/account-endpoints
func parseSpotAccount(raw json.RawMessage) ([]string, int, error) {
	var account struct {
		AccountType string `json:"accountType"`
		CanTrade    bool   `json:"canTrade"`
		CanWithdraw bool   `json:"canWithdraw"`
		Balances    []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := decode(raw, &account); err != nil {
		return nil, 0, err
	}
	assets, nonZero := make([]string, 0, len(account.Balances)), 0
	for _, b := range account.Balances {
		assets = append(assets, b.Asset)
		if isNonZero(b.Free) || isNonZero(b.Locked) {
			nonZero++
		}
	}
	// canWithdraw is printed as a WARNING, not a failure: the key's permissions
	// are the operator's to set, and PLAN 4.1 asks for withdrawal disabled.
	if account.CanWithdraw {
		fmt.Println("   ⚠ key này có quyền RÚT TIỀN. Bước 4.1 yêu cầu bật giao dịch, TẮT rút tiền — sửa quyền ở trang testnet.")
	}
	return assets, nonZero, nil
}

func isNonZero(s string) bool {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && v != 0
}

// decode wraps json.Unmarshal so the venue's body does not travel into a
// message this command prints. encoding/json's own errors describe the shape
// rather than echo the input, but the body is venue-controlled text and this
// output is pasted into reports.
func decode(raw json.RawMessage, into any) error {
	if err := json.Unmarshal(raw, into); err != nil {
		return errors.New("không giải mã được câu trả lời của sàn (thân phản hồi không in ra): " + err.Error())
	}
	return nil
}
