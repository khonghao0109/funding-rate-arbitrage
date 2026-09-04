package exchanges

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Binance funding collection over REST (step 2.5).
//
// premiumIndex carries the rate, both prices and the settlement stamp. It is
// queried ONE SYMBOL AT A TIME: the unfiltered form returns every listed
// perpetual — 198,811 bytes for ~780 entries, measured 2026-09-04 — which at
// this poll rate would be ~1.1 GB a day to keep four rows, and it costs
// request weight 10 against weight 1 for the per-symbol form. Four requests of
// weight 1 beat one request of weight 10 on both axes.
// fundingInfo carries the interval and the venue's own cap/floor, is the same
// size for one symbol as for all of them, and is re-read on the slow schedule.
//
// Why REST at all, when the plan says WebSocket first: the mark-price stream
// delivers nothing here — see the measurement in funding_rest.go and
// docs/DATA-REQUIREMENTS.md §3.4.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Mark-Price
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Get-Funding-Rate-Info

// binanceDefaultFundingIntervalHours is the documented default. fundingInfo
// returns only symbols whose configuration DIFFERS from it (on 2026-09-03 that
// happened to be all 778 trading perpetuals, but the endpoint promises no such
// coverage), so this is the base and fundingInfo is the override — never the
// other way round. CLAUDE.md, Binance trap row.
const binanceDefaultFundingIntervalHours = 8

type binancePremiumIndex struct {
	Symbol          string `json:"symbol"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`
	LastFundingRate string `json:"lastFundingRate"`
	NextFundingTime int64  `json:"nextFundingTime"`
	Time            int64  `json:"time"`
}

type binanceFundingInfo struct {
	Symbol string `json:"symbol"`
	// Documented as a NUMBER here while every rate on the same endpoint is a
	// string; measured 2026-09-04: {"fundingIntervalHours":8}.
	FundingIntervalHours     int64  `json:"fundingIntervalHours"`
	AdjustedFundingRateCap   string `json:"adjustedFundingRateCap"`
	AdjustedFundingRateFloor string `json:"adjustedFundingRateFloor"`
}

// runBinanceFunding polls premiumIndex for the configured symbols and
// publishes one FundingData per symbol per tick, refreshing fundingInfo on the
// slower schedule. It returns when ctx ends.
func runBinanceFunding(f Feeds, source string, symbols []Symbol) {
	meta := newFundingMetaCache()

	// Waited on, not fired and forgotten: this function returning is what tells
	// ConnectBinanceFutures that funding collection has stopped, and a poller
	// still inside a 30s HTTP call would make that claim false.
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		pollFunding(f.Ctx, source, "fundingInfo", fundingMetaEvery, func(ctx context.Context) error {
			return refreshBinanceFundingMeta(ctx, meta, symbols)
		})
	}()

	pollFunding(f.Ctx, source, "premiumIndex", fundingPollEvery, func(ctx context.Context) error {
		return pollBinancePremiumIndex(ctx, f, source, symbols, meta)
	})
	running.Wait()
}

func refreshBinanceFundingMeta(ctx context.Context, meta *fundingMetaCache, symbols []Symbol) error {
	var infos []binanceFundingInfo
	if err := fetchInstrumentJSON(ctx, "https://fapi.binance.com/fapi/v1/fundingInfo", &infos); err != nil {
		return err
	}
	meta.put(binanceFundingMeta(infos, symbols))
	return nil
}

// binanceFundingMeta indexes fundingInfo by standard symbol, defaulting the
// interval for every configured symbol the endpoint omits.
func binanceFundingMeta(infos []binanceFundingInfo, symbols []Symbol) map[string]fundingMetaEntry {
	byNative := make(map[string]binanceFundingInfo, len(infos))
	for _, info := range infos {
		byNative[info.Symbol] = info
	}

	out := make(map[string]fundingMetaEntry, len(symbols))
	for _, s := range symbols {
		entry := fundingMetaEntry{IntervalHours: binanceDefaultFundingIntervalHours}
		if info, ok := byNative[s.Venue]; ok {
			if info.FundingIntervalHours > 0 {
				entry.IntervalHours = info.FundingIntervalHours
			}
			if capFrac, err := strconv.ParseFloat(info.AdjustedFundingRateCap, 64); err == nil {
				entry.CapFrac, entry.HasCap = capFrac, true
			}
			if floorFrac, err := strconv.ParseFloat(info.AdjustedFundingRateFloor, 64); err == nil {
				entry.FloorFrac, entry.HasFloor = floorFrac, true
			}
		}
		out[s.Standard] = entry
	}
	return out
}

func pollBinancePremiumIndex(ctx context.Context, f Feeds, source string, symbols []Symbol, meta *fundingMetaCache) error {
	var errs []error
	for _, s := range symbols {
		var entry binancePremiumIndex
		err := fetchInstrumentJSON(ctx, "https://fapi.binance.com/fapi/v1/premiumIndex?symbol="+s.Venue, &entry)
		// THE receive stamp for this transport, taken per response as close to
		// the read as the shared JSON helper allows. See CLAUDE.md rule 13:
		// this REST poller is the third transport and is named there.
		recvAt := time.Now()
		if err != nil {
			// One symbol failing must not cost the others their reading, and
			// must not drop the whole poller into its retry interval.
			errs = append(errs, err)
			continue
		}
		if err := publishBinanceFunding(f, source, symbols, meta, []binancePremiumIndex{entry}, recvAt); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func publishBinanceFunding(f Feeds, source string, symbols []Symbol, meta *fundingMetaCache,
	entries []binancePremiumIndex, recvAt time.Time) error {

	wanted := make(map[string]string, len(symbols)) // native → standard
	for _, s := range symbols {
		wanted[s.Venue] = s.Standard
	}
	var failures []error

	for _, entry := range entries {
		standard, ok := wanted[entry.Symbol]
		if !ok {
			continue // premiumIndex covers every listed perpetual
		}
		rateFrac, err := strconv.ParseFloat(entry.LastFundingRate, 64)
		if err != nil {
			// Reported, never fatal to the rest: every WebSocket handler in
			// this package skips an unparseable entry the same way.
			failures = append(failures, fmt.Errorf("%s %s: lastFundingRate %q does not parse", source, entry.Symbol, entry.LastFundingRate))
			continue
		}
		// No interval means no reading: RatePer8hFrac and APRFrac both divide
		// by it, so publishing one without would be publishing a guess.
		entryMeta, ok := meta.get(standard)
		if !ok {
			continue
		}

		data, err := normalizeBinanceFunding(binanceFundingInput{
			fundingReading: fundingReading{
				Symbol:      standard,
				Source:      source,
				RecvAt:      recvAt,
				VenueTimeMs: entry.Time,
				RateFrac:    rateFrac,
			},
			IntervalHours:   entryMeta.IntervalHours,
			NextFundingAtMs: entry.NextFundingTime,
		})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		data.MarkPrice, _ = strconv.ParseFloat(entry.MarkPrice, 64)
		data.IndexPrice, _ = strconv.ParseFloat(entry.IndexPrice, 64)
		data.RateCapFrac, data.HasCap = entryMeta.CapFrac, entryMeta.HasCap
		data.RateFloorFrac, data.HasFloor = entryMeta.FloorFrac, entryMeta.HasFloor
		// lastFundingRate is the rate for the period now running, still moving
		// with the premium until settlement.
		data.IsEstimated = true

		if !f.SendFunding(data) {
			return nil // shutting down
		}
	}
	return errors.Join(failures...)
}
