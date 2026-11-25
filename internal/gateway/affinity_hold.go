package gateway

import (
	"os"
	"strings"
	"time"
)

// Detour-hold keeps a warm session pinned to its home account when a single
// request detours onto another account because the home account is briefly
// cooling (e.g. short 5xx/overload rest) or momentarily at its concurrency
// limit (laneMate). Without this hold, the detour reply permanently overwrites
// sticks.m[key], abandoning a warm 5m/1h prompt cache and paying a full cache
// rewrite (12x-34x) on the detour account.
//
// Set MAGPIE_AFFINITY_DETOUR_HOLD=off to disable.
const (
	claudeDetourHoldWindow  = 5 * time.Minute
	codexDetourHoldWindow   = 10 * time.Minute
	defaultDetourHoldWindow = 3 * time.Minute

	// spentHysteresisMargin requires the replacement account to be at least 20
	// percentage points healthier before evicting a warm session below 100%.
	spentHysteresisMargin = 20.0
)

func detourHoldEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_AFFINITY_DETOUR_HOLD")))
	return v != "off" && v != "0" && v != "false"
}

func detourHoldWindow(providerID string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(providerID)) {
	case "claude", "anthropic":
		return claudeDetourHoldWindow
	case "codex", "chatgpt":
		return codexDetourHoldWindow
	default:
		return defaultDetourHoldWindow
	}
}

// spentEvictable reports whether a bound account at >= SpentShare should be
// evicted in favor of the top-ranked peer `best`. When the bound account is not
// completely exhausted (< 100%) and `best` has a known usage within 20 points,
// stay put to avoid ping-pong cache rebuilds between two nearly full accounts.
func spentEvictable(bound, best Weighed, spentShare float64) bool {
	if !bound.Known || bound.Used < spentShare || bound.CreditsFirst {
		return false
	}
	if bound.Used < 100 && best.Known && (bound.Used-best.Used) < spentHysteresisMargin {
		return false
	}
	return true
}

// shouldHoldHomeStick decides whether a reply from detour candidate `c` should
// leave an existing warm home stick `st` intact (recording detourStart) instead
// of permanently migrating the session onto `c`.
//
// It triggers ONLY on involuntary non-failing detours — either the home account
// was skipped due to a brief transient rest (`homeWeighed.Rest != nil`), or it
// gave way before execution because its concurrency lane was full
// (`gaveWay[st.who]`). Active request failures on the home account (e.g. a 500
// that fails over within a group/rule) and intentional rotations never hold the
// old stick.
func shouldHoldHomeStick(st stick, c candidate, gaveWay map[string]bool, plannedCs []candidate, plannedOrder []Weighed, now time.Time) (bool, time.Time) {
	if !detourHoldEnabled() || st.who == "" || st.who == c.who() {
		return false, time.Time{}
	}
	// Find the home account in this request's planned candidate list.
	homeIdx := -1
	for i, pc := range plannedCs {
		if pc.who() == st.who {
			homeIdx = i
			break
		}
	}
	if homeIdx < 0 || homeIdx >= len(plannedOrder) {
		return false, time.Time{}
	}
	homeCand := plannedCs[homeIdx]
	homeWeighed := plannedOrder[homeIdx]

	// Require an involuntary detour: either brief rest on home, or home gave way
	// due to concurrency lane saturation before a request failed on it.
	if homeWeighed.Rest == nil && !gaveWay[st.who] {
		return false, time.Time{}
	}

	// Home must still have a warm cache worth returning to.
	if st.cacheRead+st.cacheWrite < cacheWorth {
		return false, time.Time{}
	}
	if now.Sub(st.at) > coldAfter(homeCand.p) && !st.hour.warm(now) {
		return false, time.Time{}
	}

	// Never hold onto a home account that is barred, quota/credit/auth/restricted
	// cooled, or spent past eviction threshold.
	if homeWeighed.Barred || homeWeighed.Window > 0 || homeWeighed.Reserve > 0 {
		return false, time.Time{}
	}
	holdWindow := detourHoldWindow(homeCand.p.ID)
	if homeWeighed.Rest != nil {
		why := homeWeighed.Rest.Why
		if why == failQuota || why == failCredit || why == failAuth || why == failRestricted {
			return false, time.Time{}
		}
		if homeWeighed.Rest.Until.Sub(now) > holdWindow {
			return false, time.Time{}
		}
	} else if len(plannedOrder) > 0 && spentEvictable(homeWeighed, plannedOrder[0], 98) {
		return false, time.Time{}
	}

	// Bound total detour duration so a persistently unreachable home account
	// eventually migrates cleanly rather than flapping forever.
	start := st.detourStart
	if start.IsZero() {
		start = now
	}
	if now.Sub(start) > holdWindow {
		return false, time.Time{}
	}
	return true, start
}
