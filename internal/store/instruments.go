package store

import (
	"context"
	"fmt"

	"futures-arbitrage-scanner/exchanges"
)

// PutInstrumentSnapshot writes one day's trading rules.
//
// Keyed by (day, source, symbol) and written with INSERT OR REPLACE: the
// snapshot is taken more than once a day (every refresh cycle, and again on
// every restart) and the last reading of a day is the one that describes it.
// The day boundary is UTC, so a restart at 23:59 and one at 00:01 land in
// different snapshots rather than in whichever local day the host believes in.
func (s *Store) PutInstrumentSnapshot(ctx context.Context, day string, instruments []exchanges.Instrument) (int, error) {
	if day == "" {
		return 0, fmt.Errorf("store: instrument snapshot needs a day")
	}
	if len(instruments) == 0 {
		// An empty registry means every fetch failed. Writing that as a day's
		// snapshot would record "no venue listed anything today", which is a
		// claim about the venues rather than about this process.
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin instrument insert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO instrument_snapshots (
			snapshot_day, source, symbol, native_symbol, market_type,
			base_asset, quote_asset, status,
			tick_size_quote, step_size_coin, min_qty_coin, max_qty_coin, min_notional_quote,
			is_contract, contract_size_coin, max_leverage_x, recorded_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare instrument insert: %w", err)
	}
	defer stmt.Close()

	recordedAtMs := s.now().UnixMilli()
	written := 0
	for _, inst := range instruments {
		if inst.Source == "" || inst.Symbol == "" {
			return 0, fmt.Errorf("store: refusing an unidentified instrument: %+v", inst)
		}
		if _, err := stmt.ExecContext(ctx,
			day, inst.Source, inst.Symbol, inst.NativeSymbol, inst.MarketType,
			inst.BaseAsset, inst.QuoteAsset, inst.Status,
			inst.TickSizeQuote, inst.StepSizeCoin, inst.MinQtyCoin, inst.MaxQtyCoin, inst.MinNotionalQuote,
			inst.IsContract, inst.ContractSizeCoin, inst.MaxLeverageX, recordedAtMs,
		); err != nil {
			return 0, fmt.Errorf("store: insert instrument %s/%s: %w", inst.Source, inst.Symbol, err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit instrument insert: %w", err)
	}
	return written, nil
}

// InstrumentSnapshot reads one day's stored rules.
func (s *Store) InstrumentSnapshot(ctx context.Context, day string) ([]exchanges.Instrument, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, symbol, native_symbol, market_type, base_asset, quote_asset, status,
		       tick_size_quote, step_size_coin, min_qty_coin, max_qty_coin, min_notional_quote,
		       is_contract, contract_size_coin, max_leverage_x
		FROM instrument_snapshots WHERE snapshot_day = ? ORDER BY source, symbol`, day)
	if err != nil {
		return nil, fmt.Errorf("store: query instrument snapshot: %w", err)
	}
	defer rows.Close()

	var out []exchanges.Instrument
	for rows.Next() {
		var inst exchanges.Instrument
		if err := rows.Scan(&inst.Source, &inst.Symbol, &inst.NativeSymbol, &inst.MarketType,
			&inst.BaseAsset, &inst.QuoteAsset, &inst.Status,
			&inst.TickSizeQuote, &inst.StepSizeCoin, &inst.MinQtyCoin, &inst.MaxQtyCoin, &inst.MinNotionalQuote,
			&inst.IsContract, &inst.ContractSizeCoin, &inst.MaxLeverageX); err != nil {
			return nil, fmt.Errorf("store: scan instrument snapshot: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}
