package lowmem

import (
	"math"
	"runtime/debug"
	"testing"
	"time"
)

func TestLowMemDefaultsAndOverrides(t *testing.T) {
	if Enabled() {
		t.Fatal("lowmem should be disabled by default")
	}
	if got := ClaudeIdleMax(6); got != 6 {
		t.Fatalf("default ClaudeIdleMax = %d, want 6", got)
	}
	if got := ClaudeIdleTTL(time.Hour); got != time.Hour {
		t.Fatalf("default ClaudeIdleTTL = %v, want 1h", got)
	}
	if got := ClaudeRunRSSFloor(1 << 30); got != 1<<30 {
		t.Fatalf("default ClaudeRunRSSFloor = %d, want 1 GiB", got)
	}

	t.Setenv("MAGPIE_LOW_MEMORY", "1")
	if !Enabled() {
		t.Fatal("expected Enabled()=true")
	}
	if got := ClaudeIdleMax(6); got != 2 {
		t.Fatalf("lowmem ClaudeIdleMax = %d, want 2", got)
	}
	if got := ClaudeParkedMax(16); got != 6 {
		t.Fatalf("lowmem ClaudeParkedMax = %d, want 6", got)
	}
	if got := ClaudeIdleTTL(time.Hour); got != 5*time.Minute {
		t.Fatalf("lowmem ClaudeIdleTTL = %v, want 5m", got)
	}
	if got := ClaudeRunRSSFloor(1 << 30); got != 384<<20 {
		t.Fatalf("lowmem ClaudeRunRSSFloor = %d, want 384 MiB", got)
	}
	if got := CodexImageBudget(512 << 20); got != 128<<20 {
		t.Fatalf("lowmem CodexImageBudget = %d, want 128 MiB", got)
	}
	if got := BlockPriceLinger(30 * time.Second); got != 5*time.Second {
		t.Fatalf("lowmem BlockPriceLinger = %v, want 5s", got)
	}

	// Explicit fine-grained overrides beat MAGPIE_LOW_MEMORY defaults.
	t.Setenv("MAGPIE_CLAUDE_IDLE_MAX", "3")
	t.Setenv("MAGPIE_CLAUDE_IDLE_TTL", "10m")
	t.Setenv("MAGPIE_CLAUDE_RUN_RSS_MB", "512")
	if got := ClaudeIdleMax(6); got != 3 {
		t.Fatalf("override ClaudeIdleMax = %d, want 3", got)
	}
	if got := ClaudeIdleTTL(time.Hour); got != 10*time.Minute {
		t.Fatalf("override ClaudeIdleTTL = %v, want 10m", got)
	}
	if got := ClaudeRunRSSFloor(1 << 30); got != 512<<20 {
		t.Fatalf("override ClaudeRunRSSFloor = %d, want 512 MiB", got)
	}
}

func TestApplyRuntimeTuningRespectsUserGOMEMLIMIT(t *testing.T) {
	prevLimit := debug.SetMemoryLimit(-1)
	prevGC := debug.SetGCPercent(-1)
	debug.SetGCPercent(prevGC)
	t.Cleanup(func() {
		debug.SetMemoryLimit(prevLimit)
		debug.SetGCPercent(prevGC)
	})

	t.Setenv("MAGPIE_LOW_MEMORY", "1")
	t.Setenv("GOMEMLIMIT", "2GiB")
	t.Setenv("GOGC", "100")
	limit, gc := ApplyRuntimeTuning()
	if limit != 0 || gc != -1 {
		t.Fatalf("explicit GOMEMLIMIT/GOGC must not be overridden, got limit=%d gc=%d", limit, gc)
	}

	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("GOGC", "")
	t.Setenv("MAGPIE_MEM_LIMIT_MB", "512")
	limit, gc = ApplyRuntimeTuning()
	if limit != 512<<20 || gc != lowMemGCPercent {
		t.Fatalf("expected 512 MiB / gc 50, got limit=%d gc=%d", limit, gc)
	}
	if cur := debug.SetMemoryLimit(-1); cur == math.MaxInt64 || cur != 512<<20 {
		t.Fatalf("runtime memory limit not applied: %d", cur)
	}
}
