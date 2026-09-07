package store

import (
	"context"
	"fmt"

	"futures-arbitrage-scanner/exchanges"
)

// PutPriceHistory writes hourly candles.
//
// INSERT OR REPLACE on (source, symbol, open_time_ms): re-fetching a window is
// a normal thing to do — a backfill is safe to re-run — and the newer copy of a
// closed interval is identical while the newer copy of the interval still
// forming is the better measurement.
func (s *Store) PutPriceHistory(ctx context.Context, candles []exchanges.PriceCandle) (int, error) {
	if len(candles) == 0 {
		return 0, nil
	}
	recordedAtMs := s.now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin price history insert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO price_history (
			source, symbol, open_time_ms, interval_sec,
			open_price_quote, high_price_quote, low_price_quote, close_price_quote,
			base_volume_coin, recorded_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare price history insert: %w", err)
	}
	defer stmt.Close()

	written := 0
	for _, candle := range candles {
		if candle.Source == "" || candle.Symbol == "" || candle.OpenTimeMs <= 0 || candle.IntervalSec <= 0 {
			return 0, fmt.Errorf("store: refusing an unidentified candle: %+v", candle)
		}
		if _, err := stmt.ExecContext(ctx,
			candle.Source, candle.Symbol, candle.OpenTimeMs, candle.IntervalSec,
			candle.OpenPriceQuote, candle.HighPriceQuote, candle.LowPriceQuote, candle.ClosePriceQuote,
			candle.BaseVolumeCoin, recordedAtMs,
		); err != nil {
			return 0, fmt.Errorf("store: insert candle %s/%s: %w", candle.Source, candle.Symbol, err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit price history insert: %w", err)
	}
	return written, nil
}

// PriceHistory reads one market's candles in a window, OLDEST FIRST.
func (s *Store) PriceHistory(ctx context.Context, source, symbol string, fromMs, toMs int64) ([]exchanges.PriceCandle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, symbol, open_time_ms, interval_sec,
		       open_price_quote, high_price_quote, low_price_quote, close_price_quote,
		       base_volume_coin
		FROM price_history
		WHERE source = ? AND symbol = ? AND open_time_ms >= ? AND open_time_ms < ?
		ORDER BY open_time_ms`, source, symbol, fromMs, toMs)
	if err != nil {
		return nil, fmt.Errorf("store: read price history %s/%s: %w", source, symbol, err)
	}
	defer rows.Close()

	var out []exchanges.PriceCandle
	for rows.Next() {
		var candle exchanges.PriceCandle
		if err := rows.Scan(&candle.Source, &candle.Symbol, &candle.OpenTimeMs, &candle.IntervalSec,
			&candle.OpenPriceQuote, &candle.HighPriceQuote, &candle.LowPriceQuote, &candle.ClosePriceQuote,
			&candle.BaseVolumeCoin); err != nil {
			return nil, fmt.Errorf("store: scan price history: %w", err)
		}
		out = append(out, candle)
	}
	return out, rows.Err()
}

// PriceHistoryCoverage is what one market's candle series actually holds.
//
// It exists for the same reason FundingCoverage does: the corpus is NOT
// uniformly deep and never will be — measured 2026-09-07, Hyperliquid's hourly
// candles reach about 208 days while the other five reach a year — and anything
// comparing two sources has to read this first or it is comparing seven months
// against twelve and calling the difference a result.
type PriceHistoryCoverage struct {
	Source      string
	Symbol      string
	Candles     int
	FirstOpenMs int64
	LastOpenMs  int64
}

// PriceCoverage reports coverage per source for one symbol, or for every symbol
// when symbol is empty.
func (s *Store) PriceCoverage(ctx context.Context, symbol string) ([]PriceHistoryCoverage, error) {
	// Composed rather than `(? = '' OR symbol = ?)` — see FundingHistory: the
	// OR form defeats the index on a table with a row per market per hour.
	query := `SELECT source, symbol, count(*), min(open_time_ms), max(open_time_ms)
	          FROM price_history`
	args := []any{}
	if symbol != "" {
		query += " WHERE symbol = ?"
		args = append(args, symbol)
	}
	query += " GROUP BY source, symbol ORDER BY symbol, source"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: price coverage: %w", err)
	}
	defer rows.Close()

	var out []PriceHistoryCoverage
	for rows.Next() {
		var c PriceHistoryCoverage
		if err := rows.Scan(&c.Source, &c.Symbol, &c.Candles, &c.FirstOpenMs, &c.LastOpenMs); err != nil {
			return nil, fmt.Errorf("store: scan price coverage: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
