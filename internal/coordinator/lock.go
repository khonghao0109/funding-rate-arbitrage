package coordinator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

// EngineID names the engine a symbol belongs to.
type EngineID string

const (
	// EngineCashAndCarry is Engine 1: long spot + short perp on ONE venue.
	EngineCashAndCarry EngineID = "engine_1_cash_and_carry"
	// EngineCrossPerp is Engine 2: long perp on one venue + short perp on another.
	EngineCrossPerp EngineID = "engine_2_cross_perp"
)

func (e EngineID) valid() bool { return e == EngineCashAndCarry || e == EngineCrossPerp }

// perpVenues is how many venues one position of this engine holds a PERP on.
func (e EngineID) perpVenues() int {
	if e == EngineCrossPerp {
		return 2
	}
	return 1
}

// SymbolState is a symbol's state in the lock table. See doc.go.
type SymbolState string

const (
	StateIdle     SymbolState = "idle"
	StateOccupied SymbolState = "occupied"
	StateConflict SymbolState = "conflict"
)

// LockSource says how a lock came to exist.
type LockSource string

const (
	// SourceAcquired: an engine won it through TryAcquire.
	SourceAcquired LockSource = "acquired"
	// SourceReconciledInferred: ReconcileActivePositions found a live position
	// with no lock record and attributed it by its shape.
	SourceReconciledInferred LockSource = "reconciled_inferred"
)

// SymbolLock is one symbol's entry, exactly as it is written to the lock file.
type SymbolLock struct {
	Symbol      string      `json:"symbol"`
	State       SymbolState `json:"state"`
	OwnerEngine EngineID    `json:"owner_engine,omitempty"`

	// IntentID is empty on a lock inferred from the venues: nobody knows which
	// intent opened that position until its engine adopts it (Adopt).
	IntentID   string `json:"intent_id,omitempty"`
	LockedAtMs int64  `json:"locked_at_ms,omitempty"`
	Details    string `json:"details,omitempty"`

	// Venues are the venues the owner's legs sit on.
	Venues []string   `json:"venues,omitempty"`
	Source LockSource `json:"source,omitempty"`

	// OrdersSentAtMs is when the owner said it was about to send its first order
	// under this lock (MarkOrdersSent) — written to disk BEFORE that order. Zero
	// means no order was ever sent under it: the only lock Withdraw may give back
	// without reading a venue, and a lock recovered from the file that no order of
	// its intent can still haunt (review 4.5k, m-g and N7).
	OrdersSentAtMs int64 `json:"orders_sent_at_ms,omitempty"`

	// The figure the lock was won with, and what it had deducted. Compared,
	// never computed here (doc.go).
	PriorityAPROnCapitalFrac float64 `json:"priority_apr_on_capital_frac,omitempty"`
	PriorityAPRBasisVI       string  `json:"priority_apr_basis_vi,omitempty"`

	// EvidenceVI is what the venues said at the last reconcile, and when.
	// ClearConflict takes EvidenceAtMs back, so an operator clears the
	// conflict they read and not a newer one.
	EvidenceVI   string `json:"evidence_vi,omitempty"`
	EvidenceAtMs int64  `json:"evidence_at_ms,omitempty"`
}

func (l SymbolLock) clone() SymbolLock {
	l.Venues = append([]string(nil), l.Venues...)
	return l
}

// VenueReader is what the coordinator asks a venue: the perp position, and the
// orders still resting there. A broker.Broker satisfies it. The orders matter
// as much as the position: a leg whose order is still working holds nothing
// yet and may hold something a second later.
type VenueReader interface {
	GetPosition(ctx context.Context, market broker.Market, symbol string) (broker.Position, error)
	OpenOrders(ctx context.Context, market broker.Market, symbol string) ([]broker.Order, error)
}

// Venue is one perp venue, by the name engines and the margin guard use for it.
type Venue struct {
	Name   string
	Reader VenueReader
}

// DefaultLocksPath is where the lock table lives, beside the execution portal's
// intent files.
const DefaultLocksPath = ".paper/coordinator-locks.json"

// DefaultContestWindow is how long the first request for an idle symbol waits
// for competitors. Long enough to catch two engines deciding on the same scan
// tick, short against a position held for days. It is a choice, not a
// measurement — nothing has measured how far apart the two engines' decisions
// on one symbol really land.
const DefaultContestWindow = 100 * time.Millisecond

// DefaultMaxCrossImbalanceFrac: one percent — the line between the two shapes a
// reconcile REPORTS for a long beside a short. It decides nothing (Config).
const DefaultMaxCrossImbalanceFrac = 0.01

// Config holds the coordinator's parameters.
type Config struct {
	// Path is the lock file. DefaultLocksPath when empty.
	Path string

	// Venues are every perp venue a lock's legs can sit on. Release and
	// ReconcileActivePositions read ALL of them.
	Venues []Venue

	// Symbols is the universe ReconcileActivePositions reads. A symbol outside
	// it has never been read from the venues and is never granted.
	Symbols []string

	// ContestWindow: DefaultContestWindow when zero or negative. There is no
	// first-come-first-served mode: the priority rule is the operator's.
	ContestWindow time.Duration

	// ReadTimeout bounds one venue's position read.
	ReadTimeout time.Duration

	// MaxCrossImbalanceFrac is how far apart, as a fraction of the larger, a long
	// on one venue and a short on the other may be and still be REPORTED as a
	// balanced pair (ShapeCrossPair) rather than ShapeCrossUnbalanced. It no
	// longer decides anything: both shapes are Engine 2's, and whether one is a
	// hedge is judged by Engine 2 against the two venues' own grids and minimum
	// notionals, which this package does not hold. A fraction cannot be that
	// judgement — 0.049 against 0.05 is 2% apart and one step of a 0.001 grid, a
	// hedge the executor keeps (review 4.5k, N4). DefaultMaxCrossImbalanceFrac
	// when zero.
	MaxCrossImbalanceFrac float64

	Now func() time.Time
}

// The errors a caller branches on. Each refusal wraps one of them.
var (
	ErrInvalidRequest  = errors.New("coordinator: yêu cầu không hợp lệ")
	ErrNotReconciled   = errors.New("coordinator: symbol chưa được đối soát với sàn — không cấp khóa trên trạng thái chưa đọc")
	ErrOccupied        = errors.New("coordinator: symbol đang BẬN")
	ErrConflict        = errors.New("coordinator: symbol ở trạng thái XUNG ĐỘT BẰNG CHỨNG — không ai mở, không ai nhả; người vận hành xử lý")
	ErrLostContest     = errors.New("coordinator: thua ưu tiên trong cửa sổ tranh chấp")
	ErrPersist         = errors.New("coordinator: không ghi được tệp khóa — không cấp và không nhả khóa nào chưa được ghi bền")
	ErrNotLocked       = errors.New("coordinator: symbol không bị khóa")
	ErrNotOwner        = errors.New("coordinator: không phải chủ khóa")
	ErrLockChanged     = errors.New("coordinator: khóa đã đổi trong lúc đọc sàn — đọc lại rồi thử lại")
	ErrVenueNotFlat    = errors.New("coordinator: sàn còn vị thế — khóa được GIỮ")
	ErrVenueUnreadable = errors.New("coordinator: không đọc được vị thế từ sàn — khóa được GIỮ cho tới khi chứng minh được phẳng")
	ErrOwnerLive       = errors.New("coordinator: khóa được cấp trong chính tiến trình này — chủ của nó đang sống; Adopt chỉ dành cho khóa từ tệp hoặc suy ra")
)

// entry is one lock in memory.
type entry struct {
	lock SymbolLock

	// version changes whenever the lock's state, owner or intent changes, so a
	// caller that read the venues without the mutex can tell whether it is
	// still looking at the same lock.
	version uint64

	// liveOwner: an engine in THIS process acquired or adopted it. Its
	// mid-operation shapes are not a conflict (doc.go).
	liveOwner bool

	// grantedHere: won through TryAcquire in THIS process — not loaded from the
	// file, not inferred, not adopted. Only such a lock can be withdrawn.
	grantedHere bool
}

type bid struct {
	req       AcquireRequest
	arrival   uint64
	withdrawn bool
	dec       Decision
	err       error
}

type contest struct {
	bids     []*bid
	done     chan struct{}
	resolved bool
}

// Coordinator is the lock table. Safe for concurrent use.
type Coordinator struct {
	cfg    Config
	venues map[string]bool

	mu             sync.Mutex
	locks          map[string]*entry
	contests       map[string]*contest
	universe       map[string]bool
	unverified     map[string]string
	reconciledAtMs int64
	arrivals       uint64
	versions       uint64
	load           LoadReport
}

// New builds a coordinator and loads its lock file. The report says what the
// file held, and whether a corrupt one was moved aside.
func New(cfg Config) (*Coordinator, LoadReport, error) {
	if cfg.Path == "" {
		cfg.Path = DefaultLocksPath
	}
	if cfg.ContestWindow <= 0 {
		cfg.ContestWindow = DefaultContestWindow
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	if !(cfg.MaxCrossImbalanceFrac > 0) {
		cfg.MaxCrossImbalanceFrac = DefaultMaxCrossImbalanceFrac
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c := &Coordinator{
		cfg: cfg, venues: map[string]bool{},
		locks: map[string]*entry{}, contests: map[string]*contest{},
		universe: map[string]bool{}, unverified: map[string]string{},
	}
	if len(cfg.Venues) == 0 {
		return nil, LoadReport{}, errors.New("coordinator: no venue to read positions from — a lock nobody can verify is not a lock")
	}
	for _, v := range cfg.Venues {
		switch {
		case strings.TrimSpace(v.Name) == "":
			return nil, LoadReport{}, errors.New("coordinator: a venue with no name")
		case v.Reader == nil:
			return nil, LoadReport{}, fmt.Errorf("coordinator: venue %q has no reader", v.Name)
		case c.venues[v.Name]:
			return nil, LoadReport{}, fmt.Errorf("coordinator: venue %q configured twice", v.Name)
		}
		c.venues[v.Name] = true
	}
	for _, s := range cfg.Symbols {
		if strings.TrimSpace(s) == "" {
			return nil, LoadReport{}, errors.New("coordinator: an empty symbol in the universe")
		}
		c.universe[s] = true
	}
	if len(c.universe) == 0 {
		return nil, LoadReport{}, errors.New("coordinator: an empty universe grants nothing")
	}

	report, loaded, err := loadLocks(cfg.Path, cfg.Now)
	if err != nil {
		return nil, report, err
	}
	for _, l := range loaded {
		if l.Symbol == "" {
			report.ProblemsVI = append(report.ProblemsVI, "một mục không có symbol bị bỏ qua")
			continue
		}
		if _, dup := c.locks[l.Symbol]; dup {
			// Two entries for one symbol is a file nobody should trust about
			// that symbol: it becomes a conflict, not whichever came last.
			c.locks[l.Symbol].lock.State = StateConflict
			c.locks[l.Symbol].lock.Details = "tệp khóa có HAI mục cho symbol này"
			report.ProblemsVI = append(report.ProblemsVI, l.Symbol+": hai mục trong tệp → xung đột")
			continue
		}
		if !(l.State == StateOccupied && l.OwnerEngine.valid() || l.State == StateConflict) {
			report.ProblemsVI = append(report.ProblemsVI, fmt.Sprintf("%s: trạng thái %q / chủ %q không hợp lệ → xung đột", l.Symbol, l.State, l.OwnerEngine))
			l.Details = fmt.Sprintf("tệp khóa ghi trạng thái %q, chủ %q — không hiểu được", l.State, l.OwnerEngine)
			l.State = StateConflict
		}
		c.versions++
		c.locks[l.Symbol] = &entry{lock: l.clone(), version: c.versions}
		c.universe[l.Symbol] = true
	}
	report.Locks = len(c.locks)
	c.load = report
	return c, report, nil
}

// AcquireRequest is one engine asking for one symbol.
type AcquireRequest struct {
	Symbol   string
	Engine   EngineID
	IntentID string
	Details  string

	// Venues the intent will place perp legs on: one for Engine 1, two for
	// Engine 2, each a configured venue name.
	Venues []string

	// PriorityAPROnCapitalFrac ranks competing requests inside one contest.
	// Annualized, ON CAPITAL, after whatever PriorityAPRBasisVI names — which is
	// required, so no figure travels without the words for what it is.
	PriorityAPROnCapitalFrac float64
	PriorityAPRBasisVI       string
}

// Decision is the answer. The error is non-nil exactly when Granted is false.
type Decision struct {
	Granted bool

	// Lock is the lock now held on a grant, and the holder on a refusal
	// (State idle when there was none).
	Lock SymbolLock

	// Contenders is how many live requests the contest compared.
	Contenders int
	ReasonVI   string
}

func (c *Coordinator) validate(req AcquireRequest) error {
	switch {
	case strings.TrimSpace(req.Symbol) == "":
		return fmt.Errorf("%w: không có symbol", ErrInvalidRequest)
	case !req.Engine.valid():
		return fmt.Errorf("%w: động cơ %q không có trong bảng", ErrInvalidRequest, req.Engine)
	case strings.TrimSpace(req.IntentID) == "":
		return fmt.Errorf("%w: không có id ý định — một khóa không truy được về lệnh nào", ErrInvalidRequest)
	case math.IsNaN(req.PriorityAPROnCapitalFrac) || math.IsInf(req.PriorityAPROnCapitalFrac, 0):
		return fmt.Errorf("%w: con số ưu tiên %v không hữu hạn", ErrInvalidRequest, req.PriorityAPROnCapitalFrac)
	case strings.TrimSpace(req.PriorityAPRBasisVI) == "":
		return fmt.Errorf("%w: con số ưu tiên không nói nó đã trừ những gì (PriorityAPRBasisVI)", ErrInvalidRequest)
	case len(req.Venues) != req.Engine.perpVenues():
		return fmt.Errorf("%w: %s giữ perp trên %d sàn, yêu cầu nêu %d", ErrInvalidRequest, req.Engine, req.Engine.perpVenues(), len(req.Venues))
	}
	seen := map[string]bool{}
	for _, v := range req.Venues {
		if !c.venues[v] {
			return fmt.Errorf("%w: sàn %q không được cấu hình — không đọc được vị thế ở đó", ErrInvalidRequest, v)
		}
		if seen[v] {
			return fmt.Errorf("%w: sàn %q nêu hai lần", ErrInvalidRequest, v)
		}
		seen[v] = true
	}
	return nil
}

// gateLocked refuses a symbol whose venue state is not known.
func (c *Coordinator) gateLocked(symbol string) error {
	switch {
	case c.reconciledAtMs == 0:
		return fmt.Errorf("%w: chưa đối soát lần nào từ khi khởi động", ErrNotReconciled)
	case !c.universe[symbol]:
		return fmt.Errorf("%w: %s nằm ngoài vũ trụ đối soát — chưa từng đọc vị thế của nó", ErrNotReconciled, symbol)
	}
	if why, ok := c.unverified[symbol]; ok {
		return fmt.Errorf("%w: %s: %s", ErrNotReconciled, symbol, why)
	}
	return nil
}

func refusedByHolder(e *entry) (Decision, error) {
	l := e.lock.clone()
	if l.State == StateConflict {
		return Decision{Lock: l, ReasonVI: ErrConflict.Error() + ": " + l.Details},
			fmt.Errorf("%w: %s — %s", ErrConflict, l.Symbol, l.Details)
	}
	reason := fmt.Sprintf("%s đang thuộc %s (ý định %q, %s)", l.Symbol, l.OwnerEngine, l.IntentID, l.Source)
	return Decision{Lock: l, ReasonVI: reason}, fmt.Errorf("%w: %s", ErrOccupied, reason)
}

// TryAcquire asks for an idle symbol. It returns once the contest the request
// joined has been decided, or when ctx ends — and a request whose caller gave up
// never leaves a lock behind.
func (c *Coordinator) TryAcquire(ctx context.Context, req AcquireRequest) (Decision, error) {
	idle := SymbolLock{Symbol: req.Symbol, State: StateIdle}
	if err := c.validate(req); err != nil {
		return Decision{Lock: idle, ReasonVI: err.Error()}, err
	}

	c.mu.Lock()
	if e := c.locks[req.Symbol]; e != nil {
		// Refused whoever holds it — the SAME intent included. Granting a lock
		// its own intent already holds would let a restarted engine re-send that
		// intent's opening orders under the ids it already used, before any
		// reconcile, on a lock not marked live (review 4.5k, M5). A lock that
		// survived a restart is taken back with Adopt, after the venues are read.
		defer c.mu.Unlock()
		return refusedByHolder(e)
	}
	if err := c.gateLocked(req.Symbol); err != nil {
		c.mu.Unlock()
		return Decision{Lock: idle, ReasonVI: err.Error()}, err
	}
	ct := c.contests[req.Symbol]
	if ct == nil {
		ct = &contest{done: make(chan struct{})}
		c.contests[req.Symbol] = ct
		symbol := req.Symbol
		time.AfterFunc(c.cfg.ContestWindow, func() { c.resolve(symbol, ct) })
	}
	c.arrivals++
	b := &bid{req: req, arrival: c.arrivals}
	b.req.Venues = append([]string(nil), req.Venues...)
	ct.bids = append(ct.bids, b)
	c.mu.Unlock()

	select {
	case <-ct.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return b.dec, b.err
	case <-ctx.Done():
		return c.withdrawBid(req.Symbol, ct, b, ctx.Err())
	}
}

// resolve decides one contest, once.
func (c *Coordinator) resolve(symbol string, ct *contest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ct.resolved {
		return
	}
	defer close(ct.done)
	if c.contests[symbol] == ct {
		delete(c.contests, symbol)
	}
	ct.resolved = true

	var live []*bid
	for _, b := range ct.bids {
		if !b.withdrawn {
			live = append(live, b)
		}
	}
	if len(live) == 0 {
		return
	}
	refuseAll := func(dec Decision, err error) {
		for _, b := range live {
			b.dec, b.err = dec, err
			b.dec.Contenders = len(live)
		}
	}
	// A reconcile may have occupied or un-verified the symbol while the window
	// was open.
	if e := c.locks[symbol]; e != nil {
		refuseAll(refusedByHolder(e))
		return
	}
	if err := c.gateLocked(symbol); err != nil {
		refuseAll(Decision{Lock: SymbolLock{Symbol: symbol, State: StateIdle}, ReasonVI: err.Error()}, err)
		return
	}

	sort.SliceStable(live, func(i, j int) bool {
		if live[i].req.PriorityAPROnCapitalFrac != live[j].req.PriorityAPROnCapitalFrac {
			return live[i].req.PriorityAPROnCapitalFrac > live[j].req.PriorityAPROnCapitalFrac
		}
		return live[i].arrival < live[j].arrival
	})
	winner := live[0]
	c.versions++
	e := &entry{version: c.versions, liveOwner: true, grantedHere: true, lock: SymbolLock{
		Symbol: symbol, State: StateOccupied, OwnerEngine: winner.req.Engine,
		IntentID: winner.req.IntentID, LockedAtMs: c.cfg.Now().UnixMilli(), Details: winner.req.Details,
		Venues: winner.req.Venues, Source: SourceAcquired,
		PriorityAPROnCapitalFrac: winner.req.PriorityAPROnCapitalFrac, PriorityAPRBasisVI: winner.req.PriorityAPRBasisVI,
	}}
	c.locks[symbol] = e
	if err := c.persistLocked(); err != nil {
		delete(c.locks, symbol)
		wrapped := fmt.Errorf("%w: %v", ErrPersist, err)
		refuseAll(Decision{Lock: SymbolLock{Symbol: symbol, State: StateIdle}, ReasonVI: wrapped.Error()}, wrapped)
		return
	}
	winner.dec = Decision{Granted: true, Lock: e.lock.clone(), Contenders: len(live),
		ReasonVI: fmt.Sprintf("thắng %d/%d yêu cầu với %s trên vốn (%s)", 1, len(live),
			pctText(winner.req.PriorityAPROnCapitalFrac), winner.req.PriorityAPRBasisVI)}
	for _, b := range live[1:] {
		reason := fmt.Sprintf("%s thuộc %s: %s trên vốn thắng %s của %s (bằng nhau thì ai đến trước)",
			symbol, winner.req.Engine, pctText(winner.req.PriorityAPROnCapitalFrac),
			pctText(b.req.PriorityAPROnCapitalFrac), b.req.Engine)
		b.dec = Decision{Lock: e.lock.clone(), Contenders: len(live), ReasonVI: reason}
		b.err = fmt.Errorf("%w: %s", ErrLostContest, reason)
	}
}

// withdrawBid takes back a request whose caller stopped waiting. A grant the
// caller never received is returned at once: nothing can have been sent on it.
func (c *Coordinator) withdrawBid(symbol string, ct *contest, b *bid, cause error) (Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idle := SymbolLock{Symbol: symbol, State: StateIdle}
	if !ct.resolved {
		b.withdrawn = true
		return Decision{Lock: idle, ReasonVI: "người gọi bỏ cuộc trước khi phân xử"}, cause
	}
	if !b.dec.Granted {
		return b.dec, errors.Join(cause, b.err)
	}
	if e := c.locks[symbol]; e != nil && e.lock.OwnerEngine == b.req.Engine && e.lock.IntentID == b.req.IntentID {
		delete(c.locks, symbol)
		if err := c.persistLocked(); err != nil {
			c.locks[symbol] = e
			return Decision{Lock: e.lock.clone(), ReasonVI: "người gọi bỏ cuộc sau khi được cấp, và tệp khóa không ghi được — khóa vẫn giữ"},
				errors.Join(cause, fmt.Errorf("%w: %v", ErrPersist, err))
		}
	}
	return Decision{Lock: idle, ReasonVI: "được cấp nhưng người gọi đã bỏ cuộc — khóa được trả lại, chưa lệnh nào có thể đã gửi"}, cause
}

// ErrNotWithdrawable: Withdraw applies only to a lock granted in this process
// under which no order was ever marked as sent.
var ErrNotWithdrawable = errors.New("coordinator: chỉ trả được khóa vừa được cấp trong chính tiến trình này mà CHƯA lệnh nào được gửi — mọi khóa khác phải nhả bằng Release (đọc sàn)")

// MarkOrdersSent records, durably and BEFORE the first order leaves, that the
// owner is about to send orders under its lock. From then on Withdraw refuses the
// lock: once an order may exist, only Release — which reads every venue — frees
// the symbol (review 4.5k, m-g). An already marked lock is left as it is. An
// error means the mark is not on disk, and the caller must not send.
func (c *Coordinator) MarkOrdersSent(symbol string, engine EngineID, intentID string) error {
	if intentID == "" {
		return fmt.Errorf("%w: đánh dấu gửi lệnh phải nêu id ý định", ErrInvalidRequest)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.locks[symbol]
	switch {
	case e == nil:
		return fmt.Errorf("%w: %s", ErrNotLocked, symbol)
	case e.lock.State != StateOccupied || e.lock.OwnerEngine != engine || e.lock.IntentID != intentID:
		return fmt.Errorf("%w: %s thuộc %s / ý định %q", ErrNotOwner, symbol, e.lock.OwnerEngine, e.lock.IntentID)
	case e.lock.OrdersSentAtMs != 0:
		return nil
	}
	// Zero means "never sent", so a clock at the epoch still marks.
	e.lock.OrdersSentAtMs = max(1, c.cfg.Now().UnixMilli())
	if err := c.persistLocked(); err != nil {
		e.lock.OrdersSentAtMs = 0
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Withdraw gives back a lock under which NOTHING was sent: an open refused
// before its first order. It reads no venue, on purpose — what the venues hold is
// not this intent's, and a lock kept because another engine holds the symbol is a
// symbol neither engine can ever take (review 4.5k, M6). It does not take the
// caller's word for "nothing was sent": a lock the owner marked with
// MarkOrdersSent is refused, and so is every lock not granted in THIS process.
func (c *Coordinator) Withdraw(symbol string, engine EngineID, intentID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.locks[symbol]
	switch {
	case e == nil:
		return fmt.Errorf("%w: %s", ErrNotLocked, symbol)
	case e.lock.State != StateOccupied || e.lock.OwnerEngine != engine || e.lock.IntentID != intentID || intentID == "":
		return fmt.Errorf("%w: %s thuộc %s / ý định %q", ErrNotOwner, symbol, e.lock.OwnerEngine, e.lock.IntentID)
	case !e.grantedHere || e.lock.Source != SourceAcquired:
		return fmt.Errorf("%w: %s (%s, không được cấp trong tiến trình này)", ErrNotWithdrawable, symbol, e.lock.Source)
	case e.lock.OrdersSentAtMs != 0:
		return fmt.Errorf("%w: %s — lệnh đã được đánh dấu gửi dưới khóa này lúc %d ms", ErrNotWithdrawable, symbol, e.lock.OrdersSentAtMs)
	}
	delete(c.locks, symbol)
	if err := c.persistLocked(); err != nil {
		c.locks[symbol] = e
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Holds reports whether this engine's intent holds the symbol right now.
func (c *Coordinator) Holds(symbol string, engine EngineID, intentID string) bool {
	if intentID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.locks[symbol]
	return e != nil && e.lock.State == StateOccupied && e.lock.OwnerEngine == engine && e.lock.IntentID == intentID
}

// QueryLock reads one symbol. The bool is false for an idle symbol.
func (c *Coordinator) QueryLock(symbol string) (SymbolLock, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.locks[symbol]; e != nil {
		return e.lock.clone(), true
	}
	return SymbolLock{Symbol: symbol, State: StateIdle}, false
}

// ListLocks lists every occupied or conflicting symbol, by symbol.
func (c *Coordinator) ListLocks() []SymbolLock {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Coordinator) snapshotLocked() []SymbolLock {
	out := make([]SymbolLock, 0, len(c.locks))
	for _, e := range c.locks {
		out = append(out, e.lock.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// VenuePosition is one venue's answer about one symbol.
type VenuePosition struct {
	Venue   string
	QtyCoin float64 // SIGNED: + long, − short

	// OpenOrders is how many orders on the symbol are still resting there.
	OpenOrders int

	// Read is true only when BOTH the position and the open orders were read.
	Read  bool
	ErrVI string
}

// ReleaseReport is what a release read on the venues.
type ReleaseReport struct {
	Symbol     string
	Released   bool
	Evidence   []VenuePosition
	EvidenceVI string
}

// Release frees a symbol for its owner, and only once EVERY configured venue
// reads the symbol's perp position as exactly zero with no order still resting.
func (c *Coordinator) Release(ctx context.Context, symbol string, engine EngineID, intentID string) (ReleaseReport, error) {
	report := ReleaseReport{Symbol: symbol}
	if intentID == "" {
		return report, fmt.Errorf("%w: nhả khóa phải nêu id ý định", ErrInvalidRequest)
	}
	c.mu.Lock()
	e := c.locks[symbol]
	switch {
	case e == nil:
		c.mu.Unlock()
		return report, fmt.Errorf("%w: %s", ErrNotLocked, symbol)
	case e.lock.State == StateConflict:
		c.mu.Unlock()
		return report, fmt.Errorf("%w: %s — %s", ErrConflict, symbol, e.lock.Details)
	case e.lock.OwnerEngine != engine || e.lock.IntentID != "" && e.lock.IntentID != intentID:
		holder := fmt.Sprintf("%s / ý định %q", e.lock.OwnerEngine, e.lock.IntentID)
		c.mu.Unlock()
		return report, fmt.Errorf("%w: %s thuộc %s, yêu cầu từ %s / %q", ErrNotOwner, symbol, holder, engine, intentID)
	}
	version := e.version
	c.mu.Unlock()

	if err := c.proveFlat(ctx, symbol, &report); err != nil {
		return report, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.locks[symbol]
	if cur == nil || cur.version != version {
		return report, fmt.Errorf("%w: %s", ErrLockChanged, symbol)
	}
	delete(c.locks, symbol)
	if err := c.persistLocked(); err != nil {
		c.locks[symbol] = cur
		return report, fmt.Errorf("%w: %v", ErrPersist, err)
	}
	report.Released = true
	return report, nil
}

// proveFlat reads every venue and fails unless each reads exactly zero with no
// order resting.
func (c *Coordinator) proveFlat(ctx context.Context, symbol string, report *ReleaseReport) error {
	report.Evidence = c.readVenues(ctx, symbol)
	report.EvidenceVI = evidenceVI(report.Evidence)
	for _, ev := range report.Evidence {
		if !ev.Read {
			return fmt.Errorf("%w: %s trên %s: %s", ErrVenueUnreadable, symbol, ev.Venue, ev.ErrVI)
		}
	}
	for _, ev := range report.Evidence {
		if ev.QtyCoin != 0 || ev.OpenOrders > 0 {
			return fmt.Errorf("%w: %s — %s", ErrVenueNotFlat, symbol, report.EvidenceVI)
		}
	}
	return nil
}

// readVenues asks every venue about one symbol, all venues at once. No lock is
// held.
func (c *Coordinator) readVenues(ctx context.Context, symbol string) []VenuePosition {
	out := make([]VenuePosition, len(c.cfg.Venues))
	var wg sync.WaitGroup
	for i, v := range c.cfg.Venues {
		wg.Add(1)
		go func(i int, v Venue) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout)
			defer cancel()
			out[i] = readPosition(rctx, v, symbol)
			if !out[i].Read {
				return
			}
			orders, err := v.Reader.OpenOrders(rctx, broker.MarketFuturesUSDM, symbol)
			if err != nil {
				out[i].Read, out[i].ErrVI = false, "lệnh đang treo: "+err.Error()
				return
			}
			out[i].OpenOrders = countOrders(orders, symbol)
		}(i, v)
	}
	wg.Wait()
	return out
}

// readPosition reads one venue's perp position for one symbol.
func readPosition(ctx context.Context, v Venue, symbol string) VenuePosition {
	pos, err := v.Reader.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	switch {
	case err != nil:
		return VenuePosition{Venue: v.Name, ErrVI: "vị thế: " + err.Error()}
	case math.IsNaN(pos.QtyCoin) || math.IsInf(pos.QtyCoin, 0):
		return VenuePosition{Venue: v.Name, ErrVI: fmt.Sprintf("khối lượng %v không phải số", pos.QtyCoin)}
	case pos.Symbol != "" && pos.Symbol != symbol:
		return VenuePosition{Venue: v.Name, ErrVI: fmt.Sprintf("hỏi %s, sàn trả vị thế của %s", symbol, pos.Symbol)}
	}
	return VenuePosition{Venue: v.Name, QtyCoin: pos.QtyCoin, Read: true}
}

// countOrders counts the orders on one symbol that can still change a position.
func countOrders(orders []broker.Order, symbol string) int {
	n := 0
	for _, o := range orders {
		if o.Symbol == symbol && !o.Status.Done() {
			n++
		}
	}
	return n
}

// Adopt binds a lock that survived a restart to the intent its engine
// recovered, and marks the owner live in this process.
func (c *Coordinator) Adopt(symbol string, engine EngineID, intentID string) (SymbolLock, error) {
	if intentID == "" {
		return SymbolLock{}, fmt.Errorf("%w: nhận khóa phải nêu id ý định", ErrInvalidRequest)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.locks[symbol]
	switch {
	case e == nil:
		return SymbolLock{Symbol: symbol, State: StateIdle}, fmt.Errorf("%w: %s", ErrNotLocked, symbol)
	case e.lock.State == StateConflict:
		return e.lock.clone(), fmt.Errorf("%w: %s — %s", ErrConflict, symbol, e.lock.Details)
	case e.lock.OwnerEngine != engine || e.lock.IntentID != "" && e.lock.IntentID != intentID:
		return e.lock.clone(), fmt.Errorf("%w: %s thuộc %s / ý định %q", ErrNotOwner, symbol, e.lock.OwnerEngine, e.lock.IntentID)
	case e.grantedHere:
		// Its owner is operating it now — between the grant and its first order,
		// say. Adopting it would give the same intent a second record the owner
		// does not know about (review 4.5k round 3, minor 4).
		return e.lock.clone(), fmt.Errorf("%w: %s", ErrOwnerLive, symbol)
	}
	prev, prevLive, prevVersion := e.lock.clone(), e.liveOwner, e.version
	e.lock.IntentID, e.liveOwner = intentID, true
	c.versions++
	e.version = c.versions
	if err := c.persistLocked(); err != nil {
		e.lock, e.liveOwner, e.version = prev, prevLive, prevVersion
		return prev, fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return e.lock.clone(), nil
}

// ClearConflict is the operator's way out of a conflict: it takes the evidence
// stamp the operator read, so a newer conflict is not cleared by an older look,
// and it still requires every venue to read flat now.
func (c *Coordinator) ClearConflict(ctx context.Context, symbol string, seenEvidenceAtMs int64) (ReleaseReport, error) {
	report := ReleaseReport{Symbol: symbol}
	c.mu.Lock()
	e := c.locks[symbol]
	switch {
	case e == nil:
		c.mu.Unlock()
		return report, fmt.Errorf("%w: %s", ErrNotLocked, symbol)
	case e.lock.State != StateConflict:
		c.mu.Unlock()
		return report, fmt.Errorf("%w: %s không ở trạng thái xung đột — chủ khóa nhả nó bằng Release", ErrInvalidRequest, symbol)
	case e.lock.EvidenceAtMs != seenEvidenceAtMs:
		at := e.lock.EvidenceAtMs
		c.mu.Unlock()
		return report, fmt.Errorf("%w: bằng chứng của %s đã được đọc lại lúc %d, người vận hành xem bản lúc %d — xem lại rồi xác nhận",
			ErrLockChanged, symbol, at, seenEvidenceAtMs)
	}
	version := e.version
	c.mu.Unlock()

	if err := c.proveFlat(ctx, symbol, &report); err != nil {
		return report, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.locks[symbol]
	if cur == nil || cur.version != version || cur.lock.EvidenceAtMs != seenEvidenceAtMs {
		return report, fmt.Errorf("%w: %s", ErrLockChanged, symbol)
	}
	delete(c.locks, symbol)
	if err := c.persistLocked(); err != nil {
		c.locks[symbol] = cur
		return report, fmt.Errorf("%w: %v", ErrPersist, err)
	}
	report.Released = true
	return report, nil
}

// Status is the coordinator's view of its own knowledge.
type Status struct {
	ReconciledAtMs int64
	Unverified     map[string]string
	Load           LoadReport
	Locks          []SymbolLock
}

// Status copies it.
func (c *Coordinator) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{ReconciledAtMs: c.reconciledAtMs, Unverified: map[string]string{}, Load: c.load, Locks: c.snapshotLocked()}
	for k, v := range c.unverified {
		st.Unverified[k] = v
	}
	return st
}

func pctText(frac float64) string {
	return strconv.FormatFloat(frac*100, 'f', 2, 64) + "%"
}

func evidenceVI(ev []VenuePosition) string {
	parts := make([]string, 0, len(ev))
	for _, v := range ev {
		if !v.Read {
			parts = append(parts, v.Venue+" KHÔNG ĐỌC ĐƯỢC ("+v.ErrVI+")")
			continue
		}
		qty := strconv.FormatFloat(v.QtyCoin, 'f', -1, 64)
		if v.QtyCoin > 0 {
			qty = "+" + qty
		}
		if v.OpenOrders > 0 {
			qty += fmt.Sprintf(" (%d lệnh đang treo)", v.OpenOrders)
		}
		parts = append(parts, v.Venue+" "+qty)
	}
	return strings.Join(parts, ", ")
}
