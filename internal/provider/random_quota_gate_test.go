package provider

import (
	"testing"
	"time"
)

func TestRandomQuotaGateDeterministicAndRespectsUserCaps(t *testing.T) {
	if _, _, ok := ParseRandomQuotaGate(""); ok {
		t.Fatal("empty should not parse")
	}
	if _, _, ok := ParseRandomQuotaGate("95-85"); ok {
		t.Fatal("inverted range should not parse")
	}
	lo, hi, ok := ParseRandomQuotaGate("93-97")
	if !ok || lo != 93 || hi != 97 {
		t.Fatalf("ParseRandomQuotaGate(93-97) = (%d,%d,%v)", lo, hi, ok)
	}

	t.Setenv("MAGPIE_CLAUDE_RANDOM_GATE", "93-97")
	p := Provider{ID: "claude"}
	caps := p.CapsOf("alice@example.com")
	if caps.RandomGate == nil {
		t.Fatal("expected RandomGate on claude provider when MAGPIE_CLAUDE_RANDOM_GATE is set")
	}
	reset := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	th1 := caps.RandomGate.Threshold("weekly", reset)
	th2 := caps.RandomGate.Threshold("weekly", reset.Add(2*time.Minute)) // same rounded hour
	if th1 < 93 || th1 > 97 {
		t.Fatalf("threshold %d out of [93,97]", th1)
	}
	if th1 != th2 {
		t.Fatalf("small reset jitter changed threshold: %d vs %d", th1, th2)
	}

	now := reset.Add(-2 * time.Hour)
	al := Allowance{{Name: "Weekly", Used: float64(th1), Resets: reset}}
	if h := al.CapHeld("claude-opus-5-5", caps, now); h == nil || h.Cap != th1 {
		t.Fatalf("expected CapHeld at random threshold %d, got %+v", th1, h)
	}
	// Just below random threshold must pass.
	alBelow := Allowance{{Name: "Weekly", Used: float64(th1 - 1), Resets: reset}}
	if h := alBelow.CapHeld("claude-opus-5-5", caps, now); h != nil {
		t.Fatalf("expected nil below random threshold %d, got %+v", th1, h)
	}

	// Explicit user cap overrides RandomGate.
	pUser := Provider{ID: "claude", AccountCaps: map[string]int{"alice@example.com": 80}}
	al85 := Allowance{{Name: "Weekly", Used: 85, Resets: reset}}
	if h := al85.CapHeld("claude-opus-5-5", pUser.CapsOf("alice@example.com"), now); h == nil || h.Cap != 80 {
		t.Fatalf("explicit user cap 80 should win over random gate, got %+v", h)
	}

	// Explicit 100% window exemption disables RandomGate on that window.
	pExempt := Provider{ID: "claude", AccountWindowCaps: map[string]map[string]int{"alice@example.com": {"weekly": 100}}}
	al99 := Allowance{{Name: "Weekly", Used: 99, Resets: reset}}
	if h := al99.CapHeld("claude-opus-5-5", pExempt.CapsOf("alice@example.com"), now); h != nil {
		t.Fatalf("explicit 100%% window exemption should bypass random gate, got %+v", h)
	}

	// Non-claude provider does not receive MAGPIE_CLAUDE_RANDOM_GATE.
	pCodex := Provider{ID: "codex"}
	if pCodex.CapsOf("alice@example.com").RandomGate != nil {
		t.Fatal("codex provider must not inherit MAGPIE_CLAUDE_RANDOM_GATE")
	}
}

func TestClaudeFailedReadKeepsLastKnownQuota(t *testing.T) {
	// Verify that loginUsageEntry fallback logic preserves last good Claude reading
	// on non-denial errors while still surfacing claudeDenied errors.
	rst := time.Now().Add(24 * time.Hour)
	prev := SubscriptionQuota{
		Provider: "claude",
		User:     "alice@example.com",
		Windows:  []QuotaWindow{{Name: "Weekly", Used: 86, ResetsAt: &rst}},
	}
	transientErr := SubscriptionQuota{
		Provider: "claude",
		User:     "alice@example.com",
		Error:    "upstream timeout reading usage",
	}
	if claudeDenied(transientErr.Error) {
		t.Fatal("transient timeout should not classify as claudeDenied")
	}
	// Simulate entry merge contract from loginUsageFor.
	apply := func(e SubscriptionQuota, read SubscriptionQuota) SubscriptionQuota {
		q := read
		if q.Error != "" && e.Error == "" && (q.Provider != "claude" || !claudeDenied(q.Error)) {
			kept := e
			kept.ReadError = q.Error
			q = kept
		}
		return q
	}
	got := apply(prev, transientErr)
	if got.Error != "" || len(got.Windows) != 1 || got.Windows[0].Used != 86 || got.ReadError != transientErr.Error {
		t.Fatalf("transient Claude probe error must preserve last good windows and set ReadError, got %+v", got)
	}
	denied := SubscriptionQuota{
		Provider: "claude",
		User:     "alice@example.com",
		Error:    "Error: 401 unauthorized",
	}
	gotDenied := apply(prev, denied)
	if gotDenied.Error == "" {
		t.Fatalf("claudeDenied error must not be masked by stale reading, got %+v", gotDenied)
	}
}
