package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

// AsterGate-compatible self-usage endpoint and zero-404 Claude Code CLI
// control endpoints (GET /me/usage, GET /v1/me/usage, GET /managed/settings,
// POST /api/event_logging/batch).
const (
	MeUsagePath         = "/me/usage"
	V1MeUsagePath       = "/v1/me/usage"
	ManagedSettingsPath = "/managed/settings"
)

type meUsageResponse struct {
	APIKey             string           `json:"api_key,omitempty"`
	GeneratedAt        string           `json:"generated_at"`
	Timezone           string           `json:"timezone"`
	Currency           string           `json:"currency"`
	AccountQuotaShared bool             `json:"account_quota_shared"`
	Accounts           []meUsageAccount `json:"accounts"`
}

type meUsageAccount struct {
	AccountID       string          `json:"account_id"`
	Label           string          `json:"label"`
	Provider        string          `json:"provider"`
	Plan            string          `json:"plan,omitempty"`
	PlanRaw         string          `json:"plan_raw,omitempty"`
	PlanDisplayName string          `json:"plan_display_name,omitempty"`
	State           string          `json:"state"`
	AvailableAt     string          `json:"available_at,omitempty"`
	Current         bool            `json:"current"`
	CheckedAt       string          `json:"checked_at,omitempty"`
	Windows         []meUsageWindow `json:"windows"`
}

type meUsageWindow struct {
	Kind           string   `json:"kind"`
	Scope          string   `json:"scope,omitempty"`
	ScopedModel    string   `json:"scoped_model,omitempty"`
	UsedRatio      float64  `json:"used_ratio"`
	ProtectPercent *float64 `json:"protect_percent,omitempty"`
	ResetsAt       string   `json:"resets_at,omitempty"`
	CheckedAt      string   `json:"checked_at,omitempty"`
}

func (s *Server) meUsage(w http.ResponseWriter, r *http.Request) {
	if !local(r) && !sharedWith(r) {
		writeError(w, provider.Chat, http.StatusForbidden, "self usage requires local access or a valid gateway key when shared on the network")
		return
	}
	scope, ok := scopeQuotas(w, r)
	if !ok {
		return
	}

	who := access.Caller(r.Context())
	session := strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id"))
	if session == "" {
		session = strings.TrimSpace(r.Header.Get("Session-Id"))
	}
	if session == "" {
		session = strings.TrimSpace(r.URL.Query().Get("session"))
	}
	if len(session) > 128 {
		session = session[:128]
	}

	var curProvider, curAccount string
	if session != "" && !who.ReadOnly {
		only := ""
		if heldKey(who) {
			only = who.KeyID
		}
		if _, route := s.trace.sessionLatest(r.Context(), session, only, 0, 0); route != nil {
			curProvider, curAccount = routeCurrentAccount(*route)
		}
	}

	now := time.Now()
	qs := scope.quotas(provider.KeptQuotaReport(now))
	out := buildMeUsageResponse(who.KeyName, qs, curProvider, curAccount, now)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func routeCurrentAccount(r Route) (prov, acct string) {
	if len(r.Tries) > 0 {
		lastID := r.Tries[len(r.Tries)-1].ID
		for _, w := range r.Order {
			if w.ID == lastID {
				return strings.ToLower(strings.TrimSpace(w.Provider)), strings.ToLower(strings.TrimSpace(w.Who))
			}
		}
	}
	if len(r.Order) > 0 {
		return strings.ToLower(strings.TrimSpace(r.Order[0].Provider)), strings.ToLower(strings.TrimSpace(r.Order[0].Who))
	}
	return strings.ToLower(strings.TrimSpace(r.Provider)), ""
}

func buildMeUsageResponse(keyName string, qs []provider.Quota, curProvider, curAccount string, now time.Time) meUsageResponse {
	accounts := make([]meUsageAccount, 0, len(qs))
	for _, q := range qs {
		prov := normalizeMeProvider(q.Provider)
		acctID := meAccountID(q)
		label := strings.TrimSpace(q.Alias)
		if label == "" {
			label = strings.TrimSpace(q.User)
		}
		if label == "" {
			label = strings.TrimSpace(q.Name)
		}
		planRaw := strings.TrimSpace(q.Plan)
		planNorm := normalizeMePlan(prov, planRaw)

		checkedAt := ""
		if q.ReadAt != nil && !q.ReadAt.IsZero() {
			checkedAt = q.ReadAt.UTC().Format(time.RFC3339)
		} else if q.AsOf != nil && !q.AsOf.IsZero() {
			checkedAt = q.AsOf.UTC().Format(time.RFC3339)
		}

		caps := provider.AccountCapsOf(q.Provider, q.User)
		wins := make([]meUsageWindow, 0, len(q.Windows))
		allExhausted := false
		var earliestReset time.Time
		for _, sp := range q.Windows {
			if sp.Unlimited || sp.Text {
				continue
			}
			kind, scopeKind, scopedModel := mapQuotaSpanKind(prov, sp.Name)
			if kind == "" {
				continue
			}
			ratio := sp.Used / 100.0
			if ratio < 0 {
				ratio = 0
			} else if ratio > 1 {
				ratio = 1
			}
			var rstStr string
			var resets time.Time
			if sp.ResetsAt != nil && !sp.ResetsAt.IsZero() {
				resets = *sp.ResetsAt
				rstStr = sp.ResetsAt.UTC().Format(time.RFC3339)
				if ratio >= 0.995 && sp.ResetsAt.After(now) {
					allExhausted = true
					if earliestReset.IsZero() || sp.ResetsAt.Before(earliestReset) {
						earliestReset = *sp.ResetsAt
					}
				}
			}
			var protectPct *float64
			if scopeKind == "account" && sp.Lane == "" {
				protectPct = caps.EffectiveProtectPercent(sp.Name, sp.Limit, false, resets)
			}
			wins = append(wins, meUsageWindow{
				Kind:           kind,
				Scope:          scopeKind,
				ScopedModel:    scopedModel,
				UsedRatio:      ratio,
				ProtectPercent: protectPct,
				ResetsAt:       rstStr,
				CheckedAt:      checkedAt,
			})
		}

		state := "available"
		availableAt := ""
		if q.Error != "" {
			state = "unavailable"
		} else if allExhausted {
			state = "exhausted"
			if !earliestReset.IsZero() {
				availableAt = earliestReset.UTC().Format(time.RFC3339)
			}
		}

		isCurrent := false
		if curProvider != "" && curAccount != "" {
			qProv := strings.ToLower(strings.TrimSpace(q.Provider))
			qUser := strings.ToLower(strings.TrimSpace(q.User))
			if (qProv == curProvider || prov == curProvider) && qUser != "" && qUser == curAccount {
				isCurrent = true
			}
		}

		accounts = append(accounts, meUsageAccount{
			AccountID:       acctID,
			Label:           label,
			Provider:        prov,
			Plan:            planNorm,
			PlanRaw:         planRaw,
			PlanDisplayName: planRaw,
			State:           state,
			AvailableAt:     availableAt,
			Current:         isCurrent,
			CheckedAt:       checkedAt,
			Windows:         wins,
		})
	}
	tz, _ := now.Zone()
	if tz == "" {
		tz = "UTC"
	}
	return meUsageResponse{
		APIKey:             keyName,
		GeneratedAt:        now.UTC().Format(time.RFC3339),
		Timezone:           tz,
		Currency:           "USD",
		AccountQuotaShared: true,
		Accounts:           accounts,
	}
}

func normalizeMeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "agy-cli", "antigravity":
		return "antigravity"
	case "claude", "anthropic":
		return "claude"
	case "codex", "chatgpt":
		return "codex"
	default:
		return strings.ToLower(strings.TrimSpace(p))
	}
}

func normalizeMePlan(prov, raw string) string {
	l := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(l, "20x"), strings.Contains(l, "ultra"):
		if prov == "claude" {
			return "max"
		}
		return "ultra"
	case strings.Contains(l, "5x"), strings.Contains(l, "max"):
		return "max"
	case strings.Contains(l, "pro"):
		return "pro"
	case strings.Contains(l, "plus"):
		return "plus"
	case strings.Contains(l, "team"):
		return "team"
	case strings.Contains(l, "free"):
		return "free"
	default:
		return l
	}
}

func meAccountID(q provider.Quota) string {
	key := strings.ToLower(strings.TrimSpace(q.Provider)) + ":" + strings.ToLower(strings.TrimSpace(q.User))
	if strings.TrimSpace(q.User) == "" {
		key += strings.ToLower(strings.TrimSpace(q.Name))
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func mapQuotaSpanKind(prov, name string) (kind, scope, scopedModel string) {
	l := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(l, "fable"):
		return "weekly_fable", "model", "Fable"
	case strings.Contains(l, "opus") && (strings.Contains(l, "week") || strings.Contains(l, "7")):
		return "weekly_opus", "model", "Opus"
	case strings.Contains(l, "sonnet") && (strings.Contains(l, "week") || strings.Contains(l, "7")):
		return "weekly_sonnet", "model", "Sonnet"
	case strings.Contains(l, "gemini") && (strings.Contains(l, "5") || strings.Contains(l, "hour")):
		return "gemini_five_hour", "model", "gemini"
	case strings.Contains(l, "gemini") && (strings.Contains(l, "week") || strings.Contains(l, "7")):
		return "gemini_weekly", "model", "gemini"
	case strings.Contains(l, "5 hour") || strings.Contains(l, "5h") || l == "session":
		if prov == "antigravity" {
			return "gemini_five_hour", "model", "gemini"
		}
		return "five_hour", "account", ""
	case strings.Contains(l, "week") || strings.Contains(l, "7 day") || l == "7d":
		if prov == "antigravity" {
			return "gemini_weekly", "model", "gemini"
		}
		return "weekly", "account", ""
	case strings.Contains(l, "month") || strings.Contains(l, "30 day"):
		return "monthly", "account", ""
	default:
		return "", "", ""
	}
}

var emptyManagedSettingsJSON = []byte("{}\n")
var emptyManagedSettingsETag = func() string {
	sum := sha256.Sum256(emptyManagedSettingsJSON)
	return `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}()

// managedSettings serves GET /managed/settings with ETag + 304 so Claude Code
// CLI pointed at Magpie does not log startup 404s.
func (s *Server) managedSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("ETag", emptyManagedSettingsETag)
	w.Header().Set("Cache-Control", "no-cache")
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == emptyManagedSettingsETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(emptyManagedSettingsJSON)
}

// eventLoggingSink accepts Claude Code CLI telemetry flushes cleanly (200 OK)
// instead of returning 404 errors when Claude Code runs against Magpie.
func (s *Server) eventLoggingSink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"accepted":true}`))
}
