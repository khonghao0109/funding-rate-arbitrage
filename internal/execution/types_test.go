package execution

import (
	"regexp"
	"testing"
)

// The derived ClientOrderID is the foundation of step 5.3, so its properties
// are pinned rather than assumed.
//
// A process killed between placing a leg and recording that it did so comes
// back knowing only the intent id. From that alone it must be able to
// reconstruct exactly what it called each order, or the position it opened is
// invisible to it. Three things therefore have to hold: the same intent always
// yields the same id, two intents never collide, and the result is something
// the venue will actually accept.
func TestLegClientOrderID_IsDeterministicPerIntentAndLeg(t *testing.T) {
	const intentA, intentB = "intent-2026-09-13-0001", "intent-2026-09-13-0002"

	// Same intent, same leg, twice — this is the restart case.
	if first, second := LegClientOrderID(intentA, LegSpot), LegClientOrderID(intentA, LegSpot); first != second {
		t.Fatalf("the same intent produced two ids, %q then %q — a restarted process could not find its own order", first, second)
	}

	// The two legs of ONE intent must differ, or a cancel aimed at the perp
	// leg would name the spot order.
	if LegClientOrderID(intentA, LegSpot) == LegClientOrderID(intentA, LegPerp) {
		t.Fatal("both legs of one intent got the same id")
	}

	// Two intents must differ on both legs.
	for _, leg := range []LegName{LegSpot, LegPerp} {
		if LegClientOrderID(intentA, leg) == LegClientOrderID(intentB, leg) {
			t.Errorf("two intents collided on the %s leg", leg)
		}
	}

	// The legs differ EVERYWHERE, not by a trailing character: the leg name is
	// mixed in before hashing. A suffix scheme would make two ids that a
	// truncating venue could flatten into one.
	spot, perp := LegClientOrderID(intentA, LegSpot), LegClientOrderID(intentA, LegPerp)
	if len(spot) != len(perp) {
		t.Fatalf("ids differ in length: %q vs %q", spot, perp)
	}
	shared := 0
	for i := range spot {
		if spot[i] == perp[i] {
			shared++
		}
	}
	if shared > len(spot)/2 {
		t.Errorf("the two legs' ids share %d of %d characters — they differ by a suffix rather than throughout: %q %q",
			shared, len(spot), spot, perp)
	}
}

// Binance documents clientOrderId as matching ^[\.A-Z\:/a-z0-9_-]{1,36}$, so
// an id this package derives has to satisfy it for ANY caller intent id —
// including a long one, an empty one, and one full of characters the venue
// does not accept. That is what the hash is for.
func TestLegClientOrderID_IsAlwaysAcceptableToTheVenue(t *testing.T) {
	venueForm := regexp.MustCompile(`^[\.A-Z\:/a-z0-9_-]{1,36}$`)
	intents := []string{
		"",
		"simple",
		"intent with spaces and ünïcødé ✓",
		"a-very-long-intent-id-" + string(make([]byte, 500)),
		`{"json":"shaped","id":42}`,
	}
	for _, intentID := range intents {
		for _, leg := range []LegName{LegSpot, LegPerp} {
			got := LegClientOrderID(intentID, leg)
			if !venueForm.MatchString(got) {
				t.Errorf("intent %q leg %s produced %q, which the venue's documented form rejects", truncate(intentID), leg, got)
			}
			if len(got) > 36 {
				t.Errorf("intent %q leg %s produced a %d-character id", truncate(intentID), leg, len(got))
			}
		}
	}
}

// The version prefix exists so a future change of scheme is visible rather
// than silent: ids minted by two schemes must not be confusable, and a 5.3
// recovery that finds nothing should be able to tell which scheme it looked
// for.
func TestLegClientOrderID_CarriesTheSchemeVersion(t *testing.T) {
	for _, leg := range []LegName{LegSpot, LegPerp} {
		if got := LegClientOrderID("x", leg); got[:len(clientOrderIDPrefix)] != clientOrderIDPrefix {
			t.Errorf("%s id %q does not start with the scheme prefix %q", leg, got, clientOrderIDPrefix)
		}
	}
}

// The squaring order an operator sends to rebalance a pair is the fourth id
// family, and two commands send it: cmd/execcheck -reconcile and
// cmd/execportal's /api/reconcile. They must derive the SAME id, or a second
// tool run after the first cannot see the order the first one sent and squares
// the pair a second time — a hedge turned into a position by the tool meant to
// fix one. So the derivation lives here, beside the other three, and is pinned
// to the exact formula execcheck shipped with on 2026-09-13.
func TestReconcileClientOrderID_IsTheSchemeExeccheckShippedAndDistinct(t *testing.T) {
	const intentID = "xbtcusdt-20260913-074359"
	for _, leg := range []LegName{LegSpot, LegPerp} {
		got := ReconcileClientOrderID(intentID, leg)
		if want := LegClientOrderID(intentID+"|reconcile", leg); got != want {
			t.Errorf("%s: ReconcileClientOrderID = %q, want %q — the id an existing -reconcile order already carries", leg, got, want)
		}
		for name, other := range map[string]string{
			"open":   LegClientOrderID(intentID, leg),
			"close":  CloseClientOrderID(intentID, leg),
			"unwind": UnwindClientOrderID(intentID, leg),
		} {
			if got == other {
				t.Errorf("%s: the reconcile id equals the %s id %q", leg, name, other)
			}
		}
	}
	if ReconcileClientOrderID(intentID, LegSpot) == ReconcileClientOrderID(intentID, LegPerp) {
		t.Error("both legs derive the same reconcile id")
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
