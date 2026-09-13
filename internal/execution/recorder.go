package execution

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Every state transition goes through a Recorder.
//
// In 4.4a the only implementation is in memory and the tests assert on what it
// captured. Writing transitions to internal/store is 4.4b, and it is 4.4b for
// an operational reason rather than a design one: the step-3.5 gate process is
// writing data/scanner.db right now, and adding a table means a migration under
// a database a fortnight-long unattended run has open.
//
// The interface exists NOW, rather than being retrofitted later, because the
// sequence of transitions is the only evidence of what a crashed process had
// done. Step 5.3 recovers a position by asking the venue about the derived
// ClientOrderIDs; the recorded stream is how an operator afterwards learns WHY
// the process was doing that.

// EventKind is what happened. The set is closed and each value names a real
// transition, not a log level.
type EventKind string

const (
	EventIntentReceived EventKind = "intent_received"
	EventSized          EventKind = "sized"
	EventRefused        EventKind = "refused"
	EventBookChecked    EventKind = "book_checked"
	EventLegPlacing     EventKind = "leg_placing"
	EventLegPlaced      EventKind = "leg_placed"
	EventLegAmbiguous   EventKind = "leg_ambiguous"
	EventLegResolved    EventKind = "leg_resolved"
	EventLegResent      EventKind = "leg_resent"
	EventLegCancelling  EventKind = "leg_cancelling"
	EventLegReadBack    EventKind = "leg_read_back"
	EventLegFilled      EventKind = "leg_filled"
	EventUnwindStarted  EventKind = "unwind_started"
	EventUnwindLeg      EventKind = "unwind_leg"
	EventUnwindDone     EventKind = "unwind_done"
	EventResolved       EventKind = "resolved"
)

// Event is one transition. Quantities are GROSS, like everything else here.
type Event struct {
	IntentID string
	At       time.Time
	Kind     EventKind

	// Leg is empty for events that belong to the intent rather than to a leg.
	Leg LegName

	ClientOrderID string
	VenueOrderID  string

	QtyCoin       float64
	PriceQuote    float64
	FilledQtyCoin float64

	// Outcome is set on EventResolved and is the invariant.
	Outcome Outcome

	// DetailVI is the operator-facing description; Err is the machine-facing
	// one. Both may be empty.
	DetailVI string
	Err      error
}

// String renders an event for a test failure message.
func (e Event) String() string {
	s := string(e.Kind)
	if e.Leg != "" {
		s += "/" + string(e.Leg)
	}
	if e.Err != nil {
		s += " err=" + e.Err.Error()
	}
	if e.DetailVI != "" {
		s += " (" + e.DetailVI + ")"
	}
	return s
}

// Recorder receives every transition.
//
// A Recorder that fails must NOT be able to change what the state machine
// does: losing the record of an unwind is bad, and abandoning the unwind
// because the record failed is catastrophic. So Open ignores the error beyond
// noting it, and that is a deliberate asymmetry rather than an oversight.
type Recorder interface {
	Record(ctx context.Context, ev Event) error
}

// MemoryRecorder keeps events in memory. Safe for concurrent use, because the
// state machine polls legs and a future version may place them concurrently.
type MemoryRecorder struct {
	mu     sync.Mutex
	events []Event
}

// NewMemoryRecorder returns an empty recorder.
func NewMemoryRecorder() *MemoryRecorder { return &MemoryRecorder{} }

// Record implements Recorder.
func (m *MemoryRecorder) Record(_ context.Context, ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
	return nil
}

// Events returns a copy of what was recorded.
func (m *MemoryRecorder) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}

// Kinds returns just the kinds, in order — what most assertions actually want.
func (m *MemoryRecorder) Kinds() []EventKind {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]EventKind, 0, len(m.events))
	for _, e := range m.events {
		out = append(out, e.Kind)
	}
	return out
}

// Has reports whether any event of this kind was recorded.
func (m *MemoryRecorder) Has(kind EventKind) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// Count returns how many events of this kind were recorded.
func (m *MemoryRecorder) Count(kind EventKind) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// Dump renders the whole stream, for a failing test's message.
func (m *MemoryRecorder) Dump() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := ""
	for i, e := range m.events {
		s += fmt.Sprintf("\n  %2d %s", i, e)
	}
	return s
}

// nopRecorder is used when a caller supplies none.
type nopRecorder struct{}

func (nopRecorder) Record(context.Context, Event) error { return nil }
