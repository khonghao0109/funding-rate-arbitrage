package scanner

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"

	"github.com/gorilla/websocket"
)

// A client must receive meta before any data message: it builds its source list,
// symbol selector and cost disclaimer from meta, and cannot render a price for a
// source it has never heard of.
func TestHandleWebSocket_SendsMetaBeforeAnyData(t *testing.T) {
	scanner := New([]string{"BTCUSDT", "ETHUSDT"})

	server := httptest.NewServer(http.HandlerFunc(scanner.HandleWebSocket))
	defer server.Close()

	// Data must already be flowing BEFORE the client connects. Waiting for the
	// client to register first would make the harness produce the very ordering
	// this test claims to verify, and it would stay green even if the meta write
	// moved after registration.
	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 65100))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
			}
		}
	}()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read first message: %v", err)
	}

	var envelope struct {
		Type    string `json:"type"`
		V       int    `json:"v"`
		Symbols []string
		Sources []struct {
			Source string `json:"source"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal first message: %v", err)
	}

	if envelope.Type != "meta" {
		t.Fatalf("first message type = %q, want meta (payload: %s)", envelope.Type, raw)
	}
	if envelope.V != wireVersion {
		t.Errorf("first message v = %d, want %d", envelope.V, wireVersion)
	}
	if len(envelope.Sources) != len(sourceRegistry) {
		t.Errorf("meta carried %d sources, want %d", len(envelope.Sources), len(sourceRegistry))
	}
}

// Every message the scanner emits must carry the contract envelope, or the
// dashboard cannot tell a version mismatch from a parse failure.
func TestBroadcastMessages_CarryContractEnvelope(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})

	server := httptest.NewServer(http.HandlerFunc(scanner.HandleWebSocket))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, _, err := conn.ReadMessage(); err != nil { // meta
		t.Fatalf("read meta: %v", err)
	}
	waitForClient(t, scanner)

	startSpreadsFlush(t, scanner)

	// Two sources on one symbol with a wide gap produces both a spreads message
	// and an arbitrage message.
	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// The funding table follows meta on connect (step 2.7), so the stream now
	// carries a third type before the two this test is about. Read until BOTH
	// wanted types have arrived rather than until any two have.
	seen := make(map[string]bool)
	for i := 0; i < 8 && !(seen["spreads"] && seen["arbitrage"]); i++ {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var envelope struct {
			Type         string `json:"type"`
			V            int    `json:"v"`
			ServerTimeMs int64  `json:"server_time_ms"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if envelope.V != wireVersion {
			t.Errorf("%s: v = %d, want %d", envelope.Type, envelope.V, wireVersion)
		}
		if envelope.ServerTimeMs <= 0 {
			t.Errorf("%s: server_time_ms = %d, want a real clock reading", envelope.Type, envelope.ServerTimeMs)
		}
		seen[envelope.Type] = true
	}

	for _, want := range []string{"spreads", "arbitrage"} {
		if !seen[want] {
			t.Errorf("never received a %q message, got %v", want, seen)
		}
	}
}

// startSpreadsFlush runs the broadcast ticker for a test.
//
// Step 2.7 stopped sending the matrix on the tick that produced it — it fired
// on every price tick from every venue, and 58 of 60 frames on the wire were
// spreads (PLAN §7.3). Production starts this in Run; a test that drives
// updatePrice directly has to start it too, or nothing is ever sent.
func startSpreadsFlush(t *testing.T, scanner *Scanner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go scanner.flushSpreads(ctx)
}

// waitForClient blocks until the handler has registered the dialled connection.
// Registration happens after the meta write, so anything broadcast before it
// reaches no one.
func waitForClient(t *testing.T, s *Scanner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.clientsMutex.RLock()
		n := len(s.wsClients)
		s.clientsMutex.RUnlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("handler never registered the websocket client")
}

func mustPriceData(symbol, source string, price float64) exchanges.PriceData {
	return exchanges.PriceData{Symbol: symbol, Source: source, Price: price}
}

// A symbol that drops below two usable prices must still republish its matrix.
// Returning early instead would leave the previous matrix frozen on screen with
// no field contradicting it.
func TestCheckArbitrage_RepublishesMatrixWhenItShrinks(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})

	server := httptest.NewServer(http.HandlerFunc(scanner.HandleWebSocket))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, _, err := conn.ReadMessage(); err != nil { // meta
		t.Fatalf("read meta: %v", err)
	}
	waitForClient(t, scanner)

	startSpreadsFlush(t, scanner)

	// One usable price, one broken source: not enough to compare, but the
	// dashboard must still be told what the matrix looks like now.
	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "okx_futures", 0))

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	for i := 0; i < 6; i++ {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var msg struct {
			Type             string `json:"type"`
			CrossVenueGroups []struct {
				Sources []string `json:"sources"`
			} `json:"cross_venue_groups"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != "spreads" {
			continue
		}
		// One usable perp has nobody to compare against, so step 1.2 emits no
		// group at all. What matters is that the message was still published:
		// silence would leave the previous matrix frozen on screen.
		for _, group := range msg.CrossVenueGroups {
			for _, source := range group.Sources {
				if source == "okx_futures" {
					t.Errorf("the zero-priced source appeared in group %v", group.Sources)
				}
			}
		}
		return
	}
	t.Fatal("no spreads message published for a symbol with fewer than two usable prices")
}

// The exclusion must be produced by checkArbitrage and reach the wire. Asserting
// it end-to-end is what makes it a guard: a test that recomputed the exclusion
// itself would stay green if checkArbitrage stopped reporting one.
func TestCheckArbitrage_PublishesWhyASourceWasDropped(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})

	server := httptest.NewServer(http.HandlerFunc(scanner.HandleWebSocket))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, _, err := conn.ReadMessage(); err != nil { // meta
		t.Fatalf("read meta: %v", err)
	}
	waitForClient(t, scanner)

	startSpreadsFlush(t, scanner)
	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "okx_futures", math.NaN()))

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	for i := 0; i < 8; i++ {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var msg struct {
			Type            string `json:"type"`
			ExcludedSources []struct {
				Source string `json:"source"`
				Reason string `json:"reason"`
				NoteVI string `json:"note_vi"`
			} `json:"excluded_sources"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != "spreads" || len(msg.ExcludedSources) == 0 {
			continue
		}
		for _, e := range msg.ExcludedSources {
			if e.Source != "okx_futures" {
				continue
			}
			if e.Reason != "no_price" {
				t.Errorf("reason = %q, want no_price", e.Reason)
			}
			if e.NoteVI == "" {
				t.Error("an excluded source must carry an explanation for the user")
			}
			return
		}
	}
	t.Fatal("the dropped source was never explained on the wire")
}

// The connect-time push order is meta → funding → depth (scanner.go's
// handshake): the client renders the Funding tab from the very first frames
// instead of staring at an empty table for up to 5s (funding ticker) or an
// hour (next depth sweep). Nothing pinned that order — a regression dropping
// the funding/depth push would have failed no test.
func TestHandleWebSocket_PushesFundingAndDepthAfterMeta(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})

	server := httptest.NewServer(http.HandlerFunc(scanner.HandleWebSocket))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	var order []string
	for i := 0; i < 3; i++ {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read message %d: %v", i, err)
		}
		var envelope struct {
			Type string `json:"type"`
			V    int    `json:"v"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("unmarshal message %d: %v", i, err)
		}
		if envelope.V != wireVersion {
			t.Errorf("message %d v = %d, want %d", i, envelope.V, wireVersion)
		}
		order = append(order, envelope.Type)
	}
	want := []string{"meta", "funding", "depth"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("connect-time push order = %v, want %v", order, want)
		}
	}
}
