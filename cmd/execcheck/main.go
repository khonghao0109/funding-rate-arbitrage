// Command execcheck opens and closes ONE delta-neutral position on Binance
// TESTNET, by hand, and prints what the venues say happened.
//
//	go run ./cmd/execcheck -open  -symbol BTCUSDT -notional-quote 120
//	go run ./cmd/execcheck -status -intent fa1-20260913-...
//	go run ./cmd/execcheck -close  -intent fa1-20260913-...
//	go run ./cmd/execcheck -list
//
// It is the step 4.4b / 4.5 acceptance tool and it is a DIAGNOSTIC, in the
// manner of cmd/brokercheck and cmd/fundingcheck: it opens sockets, it prints a
// table, and nothing in the trading path imports it. Two commands link
// internal/broker and internal/execution — this one and brokercheck — and a
// test holds every other binary away from both (internal/broker/boundary_test.go).
//
// # What it may and may not do (PLAN §7.1 Q15, 2026-09-13)
//
// It may open and close a two-leg position on TESTNET, because the operator
// decided 4.4b and 4.5 run beside the step-3.5 gate. It may not reach a mainnet
// host: broker.NewClient refuses any base URL outside the documented testnet
// allow-list, and this command takes no flag that could widen it. And there is
// NO path from a live signal to an order here — every position is one a person
// typed. Wiring internal/strategy to internal/execution waits for both the 3.5
// verdict and step 3.4.
//
// # The state file is a CACHE, and the venue is the truth (CLAUDE.md rule 7)
//
// An open writes .paper/exec/<intent-id>.json. That file exists so a later
// -status or -close knows the intent id, the entry fills and the reference mids
// — figures that were true at an instant this process cannot revisit.
//
// It is NOT where the position lives. -status and -close read the ORDERS, the
// POSITION and the BALANCES back from the venue, by the ClientOrderIDs DERIVED
// from the intent id, and print both the cache and the venue whenever they
// disagree. A file that says a position is open proves nothing; a venue that
// says so does.
//
// No database, no schema, no migration: the step-3.5 gate has data/scanner.db
// open for writing and nothing here goes near it.
//
// Exit codes: 0 the action succeeded · 1 it failed · 2 no credential for a
// venue this action needs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/execution"

	"github.com/joho/godotenv"
)

const (
	exitOK            = 0
	exitFailed        = 1
	exitNoCredentials = 2
)

// stateDir holds one JSON file per intent. It sits under .paper/ because that
// directory is already gitignored and already the operator's scratch space for
// this project's live runs — and because putting it anywhere under data/ would
// invite somebody to make it a table.
const stateDir = ".paper/exec"

var (
	futuresEnv = []broker.EnvPair{
		{KeyVar: "BINANCE_FUTURES_TESTNET_API_KEY", SecretVar: "BINANCE_FUTURES_TESTNET_API_SECRET"},
		{KeyVar: "BINANCE_TESTNET_API_KEY", SecretVar: "BINANCE_TESTNET_API_SECRET"},
	}
	spotEnv = []broker.EnvPair{
		{KeyVar: "BINANCE_SPOT_TESTNET_API_KEY", SecretVar: "BINANCE_SPOT_TESTNET_API_SECRET"},
	}
)

func main() {
	var (
		doOpen      = flag.Bool("open", false, "open one delta-neutral position on testnet")
		doStatus    = flag.Bool("status", false, "read one intent's orders, position and balances back FROM THE VENUE")
		doClose     = flag.Bool("close", false, "close one intent's position on testnet")
		doList      = flag.Bool("list", false, "list the intents this machine has a state file for")
		doReconcile = flag.Bool("reconcile", false, "read this intent's own orders back from the venues and square an unbalanced pair")
		apply       = flag.Bool("apply", false, "with -reconcile: actually send the squaring order (without it, only say what would be sent)")

		symbol        = flag.String("symbol", "BTCUSDT", "symbol, the same string on both markets")
		notionalQuote = flag.Float64("notional-quote", 0,
			"target notional per leg, in quote. 0 asks the venues for the smallest size that clears BOTH minimums")
		intentID  = flag.String("intent", "", "intent id for -status and -close")
		legOrder  = flag.String("leg-order", string(execution.LegOrderSequentialSpotFirst), "sequential_spot_first | parallel")
		failLeg2  = flag.String("fail-leg2", "", "FAULT INJECTION for the 4.4b acceptance: min-notional | bad-symbol — makes the SECOND leg be refused by the venue")
		marginFr  = flag.Float64("margin-frac", 0.50, "collateral posted on the perp leg as a fraction of notional — a DECISION, not a venue fact")
		slipBps   = flag.Float64("max-slippage-bps", execution.DefaultMaxSlippageBps, "how far past the touch a leg's marketable limit may sit, in basis points")
		legTmo    = flag.Duration("leg-timeout", execution.DefaultLegTimeout, "how long one leg may work before its remainder is cancelled")
		timeout   = flag.Duration("timeout", 3*time.Minute, "overall deadline")
		widenBps  = flag.Float64("max-widen-bps", 5, "how much worse than the signal's entry cost the book may have become")
		jsonOut   = flag.Bool("json", false, "print the result as JSON as well as the table")
		waitUntil = flag.String("wait-until", "", "for -close: wait until this RFC3339 instant before closing (used to hold across a settlement)")
	)
	flag.Parse()
	_ = godotenv.Load()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Println("EXECCHECK — Bước 4.4b/4.5, CHỈ TESTNET, vị thế do người vận hành ra lệnh")
	fmt.Printf("host cho phép: %s\n\n", strings.Join(broker.TestnetHosts(), ", "))

	switch {
	case *doList:
		os.Exit(runList())
	case *doOpen:
		os.Exit(runOpen(ctx, openArgs{
			Symbol: *symbol, NotionalQuote: *notionalQuote, LegOrder: execution.LegOrder(*legOrder),
			FailLeg2: *failLeg2, MarginFrac: *marginFr, MaxSlippageBps: *slipBps,
			LegTimeout: *legTmo, MaxWidenBps: *widenBps, JSON: *jsonOut,
		}))
	case *doReconcile:
		os.Exit(runReconcile(ctx, *intentID, *apply))
	case *doStatus:
		os.Exit(runStatus(ctx, *intentID, *jsonOut))
	case *doClose:
		os.Exit(runClose(ctx, *intentID, *waitUntil, *jsonOut))
	default:
		flag.Usage()
		os.Exit(exitFailed)
	}
}

// ------------------------------------------------------------------ venues

// clients is one pair of market clients plus the rules and books they read.
type clients struct {
	spot *binancebroker.Client
	perp *binancebroker.Client
}

func dial() (clients, error) {
	var out clients
	perpCreds, err := broker.CredentialsFromEnvAny(futuresEnv...)
	if err != nil {
		return out, fmt.Errorf("futures testnet: %w", err)
	}
	spotCreds, err := broker.CredentialsFromEnvAny(spotEnv...)
	if err != nil {
		return out, fmt.Errorf("spot testnet: %w", err)
	}
	perpCfg, err := binancebroker.DefaultConfig(broker.MarketFuturesUSDM, perpCreds)
	if err != nil {
		return out, err
	}
	spotCfg, err := binancebroker.DefaultConfig(broker.MarketSpot, spotCreds)
	if err != nil {
		return out, err
	}
	if out.perp, err = binancebroker.New(broker.MarketFuturesUSDM, perpCfg); err != nil {
		return out, err
	}
	if out.spot, err = binancebroker.New(broker.MarketSpot, spotCfg); err != nil {
		return out, err
	}
	return out, nil
}

// market is one leg's rules, book and reference price, all read from the
// TESTNET that leg trades on.
type market struct {
	Rules binancebroker.MarketRules
	Book  depth.Summary
	Price float64
}

func readMarket(ctx context.Context, c *binancebroker.Client, symbol string) (market, error) {
	var out market
	rules, err := c.FetchInstrument(ctx, symbol)
	if err != nil {
		return out, fmt.Errorf("rules: %w", err)
	}
	out.Rules = rules

	book, err := c.FetchDepthBook(ctx, symbol)
	if err != nil {
		return out, fmt.Errorf("book: %w", err)
	}
	// Stamped by US at the moment of the read, never from the venue's own
	// clock — CLAUDE.md rule 13: a venue stamp measures skew, not age.
	out.Book = depth.Summarize(book, time.Now().UnixMilli(), func(string, string) (float64, bool) {
		// Binance denominates both books in COIN, so no conversion is needed
		// and none is offered: a multiplier returned here would be invented.
		return 1, true
	})
	if !out.Book.OK() {
		return out, fmt.Errorf("book: %s", out.Book.ErrVI)
	}
	out.Price = out.Book.MidPriceQuote
	return out, nil
}

// ------------------------------------------------------------------- state

// state is the CACHE — see the package comment. Every field here was true at
// an instant; none of it is evidence about now.
type state struct {
	IntentID      string  `json:"intent_id"`
	Symbol        string  `json:"symbol"`
	OpenedAtMs    int64   `json:"opened_at_ms"`
	LegOrder      string  `json:"leg_order"`
	NotionalQuote float64 `json:"notional_quote"`
	TargetQtyCoin float64 `json:"target_qty_coin"`

	SpotClientOrderID   string  `json:"spot_client_order_id"`
	PerpClientOrderID   string  `json:"perp_client_order_id"`
	SpotFilledQtyCoin   float64 `json:"spot_filled_qty_coin"`
	PerpFilledQtyCoin   float64 `json:"perp_filled_qty_coin"`
	SpotAvgPriceQuote   float64 `json:"spot_avg_fill_price_quote"`
	PerpAvgPriceQuote   float64 `json:"perp_avg_fill_price_quote"`
	SpotRefMidQuote     float64 `json:"spot_ref_mid_quote"`
	PerpRefMidQuote     float64 `json:"perp_ref_mid_quote"`
	SpotBestAskQuote    float64 `json:"spot_best_ask_quote"`
	PerpBestBidQuote    float64 `json:"perp_best_bid_quote"`
	BookSampledAtMs     int64   `json:"book_sampled_at_ms"`
	UnhedgedWindowMs    int64   `json:"unhedged_window_ms"`
	ReducedToMatch      bool    `json:"reduced_to_match"`
	Outcome             string  `json:"outcome"`
	NextFundingTimeMs   int64   `json:"next_funding_time_ms"`
	ClosedAtMs          int64   `json:"closed_at_ms"`
	ClosedQtyCoin       float64 `json:"closed_qty_coin"`
	RealizedQuote       float64 `json:"realized_quote"`
	FundingQuote        float64 `json:"funding_received_quote"`
	CommissionQuote     float64 `json:"commission_quote"`
	SlippageQuote       float64 `json:"slippage_quote"`
	SettlementsCounted  int     `json:"settlements_counted"`
	PairPriceDriftQuote float64 `json:"pair_price_drift_quote"`
	NoteVI              string  `json:"note_vi"`
}

func statePath(id string) string { return filepath.Join(stateDir, id+".json") }

func saveState(s state) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(s.IntentID), append(blob, '\n'), 0o644)
}

func loadState(id string) (state, error) {
	var s state
	blob, err := os.ReadFile(statePath(id))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(blob, &s)
}

func runList() int {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		fmt.Printf("chưa có ý định nào (%s)\n", stateDir)
		return exitOK
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(names)
	fmt.Printf("%d ý định trong %s (ĐÂY LÀ CACHE — hỏi sàn bằng -status mới biết thật)\n", len(names), stateDir)
	for _, n := range names {
		s, err := loadState(n)
		if err != nil {
			fmt.Printf("  %s  (không đọc được: %v)\n", n, err)
			continue
		}
		status := "ĐANG GIỮ"
		if s.ClosedAtMs > 0 {
			status = "đã đóng"
		}
		fmt.Printf("  %-40s %-10s %s  %.6f coin  %s\n", n, s.Symbol, status, s.SpotFilledQtyCoin,
			time.UnixMilli(s.OpenedAtMs).Format(time.RFC3339))
	}
	return exitOK
}
