package provider

import (
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"
)

// RandomQuotaGate deterministically chooses a per-account, per-window stop
// threshold in [MinPct, MaxPct] using FNV-64a over (account, window, resetHour).
// It prevents a fleet of pooled Claude accounts without explicit static caps
// from hitting 100% or stopping at identical fixed percentages in lockstep,
// while drawing a fresh stop point after each window resets.
//
// Configured via MAGPIE_CLAUDE_RANDOM_GATE="93-97" (empty disables it).
// Explicit per-account or per-window user caps always take precedence.
type RandomQuotaGate struct {
	Account string
	MinPct  int
	MaxPct  int
}

// ParseRandomQuotaGate parses "min-max" (e.g. "93-97" or "85-95") into bounds
// clamped to [MinCap, MaxCap]. Returns ok=false for empty or invalid input.
func ParseRandomQuotaGate(raw string) (minPct, maxPct int, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, 0, false
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || lo < MinCap || hi > MaxCap || lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

func envClaudeRandomGate(user string) *RandomQuotaGate {
	minPct, maxPct, ok := ParseRandomQuotaGate(os.Getenv("MAGPIE_CLAUDE_RANDOM_GATE"))
	if !ok {
		return nil
	}
	return &RandomQuotaGate{
		Account: accountKey(user),
		MinPct:  minPct,
		MaxPct:  maxPct,
	}
}

// Threshold returns the deterministic stop threshold in [MinPct, MaxPct] for
// this account's window instance. ResetsAt is rounded to the nearest hour so
// small upstream timestamp jitter stays on the same line until rollover.
func (g *RandomQuotaGate) Threshold(windowName string, resetsAt time.Time) int {
	if g == nil || g.MinPct < MinCap || g.MaxPct > MaxCap || g.MinPct > g.MaxPct {
		return 0
	}
	if g.MinPct == g.MaxPct {
		return g.MinPct
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(g.Account))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(WindowCapID(windowName)))
	_, _ = h.Write([]byte{0})
	if !resetsAt.IsZero() {
		_, _ = h.Write([]byte(strconv.FormatInt(resetsAt.UTC().Round(time.Hour).Unix(), 10)))
	}
	span := uint64(g.MaxPct - g.MinPct + 1)
	return g.MinPct + int(h.Sum64()%span)
}
