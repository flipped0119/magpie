package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	extendedCacheTTLBeta    = "extended-cache-ttl-2025-04-11"
	maxAnthropicCacheMarks  = 4
	systemReminderOpenTag   = "<system-reminder>"
	contextBreakpointInsert = `,"cache_control":{"type":"ephemeral","ttl":"1h"}`
)

func contextBreakpointEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_CLAUDE_CONTEXT_BREAKPOINT")))
	return v != "off" && v != "0" && v != "false"
}

// placeClaudeContextBreakpoint byte-splices a 1h cache_control breakpoint onto
// the last leading <system-reminder> text block in messages[0].content so that
// parallel Claude Code subagents share the ~50k workspace context cache instead
// of each paying a fresh 1h cache write.
//
// Invariants (matching AsterGate's claude_context_cache_breakpoint):
//   - Only runs when enabled (default on, MAGPIE_CLAUDE_CONTEXT_BREAKPOINT=off disables).
//   - Only touches requests where messages[0].role == "user" and content is an
//     array with >=1 leading <system-reminder> text block followed by >=1 task block.
//   - Only splices when all existing cache_control markers in the request are
//     1h (so 5m requests are never promoted into 2x 1h writes) and total markers
//     are strictly between 1 and 3 (never exceeding Anthropic's 4-breakpoint cap).
//   - Pure byte slice insertion right before the closing '}' of the target block:
//     zero JSON re-encoding of untouched sections.
func placeClaudeContextBreakpoint(body []byte) []byte {
	if !contextBreakpointEnabled() || len(body) == 0 {
		return body
	}
	if !bytes.Contains(body, []byte(systemReminderOpenTag)) {
		return body
	}
	// Collect existing cache_control markers across protocol positions to verify
	// count in [1, 3] and all markers use 1h TTL.
	totalMarks, allOneHour := inspectAnthropicCacheMarkers(body)
	if totalMarks < 1 || totalMarks >= maxAnthropicCacheMarks || !allOneHour {
		return body
	}

	msg0 := gjson.GetBytes(body, "messages.0")
	if !msg0.Exists() || msg0.Get("role").String() != "user" {
		return body
	}
	content := msg0.Get("content")
	if !content.IsArray() {
		return body
	}

	var target gjson.Result
	leadingCount := 0
	totalBlocks := 0
	stillLeading := true
	content.ForEach(func(_, blk gjson.Result) bool {
		totalBlocks++
		if !stillLeading {
			return true
		}
		if blk.Get("type").String() == "text" && strings.HasPrefix(strings.TrimSpace(blk.Get("text").String()), systemReminderOpenTag) {
			leadingCount++
			target = blk
		} else {
			stillLeading = false
		}
		return true
	})

	// Must have at least one leading <system-reminder> block AND at least one
	// trailing task block after it, and target must not already carry cache_control.
	if leadingCount == 0 || leadingCount >= totalBlocks || !target.Exists() || target.Get("cache_control").Exists() {
		return body
	}
	if target.Index <= 0 || target.Index+len(target.Raw) > len(body) {
		return body
	}
	// Locate the closing '}' of the target object in raw bytes.
	closePos := target.Index + len(target.Raw) - 1
	for closePos > target.Index && isJSONSpace(body[closePos]) {
		closePos--
	}
	if closePos <= target.Index || body[closePos] != '}' {
		return body
	}
	out := make([]byte, 0, len(body)+len(contextBreakpointInsert))
	out = append(out, body[:closePos]...)
	out = append(out, contextBreakpointInsert...)
	out = append(out, body[closePos:]...)
	return out
}

func inspectAnthropicCacheMarkers(body []byte) (count int, allOneHour bool) {
	allOneHour = true
	check := func(cc gjson.Result) {
		if !cc.Exists() || cc.Type == gjson.Null {
			return
		}
		count++
		if cc.Get("ttl").String() != "1h" {
			allOneHour = false
		}
	}
	check(gjson.GetBytes(body, "cache_control"))
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		check(t.Get("cache_control"))
		return true
	})
	sys := gjson.GetBytes(body, "system")
	if sys.IsArray() {
		sys.ForEach(func(_, s gjson.Result) bool {
			check(s.Get("cache_control"))
			return true
		})
	}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		c := m.Get("content")
		if !c.IsArray() {
			return true
		}
		c.ForEach(func(_, b gjson.Result) bool {
			check(b.Get("cache_control"))
			if b.Get("type").String() == "tool_result" && b.Get("content").IsArray() {
				b.Get("content").ForEach(func(_, sub gjson.Result) bool {
					check(sub.Get("cache_control"))
					return true
				})
			}
			return true
		})
		return true
	})
	return count, allOneHour
}

// normalizeAnthropicCacheControl sanitizes illegal cache_control placements on
// direct Anthropic passthrough requests so upstream does not reject them with
// 400:
//  1. Strips cache_control from thinking/redacted_thinking blocks and
//     defer_loading:true tools.
//  2. Repairs TTL ordering violations where a 1h marker follows a 5m marker
//     (tools -> system -> messages -> top-level) by downgrading the later 1h
//     marker to 5m.
//  3. Caps total markers at 4 using value-preserving eviction (oldest message
//     blocks -> earlier tools -> earlier system blocks, preserving top-level
//     and latest markers).
//
// Fast-path: returns the original byte slice untouched with zero re-encoding
// whenever the body already obeys all Anthropic cache_control rules.
func normalizeAnthropicCacheControl(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"cache_control"`)) {
		return body
	}
	if anthropicCacheControlClean(body) {
		return body
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}

	type markerRef struct {
		holder   map[string]any
		ttl      string // "1h" or "5m"
		priority int    // lower = evicted first
		order    int    // protocol evaluation order
	}

	var refs []markerRef
	seq := 0
	record := func(holder map[string]any, priority int) {
		cc, ok := holder["cache_control"].(map[string]any)
		if !ok {
			return
		}
		ttl := "5m"
		if s, _ := cc["ttl"].(string); s == "1h" {
			ttl = "1h"
		}
		refs = append(refs, markerRef{holder: holder, ttl: ttl, priority: priority, order: seq})
		seq++
	}

	// 1. Tools (strip from defer_loading: true)
	if tools, ok := doc["tools"].([]any); ok {
		for i, raw := range tools {
			tm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if dl, _ := tm["defer_loading"].(bool); dl {
				delete(tm, "cache_control")
				continue
			}
			prio := 20
			if i == len(tools)-1 {
				prio = 40
			}
			record(tm, prio)
		}
	}

	// 2. System blocks
	if sys, ok := doc["system"].([]any); ok {
		for i, raw := range sys {
			sm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			prio := 15
			if i == len(sys)-1 {
				prio = 45
			}
			record(sm, prio)
		}
	}

	// 3. Messages (strip from thinking/redacted_thinking)
	if msgs, ok := doc["messages"].([]any); ok {
		for mi, raw := range msgs {
			mm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			blocks, ok := mm["content"].([]any)
			if !ok {
				continue
			}
			for _, braw := range blocks {
				bm, ok := braw.(map[string]any)
				if !ok {
					continue
				}
				bt, _ := bm["type"].(string)
				if bt == "thinking" || bt == "redacted_thinking" {
					delete(bm, "cache_control")
					continue
				}
				// Older messages evict first; newest messages stay longest.
				prio := 10
				if mi >= len(msgs)-2 {
					prio = 35
				}
				record(bm, prio)
				if bt == "tool_result" {
					if subs, ok := bm["content"].([]any); ok {
						for _, sraw := range subs {
							if sm, ok := sraw.(map[string]any); ok {
								record(sm, prio)
							}
						}
					}
				}
			}
		}
	}

	// 4. Top-level cache_control (highest retention priority)
	record(doc, 100)

	// Evict lowest-priority (and earliest) markers until <= 4 remain.
	dropped := make([]bool, len(refs))
	alive := len(refs)
	for alive > maxAnthropicCacheMarks {
		evict := -1
		for i, r := range refs {
			if dropped[i] {
				continue
			}
			if evict < 0 || r.priority < refs[evict].priority || (r.priority == refs[evict].priority && r.order < refs[evict].order) {
				evict = i
			}
		}
		if evict < 0 {
			break
		}
		delete(refs[evict].holder, "cache_control")
		dropped[evict] = true
		alive--
	}

	// Repair TTL ordering: once a 5m marker appears, no later marker may ask for 1h.
	seen5m := false
	for i, r := range refs {
		if dropped[i] {
			continue
		}
		if r.ttl == "5m" {
			seen5m = true
			continue
		}
		if seen5m && r.ttl == "1h" {
			if cc, ok := r.holder["cache_control"].(map[string]any); ok {
				delete(cc, "ttl")
			}
		}
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// anthropicCacheControlClean checks via zero-alloc gjson scan whether all
// cache_control rules already hold (no illegal placements, <= 4 markers, and
// no 1h marker after a 5m marker).
func anthropicCacheControlClean(body []byte) bool {
	count := 0
	seen5m := false
	clean := true
	visit := func(cc gjson.Result) {
		if !cc.Exists() || cc.Type == gjson.Null {
			return
		}
		count++
		if count > maxAnthropicCacheMarks {
			clean = false
		}
		if cc.Get("ttl").String() == "1h" {
			if seen5m {
				clean = false
			}
		} else {
			seen5m = true
		}
	}

	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		if t.Get("defer_loading").Bool() && t.Get("cache_control").Exists() {
			clean = false
			return false
		}
		visit(t.Get("cache_control"))
		return clean
	})
	if !clean {
		return false
	}
	if sys := gjson.GetBytes(body, "system"); sys.IsArray() {
		sys.ForEach(func(_, s gjson.Result) bool {
			visit(s.Get("cache_control"))
			return clean
		})
	}
	if !clean {
		return false
	}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		c := m.Get("content")
		if !c.IsArray() {
			return true
		}
		c.ForEach(func(_, b gjson.Result) bool {
			bt := b.Get("type").String()
			if (bt == "thinking" || bt == "redacted_thinking") && b.Get("cache_control").Exists() {
				clean = false
				return false
			}
			visit(b.Get("cache_control"))
			if clean && bt == "tool_result" && b.Get("content").IsArray() {
				b.Get("content").ForEach(func(_, sub gjson.Result) bool {
					visit(sub.Get("cache_control"))
					return clean
				})
			}
			return clean
		})
		return clean
	})
	if !clean {
		return false
	}
	visit(gjson.GetBytes(body, "cache_control"))
	return clean
}

// isClaudeCodeUserID reports whether metadata.user_id carries Claude Code's
// Anthropic identity fields (legacy `user_<id>_account_<uuid>_session_<uuid>`
// or JSON `{"device_id":...,"account_uuid":...,"session_id":...}`). Plain
// caller-supplied IDs (e.g. "u1") are preserved verbatim.
func isClaudeCodeUserID(v string) bool {
	if strings.Contains(v, "_account_") || strings.Contains(v, "_session_") {
		return true
	}
	if strings.HasPrefix(v, "{") && (strings.Contains(v, `"account_uuid"`) || strings.Contains(v, `"device_id"`)) {
		return true
	}
	return false
}

// anonymizeThirdPartyUserID replaces Claude Code's Anthropic metadata.user_id
// with a deterministic opaque SHA-256 digest (in-place byte splice) when
// forwarding traffic to non-Claude third-party vendors (DeepSeek, Kimi, etc.),
// preventing Anthropic device_id/account_uuid leakage while preserving stable
// per-caller KV-cache isolation and keeping ordinary caller user_ids verbatim.
func anonymizeThirdPartyUserID(body []byte) []byte {
	uid := gjson.GetBytes(body, "metadata.user_id")
	if !uid.Exists() || uid.Type != gjson.String || uid.Index <= 0 || uid.Index+len(uid.Raw) > len(body) {
		return body
	}
	rawVal := uid.String()
	if rawVal == "" || strings.HasPrefix(rawVal, "magpie_u_") || !isClaudeCodeUserID(rawVal) {
		return body
	}
	sum := sha256.Sum256([]byte("magpie-third-party-uid\x00" + rawVal))
	replacement := `"magpie_u_` + hex.EncodeToString(sum[:16]) + `"`
	out := make([]byte, 0, len(body)-len(uid.Raw)+len(replacement))
	out = append(out, body[:uid.Index]...)
	out = append(out, replacement...)
	out = append(out, body[uid.Index+len(uid.Raw):]...)
	return out
}
