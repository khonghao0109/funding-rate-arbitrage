package execution

import (
	"context"
	"errors"
	"fmt"
	"math"

	"futures-arbitrage-scanner/internal/broker"
)

// What the position made, read from the VENUE — and what is deliberately not
// in that figure. See close.go's file comment for the contract; this file is
// the arithmetic.

// priceClose fills in the three components and their sum.
//
// Every one of them comes from the venue: the settlements it says it paid, the
// commission it says it charged, and the prices it says the four fills happened
// at. Nothing here is derived from a rate, a schedule or an assumption, and
// where a figure cannot be read it is labelled as unread rather than left at
// zero — a zero that means "not read" and a zero that means "nothing was
// charged" are different facts and only one of them may be summed.
func (o *Trader) priceClose(ctx context.Context, req CloseRequest, res *CloseResult) {
	intent := req.Intent
	o.priceFunding(ctx, req, res)
	o.priceCommission(ctx, req, res)
	res.SlippageQuote, res.PriceDriftPricedVI = slippageQuote(req, res)

	// The basis move, reported BESIDE the realized figure and never inside it.
	if req.EntrySpotAvgPriceQuote > 0 && req.EntryPerpAvgPriceQuote > 0 &&
		res.Spot.AvgFillPriceQuote > 0 && res.Perp.AvgFillPriceQuote > 0 && res.ClosedQtyCoin > 0 {
		spotLeg := (res.Spot.AvgFillPriceQuote - req.EntrySpotAvgPriceQuote) * res.ClosedQtyCoin
		perpLeg := (req.EntryPerpAvgPriceQuote - res.Perp.AvgFillPriceQuote) * res.ClosedQtyCoin
		res.PairPriceDriftQuote = spotLeg + perpLeg
	} else {
		res.PriceDriftPricedVI = joinVI([]string{res.PriceDriftPricedVI,
			"chưa đủ bốn giá khớp để tính trôi giá của cặp — KHÔNG phải bằng 0"})
	}
	_ = intent

	res.RealizedQuote = res.FundingReceivedQuote - res.CommissionQuote - res.SlippageQuote
}

// priceFunding counts the settlements the venue says actually paid.
//
// CLAUDE.md rule 6: a settlement is an event, not a yield. Each row is one
// settlement crossed while the position was open, and there is no interval
// anywhere in this function to multiply by.
func (o *Trader) priceFunding(ctx context.Context, req CloseRequest, res *CloseResult) {
	reader, ok := o.perp.(broker.FundingReader)
	if !ok {
		res.FundingSourceVI = "sàn perp không đọc được lịch sử funding — số 0 ở đây nghĩa là CHƯA ĐỌC ĐƯỢC, không phải chưa trả đồng nào"
		return
	}
	endMs := o.cfg.Now().UnixMilli()
	rows, err := reader.FundingIncome(ctx, broker.MarketFuturesUSDM, req.Intent.Symbol, req.OpenedAtMs, endMs)
	if err != nil {
		if errors.Is(err, broker.ErrNotSupported) {
			res.FundingSourceVI = "sàn này không có khái niệm funding rời rạc"
			return
		}
		res.FundingSourceVI = "KHÔNG đọc được funding từ sàn: " + err.Error()
		return
	}
	sum := 0.0
	for _, r := range rows {
		sum += r.IncomeQuote
	}
	res.FundingReceivedQuote = sum
	res.SettlementsCounted = len(rows)
	res.FundingSourceVI = fmt.Sprintf("đọc từ sàn: %d mốc settle trong cửa sổ giữ, cộng lại %.8f (dấu theo sàn: dương là NHẬN)",
		len(rows), sum)
	if len(rows) == 0 {
		res.FundingSourceVI += " — giữ chưa qua mốc settle nào"
	}
}

// priceCommission sums what the venue charged on the four orders.
//
// Only commission taken in the QUOTE asset is summed. Anything else — spot
// takes its fee in the asset received, or in BNB when the account elects that —
// is listed unconverted in CommissionOtherVI, because converting it needs a
// price at a moment and picking one silently is how a cost becomes a number
// that is wrong but not obviously wrong (rule 5).
func (o *Trader) priceCommission(ctx context.Context, req CloseRequest, res *CloseResult) {
	quote := req.Intent.SpotInstrument.QuoteAsset
	if quote == "" {
		quote = req.Intent.PerpInstrument.QuoteAsset
	}
	type source struct {
		nameVI string
		b      broker.Broker
		market broker.Market
		id     string
	}
	sources := []source{
		{"mở spot", o.spot, broker.MarketSpot, LegClientOrderID(req.Intent.ID, LegSpot)},
		{"mở perp", o.perp, broker.MarketFuturesUSDM, LegClientOrderID(req.Intent.ID, LegPerp)},
		{"đóng spot", o.spot, broker.MarketSpot, closeClientOrderID(req.Intent.ID, LegSpot)},
		{"đóng perp", o.perp, broker.MarketFuturesUSDM, closeClientOrderID(req.Intent.ID, LegPerp)},
	}

	var summed, missing, other []string
	total := 0.0
	otherByAsset := map[string]float64{}
	for _, s := range sources {
		reader, ok := s.b.(broker.TradeReader)
		if !ok {
			missing = append(missing, s.nameVI+" (sàn không trả được danh sách khớp)")
			continue
		}
		trades, err := reader.OrderTrades(ctx, broker.OrderQuery{
			Market: s.market, Symbol: req.Intent.Symbol, ClientOrderID: s.id})
		if err != nil {
			missing = append(missing, s.nameVI+": "+shortErr(err))
			continue
		}
		legTotal := 0.0
		for _, t := range trades {
			switch {
			case t.CommissionAsset == "":
				// The venue stated none. Not the same as zero, and said so.
				missing = append(missing, s.nameVI+" (sàn không nêu phí trên khớp này)")
			case t.CommissionAsset == quote:
				legTotal += t.CommissionQtyInAsset
			default:
				otherByAsset[t.CommissionAsset] += t.CommissionQtyInAsset
			}
		}
		total += legTotal
		summed = append(summed, fmt.Sprintf("%s %.8f %s", s.nameVI, legTotal, quote))
	}
	res.CommissionQuote = total
	for asset, qty := range otherByAsset {
		other = append(other, fmt.Sprintf("%.8f %s", qty, asset))
	}
	if len(other) > 0 {
		res.CommissionOtherVI = "phí thu bằng tài sản KHÁC quote, KHÔNG quy đổi và KHÔNG cộng vào CommissionQuote: " + joinVI(other)
	}
	res.CommissionSourceVI = "đọc từ sàn: " + joinVI(summed)
	if len(missing) > 0 {
		res.CommissionSourceVI += " | CHƯA ĐỌC ĐƯỢC: " + joinVI(missing) + " — phần này thiếu khỏi CommissionQuote"
	}
}

// slippageQuote prices all four fills against the reference mid that was
// current when each decision was made. Positive is PAID.
//
// The entry mids come from the caller, because they were current at a moment
// this call cannot reach; the exit mids come from the books on the Intent,
// which the caller refreshed before closing.
func slippageQuote(req CloseRequest, res *CloseResult) (float64, string) {
	type fill struct {
		nameVI     string
		priceQuote float64
		midQuote   float64
		qtyCoin    float64
		// paidAbove is true for a BUY, where paying MORE than the mid is the
		// cost, and false for a SELL, where receiving LESS than it is.
		paidAbove bool
	}
	fills := []fill{
		{"mở spot (mua)", req.EntrySpotAvgPriceQuote, req.EntrySpotRefMidQuote, res.ClosedQtyCoin, true},
		{"mở perp (bán)", req.EntryPerpAvgPriceQuote, req.EntryPerpRefMidQuote, res.ClosedQtyCoin, false},
		{"đóng spot (bán)", res.Spot.AvgFillPriceQuote, req.Intent.SpotBook.MidPriceQuote, res.Spot.UnwoundQtyCoin, false},
		{"đóng perp (mua)", res.Perp.AvgFillPriceQuote, req.Intent.PerpBook.MidPriceQuote, res.Perp.UnwoundQtyCoin, true},
	}
	total := 0.0
	var unpriced []string
	for _, f := range fills {
		if f.priceQuote <= 0 || f.midQuote <= 0 || f.qtyCoin <= 0 {
			unpriced = append(unpriced, f.nameVI)
			continue
		}
		d := f.priceQuote - f.midQuote
		if !f.paidAbove {
			d = -d
		}
		total += d * f.qtyCoin
	}
	noteVI := "trượt giá đo bằng giá khớp so với mid tham chiếu lúc ra quyết định, cả bốn fill"
	if len(unpriced) > 0 {
		noteVI += " | CHƯA tính được: " + joinVI(unpriced) + " (thiếu giá khớp hoặc mid tham chiếu) — phần này thiếu khỏi SlippageQuote"
	}
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, noteVI + " | số không hữu hạn, trả về 0 và nói rõ"
	}
	return total, noteVI
}

// shortErr keeps a venue's own words out of a report while keeping the sentinel
// readable.
func shortErr(err error) string {
	switch {
	case errors.Is(err, broker.ErrOrderNotFound):
		return "sàn không còn giữ lệnh này"
	case errors.Is(err, broker.ErrNotSupported):
		return "sàn không hỗ trợ"
	default:
		return err.Error()
	}
}
