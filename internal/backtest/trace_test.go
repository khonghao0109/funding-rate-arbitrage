package backtest

import (
	"reflect"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/strategy"
)

// RunTraced is Run plus every decision the rules made inside the window, one
// per in-window settlement, so the step-3.5 comparison can line the replay
// up against the live journal decision by decision (PLAN 3.5 ③ step 4). It
// must change nothing about the result: the trace is a witness, not a rule.
func TestRunTraced_ReturnsTheSameResultAndOneDecisionPerInWindowSettlement(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2, -3, -3, 2, 2)
	series, window := seriesOf(entries), fullWindow(entries)
	p := testParams()

	plain := Run(series, window, p)
	traced, decisions := RunTraced(series, window, p)
	if !reflect.DeepEqual(plain, traced) {
		t.Fatalf("the trace changed the result:\nplain  %+v\ntraced %+v", plain, traced)
	}
	if len(decisions) != traced.Settlements {
		t.Fatalf("want one decision per in-window settlement (%d), got %d", traced.Settlements, len(decisions))
	}
	for i := 1; i < len(decisions); i++ {
		if decisions[i].AtMs <= decisions[i-1].AtMs {
			t.Fatalf("decisions must be oldest first: %d then %d", decisions[i-1].AtMs, decisions[i].AtMs)
		}
	}
	// The first decision is an ENTRY evaluation (flat), and once a position
	// is open the next ones are EXIT evaluations, until it closes.
	enters, exits := 0, 0
	for _, d := range decisions {
		if d.Holding {
			exits++
			if d.Decision.Action != strategy.ActionHold && d.Decision.Action != strategy.ActionExit {
				t.Errorf("an exit evaluation must hold or exit, got %s", d.Decision.Action)
			}
		} else {
			enters++
			if d.Decision.Action != strategy.ActionEnter && d.Decision.Action != strategy.ActionSkip {
				t.Errorf("an entry evaluation must enter or skip, got %s", d.Decision.Action)
			}
		}
		if len(d.Decision.Checks) == 0 {
			t.Errorf("a traced decision must carry its checks: %+v", d)
		}
	}
	if len(traced.Trades) > 0 && exits == 0 {
		t.Error("a run that traded must have evaluated the exit rule at least once")
	}
	if enters == 0 {
		t.Error("a run must have evaluated the entry rule at least once")
	}
}

// Decisions are made only INSIDE the window: rows loaded as lookback before
// it (cmd/backtest loads 200 days of history before -from) are context for
// the trailing checks, never decisions of their own.
func TestRunTraced_TracesNothingBeforeTheWindow(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2, 2, 2)
	series := seriesOf(entries)
	window := Window{FromMs: entries[4].SettledAtMs, ToMs: entries[len(entries)-1].SettledAtMs + 1}
	_, decisions := RunTraced(series, window, testParams())
	for _, d := range decisions {
		if d.AtMs < window.FromMs {
			t.Fatalf("decision before the window at %d (window from %d)", d.AtMs, window.FromMs)
		}
	}
	if len(decisions) == 0 {
		t.Fatal("the in-window settlements must have been decided on")
	}
}

// A liquidation closes the position without asking the rule, so that
// settlement has no decision: the trace holds one decision per in-window
// settlement MINUS the liquidations, and the result is still Run's.
func TestRunTraced_ALiquidatedSettlementHasNoDecision(t *testing.T) {
	series, window := marginSeries(t, func(i int, c *exchanges.PriceCandle) {
		if i == 20 { // hour 20: inside the first holding period, as the margin test places it
			c.HighPriceQuote = 115
		}
	})
	p := marginParams()
	plain := Run(series, window, p)
	traced, decisions := RunTraced(series, window, p)
	if !reflect.DeepEqual(plain, traced) {
		t.Fatalf("the trace changed a liquidating run:\nplain  %+v\ntraced %+v", plain, traced)
	}
	if traced.Liquidations == 0 {
		t.Fatal("the scenario must liquidate, or it tests nothing")
	}
	if len(decisions) != traced.Settlements-traced.Liquidations {
		t.Errorf("want %d decisions (%d settlements − %d liquidations), got %d",
			traced.Settlements-traced.Liquidations, traced.Settlements, traced.Liquidations, len(decisions))
	}
}
