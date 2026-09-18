// The Engine-2 pilot: the funding-spread signal and the periodic exit loop
// (PLAN 4.5k step 4, item 3).
//
// # What it decides on, and why not the radar
//
// The design names /api/cross-radar as Engine 2's signal source. That radar is
// cmd/scanner's, and it reads MAINNET public data: it ranks what a cross-venue
// funding trade would pay in the real market. The orders here go to two
// TESTNETS, whose funding is their own and is not the mainnet's — pricing a
// testnet trade on mainnet funding would measure nothing about either. So the
// pilot follows the precedent decision Q18 set for the Binance auto-trader and
// reads the signal from THE VENUES IT TRADES ON: each venue's own settled
// funding history, each venue's own book, and this account's own taker fee at
// each venue. The mainnet radar stays on the page beside it as context, relayed
// read-only, and nothing here reads it.
//
// # Rule 3 and rule 6, which this file is mostly about
//
// The two venues do not settle on the same clock — Binance runs 8h, 4h or 1h
// per symbol and moves symbols between them, Bybit publishes its own interval —
// so a spread is NOT the difference of two published rates. Each venue's rate is
// annualized on ITS OWN cadence, MEASURED as the modal spacing of its settled
// stamps (never assumed, never derived from a documented default), and only the
// two annual figures are subtracted. A venue whose cadence cannot be measured
// produces no signal rather than a number on a guessed interval.
//
// # What it never does
//
// It never squares anything, never releases a lock and never resolves
// conflicting evidence. Decision Q21 is explicit: when the fills and the venue's
// position disagree, no reduce-only "probe" is sent — the pilot HALTS, the alarm
// stands and the lock is kept until a person looks. Anything the engine reports
// as unresolved, close-blocked, or carrying an order it could not prove finished
// stops the pilot for every symbol, not just that one: the same account is on
// the other side of each pair.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/execution/crossperp"
)

// crossPilotConfig is the pilot's rule set. Every threshold is a FRACTION and
// says so in its name (rule 4); every duration is a Duration.
type crossPilotConfig struct {
	// Enabled is the operator's -crossperp-pilot. False makes every scan an
	// ADVISORY one: the signal is computed and shown, and no order is sent.
	Enabled bool

	ScanEvery     time.Duration
	NotionalQuote float64

	// FundingLookback is how far back the settled history is read. It must cover
	// enough settlements for the cadence to be MEASURED rather than assumed, at
	// the slowest cadence a venue runs.
	FundingLookback time.Duration

	// PlannedHoldDays is what the entry figure is spread over. It is an
	// ASSUMPTION and travels into the basis text, because an APR quoted over a
	// hold nobody committed to is the trap PLAN 4.5i records.
	PlannedHoldDays float64

	// MinAfterCostAPROnCapitalFrac is the entry floor: the funding spread, over
	// the planned hold, must repay the round trip and clear this much on the
	// capital BOTH levered legs tie up.
	MinAfterCostAPROnCapitalFrac float64

	// MinHoldEpochs locks the SPREAD exit for the first N settlements of the
	// slower venue, so a pair born owing a whole round trip is not closed to
	// dodge one small charge (the 4.5f finding, carried over).
	MinHoldEpochs int

	// ExitSpreadAPROnCapitalFrac is where a held pair's spread has fallen far
	// enough to leave. Zero is the design's "spread reversed".
	ExitSpreadAPROnCapitalFrac float64

	// TakeProfitOnCapitalFrac and StopLossOnCapitalFrac act on an ESTIMATE of
	// the pair's own result (crossHolding), never on a figure read from an
	// account statement.
	TakeProfitOnCapitalFrac float64
	StopLossOnCapitalFrac   float64

	// MaxPairs bounds how many Engine-2 pairs may be open at once.
	MaxPairs int

	// PerpMarginFrac is the collateral posted on ONE leg as a fraction of its
	// notional — a DECISION, and the denominator of every figure here.
	PerpMarginFrac float64
}

func defaultCrossPilotConfig() crossPilotConfig {
	return crossPilotConfig{
		Enabled:         false,
		ScanEvery:       60 * time.Second,
		NotionalQuote:   100,
		FundingLookback: 7 * 24 * time.Hour,
		PlannedHoldDays: 7,
		// A perp–perp pair on this testnet pays 5.5 bps taker at Bybit and 4 bps
		// at Binance, so a round trip is ~19 bps before spreads. Over a 7-day
		// hold at K=1 that alone is ~10%/yr, which is why the floor is not a
		// small number: anything under it is a trade that pays the venues.
		MinAfterCostAPROnCapitalFrac: 0.15,
		MinHoldEpochs:                3,
		ExitSpreadAPROnCapitalFrac:   0,
		TakeProfitOnCapitalFrac:      0.02,
		StopLossOnCapitalFrac:        0.03,
		MaxPairs:                     2,
		PerpMarginFrac:               0.50,
	}
}

// crossVenueFunding is one venue's funding as the pilot measured it — never as
// a document describes it.
type crossVenueFunding struct {
	Venue string

	// SettledRateFrac is the newest SETTLED rate, a fraction per settlement.
	SettledRateFrac float64
	SettledAtMs     int64

	// MeanRateFrac is the mean settled rate over the lookback, which is what the
	// entry is priced on: one print is noise.
	MeanRateFrac float64
	Settlements  int

	// IntervalSec is the MEASURED modal spacing of the settled stamps.
	IntervalSec int64

	// SettlementsPerYear follows from IntervalSec and from nothing else.
	SettlementsPerYear float64

	// AnnualRateFrac is MeanRateFrac × SettlementsPerYear.
	AnnualRateFrac float64

	NextFundingTimeMs int64
	TakerFeeFrac      float64
	MidPriceQuote     float64
	TouchSpreadFrac   float64

	ProblemVI string
}

func (f crossVenueFunding) ok() bool { return f.ProblemVI == "" }

// crossSignal is one symbol's verdict for one direction.
type crossSignal struct {
	Symbol     string `json:"symbol"`
	LongVenue  string `json:"long_venue"`
	ShortVenue string `json:"short_venue"`

	// AnnualSpreadFrac is (short venue's annual rate) − (long venue's annual
	// rate): what the pair collects in a year, as a fraction of ONE leg's
	// notional, before any cost. GROSS (rule 2).
	AnnualSpreadFrac float64 `json:"annual_spread_frac"`

	RoundTripCostFrac float64 `json:"round_trip_cost_frac"`

	// AfterCostAPROnCapitalFrac is the entry figure. It is NOT called "net":
	// that word belongs to internal/strategy, and five costs are excluded by
	// name in BasisVI.
	AfterCostAPROnCapitalFrac float64 `json:"after_cost_apr_on_capital_frac"`
	BasisVI                   string  `json:"basis_vi"`

	CapitalPerNotionalFrac float64 `json:"capital_per_notional_frac"`
	BreakevenDays          float64 `json:"breakeven_days"`

	Eligible  bool   `json:"eligible"`
	WhyNotVI  string `json:"why_not_vi,omitempty"`
	LockState string `json:"lock_state"`

	Venues []crossVenueFunding `json:"-"`
}

// crossPilotView is what the page shows about the pilot.
type crossPilotView struct {
	Enabled    bool   `json:"enabled"`
	Halted     bool   `json:"halted"`
	HaltedVI   string `json:"halted_vi,omitempty"`
	ScanEveryS int64  `json:"scan_every_s"`

	LastScanAtMs int64 `json:"last_scan_at_ms"`
	Scans        int   `json:"scans"`

	NotionalQuote                float64 `json:"notional_quote"`
	MinAfterCostAPROnCapitalFrac float64 `json:"min_after_cost_apr_on_capital_frac"`
	PlannedHoldDays              float64 `json:"planned_hold_days"`
	MaxPairs                     int     `json:"max_pairs"`

	// AdvisoryOnly is true whenever the pilot computes and shows but may not
	// send: the shipped default, and the state the acceptance ran in.
	AdvisoryOnly bool `json:"advisory_only"`

	Signals  []crossSignal `json:"signals"`
	NotesVI  []string      `json:"notes_vi,omitempty"`
	ReadAtMs int64         `json:"read_at_ms"`
}

// crossPilot is the loop. One goroutine drives it; everything a reader sees is
// behind its mutex.
type crossPilot struct {
	desk *crossDesk
	cfg  crossPilotConfig

	mu           sync.Mutex
	halted       bool
	haltedVI     string
	lastScanAtMs int64
	scans        int
	signals      []crossSignal
	notesVI      []string

	// fees are this account's taker rate per venue. They move rarely and cost a
	// signed call each, so they are read once per venue and kept.
	feeMu sync.Mutex
	fees  map[string]float64
}

func newCrossPilot(desk *crossDesk, cfg crossPilotConfig) *crossPilot {
	return &crossPilot{desk: desk, cfg: cfg, fees: map[string]float64{}}
}

// pilotView is the desk's accessor, so the handlers never reach past it.
func (d *crossDesk) pilotView() *crossPilotView {
	if d.pilot == nil {
		return nil
	}
	return d.pilot.view()
}

func (p *crossPilot) view() *crossPilotView {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := crossPilotView{
		Enabled: p.cfg.Enabled, Halted: p.halted, HaltedVI: p.haltedVI,
		ScanEveryS:   int64(p.cfg.ScanEvery / time.Second),
		LastScanAtMs: p.lastScanAtMs, Scans: p.scans,
		NotionalQuote: p.cfg.NotionalQuote, MinAfterCostAPROnCapitalFrac: p.cfg.MinAfterCostAPROnCapitalFrac,
		PlannedHoldDays: p.cfg.PlannedHoldDays, MaxPairs: p.cfg.MaxPairs,
		AdvisoryOnly: !p.cfg.Enabled,
		Signals:      append([]crossSignal(nil), p.signals...),
		NotesVI:      append([]string(nil), p.notesVI...),
		ReadAtMs:     p.desk.now().UnixMilli(),
	}
	return &v
}

// Run scans until the context ends. A scan that cannot read a venue produces no
// signal for that symbol and no order; it does not halt the pilot, because an
// unreadable venue is the normal weather of a network.
func (p *crossPilot) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.ScanEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.scan(ctx)
		}
	}
}

// scan is one pass: read the venues, judge every symbol, then act on at most one
// thing — an exit before an entry, because leaving is what protects capital.
func (p *crossPilot) scan(ctx context.Context) {
	signals, notes := p.measure(ctx)

	p.mu.Lock()
	p.signals, p.notesVI = signals, notes
	p.lastScanAtMs = p.desk.now().UnixMilli()
	p.scans++
	halted := p.halted
	p.mu.Unlock()

	// Q21: anything the engine could not resolve stops every automatic action,
	// on every symbol. The same two accounts are behind each pair, so a pair
	// whose true size is unknown makes every OTHER pair's sizing unproven too.
	if whyVI := p.stopReasonVI(); whyVI != "" {
		p.halt(whyVI)
		return
	}
	if halted || !p.cfg.Enabled {
		return
	}
	if p.exitOne(ctx, signals) {
		return
	}
	p.enterOne(ctx, signals)
}

// stopReasonVI names the first piece of evidence that must stop automation. It
// reads the engine's own record, which is built from the venues, and the lock
// table; the JUDGEMENT itself is crossStopReasonVI, which takes values so the
// rule can be tested without having to manufacture each state on a fake venue.
func (p *crossPilot) stopReasonVI() string {
	return crossStopReasonVI(p.desk.engine2.Pairs(), p.desk.engine2.Held(), p.desk.coord.ListLocks())
}

func crossStopReasonVI(pairs []crossperp.Pair, held map[string]error, locks []coordinator.SymbolLock) string {
	for _, pair := range pairs {
		switch {
		case pair.CloseBlockedVI != "":
			return fmt.Sprintf("cặp %s BỊ CHẶN ĐÓNG vì bằng chứng mâu thuẫn: %s — Q21: KHÔNG bắn lệnh thăm dò, giữ khóa, chờ người vận hành",
				pair.Symbol, pair.CloseBlockedVI)
		case pair.Unresolved:
			return fmt.Sprintf("cặp %s CHƯA GIẢI QUYẾT — hai chân có thể không còn là phòng hộ; Q21: dừng tự động, giữ khóa", pair.Symbol)
		case pair.OpeningOrdersUnproven:
			return fmt.Sprintf("cặp %s còn lệnh MỞ chưa chứng minh được là kết thúc — nó vẫn có thể khớp; dừng tự động", pair.Symbol)
		case len(pair.PendingOrders) > 0:
			return fmt.Sprintf("cặp %s còn %d lệnh thu nhỏ chưa chứng minh được là kết thúc — chúng sẽ giảm BẤT KỲ vị thế nào có trên symbol khi khớp; dừng tự động",
				pair.Symbol, len(pair.PendingOrders))
		}
	}
	for symbol, err := range held {
		return fmt.Sprintf("khóa %s vẫn đang bị GIỮ sau khi nhả hỏng (%v) — dừng tự động cho tới khi nhả được", symbol, err)
	}
	for _, lock := range locks {
		if lock.State == coordinator.StateConflict {
			return fmt.Sprintf("khóa %s ở trạng thái XUNG ĐỘT BẰNG CHỨNG: %s — dừng tự động", lock.Symbol, lock.EvidenceVI)
		}
	}
	return ""
}

func (p *crossPilot) halt(whyVI string) {
	p.mu.Lock()
	first := !p.halted
	p.halted, p.haltedVI = true, whyVI
	p.mu.Unlock()
	if first {
		log.Printf("execportal/crossperp: PHI CÔNG DỪNG — %s", whyVI)
	}
}

// Resume is a person saying they have looked. It clears nothing about the
// evidence; the next scan re-reads it and halts again if it is still there.
func (p *crossPilot) Resume() {
	p.mu.Lock()
	p.halted, p.haltedVI = false, ""
	p.mu.Unlock()
}

// ------------------------------------------------------------------ measure

// measure reads both venues for every symbol and builds one signal each, in the
// direction the spread actually pays.
func (p *crossPilot) measure(ctx context.Context) ([]crossSignal, []string) {
	var (
		signals []crossSignal
		notes   []string
	)
	for _, symbol := range p.desk.symbols {
		readings := make([]crossVenueFunding, 0, len(p.desk.venues.list))
		for _, v := range p.desk.venues.list {
			readings = append(readings, p.readVenue(ctx, v, symbol))
		}
		if len(readings) != 2 {
			continue
		}
		a, b := readings[0], readings[1]
		if !a.ok() || !b.ok() {
			notes = append(notes, fmt.Sprintf("%s: chưa có tín hiệu — %s", symbol,
				strings.TrimSpace(a.ProblemVI+" "+b.ProblemVI)))
			continue
		}
		// The pair is LONG where funding is lower and SHORT where it is higher:
		// the short leg collects, the long leg pays, so the spread is positive
		// in exactly one direction and there is nothing to choose.
		long, short := a, b
		if a.AnnualRateFrac > b.AnnualRateFrac {
			long, short = b, a
		}
		signals = append(signals, p.judge(symbol, long, short))
	}
	sort.Slice(signals, func(i, j int) bool {
		return signals[i].AfterCostAPROnCapitalFrac > signals[j].AfterCostAPROnCapitalFrac
	})
	return signals, notes
}

func (p *crossPilot) readVenue(ctx context.Context, v crossVenue, symbol string) crossVenueFunding {
	out := crossVenueFunding{Venue: v.Name}

	mark, err := v.Perp.MarkPrice(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		out.ProblemVI = fmt.Sprintf("%s: không đọc được mark/funding: %v", v.Name, err)
		return out
	}
	out.NextFundingTimeMs = mark.NextFundingTimeMs

	endMs := p.desk.now().UnixMilli()
	startMs := endMs - p.cfg.FundingLookback.Milliseconds()
	history, err := v.Perp.FundingRateHistory(ctx, symbol, startMs, endMs)
	if err != nil {
		out.ProblemVI = fmt.Sprintf("%s: không đọc được lịch sử funding đã settle: %v", v.Name, err)
		return out
	}
	measured, problemVI := measureFunding(history)
	if problemVI != "" {
		out.ProblemVI = v.Name + ": " + problemVI
		return out
	}
	out.SettledRateFrac, out.SettledAtMs = measured.newestRateFrac, measured.newestAtMs
	out.MeanRateFrac, out.Settlements, out.IntervalSec = measured.meanRateFrac, measured.count, measured.intervalSec
	out.SettlementsPerYear = float64(365*24*3600) / float64(measured.intervalSec)
	out.AnnualRateFrac = out.MeanRateFrac * out.SettlementsPerYear

	book, err := crossReadBook(ctx, v.Perp, symbol)
	if err != nil {
		out.ProblemVI = fmt.Sprintf("%s: không đọc được sổ lệnh: %v", v.Name, err)
		return out
	}
	out.MidPriceQuote = book.MidPriceQuote
	if book.BestBidQuote > 0 && book.BestAskQuote > 0 && book.MidPriceQuote > 0 {
		out.TouchSpreadFrac = (book.BestAskQuote - book.BestBidQuote) / book.MidPriceQuote
	}

	fee, err := p.takerFeeFrac(ctx, v, symbol)
	if err != nil {
		out.ProblemVI = fmt.Sprintf("%s: không đọc được phí taker của tài khoản: %v", v.Name, err)
		return out
	}
	out.TakerFeeFrac = fee
	return out
}

func (p *crossPilot) takerFeeFrac(ctx context.Context, v crossVenue, symbol string) (float64, error) {
	key := v.Name + "|" + symbol
	p.feeMu.Lock()
	if f, ok := p.fees[key]; ok {
		p.feeMu.Unlock()
		return f, nil
	}
	p.feeMu.Unlock()

	rates, err := v.Perp.CommissionRates(ctx, symbol)
	if err != nil {
		return 0, err
	}
	// One leg of a pair is bought and the other sold, and both are reversed on
	// the way out, so every pair pays both sides at both venues. The HIGHER of
	// the two is taken for each venue: a rate that differs by side makes the
	// cheaper one an under-statement, and under-stating a cost is the direction
	// that opens a trade which does not pay.
	taker := math.Max(rates.TakerBuyFrac, rates.TakerSellFrac)
	p.feeMu.Lock()
	p.fees[key] = taker
	p.feeMu.Unlock()
	return taker, nil
}

type measuredFunding struct {
	newestRateFrac float64
	newestAtMs     int64
	meanRateFrac   float64
	count          int
	intervalSec    int64
}

// minFundingSettlements is how many settled stamps the cadence is measured over
// before the pilot will price anything. Two stamps give one gap and no mode.
const minFundingSettlements = 4

// measureFunding reads the cadence off the STAMPS (rule 3: no interval is ever
// hardcoded, and no venue publishes one beside a historical rate) and the level
// off the rates. It refuses rather than guessing.
func measureFunding(history []binancebroker.FundingRate) (measuredFunding, string) {
	var out measuredFunding
	if len(history) < minFundingSettlements {
		return out, fmt.Sprintf("chỉ có %d mốc funding đã settle trong cửa sổ, cần ít nhất %d để ĐO được nhịp settle",
			len(history), minFundingSettlements)
	}
	rows := append([]binancebroker.FundingRate(nil), history...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].SettledAtMs < rows[j].SettledAtMs })

	gaps := map[int64]int{}
	for i := 1; i < len(rows); i++ {
		gapSec := (rows[i].SettledAtMs - rows[i-1].SettledAtMs) / 1000
		if gapSec <= 0 {
			continue
		}
		// Venue stamps carry seconds of jitter (Gate lands 1–3s past the hour,
		// Hyperliquid tens of ms), so gaps are bucketed to the minute before the
		// mode is taken; the bucket is the unit the cadence is really quoted in.
		gaps[(gapSec+30)/60*60]++
	}
	var modeSec int64
	var modeCount int
	for gapSec, n := range gaps {
		if n > modeCount || (n == modeCount && gapSec < modeSec) {
			modeSec, modeCount = gapSec, n
		}
	}
	if modeSec <= 0 {
		return out, "không đo được nhịp settle từ các mốc thời gian"
	}
	var sum float64
	for _, r := range rows {
		sum += r.RatePerIntervalFrac
	}
	out.newestRateFrac, out.newestAtMs = rows[len(rows)-1].RatePerIntervalFrac, rows[len(rows)-1].SettledAtMs
	out.meanRateFrac = sum / float64(len(rows))
	out.count, out.intervalSec = len(rows), modeSec
	return out, ""
}

// judge prices one direction and says whether it clears the floor.
func (p *crossPilot) judge(symbol string, long, short crossVenueFunding) crossSignal {
	s := crossSignal{Symbol: symbol, LongVenue: long.Venue, ShortVenue: short.Venue,
		AnnualSpreadFrac: short.AnnualRateFrac - long.AnnualRateFrac}

	// Four taker fills and four half-spreads: open both legs, close both legs.
	s.RoundTripCostFrac = 2*(long.TakerFeeFrac+short.TakerFeeFrac) +
		2*(long.TouchSpreadFrac/2+short.TouchSpreadFrac/2)

	// Both legs are levered perps, so the capital is the collateral on each.
	// Engine 1's spot leg cannot be levered and ties up its whole notional;
	// this is the one structural advantage Engine 2 has, and it belongs in the
	// denominator rather than in a sentence.
	s.CapitalPerNotionalFrac = 2 * p.cfg.PerpMarginFrac

	holdYears := p.cfg.PlannedHoldDays / 365
	if holdYears <= 0 || s.CapitalPerNotionalFrac <= 0 {
		s.WhyNotVI = "cấu hình phi công không hợp lệ (thời gian giữ hoặc ký quỹ ≤ 0)"
		return s
	}
	overHold := s.AnnualSpreadFrac*holdYears - s.RoundTripCostFrac
	s.AfterCostAPROnCapitalFrac = overHold / holdYears / s.CapitalPerNotionalFrac
	if s.AnnualSpreadFrac > 0 {
		s.BreakevenDays = s.RoundTripCostFrac / s.AnnualSpreadFrac * 365
	} else {
		s.BreakevenDays = math.Inf(1)
	}

	s.BasisVI = fmt.Sprintf(
		"ĐÃ TRỪ: taker 4 lượt khớp (%s %.2f bps, %s %.2f bps) và cả hai chênh chạm (%.2f + %.2f bps), "+
			"trải trên %.0f ngày GIẢ ĐỊNH giữ lệnh, chia cho vốn %.0f%% notional (hai chân đều đòn bẩy). "+
			"CHƯA TRỪ: trượt giá ngoài chạm, funding đổi trong lúc giữ, sổ lệnh lúc THOÁT, phí chuyển vốn giữa hai sàn, "+
			"rủi ro thanh lý, và chênh lệch giá hai sàn khi vào so với khi ra. "+
			"Nhịp settle ĐO ĐƯỢC: %s %ds (%d mốc), %s %ds (%d mốc).",
		long.Venue, long.TakerFeeFrac*10_000, short.Venue, short.TakerFeeFrac*10_000,
		long.TouchSpreadFrac*10_000, short.TouchSpreadFrac*10_000,
		p.cfg.PlannedHoldDays, s.CapitalPerNotionalFrac*100,
		long.Venue, long.IntervalSec, long.Settlements, short.Venue, short.IntervalSec, short.Settlements)

	lock, held := p.desk.coord.QueryLock(symbol)
	s.LockState = string(coordinator.StateIdle)
	if held {
		s.LockState = string(lock.State)
	}
	switch {
	case s.AfterCostAPROnCapitalFrac < p.cfg.MinAfterCostAPROnCapitalFrac:
		s.WhyNotVI = fmt.Sprintf("%.2f%%/năm trên vốn sau chi phí, dưới sàn %.2f%%",
			s.AfterCostAPROnCapitalFrac*100, p.cfg.MinAfterCostAPROnCapitalFrac*100)
	case held && lock.State != coordinator.StateIdle:
		s.WhyNotVI = fmt.Sprintf("cặp đang %s (chủ %s)", lock.State, lock.OwnerEngine)
	default:
		s.Eligible = true
	}
	s.Venues = []crossVenueFunding{long, short}
	return s
}

// -------------------------------------------------------------------- acting

// exitOne closes at most ONE pair per scan and reports whether it did. One at a
// time, because each close is a sequence of reduce-only orders on two venues and
// two of them at once would compete for the same request budget.
func (p *crossPilot) exitOne(ctx context.Context, signals []crossSignal) bool {
	for _, pair := range p.desk.engine2.Pairs() {
		reasonVI, firstVenue := p.exitReasonVI(pair, signals)
		if reasonVI == "" {
			continue
		}
		log.Printf("execportal/crossperp: PHI CÔNG ĐÓNG %s — %s", pair.Symbol, reasonVI)
		view, _ := p.desk.closePair(ctx, crossCloseRequest{
			Symbol: pair.Symbol, FirstVenue: firstVenue, ReasonVI: reasonVI})
		if view.Alarm || view.ErrorVI != "" {
			p.halt(fmt.Sprintf("đóng %s không sạch (%s): %s — dừng tự động", pair.Symbol, view.Outcome, view.ErrorVI))
		}
		return true
	}
	return false
}

// exitReasonVI is the whole exit rule set, in the order the rules bind.
func (p *crossPilot) exitReasonVI(pair crossperp.Pair, signals []crossSignal) (reasonVI, firstVenue string) {
	var sig *crossSignal
	for i := range signals {
		if signals[i].Symbol == pair.Symbol {
			sig = &signals[i]
			break
		}
	}
	if sig == nil {
		// No reading this scan is not a reason to close: a close needs two
		// venues answering, and if they are not answering it would fail anyway.
		return "", ""
	}
	// The stressed venue closes first, exactly as the margin guard would name
	// it. Without a margin reading the larger leg goes first (empty string).
	firstVenue = p.stressedVenue()

	held := time.Duration(p.desk.now().UnixMilli()-pair.OpenedAtMs) * time.Millisecond
	epochs := 0
	if len(sig.Venues) == 2 {
		slower := max(sig.Venues[0].IntervalSec, sig.Venues[1].IntervalSec)
		if slower > 0 {
			epochs = int(held.Seconds() / float64(slower))
		}
	}

	switch {
	case sig.AfterCostAPROnCapitalFrac <= p.cfg.ExitSpreadAPROnCapitalFrac && epochs >= p.cfg.MinHoldEpochs:
		return fmt.Sprintf("chênh funding đã về %.2f%%/năm trên vốn sau chi phí (≤ %.2f%%), đã giữ %d kỳ settle",
			sig.AfterCostAPROnCapitalFrac*100, p.cfg.ExitSpreadAPROnCapitalFrac*100, epochs), firstVenue
	case sig.AfterCostAPROnCapitalFrac <= p.cfg.ExitSpreadAPROnCapitalFrac:
		// Below the floor but inside the amortization window: say so, hold on.
		return "", ""
	}
	return "", ""
}

// stressedVenue is the venue with the HIGHER maintenance ratio right now, which
// is the one a close should relieve first. An unread venue names nobody.
func (p *crossPilot) stressedVenue() string {
	snap := p.desk.marginGuard.Snapshot()
	best, bestRatio := "", math.Inf(-1)
	for _, v := range snap.Venues {
		if !v.HasReading {
			continue
		}
		if v.RatioFrac > bestRatio {
			best, bestRatio = v.Venue, v.RatioFrac
		}
	}
	return best
}

// enterOne opens at most ONE pair per scan, the best-ranked eligible one.
func (p *crossPilot) enterOne(ctx context.Context, signals []crossSignal) {
	if len(p.desk.engine2.Pairs()) >= p.cfg.MaxPairs {
		return
	}
	for _, sig := range signals {
		if !sig.Eligible {
			continue
		}
		intentID, err := p.desk.mintIntentID(sig.Symbol)
		if err != nil {
			log.Printf("execportal/crossperp: PHI CÔNG không sinh được id ý định cho %s: %v", sig.Symbol, err)
			return
		}
		log.Printf("execportal/crossperp: PHI CÔNG MỞ %s long=%s short=%s — %.2f%%/năm trên vốn sau chi phí",
			sig.Symbol, sig.LongVenue, sig.ShortVenue, sig.AfterCostAPROnCapitalFrac*100)
		view, _ := p.desk.openPair(ctx, crossOpenRequest{
			Symbol: sig.Symbol, LongVenue: sig.LongVenue, ShortVenue: sig.ShortVenue,
			NotionalQuote:            p.cfg.NotionalQuote,
			ExpectedAPROnCapitalFrac: sig.AfterCostAPROnCapitalFrac,
			ExpectedAPRBasisVI:       sig.BasisVI,
		}, intentID)
		if view.Alarm {
			p.halt(fmt.Sprintf("mở %s kết thúc có báo động (%s): %s — dừng tự động", sig.Symbol, view.Outcome, view.ErrorVI))
		}
		return
	}
}

// mintIntentID gives the pilot its own ids under the Engine-2 prefix. It is on
// the desk rather than on the pilot so the one id source is shared with the
// page's button (portal.mintIntentIDWith).
func (d *crossDesk) mintIntentID(symbol string) (string, error) {
	if d.mintID == nil {
		return "", errors.New("bàn Động cơ 2 chưa có nguồn sinh id ý định")
	}
	return d.mintID(crossIntentPrefix, symbol)
}
