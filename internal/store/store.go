package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// The pure-Go SQLite driver. Chosen over the cgo one (mattn/go-sqlite3)
	// because it keeps CGO_ENABLED=0 builds working: this service is meant to
	// end up on a small VPS as a single static binary, and a C toolchain on the
	// deploy target is a dependency nobody wants to discover at phase 4. The
	// cost is a larger module graph and slower bulk writes, which at 36 series
	// and a few hundred thousand rows a day is not a constraint.
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is written to SQLite's own PRAGMA user_version. A store opened
// at a HIGHER version than this binary knows is refused rather than migrated
// backwards — reading a newer file with older code is how a column quietly
// stops being written.
const schemaVersion = 1

// Store is the SQLite persistence layer.
//
// One process owns the file. MaxOpenConns is 1 on purpose: SQLite's writer is
// exclusive anyway, and serialising here converts every possible SQLITE_BUSY
// into ordinary waiting inside the pool. At this volume — a few hundred
// thousand rows a day — the throughput that costs is not measurable, and the
// class of intermittent failure it removes is the expensive kind.
type Store struct {
	db *sql.DB

	// now is the clock for recorded_at stamps and retention, injectable so the
	// pruning tests do not have to wait a year.
	now func() time.Time
}

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	// The pragmas below ride in the DSN as a query string, so a '?' in the path
	// would be read as the start of them and the store would silently open a
	// different (shorter) file than the one configured.
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("store: path %q contains ? or #, which the driver reads as DSN parameters", path)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	// The pragmas are DSN parameters rather than statements because
	// database/sql opens connections lazily: a PRAGMA executed once after Open
	// would apply to whichever connection happened to run it, and the next one
	// would come up with journal_mode=DELETE. WAL is what lets a reader work
	// while the backfill writes; busy_timeout turns a lock collision into a
	// wait instead of an error; synchronous=NORMAL is the documented safe
	// setting under WAL.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database. WAL content is checkpointed by SQLite on the
// last connection closing, so a clean shutdown leaves one file behind.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for callers that need their own query — the phase-3
// backtest reads this corpus and should not have to route every projection
// through a method here.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("store: database is at schema version %d, this binary knows %d — "+
			"an older binary writing a newer file silently stops filling columns it does not know about",
			version, schemaVersion)
	}
	// Every statement is CREATE ... IF NOT EXISTS, so applying it to an
	// existing file at the same version is a no-op and the schema file stays
	// the single description of the shape.
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	if version != schemaVersion {
		// PRAGMA does not take a bound parameter.
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("store: set schema version: %w", err)
		}
	}
	return nil
}
