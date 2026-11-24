package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestBuildMeUsageResponseCompatibleWithAsterGateMods(t *testing.T) {
	now := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	rst5h := now.Add(3 * time.Hour)
	rst7d := now.Add(72 * time.Hour)
	readAt := now.Add(-1 * time.Minute)

	qs := []provider.Quota{
		{
			Provider: "claude",
			User:     "alice@example.com",
			Plan:     "default_claude_max_20x",
			ReadAt:   &readAt,
			Windows: []provider.QuotaSpan{
				{Name: "5 hours", Used: 25, Remaining: 75, ResetsAt: &rst5h},
				{Name: "Weekly", Used: 60, Remaining: 40, ResetsAt: &rst7d},
				{Name: "7 days · Fable", Used: 10, Remaining: 90, ResetsAt: &rst7d},
			},
		},
		{
			Provider: "agy-cli",
			User:     "bob@example.com",
			Plan:     "Pro",
			ReadAt:   &readAt,
			Windows: []provider.QuotaSpan{
				{Name: "Gemini · 5 hours", Used: 40, Remaining: 60, ResetsAt: &rst5h},
				{Name: "Gemini · Weekly", Used: 15, Remaining: 85, ResetsAt: &rst7d},
			},
		},
	}

	resp := buildMeUsageResponse("my-key", qs, "claude", "alice@example.com", now)
	if len(resp.Accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(resp.Accounts))
	}
	cl := resp.Accounts[0]
	if !cl.Current || cl.Provider != "claude" || cl.Plan != "max" || cl.PlanRaw != "default_claude_max_20x" {
		t.Fatalf("unexpected claude account mapping: %+v", cl)
	}
	if len(cl.Windows) != 3 || cl.Windows[0].Kind != "five_hour" || cl.Windows[0].UsedRatio != 0.25 || cl.Windows[2].Kind != "weekly_fable" {
		t.Fatalf("unexpected claude windows: %+v", cl.Windows)
	}
	agy := resp.Accounts[1]
	if agy.Current || agy.Provider != "antigravity" || len(agy.Windows) != 2 || agy.Windows[0].Kind != "gemini_five_hour" || agy.Windows[0].ScopedModel != "gemini" {
		t.Fatalf("unexpected antigravity mapping: %+v", agy)
	}
}

func TestManagedSettingsAndEventLogging(t *testing.T) {
	s := New()
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, ManagedSettingsPath, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /managed/settings = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag on /managed/settings")
	}

	// Second request with If-None-Match returns 304 Not Modified.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, ManagedSettingsPath, nil)
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("expected 304 on matching ETag, got %d", rec2.Code)
	}

	// Telemetry endpoint accepts POST cleanly.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/api/event_logging/batch", nil)
	req3.RemoteAddr = "127.0.0.1:12345"
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("POST /api/event_logging/batch = %d", rec3.Code)
	}
	var ack map[string]any
	if err := json.Unmarshal(rec3.Body.Bytes(), &ack); err != nil || ack["accepted"] != true {
		t.Fatalf("unexpected telemetry body: %s", rec3.Body.String())
	}
}
