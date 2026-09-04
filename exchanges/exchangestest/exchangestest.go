// Package exchangestest is the shared test harness for the venue packages: the
// recorder every golden replay drains, the capture tool that (re)records
// testdata from the live venues, and the contract checkers every venue's
// recording is replayed against.
//
// It exists because the venue split must not weaken the golden tests: before
// it, one loop replayed every venue through the SAME assertions, so a venue
// could not quietly drift from the data contract. The loop is now one call per
// venue package, but the assertions are still this one implementation.
//
// Only _test.go files import this package. It imports "testing" and must never
// be linked into a binary.
package exchangestest

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// Recorder collects everything a connector publishes.
type Recorder struct {
	Feeds exchanges.Feeds

	priceChan     chan exchanges.PriceData
	orderbookChan chan exchanges.OrderbookData
	tradeChan     chan exchanges.TradeData
	fundingChan   chan exchanges.FundingData
}

func NewRecorder(t *testing.T) *Recorder {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	r := &Recorder{
		// Large enough that no send in a replay can block: a blocked send would
		// hang the test rather than fail it.
		priceChan:     make(chan exchanges.PriceData, 8192),
		orderbookChan: make(chan exchanges.OrderbookData, 8192),
		tradeChan:     make(chan exchanges.TradeData, 8192),
		fundingChan:   make(chan exchanges.FundingData, 8192),
	}
	r.Feeds = exchanges.Feeds{Ctx: ctx, Price: r.priceChan, Orderbook: r.orderbookChan, Trade: r.tradeChan, Funding: r.fundingChan}
	return r
}

func (r *Recorder) Orderbooks() []exchanges.OrderbookData {
	var out []exchanges.OrderbookData
	for {
		select {
		case data := <-r.orderbookChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

func (r *Recorder) Trades() []exchanges.TradeData {
	var out []exchanges.TradeData
	for {
		select {
		case data := <-r.tradeChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

func (r *Recorder) Prices() []exchanges.PriceData {
	var out []exchanges.PriceData
	for {
		select {
		case data := <-r.priceChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

func (r *Recorder) Fundings() []exchanges.FundingData {
	var out []exchanges.FundingData
	for {
		select {
		case data := <-r.fundingChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

// Symbols mirrors the mapping in config.yaml for the two most liquid pairs. It
// is written out rather than loaded because the exchanges tree must not import
// internal/config (docs/CONVENTIONS.md §12.1). Two pairs is enough: the point
// is one of every message SHAPE, not coverage of the pair list.
func Symbols(source string) []exchanges.Symbol {
	table := map[string][]exchanges.Symbol{
		"binance_futures": {{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "ETHUSDT", Venue: "ETHUSDT"}},
		"binance_spot":    {{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "ETHUSDT", Venue: "ETHUSDT"}},
		"bybit_futures":   {{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "ETHUSDT", Venue: "ETHUSDT"}},
		"bybit_spot":      {{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "ETHUSDT", Venue: "ETHUSDT"}},
		"okx_futures":     {{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"}, {Standard: "ETHUSDT", Venue: "ETH-USDT-SWAP"}},
		"gate_futures":    {{Standard: "BTCUSDT", Venue: "BTC_USDT"}, {Standard: "ETHUSDT", Venue: "ETH_USDT"}},
		"kraken_futures":  {{Standard: "BTCUSDT", Venue: "PF_XBTUSD"}, {Standard: "ETHUSDT", Venue: "PF_ETHUSD"}},
		"hyperliquid_futures": {
			{Standard: "BTCUSDT", Venue: "BTC"}, {Standard: "ETHUSDT", Venue: "ETH"},
		},
		"paradex_futures": {{Standard: "BTCUSDT", Venue: "BTC-USD-PERP"}, {Standard: "ETHUSDT", Venue: "ETH-USD-PERP"}},
		"pyth":            {{Standard: "BTCUSDT", Venue: "0xe62df6c8b4a85fe1a67db44dc12de5db330f7ac66b72dc658afedf0f4a415b43"}},
	}
	return table[source]
}

// ReadFrames loads a golden .jsonl recording from the calling package's own
// testdata directory.
func ReadFrames(t *testing.T, source string) [][]byte {
	t.Helper()

	path := filepath.Join("testdata", source+".jsonl")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v (re-record with %s=1 go test -run TestCapture ./exchanges/...)",
			path, err, CaptureEnv)
	}
	defer file.Close()

	var frames [][]byte
	scanner := bufio.NewScanner(file)
	// Some venues send a full book snapshot in one frame, which is larger than
	// bufio's default 64KB line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		frames = append(frames, []byte(line))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(frames) == 0 {
		t.Fatalf("%s is empty", path)
	}
	return frames
}

// LoadJSON decodes one JSON fixture from the calling package's testdata.
func LoadJSON(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("missing recording: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s does not decode: %v", name, err)
	}
}

// ReplayAt is the fixed receive stamp every replay uses, so a golden test can
// assert the stamp survived without threading a clock through.
func ReplayAt() time.Time { return time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC) }

// Replay pushes every frame of the calling package's golden file through the
// venue's production Handle, in the order the venue sent them. Order matters
// for Kraken, whose top of book is the result of every frame that came before.
func Replay(t *testing.T, r *Recorder, source string, cfg exchanges.StreamConfig) time.Time {
	t.Helper()
	if cfg.Handle == nil {
		t.Fatalf("no Handle in the stream config for %s", source)
	}
	recvAt := ReplayAt()
	for _, frame := range ReadFrames(t, source) {
		cfg.Handle(frame, recvAt)
	}
	return recvAt
}
