// Command paperledger is the paper-trading ledger of PLAN.md step 4.3 (Q12):
// a SEPARATE process that reads the signal journal and the corpus the
// step-3.5 scanner writes, prices every decision that scanner journalled —
// fills on the measured book, taker fees at the schedule the row recorded,
// funding at each settlement, the pair marked to mid — and serves a virtual
// account with an equity curve on its own port.
//
//	go run ./cmd/paperledger                                 # config.yaml's store, .paper/started_at → now, :8086
//	go run ./cmd/paperledger -db copy.db -from "2026-09-11 14:15:50 +0700" -port 8087
//	go run ./cmd/paperledger -once > ledger.json              # one build, JSON on stdout, exit
//
// It opens the database READ-ONLY (store.OpenReadOnly — SQLite refuses every
// write) and never touches the scanner process: no restart, no rebuild, no
// shared port. It decides nothing: production EvaluateEntry/EvaluateExit
// decided, the journal recorded, this prices. There is no go-live switch here
// and there will not be one — a demo with a switch to real money is the wrong
// product (PLAN 4.3).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "configuration file (its storage.path is the database unless -db is given)")
	dbPath := flag.String("db", "", "SQLite file to read (read-only); overrides config.yaml's storage.path — point it at a COPY while the 3.5 run is live if you want to be certain")
	from := flag.String("from", "", "window start: YYYY-MM-DD, YYYY-MM-DDTHH:MM:SS (UTC), RFC3339, or 'YYYY-MM-DD HH:MM:SS -0700' (default: the stamp in -started-at-file, else the oldest journal row)")
	to := flag.String("to", "", "window end, exclusive, same forms (default: now, re-read on every rebuild)")
	startedAtFile := flag.String("started-at-file", ".paper/started_at", "file holding the run's launch stamp, used when -from is empty")
	capital := flag.Float64("capital-quote", 10_000_000, "starting VIRTUAL capital in the quote asset — 10M affords every slot of the 74-series universe at 50k × 2 legs (an unlevered short posts its whole notional); lower it to watch the account ration entries")
	bind := flag.String("bind", "127.0.0.1", "interface the paper UI listens on; loopback by default because the page is unauthenticated")
	port := flag.String("port", "8086", "HTTP port for the paper UI and /api/ledger (never 8082 or 8085 — those belong to the scanner runs)")
	refresh := flag.Duration("refresh", 5*time.Minute, "how often the ledger is rebuilt from the database")
	markEvery := flag.Duration("mark-every", time.Hour, "how often open positions are marked to mid on the equity curve")
	once := flag.Bool("once", false, "build the ledger once, print the JSON report to stdout, and exit")
	flag.Parse()

	if !*once && (*port == "8082" || *port == "8085") {
		log.Fatalf("port %s belongs to a scanner run (8082 soak, 8085 step 3.5) — pick another", *port)
	}
	if *markEvery <= 0 {
		log.Fatalf("-mark-every must be positive (got %s): the equity curve is read off its marks", *markEvery)
	}
	if *refresh <= 0 {
		log.Fatalf("-refresh must be positive (got %s)", *refresh)
	}
	path := *dbPath
	if path == "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			log.Fatalf("configuration: %v", err)
		}
		if !cfg.Storage.Enabled || cfg.Storage.Path == "" {
			log.Fatalf("config.yaml has storage disabled — there is no journal to read; pass -db")
		}
		path = cfg.Storage.Path
	}
	db, err := store.OpenReadOnly(path)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer db.Close()
	log.Printf("paperledger: reading %s READ-ONLY (SQLite mode=ro; every write is refused by the engine)", path)

	fromMs, fromNote, err := resolveFrom(*from, *startedAtFile)
	if err != nil {
		log.Fatalf("window: %v", err)
	}
	var toMs int64 // 0 = now at each rebuild
	if *to != "" {
		t, err := config.ParseRunStamp(*to)
		if err != nil {
			log.Fatalf("-to %q: %v", *to, err)
		}
		toMs = t.UnixMilli()
	}
	log.Printf("paperledger: window from %s (%s) to %s · virtual capital %.0f quote · marks every %s",
		stampMs(fromMs), fromNote, orNow(toMs), *capital, *markEvery)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	build := func() (Report, error) {
		in := Inputs{DB: db, FromMs: fromMs, ToMs: toMs, NowMs: time.Now().UnixMilli(),
			CapitalQuote: *capital, MarkEvery: *markEvery, FromNoteVI: fromNote}
		if in.FromMs == 0 {
			// No stamp anywhere: the oldest journal row starts the window.
			oldest, err := oldestJournalRow(ctx, db)
			if err != nil {
				return Report{}, err
			}
			in.FromMs = oldest
		}
		return Build(ctx, in)
	}

	if *once {
		report, err := build()
		if err != nil {
			log.Fatalf("build: %v", err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			log.Fatalf("encode: %v", err)
		}
		return
	}

	srv := newServer()
	first, err := build()
	if err != nil {
		log.Fatalf("build: %v", err)
	}
	srv.set(first)
	log.Printf("paperledger: %s", first.SummaryLine())

	go func() {
		ticker := time.NewTicker(*refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				report, err := build()
				if err != nil {
					log.Printf("paperledger: rebuild failed: %v (serving the previous build)", err)
					continue
				}
				srv.set(report)
				log.Printf("paperledger: %s", report.SummaryLine())
			}
		}
	}()

	httpServer := &http.Server{Addr: *bind + ":" + *port, Handler: srv.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("paperledger: PAPER UI on http://%s:%s (JSON at /api/ledger)", *bind, *port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server stopped: %v", err)
			stop()
		}
	}()
	<-ctx.Done()
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(closeCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
}

// resolveFrom picks the window start: the flag, else the launch stamp the
// relaunch script wrote, else 0 (the caller reads the oldest journal row).
func resolveFrom(flagValue, startedAtFile string) (int64, string, error) {
	if flagValue != "" {
		t, err := config.ParseRunStamp(flagValue)
		if err != nil {
			return 0, "", fmt.Errorf("-from %q: %w", flagValue, err)
		}
		return t.UnixMilli(), "-from", nil
	}
	t, err := config.ReadRunStamp(startedAtFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "oldest journal row (no " + startedAtFile + ")", nil
		}
		return 0, "", err
	}
	return t.UnixMilli(), startedAtFile, nil
}

func stampMs(ms int64) string {
	if ms == 0 {
		return "—"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05") + " UTC"
}

func orNow(ms int64) string {
	if ms == 0 {
		return "now"
	}
	return stampMs(ms)
}
