// Master Command Center — Unified Gateway Hub: gom toàn bộ trạng thái vốn,
// van ký quỹ kép, vị thế của cả Động cơ 1 và Động cơ 2, bảng khóa cặp rảnh rỗi,
// và các kết nối relay về chung một giao diện điều khiển trung tâm.
package main

import (
	"context"
	"net/http"
	"sort"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/coordinator"
)

type masterOverview struct {
	Mode           string            `json:"mode"` // "master"
	ReadAtMs       int64             `json:"read_at_ms"`
	TotalEquityUSD float64           `json:"total_equity_usd"`
	Balances       masterBalances    `json:"balances"`
	Margin         masterMargin      `json:"margin"`
	Engine1        masterEngine1     `json:"engine1"`
	Engine2        masterEngine2     `json:"engine2"`
	Coordinator    masterCoordinator `json:"coordinator"`
	Relays         masterRelays      `json:"relays"`
}

type masterBalances struct {
	BinanceSpotUSDT    float64 `json:"binance_spot_usdt"`
	BinanceFuturesUSDT float64 `json:"binance_futures_usdt"`
	BinanceTotalUSDT   float64 `json:"binance_total_usdt"`
	BybitEquityUSD     float64 `json:"bybit_equity_usd"`
	BybitAvailableUSD  float64 `json:"bybit_available_usd"`
}

type masterMargin struct {
	BinanceTier   string  `json:"binance_tier"` // "green", "yellow", "orange", "red", "unknown"
	BinanceMMRPct float64 `json:"binance_mmr_pct"`
	BybitTier     string  `json:"bybit_tier"`
	BybitMMRPct   float64 `json:"bybit_mmr_pct"`
	Emergency     bool    `json:"emergency"`
	EmergencySeq  uint64  `json:"emergency_seq"`
	YellowFrac    float64 `json:"yellow_frac"`
	OrangeFrac    float64 `json:"orange_frac"`
	RedFrac       float64 `json:"red_frac"`
}

type masterEngine1 struct {
	Enabled           bool    `json:"enabled"`
	Strategy          string  `json:"strategy"`
	AutotradeRunning  bool    `json:"autotrade_running"`
	ActivePairsCount  int     `json:"active_pairs_count"`
	TotalPnLQuote     float64 `json:"total_pnl_quote"`
	BinancePairsCount int     `json:"binance_pairs_count"`
	BinancePnLQuote   float64 `json:"binance_pnl_quote"`
	BybitPairsCount   int     `json:"bybit_pairs_count"`
	BybitPnLQuote     float64 `json:"bybit_pnl_quote"`
}

type masterEngine2 struct {
	Enabled          bool    `json:"enabled"`
	Strategy         string  `json:"strategy"`
	PilotRunning     bool    `json:"pilot_running"`
	PilotMode        string  `json:"pilot_mode"` // "advisory" | "active"
	ActivePairsCount int     `json:"active_pairs_count"`
	TotalPnLQuote    float64 `json:"total_pnl_quote"`
}

type masterCoordinator struct {
	TotalSymbols    int                      `json:"total_symbols"`
	IdleCount       int                      `json:"idle_count"`
	OccupiedE1Count int                      `json:"occupied_e1_count"`
	OccupiedE2Count int                      `json:"occupied_e2_count"`
	ConflictCount   int                      `json:"conflict_count"`
	Locks           []coordinator.SymbolLock `json:"locks"`
}

type masterRelays struct {
	ScannerConnected     bool `json:"scanner_connected"`
	PaperLedgerConnected bool `json:"paper_ledger_connected"`
}

type masterPositionItem struct {
	ID               string  `json:"id"`
	Symbol           string  `json:"symbol"`
	EngineID         string  `json:"engine_id"`
	EngineTitle      string  `json:"engine_title"`
	Strategy         string  `json:"strategy"`
	LongLeg          string  `json:"long_leg"`
	ShortLeg         string  `json:"short_leg"`
	QtyCoin          float64 `json:"qty_coin"`
	NotionalUSD      float64 `json:"notional_usd"`
	EntryPriceLong   float64 `json:"entry_price_long"`
	EntryPriceShort  float64 `json:"entry_price_short"`
	UnrealizedPnLUSD float64 `json:"unrealized_pnl_usd"`
	Status           string  `json:"status"`
	OpenedAtMs       int64   `json:"opened_at_ms"`
}

type masterPositionsResponse struct {
	ReadAtMs  int64                `json:"read_at_ms"`
	Total     int                  `json:"total"`
	Positions []masterPositionItem `json:"positions"`
}

func (p *portal) handleMasterOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readContext(r)
	defer cancel()
	writeJSON(w, http.StatusOK, p.buildMasterOverview(ctx))
}

func (p *portal) handleMasterPositions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readContext(r)
	defer cancel()
	writeJSON(w, http.StatusOK, p.buildMasterPositions(ctx))
}

func (p *portal) buildMasterOverview(ctx context.Context) masterOverview {
	nowMs := p.now().UnixMilli()
	out := masterOverview{
		Mode:     "master",
		ReadAtMs: nowMs,
		Margin: masterMargin{
			BinanceTier: "unknown",
			BybitTier:   "unknown",
		},
		Engine1: masterEngine1{
			Enabled:  true,
			Strategy: "Spot LONG + Perp SHORT (Cash & Carry)",
		},
		Engine2: masterEngine2{
			Enabled:  p.cross != nil,
			Strategy: "Cross-Venue Perp LONG + Perp SHORT",
		},
		Coordinator: masterCoordinator{
			Locks: []coordinator.SymbolLock{},
		},
		Relays: masterRelays{
			ScannerConnected:     true,
			PaperLedgerConnected: true,
		},
	}

	// 1. Balances (Binance)
	acct := p.accountFor(ctx, "")
	for _, b := range acct.Spot.Balances {
		if b.Asset == "USDT" {
			out.Balances.BinanceSpotUSDT = b.TotalQtyInAsset
		}
	}
	for _, b := range acct.Futures.Balances {
		if b.Asset == "USDT" {
			out.Balances.BinanceFuturesUSDT = b.TotalQtyInAsset
		}
	}
	out.Balances.BinanceTotalUSDT = out.Balances.BinanceSpotUSDT + out.Balances.BinanceFuturesUSDT

	// 2. Engine 2, Margin Guard & Bybit Balances
	if p.cross != nil {
		mv := p.cross.marginView()
		out.Margin.Emergency = mv.Emergency
		out.Margin.EmergencySeq = mv.EmergencySeq
		out.Margin.YellowFrac = mv.YellowFrac
		out.Margin.OrangeFrac = mv.OrangeFrac
		out.Margin.RedFrac = mv.RedFrac

		for _, ven := range mv.Venues {
			if ven.Venue == crossVenueBinance {
				out.Margin.BinanceTier = ven.Tier
				out.Margin.BinanceMMRPct = ven.RatioPct
			} else if ven.Venue == crossVenueBybit {
				out.Margin.BybitTier = ven.Tier
				out.Margin.BybitMMRPct = ven.RatioPct
			}
		}

		// Read Bybit balances if available
		if ven, ok := p.cross.venues.get(crossVenueBybit); ok && ven.Perp != nil {
			if bybitBal, err := ven.Perp.GetBalance(ctx, broker.MarketFuturesUSDM); err == nil {
				for _, b := range bybitBal {
					if b.Asset == "USDT" || b.Asset == "USD" {
						out.Balances.BybitEquityUSD += b.FreeQtyCoin + b.LockedQtyCoin
						out.Balances.BybitAvailableUSD += b.FreeQtyCoin
					}
				}
			}
		}

		// Engine 2 Status
		cStatus := p.cross.statusView()
		out.Engine2.ActivePairsCount = len(cStatus.Pairs)
		if p.cross.pilot != nil {
			pv := p.cross.pilotView()
			out.Engine2.PilotRunning = pv.Enabled && !pv.Halted
			if pv.AdvisoryOnly {
				out.Engine2.PilotMode = "advisory"
			} else {
				out.Engine2.PilotMode = "active"
			}
		}
	}

	// 3. Engine 1 Auto-trader Status
	atSt := p.autotrade.Status()
	out.Engine1.AutotradeRunning = atSt.Enabled
	out.Engine1.ActivePairsCount = atSt.OpenPositions
	var driftSum float64
	for _, pos := range atSt.Positions {
		if pos.PairDriftQuote != nil {
			driftSum += *pos.PairDriftQuote
		}
	}
	out.Engine1.TotalPnLQuote = driftSum
	if p.markets.profile.Kind == venueBybit {
		out.Engine1.BybitPairsCount = atSt.OpenPositions
		out.Engine1.BybitPnLQuote = driftSum
	} else {
		out.Engine1.BinancePairsCount = atSt.OpenPositions
		out.Engine1.BinancePnLQuote = driftSum
	}

	// Coordinator Locks (ensure all 13 symbols are represented)
	all13Symbols := []string{
		"AAVEUSDT", "BNBUSDT", "BTCUSDT", "DOGEUSDT", "ETHUSDT",
		"HYPEUSDT", "LINKUSDT", "LTCUSDT", "NEARUSDT", "SOLUSDT",
		"SUIUSDT", "UNIUSDT", "XRPUSDT",
	}
	lockMap := make(map[string]coordinator.SymbolLock)
	if p.cross != nil {
		lView := p.cross.locksView()
		for _, l := range lView.Locks {
			lockMap[l.Symbol] = l
		}
	}
	for _, pos := range atSt.Positions {
		if _, exists := lockMap[pos.Symbol]; !exists {
			lockMap[pos.Symbol] = coordinator.SymbolLock{
				Symbol:      pos.Symbol,
				State:       coordinator.StateOccupied,
				OwnerEngine: coordinator.EngineCashAndCarry,
				Venues:      []string{string(p.markets.profile.Kind) + "_futures"},
			}
		}
	}

	allLocks := make([]coordinator.SymbolLock, 0, len(all13Symbols))
	for _, sym := range all13Symbols {
		if l, ok := lockMap[sym]; ok {
			allLocks = append(allLocks, l)
		} else {
			allLocks = append(allLocks, coordinator.SymbolLock{
				Symbol: sym,
				State:  coordinator.StateIdle,
			})
		}
	}
	out.Coordinator.Locks = allLocks
	out.Coordinator.TotalSymbols = len(allLocks)
	out.Coordinator.IdleCount = 0
	out.Coordinator.OccupiedE1Count = 0
	out.Coordinator.OccupiedE2Count = 0
	out.Coordinator.ConflictCount = 0
	for _, l := range allLocks {
		switch l.State {
		case coordinator.StateIdle:
			out.Coordinator.IdleCount++
		case coordinator.StateOccupied:
			if l.OwnerEngine == coordinator.EngineCashAndCarry {
				out.Coordinator.OccupiedE1Count++
			} else if l.OwnerEngine == coordinator.EngineCrossPerp {
				out.Coordinator.OccupiedE2Count++
			}
		case coordinator.StateConflict:
			out.Coordinator.ConflictCount++
		}
	}

	out.TotalEquityUSD = out.Balances.BinanceTotalUSDT + out.Balances.BybitEquityUSD
	return out
}

func (p *portal) buildMasterPositions(ctx context.Context) masterPositionsResponse {
	nowMs := p.now().UnixMilli()
	var items []masterPositionItem

	// 1. Collect Engine 1 active positions
	atSt := p.autotrade.Status()
	for _, pos := range atSt.Positions {
		var pnl float64
		if pos.PairDriftQuote != nil {
			pnl = *pos.PairDriftQuote
		}
		longLeg := "Binance Spot"
		shortLeg := "Binance Futures"
		if p.markets.profile.Kind == venueBybit {
			longLeg = "Bybit Spot"
			shortLeg = "Bybit Linear"
		}
		items = append(items, masterPositionItem{
			ID:               pos.IntentID,
			Symbol:           pos.Symbol,
			EngineID:         string(coordinator.EngineCashAndCarry),
			EngineTitle:      "Động cơ 1",
			Strategy:         "Spot LONG + Perp SHORT",
			LongLeg:          longLeg,
			ShortLeg:         shortLeg,
			QtyCoin:          pos.QtyCoin,
			NotionalUSD:      pos.NotionalQuote,
			EntryPriceLong:   pos.SpotEntryAvgQuote,
			EntryPriceShort:  pos.PerpEntryAvgQuote,
			UnrealizedPnLUSD: pnl,
			Status:           "hedged",
			OpenedAtMs:       pos.OpenedAtMs,
		})
	}

	// 2. Collect Engine 2 active positions
	if p.cross != nil {
		cStatus := p.cross.statusView()
		for _, cp := range cStatus.Pairs {
			longVenue := "Bybit Linear"
			shortVenue := "Binance Futures"
			if cp.LongVenue == crossVenueBinance {
				longVenue = "Binance Futures"
				shortVenue = "Bybit Linear"
			}
			status := "both_open"
			if cp.Unresolved {
				status = "unresolved"
			}
			items = append(items, masterPositionItem{
				ID:              cp.IntentID,
				Symbol:          cp.Symbol,
				EngineID:        string(coordinator.EngineCrossPerp),
				EngineTitle:     "Động cơ 2",
				Strategy:        "Perp–Perp Chéo Sàn",
				LongLeg:         longVenue,
				ShortLeg:        shortVenue,
				QtyCoin:         cp.LongQtyCoin,
				EntryPriceLong:  cp.LongAvgFillPriceQuote,
				EntryPriceShort: cp.ShortAvgFillPriceQuote,
				Status:          status,
				OpenedAtMs:      cp.OpenedAtMs,
			})
		}

		// Include coordinator locks that are occupied by reconciled positions
		seenSymbols := make(map[string]bool)
		for _, it := range items {
			seenSymbols[it.Symbol] = true
		}
		lView := p.cross.locksView()
		for _, l := range lView.Locks {
			if l.State == coordinator.StateOccupied && !seenSymbols[l.Symbol] {
				engTitle := "Động cơ 1"
				strategy := "Spot LONG + Perp SHORT"
				longLeg := "Binance Spot"
				shortLeg := "Binance Futures"
				if l.OwnerEngine == coordinator.EngineCrossPerp {
					engTitle = "Động cơ 2"
					strategy = "Perp–Perp Chéo Sàn"
					longLeg = "Bybit Linear"
					shortLeg = "Binance Futures"
				}
				items = append(items, masterPositionItem{
					ID:          "reconciled-" + l.Symbol,
					Symbol:      l.Symbol,
					EngineID:    string(l.OwnerEngine),
					EngineTitle: engTitle,
					Strategy:    strategy,
					LongLeg:     longLeg,
					ShortLeg:    shortLeg,
					Status:      "occupied",
					OpenedAtMs:  l.LockedAtMs,
				})
			}
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Symbol < items[j].Symbol
	})

	return masterPositionsResponse{
		ReadAtMs:  nowMs,
		Total:     len(items),
		Positions: items,
	}
}
