package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"futures-arbitrage-scanner/internal/broker"
)

// FundingRate is one SETTLED funding rate as the futures venue lists it.
//
// It is the settlement that happened, not the forming estimate premiumIndex
// publishes as lastFundingRate: the testnet auto-trader (PLAN Q18) projects on
// what settled and exits on what settled, because a forming rate is revised
// until the stamp and CLAUDE.md's step-3.2 lesson is that a decision made on
// one is a decision made on a number the venue has not committed to.
type FundingRate struct {
	Symbol string
	// SettledAtMs is the venue's fundingTime, stored VERBATIM — the testnet
	// stamps land a millisecond either side of the hour, and rounding them
	// would invent a stamp the venue never published.
	SettledAtMs int64
	// RatePerIntervalFrac is the venue's fundingRate as a FRACTION of notional
	// for that symbol's own period. The period is not stated here and is not
	// assumed (rule 3); a caller measures it from the spacing of the stamps.
	RatePerIntervalFrac float64
	MarkPriceQuote      float64
	// RateType is the venue's rateType when it sends one ("Regular" on the
	// documentation page), empty when it does not — the testnet did not on
	// 2026-09-15. CLAUDE.md's Binance row: a "Special" settlement is off the
	// cadence and must not be read as the regular rate.
	RateType string
}

// venueFundingRate is one element of GET /fapi/v1/fundingRate.
type venueFundingRate struct {
	Symbol      string `json:"symbol"`
	FundingRate string `json:"fundingRate"`
	FundingTime int64  `json:"fundingTime"`
	MarkPrice   string `json:"markPrice"`
	RateType    string `json:"rateType"`
}

// maxFundingRateRows is the documented ceiling of `limit`.
const maxFundingRateRows = 1000

// FundingRateHistory reads the settled rates of one symbol between two
// instants, both INCLUSIVE, oldest first.
//
// A row whose rate does not parse is REFUSED with the whole answer, not
// skipped: a missing settlement in the middle of a series moves the measured
// cadence and the trailing mean, and neither would say so.
func (c *Client) FundingRateHistory(ctx context.Context, symbol string, startMs, endMs int64) ([]FundingRate, error) {
	if c.market != broker.MarketFuturesUSDM {
		return nil, fmt.Errorf("%w: %q settles no funding", broker.ErrNotSupported, c.market)
	}
	if symbol == "" {
		return nil, fmt.Errorf("%w: no symbol", broker.ErrInvalidOrder)
	}
	params := []broker.Param{
		{Key: "symbol", Value: symbol},
		{Key: "limit", Value: strconv.Itoa(maxFundingRateRows)},
	}
	if startMs > 0 {
		params = append(params, broker.Param{Key: "startTime", Value: strconv.FormatInt(startMs, 10)})
	}
	if endMs > 0 {
		params = append(params, broker.Param{Key: "endTime", Value: strconv.FormatInt(endMs, 10)})
	}
	var raw json.RawMessage
	if err := c.http.GetPublic(ctx, broker.FuturesFundingRate, params, &raw); err != nil {
		return nil, classify(err)
	}
	return parseFundingRates(raw, symbol)
}

func parseFundingRates(raw json.RawMessage, symbol string) ([]FundingRate, error) {
	var list []venueFundingRate
	if err := decode(raw, &list); err != nil {
		return nil, err
	}
	out := make([]FundingRate, 0, len(list))
	for i, v := range list {
		if v.Symbol != symbol {
			return nil, fmt.Errorf("binance: dòng funding %d là %q, đã hỏi %q — không dùng câu trả lời", i, v.Symbol, symbol)
		}
		rate, err := strconv.ParseFloat(v.FundingRate, 64)
		if err != nil || v.FundingTime <= 0 {
			return nil, fmt.Errorf("binance: dòng funding %d thiếu fundingRate hoặc fundingTime đọc được — không bỏ qua một mốc giữa chuỗi", i)
		}
		out = append(out, FundingRate{
			Symbol: v.Symbol, SettledAtMs: v.FundingTime, RatePerIntervalFrac: rate,
			MarkPriceQuote: parseFloat(v.MarkPrice), RateType: v.RateType,
		})
	}
	// "Results are in ascending order" — sorted anyway, because every caller
	// reads the LAST element as the newest settlement.
	sort.SliceStable(out, func(i, j int) bool { return out[i].SettledAtMs < out[j].SettledAtMs })
	return out, nil
}
