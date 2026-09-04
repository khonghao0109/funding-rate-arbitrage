package exchanges

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Golden files have to come from the venues, not from anyone's memory of what a
// venue sends. This is the tool that fetches them.
//
// It is skipped unless CAPTURE_TESTDATA=1, because it opens real sockets to
// eight exchanges - `go test ./...` must stay offline and fast. Run it when a
// venue changes its payloads, or when adding a connector:
//
//	CAPTURE_TESTDATA=1 go test -run TestCaptureTestdata -timeout 5m ./exchanges/
//
// It deliberately builds each venue's REAL streamConfig - the same URL and the
// same subscription production uses - and swaps only Handle. A capture tool with
// its own copy of the subscribe messages would drift from the connector and
// record payloads nobody actually receives, which is the failure mode golden
// tests exist to prevent.

const (
	captureEnv = "CAPTURE_TESTDATA"
	// captureDuration is how long each venue is listened to. It has to be long
	// enough for the SLOWEST channel a connector subscribes to: OKX pushed 76
	// trades before its first books5 frame.
	captureDuration = 30 * time.Second
	// framesPerKind bounds how many of each message shape are kept. Keeping the
	// first N frames overall does not work - one chatty channel starves every
	// other, which is how the first recording ended up with no OKX book update
	// and no Binance futures trade at all.
	framesPerKind    = 8
	captureDirectory = "testdata"

	// framesPerSequenceKind is the cap for a message shape whose MEANING is a
	// sequence rather than a sample. Kraken's book deltas are the only such
	// shape: the connector's top of book is the result of every frame since
	// the snapshot, so a recording has to hold enough of them to actually
	// reach the best level. Measured 2026-09-04, the first eight deltas after
	// a snapshot all landed 80-90 ticks behind the top on both products, which
	// left the assembler's golden test unable to observe a single change.
	framesPerSequenceKind = 40
)

// sequenceKindPrefix marks the frame kinds capped by framesPerSequenceKind.
// frameKind renders Kraken book updates as "feed=book|<product>".
const sequenceKindPrefix = "feed=book|"

// captureSymbols mirrors the mapping in config.yaml for the two most liquid
// pairs. It is written out rather than loaded because the exchanges package must
// not import internal/config - public market data and configuration live on
// opposite sides of that line (docs/CONVENTIONS.md §12.1). Two pairs is enough:
// the point is one of every message SHAPE, not coverage of the pair list.
var captureSymbols = map[string][]Symbol{
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
}

// captureStreams builds each venue's production stream config. Pyth is absent:
// it speaks SSE over HTTP rather than WebSocket, so it has no streamConfig.
func captureStreams(f Feeds) map[string]streamConfig {
	return map[string]streamConfig{
		"binance_futures": binanceStream("binance_futures", captureSymbols["binance_futures"], f, "wss://fstream.binance.com"),
		"binance_spot":    binanceStream("binance_spot", captureSymbols["binance_spot"], f, "wss://stream.binance.com:9443"),
		"bybit_futures":   bybitStream("bybit_futures", captureSymbols["bybit_futures"], f, "wss://stream.bybit.com/v5/public/linear", true),
		"bybit_spot":      bybitStream("bybit_spot", captureSymbols["bybit_spot"], f, "wss://stream.bybit.com/v5/public/spot", false),
		"okx_futures":     okxStream("okx_futures", captureSymbols["okx_futures"], f),
		"gate_futures":    gateStream("gate_futures", captureSymbols["gate_futures"], f),
		"kraken_futures":  krakenStream("kraken_futures", captureSymbols["kraken_futures"], f),
		// The meta cache only gates PUBLISHING a reading; capture replaces
		// Handle and stores raw frames, so an empty one records the same
		// activeAssetCtx payloads production sees.
		"hyperliquid_futures": hyperliquidStream("hyperliquid_futures", captureSymbols["hyperliquid_futures"], f, newFundingMetaCache()),
		"paradex_futures":     paradexStream("paradex_futures", captureSymbols["paradex_futures"], f),
	}
}

func TestCaptureTestdata(t *testing.T) {
	if os.Getenv(captureEnv) != "1" {
		t.Skipf("set %s=1 to re-record golden payloads from the live venues", captureEnv)
	}

	for source, cfg := range captureStreams(Feeds{}) {
		t.Run(source, func(t *testing.T) {
			frames := captureFrom(t, source, cfg)
			if len(frames) == 0 {
				t.Fatalf("%s delivered nothing", source)
			}
			writeFrames(t, source, frames)
			t.Logf("%s: %d frames", source, len(frames))
		})
	}
}

// captureFrom records raw frames from one venue for captureDuration, keeping at
// most framesPerKind of each message shape.
//
// Arrival order is preserved, which matters: Kraken's book_snapshot has to come
// before the deltas that build on it, or the golden file describes a sequence
// that could not have happened.
func captureFrom(t *testing.T, source string, cfg streamConfig) [][]byte {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), captureDuration)
	defer cancel()

	var frames [][]byte
	kept := map[string]int{}
	done := make(chan struct{})

	recording := cfg
	recording.Handle = func(raw []byte, _ time.Time) {
		kind := frameKind(raw, captureSymbols[source])
		limit := framesPerKind
		if strings.HasPrefix(kind, sequenceKindPrefix) {
			limit = framesPerSequenceKind
		}
		if kept[kind] >= limit {
			return
		}
		kept[kind]++
		frames = append(frames, bytes.Clone(raw))
	}

	go func() {
		defer close(done)
		runStream(Feeds{Ctx: ctx}, recording)
	}()
	<-done

	t.Logf("%s: %d shapes", source, len(kept))
	return frames
}

// frameKind fingerprints a message so the recording keeps a spread of shapes
// rather than a spread of timestamps.
//
// The fingerprint is the venue's own discriminator fields plus which subscribed
// market the frame mentions. The market half is what forces a usable recording
// out of Paradex: it streams a summary for EVERY market it lists, hundreds of
// them including options, and without it a recording is a random slice of
// instruments this scanner does not follow.
func frameKind(raw []byte, symbols []Symbol) string {
	market := ""
	for _, symbol := range symbols {
		if bytes.Contains(raw, []byte(`"`+symbol.Venue+`"`)) {
			market = symbol.Venue
			break
		}
	}

	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return "non-json|" + market
	}

	var parts []string
	for _, key := range []string{"stream", "topic", "channel", "feed", "method", "event", "type"} {
		value, ok := probe[key]
		if !ok {
			continue
		}
		parts = append(parts, key+"="+strings.Trim(string(value), `"`))
	}
	// OKX names the channel inside arg, and Paradex inside params.
	for _, nested := range []string{"arg", "params"} {
		value, ok := probe[nested]
		if !ok {
			continue
		}
		var inner struct {
			Channel string `json:"channel"`
		}
		if json.Unmarshal(value, &inner) == nil && inner.Channel != "" {
			parts = append(parts, nested+".channel="+inner.Channel)
		}
	}
	if len(parts) == 0 {
		for key := range probe {
			parts = append(parts, key)
		}
		sort.Strings(parts)
	}

	return strings.Join(parts, "|") + "|" + market
}

// writeFrames stores one frame per line, so a golden file stays greppable and
// diffs one message at a time.
//
// JSON frames are compacted first. Paradex pretty-prints its payloads across
// several lines, which the format cannot hold. Compaction removes only
// insignificant whitespace - every key, every value and every type survives
// exactly - and these files exist to test PARSING, for which whitespace is by
// definition not part of the input. A non-JSON frame (OKX answers a keepalive
// with the bare word "pong") is stored as it arrived.
func writeFrames(t *testing.T, source string, frames [][]byte) {
	t.Helper()

	if err := os.MkdirAll(captureDirectory, 0o755); err != nil {
		t.Fatalf("create %s: %v", captureDirectory, err)
	}

	var out bytes.Buffer
	for _, frame := range frames {
		var compact bytes.Buffer
		if err := json.Compact(&compact, frame); err == nil {
			frame = compact.Bytes()
		}
		if bytes.ContainsAny(frame, "\r\n") {
			t.Errorf("%s sent a non-JSON frame containing a newline; the format cannot hold it", source)
			continue
		}
		out.Write(frame)
		out.WriteByte('\n')
	}

	path := filepath.Join(captureDirectory, source+".jsonl")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readFrames loads a golden file. Used by every connector's parse test.
func readFrames(t *testing.T, source string) [][]byte {
	t.Helper()

	path := filepath.Join(captureDirectory, source+".jsonl")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v (re-record with %s=1 go test -run TestCaptureTestdata ./exchanges/)",
			path, err, captureEnv)
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
