package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestDetourHoldPreservesWarmHomeStick(t *testing.T) {
	key := "test-detour-hold-conv"
	t.Cleanup(func() {
		sticks.Lock()
		delete(sticks.m, key)
		sticks.Unlock()
	})

	home := candidate{
		p:     provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "home@example.com"}},
		model: "claude-opus-5-5",
		rest:  "claude@home@example.com",
	}
	detour := candidate{
		p:     provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "backup@example.com"}},
		model: "claude-opus-5-5",
		rest:  "claude@backup@example.com",
	}

	// Home establishes a warm 1h prompt cache.
	answeredStanding(key, home, 1, Usage{CacheRead: 50_000, CacheWrite1h: 10_000}, false)

	// Home briefly rests for 30s (overload/transient) or is lane-saturated;
	// backup answers this turn -> home stick MUST remain intact with detourStart recorded.
	plannedCs := []candidate{detour, home}
	plannedOrder := []Weighed{
		{Known: true, Used: 40},
		{Known: true, Used: 45, Rest: &Rest{Why: failOther, Until: time.Now().Add(30 * time.Second)}},
	}
	answeredWithDetour(key, detour, 2, Usage{CacheWrite: 50_000}, false, nil, plannedCs, plannedOrder)

	sticks.Lock()
	st := sticks.m[key]
	sticks.Unlock()
	if st.who != home.who() {
		t.Fatalf("expected warm home stick %q to be preserved during brief detour, got %q", home.who(), st.who)
	}
	if st.detourStart.IsZero() {
		t.Fatal("expected detourStart to be recorded")
	}

	// Lane saturation (gaveWay[home.who()]) with no rest also holds home stick.
	laneOrder := []Weighed{{Known: true, Used: 40}, {Known: true, Used: 45}}
	answeredWithDetour(key, detour, 2, Usage{CacheWrite: 50_000}, false, map[string]bool{home.who(): true}, plannedCs, laneOrder)
	sticks.Lock()
	stLane := sticks.m[key]
	sticks.Unlock()
	if stLane.who != home.who() {
		t.Fatalf("lane saturation detour should preserve home stick %q, got %q", home.who(), stLane.who)
	}

	// Active request failure failover (no prior rest, no gaveWay) MUST migrate to detour.
	answeredWithDetour(key, detour, 2, Usage{CacheWrite: 50_000}, false, nil, plannedCs, laneOrder)
	sticks.Lock()
	stFail := sticks.m[key]
	sticks.Unlock()
	if stFail.who != detour.who() {
		t.Fatalf("active request failover should migrate to %q, got %q", detour.who(), stFail.who)
	}

	// Re-establish home stick, then test quota exhaustion MUST migrate immediately (no hold).
	answeredStanding(key, home, 3, Usage{CacheRead: 50_000, CacheWrite1h: 10_000}, false)
	quotaOrder := []Weighed{
		{Known: true, Used: 40},
		{Known: true, Used: 100, Rest: &Rest{Why: failQuota, Until: time.Now().Add(2 * time.Hour)}},
	}
	answeredWithDetour(key, detour, 4, Usage{CacheWrite: 50_000}, false, nil, plannedCs, quotaOrder)
	sticks.Lock()
	stAfter := sticks.m[key]
	sticks.Unlock()
	if stAfter.who != detour.who() {
		t.Fatalf("quota exhaustion on home must migrate to detour %q, got %q", detour.who(), stAfter.who)
	}
}

func TestSpentHysteresisPreventsPingPong(t *testing.T) {
	// Bound at 98%, peer at 95% (only 3 points healthier < 20 margin) -> stay put.
	if spentEvictable(Weighed{Known: true, Used: 98}, Weighed{Known: true, Used: 95}, 98) {
		t.Fatal("should not evict warm session at 98% when best peer is 95% (<20pt margin)")
	}
	// Bound at 98%, peer at 50% (48 points healthier >= 20 margin) -> evict.
	if !spentEvictable(Weighed{Known: true, Used: 98}, Weighed{Known: true, Used: 50}, 98) {
		t.Fatal("should evict at 98% when best peer is 50%")
	}
	// Bound at 100% -> always evict even if peer is 95%.
	if !spentEvictable(Weighed{Known: true, Used: 100}, Weighed{Known: true, Used: 95}, 98) {
		t.Fatal("should always evict at 100%")
	}
}

func TestClaudeUnhinted529DoesNotRestAccount(t *testing.T) {
	s := &Server{}
	c := candidate{
		p:     provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "alice@example.com"}},
		model: "claude-opus-5-5",
		rest:  "claude@alice@example.com",
	}
	r := s.restAfter(c, 529, http.Header{}, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if r.By != "platform-overload" || time.Until(r.Until) > 0 {
		t.Fatalf("unhinted Claude 529 should not rest account, got by=%q until=%v", r.By, time.Until(r.Until))
	}
	// With explicit Retry-After, honor the bounded retry-after rest.
	h := http.Header{}
	h.Set("Retry-After", "20")
	r2 := s.restAfter(c, 529, h, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if r2.By != "retry-after" || time.Until(r2.Until) <= 0 {
		t.Fatalf("hinted Claude 529 should honor Retry-After, got by=%q until=%v", r2.By, time.Until(r2.Until))
	}
}
