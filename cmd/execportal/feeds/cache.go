package feeds

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// cache shares one upstream read between tabs inside a TTL, and between
// concurrent callers while it is in flight. A failed read is shared for
// errorTTL only. Expired entries are dropped whenever a new one is added, so
// the map holds what is fresh and nothing else.
type cache struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	done   chan struct{}
	value  body
	err    error
	readAt time.Time
	ttl    time.Duration
}

func newCache(now func() time.Time) *cache {
	return &cache{now: now, entries: map[string]*cacheEntry{}}
}

func (c *cache) get(key string, ttl time.Duration, load func() (body, error)) (body, time.Time, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		select {
		case <-e.done:
			if c.now().Sub(e.readAt) < e.ttl {
				c.mu.Unlock()
				return e.value, e.readAt, e.err
			}
		default:
			c.mu.Unlock()
			<-e.done
			return e.value, e.readAt, e.err
		}
	}
	now := c.now()
	for k, e := range c.entries {
		select {
		case <-e.done:
			if now.Sub(e.readAt) >= e.ttl {
				delete(c.entries, k)
			}
		default:
		}
	}
	e := &cacheEntry{done: make(chan struct{}), ttl: ttl}
	c.entries[key] = e
	c.mu.Unlock()

	func() {
		// A load that panics must still release its waiters.
		defer func() {
			if r := recover(); r != nil {
				e.err = fmt.Errorf("đọc nguồn bị panic: %v", r)
			}
			if e.err != nil {
				e.ttl = min(e.ttl, errorTTL)
			}
			e.readAt = c.now()
			close(e.done)
		}()
		e.value, e.err = load()
	}()
	return e.value, e.readAt, e.err
}

func (c *cache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *cache) invalidate() {
	c.mu.Lock()
	c.entries = map[string]*cacheEntry{}
	c.mu.Unlock()
}

// writeError answers in the portal's error shape: error_code and error_vi.
func writeError(w http.ResponseWriter, status int, code, messageVI string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Execution-Mode", "feed")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		ErrorCode string `json:"error_code"`
		ErrorVI   string `json:"error_vi"`
	}{code, messageVI})
}

// quote bounds a caller-controlled string before it is echoed.
func quote(s string) string {
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return "\"" + strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s) + "\""
}
