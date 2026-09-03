package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"

	"github.com/gorilla/websocket"
)

// The whole point of step 1.1: staleness is measured from RecvAt, never from the
// venue's own timestamp. Bybit, Paradex and Kraken fill their timestamp field
// with time.Now(), so a venue that has stopped sending still looks current by
// that measure.
func TestPriceStatus_UsesRecvAtNotVenueTime(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	threshold := 10 * time.Second

	tests := []struct {
		name  string
		point PricePoint
		want  string
	}{
		{
			name:  "recently received is live",
			point: PricePoint{Price: 1, RecvAt: now.Add(-time.Second)},
			want:  statusLive,
		},
		{
			name:  "not received for longer than the threshold is stale",
			point: PricePoint{Price: 1, RecvAt: now.Add(-11 * time.Second)},
			want:  statusStale,
		},
		{
			// This is the Bybit/Paradex/Kraken trap: the venue timestamp says
			// "right now" because the connector wrote time.Now() into it, while
			// nothing has actually arrived for a minute.
			name: "a fresh venue timestamp cannot make a stale price look live",
			point: PricePoint{
				Price:       1,
				VenueTimeMs: now.UnixMilli(),
				RecvAt:      now.Add(-time.Minute),
			},
			want: statusStale,
		},
		{
			// Binance and OKX give a real venue time; a slow venue clock or a
			// missing timestamp must not mark healthy data stale.
			name: "no venue timestamp at all is still live when it just arrived",
			point: PricePoint{
				Price:       1,
				VenueTimeMs: 0,
				RecvAt:      now.Add(-time.Second),
			},
			want: statusLive,
		},
		{
			name:  "exactly at the threshold is still live",
			point: PricePoint{Price: 1, RecvAt: now.Add(-10 * time.Second)},
			want:  statusLive,
		},
	}

	for _, tt := range tests {
		if got := priceStatus(tt.point, threshold, now); got != tt.want {
			t.Errorf("%s: priceStatus = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestStaleAfter_IsPerVenue(t *testing.T) {
	if got := staleAfter("binance_futures"); got != defaultStaleAfterSec*time.Second {
		t.Errorf("binance_futures threshold = %v, want %v", got, defaultStaleAfterSec*time.Second)
	}
	// An unregistered source must still get a threshold rather than zero, which
	// would mark every one of its prices stale immediately.
	if got := staleAfter("does_not_exist"); got != defaultStaleAfterSec*time.Second {
		t.Errorf("unknown source threshold = %v, want the default", got)
	}
}

func TestNewWirePrices_FillsAgeAndStatus(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	prices := map[string]map[string]PricePoint{
		"BTCUSDT": {
			"binance_futures": {Price: 65000, VenueTimeMs: 111, RecvAt: now.Add(-2 * time.Second)},
			"bybit_futures":   {Price: 65100, VenueTimeMs: 0, RecvAt: now.Add(-30 * time.Second)},
		},
	}
	lastMsgAt := map[string]time.Time{
		"binance_futures": now.Add(-2 * time.Second),
		"bybit_futures":   now.Add(-30 * time.Second),
	}

	msg := newWirePrices(prices, lastMsgAt, nil, time.Time{}, now)

	fresh := msg.Prices["BTCUSDT"]["binance_futures"]
	if fresh.Status != statusLive {
		t.Errorf("fresh status = %q, want live", fresh.Status)
	}
	if fresh.AgeMs != 2000 {
		t.Errorf("fresh age_ms = %d, want 2000", fresh.AgeMs)
	}
	if fresh.RecvAtMs != now.Add(-2*time.Second).UnixMilli() {
		t.Errorf("fresh recv_at_ms = %d", fresh.RecvAtMs)
	}
	if fresh.VenueTimeMs != 111 {
		t.Errorf("fresh venue_time_ms = %d, want 111 passed through", fresh.VenueTimeMs)
	}

	old := msg.Prices["BTCUSDT"]["bybit_futures"]
	if old.Status != statusStale {
		t.Errorf("old status = %q, want stale", old.Status)
	}
	if old.AgeMs != 30000 {
		t.Errorf("old age_ms = %d, want 30000", old.AgeMs)
	}
}

// A stale source must stay visible. Dropping it would remove the row from the
// dashboard, which reads as "this venue is fine, it just has nothing to show".
func TestNewWirePrices_KeepsStalePricesSoTheUICanShowThem(t *testing.T) {
	now := time.Now()
	msg := newWirePrices(map[string]map[string]PricePoint{
		"BTCUSDT": {"bybit_futures": {Price: 65100, RecvAt: now.Add(-time.Hour)}},
	}, map[string]time.Time{"bybit_futures": now.Add(-time.Hour)}, nil, time.Time{}, now)

	point, ok := msg.Prices["BTCUSDT"]["bybit_futures"]
	if !ok {
		t.Fatal("a stale source was dropped from the snapshot; the dashboard row would silently disappear")
	}
	if point.Status != statusStale {
		t.Errorf("status = %q, want stale", point.Status)
	}
	if point.Price != 65100 {
		t.Errorf("price = %g, want the last known price kept for display", point.Price)
	}
}

// Connection state is per source and independent of any one symbol: a venue can
// be healthy while one thin pair goes quiet.
func TestSourceState_DerivedFromSilenceAcrossAllSymbols(t *testing.T) {
	now := time.Now()
	msg := newWirePrices(map[string]map[string]PricePoint{
		"BTCUSDT": {
			"binance_futures": {Price: 1, RecvAt: now.Add(-time.Hour)},
			"okx_futures":     {Price: 1, RecvAt: now.Add(-time.Hour)},
		},
	}, map[string]time.Time{
		// Binance is still sending other symbols; only this pair went quiet.
		"binance_futures": now.Add(-time.Second),
		// OKX has sent nothing at all for a long time.
		"okx_futures": now.Add(-time.Hour),
	}, nil, time.Time{}, now)

	if got := msg.SourceStatus["binance_futures"].State; got != stateConnected {
		t.Errorf("binance state = %q, want connected: the venue is still sending, only this pair is quiet", got)
	}
	if got := msg.Prices["BTCUSDT"]["binance_futures"].Status; got != statusStale {
		t.Errorf("binance BTCUSDT status = %q, want stale", got)
	}
	if got := msg.SourceStatus["okx_futures"].State; got != stateDisconnected {
		t.Errorf("okx state = %q, want disconnected", got)
	}
	if got := msg.SourceStatus["binance_futures"].LastMsgAtMs; got != now.Add(-time.Second).UnixMilli() {
		t.Errorf("last_msg_at_ms = %d", got)
	}
}

// The acceptance criterion of step 1.1, in unit form: a venue that stops sending
// must not keep contributing to alerts.
func TestCheckArbitrage_RaisesNoAlertFromAFrozenPrice(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})

	base := time.Now()
	scanner.now = func() time.Time { return base }

	captured := make(chan wireOpportunity, 8)
	scanner.onOpportunity = func(o wireOpportunity) { captured <- o }

	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))
	// Drain the alert raised while both were fresh.
	<-captured

	// Binance goes silent; bybit keeps ticking at a price far away. The spread
	// against the frozen binance price is enormous and entirely fictional.
	scanner.now = func() time.Time { return base.Add(time.Hour) }
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))

	select {
	case opp := <-captured:
		t.Errorf("raised an alert from a frozen price: %+v", opp)
	default:
	}
}

func TestCheckArbitrage_PublishesStaleAsTheReasonASourceIsMissing(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})
	base := time.Now()
	scanner.now = func() time.Time { return base }

	server := httptest.NewServer(http.HandlerFunc(scanner.HandleWebSocket))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	waitForClient(t, scanner)

	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.now = func() time.Time { return base.Add(time.Hour) }
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))

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
			CrossVenueGroups []struct {
				Sources []string `json:"sources"`
			} `json:"cross_venue_groups"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Before bybit arrives, binance is briefly the only source and is
		// excluded as no_peer. That is a setup transient, not the case under
		// test: wait for a message that reports something stale. If nothing ever
		// does, the loop falls through to the Fatal below.
		if msg.Type != "spreads" {
			continue
		}
		var sawStale bool
		for _, e := range msg.ExcludedSources {
			if e.Reason == "stale" {
				sawStale = true
			}
		}
		if !sawStale {
			continue
		}
		for _, group := range msg.CrossVenueGroups {
			for _, s := range group.Sources {
				if s == "binance_futures" {
					t.Error("a stale source is still in the comparison matrix")
				}
			}
		}
		for _, e := range msg.ExcludedSources {
			if e.Source != "binance_futures" {
				continue
			}
			if e.Reason != "stale" {
				t.Errorf("reason = %q, want stale", e.Reason)
			}
			if e.NoteVI == "" {
				t.Error("an excluded source must carry an explanation")
			}
			return
		}
	}
	t.Fatal("the stale source was never reported as excluded")
}

// Found by watching the real scanner: Pyth never delivered a price, so it was
// absent from source_status entirely and the dashboard could not tell the user
// the feed was dead. A registered source must always be reported.
func TestNewWirePrices_ReportsSourcesThatNeverDelivered(t *testing.T) {
	now := time.Now()
	startedAt := now.Add(-time.Hour)

	msg := newWirePrices(map[string]map[string]PricePoint{
		"BTCUSDT": {"binance_futures": {Price: 65000, RecvAt: now}},
	}, map[string]time.Time{"binance_futures": now}, nil, startedAt, now)

	if len(msg.SourceStatus) != len(sourceRegistry) {
		t.Errorf("source_status has %d entries, want one per registered source (%d)",
			len(msg.SourceStatus), len(sourceRegistry))
	}
	if got := msg.SourceStatus["pyth"].State; got != stateDisconnected {
		t.Errorf("pyth state = %q, want disconnected: it has never sent anything", got)
	}
	if got := msg.SourceStatus["pyth"].LastMsgAtMs; got != 0 {
		t.Errorf("pyth last_msg_at_ms = %d, want 0", got)
	}
}

// At startup nothing has arrived yet. Reporting every venue as disconnected in
// the first second would be a false alarm on every restart.
func TestSourceState_UnknownDuringStartupGrace(t *testing.T) {
	now := time.Now()

	justStarted := sourceState(time.Time{}, 10*time.Second, now.Add(-2*time.Second), now)
	if justStarted != stateUnknown {
		t.Errorf("state 2s after startup = %q, want unknown", justStarted)
	}

	longUp := sourceState(time.Time{}, 10*time.Second, now.Add(-2*startupGrace), now)
	if longUp != stateDisconnected {
		t.Errorf("state %v after startup with nothing received = %q, want disconnected", 2*startupGrace, longUp)
	}
}

// checkArbitrage only runs when a price arrives. If every source for a symbol
// goes quiet, nothing re-examines it and the dashboard keeps showing the last
// matrix - which is precisely the case staleness exists to catch.
func TestRefreshStaleness_ReexaminesASymbolNothingArrivesFor(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})
	base := time.Now()
	scanner.now = func() time.Time { return base }

	server := httptest.NewServer(http.HandlerFunc(scanner.HandleWebSocket))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	waitForClient(t, scanner)

	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))

	// Everything goes quiet. Nothing will call updatePrice again.
	scanner.now = func() time.Time { return base.Add(time.Hour) }
	refreshCtx, stopRefresh := context.WithCancel(context.Background())
	defer stopRefresh()
	go scanner.refreshStaleness(refreshCtx)

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	for i := 0; i < 20; i++ {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var msg struct {
			Type             string `json:"type"`
			CrossVenueGroups []struct {
				Sources []string `json:"sources"`
			} `json:"cross_venue_groups"`
			ExcludedSources []struct {
				Reason string `json:"reason"`
			} `json:"excluded_sources"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Type != "spreads" {
			continue
		}
		var sawStale bool
		for _, e := range msg.ExcludedSources {
			if e.Reason == "stale" {
				sawStale = true
			}
		}
		if len(msg.CrossVenueGroups) > 0 || !sawStale {
			continue // still the pre-silence matrix, or the lone-source transient
		}
		if len(msg.ExcludedSources) != 2 {
			t.Errorf("excluded %d sources, want both", len(msg.ExcludedSources))
		}
		for _, e := range msg.ExcludedSources {
			if e.Reason != "stale" {
				t.Errorf("reason = %q, want stale", e.Reason)
			}
		}
		return
	}
	t.Fatal("the matrix was never re-examined; a silent symbol keeps its frozen matrix on screen")
}

// A registered source with its threshold left unset must not be treated as
// having a zero threshold, which marks it stale the instant it arrives.
func TestStaleAfter_UnsetThresholdFallsBackToTheDefault(t *testing.T) {
	for _, meta := range sourceRegistry {
		if got := staleAfter(meta.Source); got <= 0 {
			t.Errorf("%s: staleAfter = %v, want a positive threshold", meta.Source, got)
		}
	}
}

// Thresholds are per venue on purpose: these feeds are change-driven, so a
// slower venue in a quiet market produces long gaps with nothing wrong. A single
// global threshold would mark it dead. The values are measured - see the comment
// on sourceRegistry.
func TestStaleAfter_SlowVenuesGetMoreHeadroom(t *testing.T) {
	if staleAfter("hyperliquid_futures") <= staleAfter("binance_futures") {
		t.Errorf("hyperliquid (%v) must tolerate longer silence than binance (%v): its measured worst gap is seven times larger",
			staleAfter("hyperliquid_futures"), staleAfter("binance_futures"))
	}
	for _, source := range []string{"binance_spot", "bybit_spot", "gate_futures"} {
		if staleAfter(source) <= staleAfter("binance_futures") {
			t.Errorf("%s threshold %v should exceed the busiest venue's %v",
				source, staleAfter(source), staleAfter("binance_futures"))
		}
	}
}

// A trade arriving proves the socket is alive even when the order book has not
// moved. Without it, a healthy venue on a change-driven feed in a quiet market
// is reported disconnected and dropped from every comparison.
func TestProcessTrades_KeepsAQuietVenueMarkedConnected(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})
	base := time.Now()
	scanner.now = func() time.Time { return base }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scanner.processTrades(ctx)

	// A price arrives, then the book goes quiet for longer than the threshold
	// while trades keep coming.
	scanner.updatePrice(mustPriceData("BTCUSDT", "gate_futures", 65000))

	quiet := base.Add(staleAfter("gate_futures") + time.Second)
	scanner.now = func() time.Time { return quiet }
	scanner.tradeChan <- exchanges.TradeData{Symbol: "BTCUSDT", Source: "gate_futures", Price: 65000}

	// sourceLastMsgAt has its own mutex; snapshotLastMsgAt is the only safe
	// way to read it from another goroutine.
	deadline := time.Now().Add(2 * time.Second)
	var lastMsgAt time.Time
	for time.Now().Before(deadline) {
		lastMsgAt = scanner.snapshotLastMsgAt()["gate_futures"]
		if lastMsgAt.Equal(quiet) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if got := sourceState(lastMsgAt, disconnectAfter("gate_futures"), scanner.startedAt, quiet); got != stateConnected {
		t.Errorf("state = %q, want connected: trades were still arriving", got)
	}

	// The price itself is still correctly stale - the venue is alive, this
	// particular quote is not fresh.
	scanner.pricesMutex.RLock()
	point := scanner.prices["BTCUSDT"]["gate_futures"]
	scanner.pricesMutex.RUnlock()
	if got := priceStatus(point, staleAfter("gate_futures"), quiet); got != statusStale {
		t.Errorf("price status = %q, want stale: the book has not moved", got)
	}
}

// "This quote is too old to compare" and "this venue is gone" are different
// claims. Sharing one threshold paints a healthy but quiet venue as dead, and
// four of the venues send no trades at all, so a quiet book is their only signal.
func TestDisconnectAfter_IsMuchLongerThanPriceStaleness(t *testing.T) {
	for _, meta := range sourceRegistry {
		stale := staleAfter(meta.Source)
		gone := disconnectAfter(meta.Source)
		if gone <= stale {
			t.Errorf("%s: disconnectAfter %v must exceed staleAfter %v", meta.Source, gone, stale)
		}
		if gone < minDisconnectAfter {
			t.Errorf("%s: disconnectAfter %v is below the %v floor", meta.Source, gone, minDisconnectAfter)
		}
	}

	// A venue that is quiet for longer than its price threshold is still
	// connected: its last quote is stale, the venue is not gone.
	now := time.Now()
	quiet := now.Add(-staleAfter("gate_futures") - time.Second)
	if got := sourceState(quiet, disconnectAfter("gate_futures"), now.Add(-time.Hour), now); got != stateConnected {
		t.Errorf("state = %q, want connected: quiet for slightly longer than the price threshold is not death", got)
	}
}

// A venue that has never delivered must not be called dead sooner than one that
// delivered and then stopped: the startup clock begins before the connectors
// have even dialled.
func TestStartupGrace_IsNotHarsherThanTheDisconnectThreshold(t *testing.T) {
	if startupGrace < minDisconnectAfter {
		t.Errorf("startupGrace %v is shorter than minDisconnectAfter %v: a venue that never delivered would be badged dead before one that died",
			startupGrace, minDisconnectAfter)
	}
	for _, meta := range sourceRegistry {
		if startupGrace < disconnectAfter(meta.Source) {
			t.Errorf("%s: startupGrace %v is shorter than its disconnect threshold %v",
				meta.Source, startupGrace, disconnectAfter(meta.Source))
		}
	}
}

// The staleness timer must publish only when the usable set actually changed.
// In steady state that is never, and republishing an identical matrix every
// second only adds contention on the write mutex ingestion also holds.
func TestRefreshStaleness_DoesNotRepublishAnUnchangedMatrix(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})
	base := time.Now()
	scanner.now = func() time.Time { return base }

	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))

	usable := map[string]float64{"binance_futures": 65000, "bybit_futures": 66000}
	if scanner.usableSetChanged("BTCUSDT", usable) {
		t.Error("the set is unchanged since updatePrice published it, so the timer must skip")
	}
	if scanner.usableSetChanged("BTCUSDT", usable) {
		t.Error("a second identical check must still report unchanged")
	}

	// One venue goes stale: that IS a change and has to reach the dashboard.
	shrunk := map[string]float64{"bybit_futures": 66000}
	if !scanner.usableSetChanged("BTCUSDT", shrunk) {
		t.Error("a source dropping out must be published")
	}
	if !scanner.usableSetChanged("BTCUSDT", usable) {
		t.Error("a source coming back must be published")
	}
}
