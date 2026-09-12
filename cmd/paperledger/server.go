package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"sync"
)

//go:embed ui/index.html
var indexHTML []byte

// server holds the newest build and serves it. Read-only by construction:
// there is no route that changes anything, and the UI has no control that
// could — PLAN 4.3: no go-live switch.
type server struct {
	mu     sync.RWMutex
	report Report
}

func newServer() *server { return &server{} }

func (s *server) set(r Report) {
	s.mu.Lock()
	s.report = r
	s.mu.Unlock()
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Execution-Mode", "paper")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/api/ledger", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		s.mu.RLock()
		report := s.report
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Execution-Mode", "paper")
		if err := json.NewEncoder(w).Encode(report); err != nil {
			log.Printf("paperledger: write /api/ledger: %v", err)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Execution-Mode", "paper")
		_, _ = w.Write([]byte("ok paper\n"))
	})
	return mux
}
