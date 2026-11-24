package lowmem

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	mib = 1 << 20

	minAutoLimitBytes = 256 * mib
	maxAutoLimitBytes = 1024 * mib
	lowMemGCPercent   = 50
)

// Enabled reports whether MAGPIE_LOW_MEMORY is turned on (1, true, yes, on).
// Intended for memory-constrained Linux VMs (e.g. 4–8 GiB RAM running multiple
// claude / agy-cli subprocesses alongside Magpie).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_LOW_MEMORY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ApplyRuntimeTuning configures Go's soft memory limit and GC target for
// constrained servers without ever overriding explicit user GOMEMLIMIT or GOGC.
// Returns the applied memory limit in bytes (0 if left untouched) and GC
// percent (-1 if left untouched).
func ApplyRuntimeTuning() (limitBytes int64, gcPercent int) {
	gcPercent = -1
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		if mb := positiveEnvInt("MAGPIE_MEM_LIMIT_MB"); mb > 0 {
			limitBytes = int64(mb) * mib
		} else if Enabled() {
			if host := hostMemoryLimitBytes(); host > 0 {
				// Reserve ~20% for the Go gateway process itself (clamped to
				// [256 MiB, 1024 MiB]) so claude/agy child processes have the
				// remaining cgroup/RAM headroom instead of fighting Go heap.
				limitBytes = clampInt64(host/5, minAutoLimitBytes, maxAutoLimitBytes)
			} else {
				limitBytes = 512 * mib
			}
		}
		if limitBytes > 0 {
			debug.SetMemoryLimit(limitBytes)
		}
	}
	if Enabled() && strings.TrimSpace(os.Getenv("GOGC")) == "" {
		gcPercent = lowMemGCPercent
		debug.SetGCPercent(gcPercent)
	}
	return limitBytes, gcPercent
}

// ClaudeIdleMax bounds how many idle Claude Code subprocesses stay resident
// between turns (evicted runs are shelved and resumed seamlessly without losing
// Anthropic's 1h prompt cache).
func ClaudeIdleMax(def int) int {
	if n := positiveEnvInt("MAGPIE_CLAUDE_IDLE_MAX"); n > 0 {
		return n
	}
	if Enabled() {
		return min(def, 2)
	}
	return def
}

// ClaudeParkedMax bounds how many Claude Code subprocesses wait simultaneously
// on caller tool results.
func ClaudeParkedMax(def int) int {
	if n := positiveEnvInt("MAGPIE_CLAUDE_PARKED_MAX"); n > 0 {
		return n
	}
	if Enabled() {
		return min(def, 6)
	}
	return def
}

// ClaudeIdleTTL bounds how long an idle Claude Code subprocess stays alive in
// memory before exiting and shelving its session ID for later --resume.
func ClaudeIdleTTL(def time.Duration) time.Duration {
	if raw := strings.TrimSpace(os.Getenv("MAGPIE_CLAUDE_IDLE_TTL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 10*time.Second {
			return d
		}
	}
	if Enabled() {
		return min(def, 5*time.Minute)
	}
	return def
}

// ClaudeRunRSSFloor lowers the per-subprocess RSS recycle floor on small Linux
// VMs (default upstream floor is 1 GiB, which allows a single run to consume
// 1–1.5 GiB before shelving on a 4–8 GiB VM).
func ClaudeRunRSSFloor(def int64) int64 {
	if mb := positiveEnvInt("MAGPIE_CLAUDE_RUN_RSS_MB"); mb >= 128 {
		return int64(mb) * mib
	}
	if Enabled() {
		return min(def, int64(384*mib))
	}
	return def
}

// CodexImageBudget bounds decoded inline image bytes held per request.
func CodexImageBudget(def int) int {
	if Enabled() {
		return min(def, 128*mib)
	}
	return def
}

// BlockPriceLinger bounds how long priced usage-log blocks linger in Go heap.
func BlockPriceLinger(def time.Duration) time.Duration {
	if Enabled() {
		return min(def, 5*time.Second)
	}
	return def
}

func positiveEnvInt(name string) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
