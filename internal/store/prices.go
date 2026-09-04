package store

import (
	"context"
	"fmt"
)

// PriceSample is one periodic sample of one source's top of book.
//
// The quantities are 0 for the four venues whose book is denominated in
// contracts — 0 means NOT KNOWN, never "no liquidity". That distinction is the
// reason the column exists at all rather than being inferred later.
type PriceSample struct {
	Source        string
	Symbol        string
	SampledAtMs   int64
	MidPriceQuote float64
	BestBidQuote  float64
	BestAskQuote  float64
	// Named Coin, not Qty: the scanner's pipeline carries base coins. Since
	// step 2.7b the contract venues (OKX, Gate, Kraken) are converted through
	// the registry's ContractSizeCoin before landing here; 0 still means "not
	// known" — Paradex publishes no size at all — never "no liquidity".
	BestBidQtyCoin float64
	BestAskQtyCoin float64
	// RecvAtMs is when the underlying message came off the socket, carried
	// through so staleness stays reconstructible from the stored row: a sample
	// whose RecvAtMs trails SampledAtMs by minutes recorded a dead feed, and
	// nothing later can tell that from a live price without it.
	RecvAtMs int64
}

// PutPriceSamples writes one round of samples.
//
// The key is (source, symbol, sampled_at_ms), so a sampler that fires twice in
// the same millisecond overwrites rather than duplicating — INSERT OR REPLACE
// because the second write is the better measurement, not a conflict.
func (s *Store) PutPriceSamples(ctx context.Context, samples []PriceSample) (int, error) {
	if len(samples) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin price insert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO price_snapshots (
			source, symbol, sampled_at_ms,
			mid_price_quote, best_bid_quote, best_ask_quote,
			best_bid_qty_coin, best_ask_qty_coin, recv_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare price insert: %w", err)
	}
	defer stmt.Close()

	written := 0
	for _, sample := range samples {
		if sample.Source == "" || sample.Symbol == "" || sample.SampledAtMs <= 0 {
			return 0, fmt.Errorf("store: refusing an unidentified price sample: %+v", sample)
		}
		if _, err := stmt.ExecContext(ctx,
			sample.Source, sample.Symbol, sample.SampledAtMs,
			sample.MidPriceQuote, sample.BestBidQuote, sample.BestAskQuote,
			sample.BestBidQtyCoin, sample.BestAskQtyCoin, sample.RecvAtMs,
		); err != nil {
			return 0, fmt.Errorf("store: insert price %s/%s: %w", sample.Source, sample.Symbol, err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit price insert: %w", err)
	}
	return written, nil
}

// PriceSnapshots reads samples for one pair in a window, oldest first. An empty
// symbol reads every pair.
func (s *Store) PriceSnapshots(ctx context.Context, symbol string, fromMs, toMs int64) ([]PriceSample, error) {
	// Composed, not `(? = '' OR symbol = ?)` — see FundingHistory. This table
	// is the larger of the two by two orders of magnitude, so losing the index
	// here costs more.
	query := `
		SELECT source, symbol, sampled_at_ms,
		       mid_price_quote, best_bid_quote, best_ask_quote,
		       best_bid_qty_coin, best_ask_qty_coin, recv_at_ms
		FROM price_snapshots
		WHERE sampled_at_ms >= ? AND sampled_at_ms < ?`
	args := []any{fromMs, toMs}
	if symbol != "" {
		query += " AND symbol = ?"
		args = append(args, symbol)
	}
	query += " ORDER BY sampled_at_ms, source"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query price snapshots: %w", err)
	}
	defer rows.Close()

	var out []PriceSample
	for rows.Next() {
		var sample PriceSample
		if err := rows.Scan(&sample.Source, &sample.Symbol, &sample.SampledAtMs,
			&sample.MidPriceQuote, &sample.BestBidQuote, &sample.BestAskQuote,
			&sample.BestBidQtyCoin, &sample.BestAskQtyCoin, &sample.RecvAtMs); err != nil {
			return nil, fmt.Errorf("store: scan price snapshot: %w", err)
		}
		out = append(out, sample)
	}
	return out, rows.Err()
}
