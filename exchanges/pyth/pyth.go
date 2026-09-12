package pyth

import (
	"futures-arbitrage-scanner/exchanges"

	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// PythPriceData represents the price information within a Pyth update
type PythPriceData struct {
	Price       string `json:"price"`
	Conf        string `json:"conf"`
	Expo        int    `json:"expo"`
	PublishTime int64  `json:"publish_time"`
}

// PythParsedFeed represents a single parsed price feed
type PythParsedFeed struct {
	ID       string        `json:"id"`
	Price    PythPriceData `json:"price"`
	EMAPrice PythPriceData `json:"ema_price"`
}

// PythSSEResponse represents the complete SSE response structure
type PythSSEResponse struct {
	Parsed []PythParsedFeed `json:"parsed"`
}

// pythReadTimeout is how long the stream may deliver nothing before it is
// treated as dead. Pyth is server-sent events over HTTP, not a WebSocket, so
// there is no ping frame and no SetReadDeadline: the watchdog below cancels the
// request instead, which unblocks the body read.
const pythReadTimeout = 60 * time.Second

// pythMaxLineBytes bounds one SSE line. Generous, but bounded: an unbounded
// reader would let a malformed stream exhaust memory.
const pythMaxLineBytes = 4 * 1024 * 1024

// pythResponseHeaderTimeout bounds the wait for response headers. The WebSocket
// path gets this from dialTimeout; without it here, a server that completes TLS
// and then sends nothing parks the Pyth goroutine forever - no error, no
// reconnect, no event, and the watchdog below is only armed once the response
// has arrived.
const pythResponseHeaderTimeout = 15 * time.Second

// pythClient is used instead of http.DefaultClient so the header timeout applies.
// No overall client Timeout: this is a stream that is meant to stay open, and a
// Timeout would cut it at a fixed age. Liveness after the headers is the
// watchdog's job.
var pythClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: pythResponseHeaderTimeout,
	},
}

// ParsePrice converts Pyth price string and exponent to float64
func ParsePrice(priceStr string, expo int) (float64, error) {
	priceInt, err := strconv.ParseInt(priceStr, 10, 64)
	if err != nil {
		return 0, err
	}
	realPrice := float64(priceInt) * math.Pow10(expo)
	return realPrice, nil
}

// ConnectPrices connects to Pyth Network SSE endpoint for price feeds.
//
// It cannot share RunStream - that speaks WebSocket - but it shares everything
// the shape of the problem has in common: the same exponential backoff, the same
// context-driven stop, the same receive stamp taken at the read, and the same
// connection events.
func ConnectPrices(source string, symbols []exchanges.Symbol, f exchanges.Feeds) {
	// A Pyth market is identified by a price feed id, which no template can
	// derive, so config.yaml lists them under symbol_map. A symbol with no id
	// there is simply not served - which is why only BTC has an oracle row.
	priceFeedIDs := exchanges.VenueSymbols(symbols)
	if len(priceFeedIDs) == 0 {
		log.Printf("%s: no price feed ids configured for symbols %v, not starting", source, symbols)
		return
	}

	idParams := make([]string, 0, len(priceFeedIDs))
	for _, id := range priceFeedIDs {
		idParams = append(idParams, fmt.Sprintf("ids[]=%s", id))
	}
	sseURL := fmt.Sprintf("https://hermes.pyth.network/v2/updates/price/stream?%s", strings.Join(idParams, "&"))

	retry := exchanges.NewBackoff()

	for {
		if f.Ctx.Err() != nil {
			log.Printf("%s: stopped", source)
			f.ReportConn(source, exchanges.ConnDisconnected)
			return
		}

		startedAt := time.Now()
		err := streamPyth(source, symbols, f, sseURL)
		lasted := time.Since(startedAt)

		if f.Ctx.Err() != nil {
			log.Printf("%s: stopped", source)
			f.ReportConn(source, exchanges.ConnDisconnected)
			return
		}

		if lasted >= exchanges.HealthySession {
			retry.Reset()
		}

		f.ReportConn(source, exchanges.ConnReconnecting)
		log.Printf("%s: stream ended after %s: %v", source, lasted.Round(time.Second), err)

		if !retry.Wait(f.Ctx) {
			log.Printf("%s: stopped", source)
			f.ReportConn(source, exchanges.ConnDisconnected)
			return
		}
	}
}

// streamPyth reads one SSE connection until it ends.
func streamPyth(source string, symbols []exchanges.Symbol, f exchanges.Feeds, sseURL string) error {
	// Derived from the feeds context so cancelling the scanner aborts an
	// in-flight body read, and so the watchdog below can cut a stalled stream
	// without touching the parent.
	ctx, cancel := context.WithCancel(f.Ctx)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	response, err := pythClient.Do(request)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", response.Status)
	}

	log.Printf("%s: connected", source)
	f.ReportConn(source, exchanges.ConnConnected)

	// TWO watchdogs, the same pair runSession runs on the WebSocket side and for
	// the same reason (docs/PLAN.md step 1.6). http has no read deadline for a
	// body being streamed, so both work by cancelling the request, which ends
	// the read.
	//
	// frameDog is silence of ANY kind and catches a dead socket. Every line
	// resets it — and that is exactly why it is not enough on its own: this
	// stream carries SSE comments and `data: heartbeat`, so a Hermes that
	// stopped publishing prices while still sending heartbeats would hold this
	// connection open forever with no price on it. That is the bybit_spot
	// failure in another transport.
	//
	// dataDog is silence of PRICES, reset only by a line that published one. It
	// is off when Feeds carries no threshold, which is what every harness and
	// every pre-2026-09-12 config gets.
	watchdog := time.AfterFunc(pythReadTimeout, cancel)
	defer watchdog.Stop()

	// Written by the timer goroutine, read by this one, so the stream can say
	// WHICH silence ended it rather than reporting a bare cancellation.
	var starved atomic.Bool
	dataSilence := f.DataSilenceTimeout
	var dataDog *time.Timer
	if dataSilence > 0 {
		dataDog = time.AfterFunc(dataSilence, func() {
			starved.Store(true)
			cancel()
		})
		defer dataDog.Stop()
	}

	scanner := bufio.NewScanner(response.Body)
	// One data line carries every subscribed feed at once, and Hermes ships a
	// binary update blob alongside the parsed prices - so the line grows with the
	// number of feed ids in config.yaml. At bufio's default 64KB cap, adding
	// enough feeds would make every session die on its first data line with
	// ErrTooLong: a permanent reconnect loop produced by a YAML edit, with
	// nothing in the log naming the cause.
	scanner.Buffer(make([]byte, 0, 64*1024), pythMaxLineBytes)

	for scanner.Scan() {
		line := scanner.Text()

		// THE receive stamp for this connector, taken before parsing and before
		// queueing. See CLAUDE.md rule 13 and runSession for the WebSocket half.
		recvAt := time.Now()
		watchdog.Reset(pythReadTimeout)

		keepGoing, produced := handlePythLine(source, symbols, f, line, recvAt)
		if produced && dataDog != nil {
			dataDog.Reset(dataSilence)
		}
		if !keepGoing {
			return f.Ctx.Err()
		}
	}

	if starved.Load() {
		return fmt.Errorf("%w in the last %s: the stream kept sending lines and none carried a price — "+
			"reconnecting", exchanges.ErrDataSilence, dataSilence)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream closed by the server")
}

// handlePythLine parses one line of the SSE stream.
//
// keepGoing is false only when the feeds context was cancelled mid-batch, which
// ends the stream; a line that is not a price update, or one that will not
// parse, is skipped and is not an error - the stream carries comments and
// heartbeats too. produced says whether a price reached the feed, which is what
// the data watchdog above measures and is the same answer
// exchanges.StreamConfig.Handle gives on the WebSocket side.
func handlePythLine(source string, symbols []exchanges.Symbol, f exchanges.Feeds, line string, recvAt time.Time) (keepGoing, produced bool) {
	// SSE format: lines starting with "data:" contain the JSON payload.
	if !strings.HasPrefix(line, "data:") {
		return true, false
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "heartbeat" {
		return true, false
	}

	var response PythSSEResponse
	if err := json.Unmarshal([]byte(data), &response); err != nil {
		log.Printf("%s: JSON unmarshal error: %v", source, err)
		return true, false
	}

	for _, feed := range response.Parsed {
		// The price feed id IS the venue identifier, listed under symbol_map in
		// config.yaml. A feed we did not subscribe to resolves to nothing and is
		// dropped.
		symbol := exchanges.StandardOf(symbols, feed.ID)
		if symbol == "" {
			continue
		}

		price, err := ParsePrice(feed.Price.Price, feed.Price.Expo)
		if err != nil {
			log.Printf("%s: price parsing error for %s: %v", source, symbol, err)
			continue
		}

		if !f.SendPrice(exchanges.PriceData{
			Symbol:      symbol,
			Source:      source,
			Price:       price,
			VenueTimeMs: feed.Price.PublishTime * 1000, // seconds to milliseconds
			RecvAt:      recvAt,
		}) {
			return false, produced
		}
		produced = true
	}
	return true, produced
}
