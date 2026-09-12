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
// Credentials come from BINANCE_TESTNET_API_KEY and BINANCE_TESTNET_API_SECRET,
// read from the environment (or a .env file, which .gitignore already covers).
// Nothing here prints a key, a secret, a signature, or a balance amount: what
// it prints is the HTTP status, the measured clock skew in milliseconds, the
// weight the venue says this IP has spent, and the NUMBER and NAMES of the
// assets each account returned.
//
// Exit codes: 0 every check passed · 1 a check failed · 2 no testnet
// credentials were configured, so nothing was attempted.
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

const (
	keyVar    = "BINANCE_TESTNET_API_KEY"
	secretVar = "BINANCE_TESTNET_API_SECRET"
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

	creds, err := broker.CredentialsFromEnv(keyVar, secretVar)
	if err != nil {
		// The error names the VARIABLES and never their contents.
		fmt.Printf("CHƯA CÓ KEY TESTNET: %v\n\n", err)
		fmt.Printf("Cách nạp: chép .env.example thành .env rồi điền %s và %s\n", keyVar, secretVar)
		fmt.Println("  - Futures testnet: https://demo-fapi.binance.com")
		fmt.Println("  - Spot testnet:    https://testnet.binance.vision")
		fmt.Println("  - Quyền: bật giao dịch, TẮT rút tiền. Không dán giá trị vào bất kỳ file nào được commit.")
		fmt.Println()
		fmt.Println("Bước 4.1: code xong, CHƯA NGHIỆM THU. Không đánh dấu ✅ khi chưa gọi được số dư testnet thật.")
		os.Exit(exitNoCredentia)
	}
	fmt.Printf("credential: nạp từ %s\n\n", creds.SourceVI)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var all []check
	all = append(all, runVenue(ctx, "binance futures testnet", venue{
		BaseURL:      broker.BinanceFuturesTestnetBaseURL,
		TimePath:     broker.BinanceFuturesTimePath,
		WeightPerMin: broker.BinanceFuturesWeightPerMin,
		Account:      broker.FuturesAccountBalance,
		Parse:        parseFuturesBalance,
	}, creds, *recvWindowMs)...)
	all = append(all, runVenue(ctx, "binance spot testnet", venue{
		BaseURL:      broker.BinanceSpotTestnetBaseURL,
		TimePath:     broker.BinanceSpotTimePath,
		WeightPerMin: broker.BinanceSpotWeightPerMin,
		Account:      broker.SpotAccount,
		Parse:        parseSpotAccount,
	}, creds, *recvWindowMs)...)

	fmt.Println()
	failed := 0
	for _, c := range all {
		mark := "ĐẠT "
		if !c.Pass {
			mark, failed = "HỎNG", failed+1
		}
		fmt.Printf("[%s] %s — %s\n", mark, c.NameVI, c.DetailVI)
	}
	fmt.Println()
	if failed > 0 {
		fmt.Printf("Bước 4.1: %d/%d mục hỏng — CHƯA NGHIỆM THU.\n", failed, len(all))
		os.Exit(exitFailed)
	}
	fmt.Printf("Bước 4.1: %d/%d mục đạt trên TESTNET. Không lệnh nào được đặt.\n", len(all), len(all))
	os.Exit(exitOK)
}

type venue struct {
	BaseURL      string
	TimePath     string
	WeightPerMin int
	Account      broker.Endpoint
	Parse        func(json.RawMessage) (assets []string, nonZero int, err error)
}

func runVenue(ctx context.Context, nameVI string, v venue, creds broker.Credentials, recvWindowMs int64) []check {
	fmt.Printf("── %s · %s\n", nameVI, v.BaseURL)

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
		return []check{{nameVI + ": dựng client", false, err.Error()}}
	}

	var out []check

	// 1. The venue clock. Unsigned, weight 1, and the measurement every signed
	//    request is corrected by.
	startedAt := time.Now()
	skewMs, err := client.SyncClock(ctx)
	elapsed := time.Since(startedAt)
	if err != nil {
		fmt.Printf("   giờ server: %v\n\n", err)
		return append(out, check{nameVI + ": giờ server", false, err.Error()})
	}
	fmt.Printf("   giờ server %s · HTTP 200 · lệch %d ms · vòng %s · %s\n",
		v.TimePath, skewMs, elapsed.Round(time.Millisecond), client.Budget().ReportVI())
	out = append(out, check{
		nameVI + ": giờ server", true,
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
		return append(out, check{nameVI + ": " + v.Account.Path, false, err.Error()})
	}

	assets, nonZero, err := v.Parse(raw)
	if err != nil {
		fmt.Printf("   %s: %v\n\n", v.Account.Path, err)
		return append(out, check{nameVI + ": " + v.Account.Path, false, err.Error()})
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
		nameVI + ": " + v.Account.Path, len(assets) > 0,
		fmt.Sprintf("HTTP 200, %d tài sản trả về (%d khác 0), weight %d, %s",
			len(assets), nonZero, v.Account.WeightIP, client.Budget().ReportVI()),
	})
	if banned, until := client.Budget().Banned(); banned {
		out = append(out, check{nameVI + ": rate limit", false,
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
