package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestMasterOverview_ReturnsValidJSON(t *testing.T) {
	p := testPortal(t, false)
	rec := do(t, p, http.MethodGet, "/api/master/overview", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var ov masterOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if ov.Mode != "master" {
		t.Errorf("expected mode 'master', got '%s'", ov.Mode)
	}
	if !ov.Engine1.Enabled {
		t.Errorf("expected Engine 1 enabled")
	}
	if ov.ReadAtMs <= 0 {
		t.Errorf("expected positive ReadAtMs")
	}
}

func TestMasterPositions_ReturnsValidJSON(t *testing.T) {
	p := testPortal(t, false)
	rec := do(t, p, http.MethodGet, "/api/master/positions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp masterPositionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp.ReadAtMs <= 0 {
		t.Errorf("expected positive ReadAtMs")
	}
}
