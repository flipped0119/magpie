package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPlaceClaudeContextBreakpoint(t *testing.T) {
	// Native Claude Code subagent payload: 2 existing 1h markers + leading <system-reminder> blocks + task block.
	raw := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],"tools":[{"name":"Bash","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>CLAUDE.md</system-reminder>"},{"type":"text","text":"<system-reminder>git status</system-reminder>"},{"type":"text","text":"fix the bug"}]}]}`)

	got := placeClaudeContextBreakpoint(raw)
	if !json.Valid(got) {
		t.Fatalf("spliced body is not valid JSON: %s", got)
	}
	if !gjson.GetBytes(got, "messages.0.content.1.cache_control").Exists() {
		t.Fatalf("expected 1h cache_control on last leading <system-reminder> block, got: %s", got)
	}
	if gjson.GetBytes(got, "messages.0.content.1.cache_control.ttl").String() != "1h" {
		t.Fatalf("expected ttl=1h, got: %s", gjson.GetBytes(got, "messages.0.content.1.cache_control").Raw)
	}
	if gjson.GetBytes(got, "messages.0.content.0.cache_control").Exists() {
		t.Fatal("earlier <system-reminder> block should not be tagged")
	}
	// Byte preservation: tools and system prefix must remain byte-for-byte identical.
	prefixEnd := bytes.Index(raw, []byte(`"messages"`))
	if !bytes.Equal(raw[:prefixEnd], got[:prefixEnd]) {
		t.Fatal("byte prefix before messages was mutated")
	}

	// Must NOT splice when any existing marker is 5m.
	fiveMin := bytes.Replace(raw, []byte(`"ttl":"1h"`), []byte(`"ttl":"5m"`), 1)
	if !bytes.Equal(placeClaudeContextBreakpoint(fiveMin), fiveMin) {
		t.Fatal("must not splice 1h breakpoint when request has 5m markers")
	}

	// Respects MAGPIE_CLAUDE_CONTEXT_BREAKPOINT=off.
	t.Setenv("MAGPIE_CLAUDE_CONTEXT_BREAKPOINT", "off")
	if !bytes.Equal(placeClaudeContextBreakpoint(raw), raw) {
		t.Fatal("must skip splicing when MAGPIE_CLAUDE_CONTEXT_BREAKPOINT=off")
	}
}

func TestNormalizeAnthropicCacheControl(t *testing.T) {
	// Clean body takes zero-alloc fast path (exact same bytes).
	clean := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	if got := normalizeAnthropicCacheControl(clean); &got[0] != &clean[0] {
		t.Fatal("clean body should take zero-copy fast path")
	}

	// Dirty body: illegal thinking marker, defer_loading marker, 5 markers total, and 1h after 5m.
	dirty := []byte(`{"tools":[{"name":"t1","defer_loading":true,"cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"s2","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"cache_control":{"type":"ephemeral","ttl":"1h"}}`)
	got := normalizeAnthropicCacheControl(dirty)
	if !json.Valid(got) {
		t.Fatalf("normalized body is invalid JSON: %s", got)
	}
	if gjson.GetBytes(got, "tools.0.cache_control").Exists() {
		t.Fatal("defer_loading tool must have cache_control stripped")
	}
	if gjson.GetBytes(got, "messages.0.content.0.cache_control").Exists() {
		t.Fatal("thinking block must have cache_control stripped")
	}
	if !anthropicCacheControlClean(got) {
		t.Fatalf("normalized body still violates cache_control invariants: %s", got)
	}
	cnt, _ := inspectAnthropicCacheMarkers(got)
	if cnt > 4 {
		t.Fatalf("expected <=4 markers after normalization, got %d", cnt)
	}
	// After 5m tool marker (t2), later markers must have been downgraded from 1h.
	if gjson.GetBytes(got, "cache_control.ttl").String() == "1h" {
		t.Fatalf("top-level 1h after earlier 5m marker must be downgraded: %s", got)
	}
}

func TestAnonymizeThirdPartyUserID(t *testing.T) {
	raw := []byte(`{"model":"deepseek-chat","metadata":{"user_id":"user_12345_account_abcd_session_xyz"},"messages":[]}`)
	got := anonymizeThirdPartyUserID(raw)
	if !json.Valid(got) {
		t.Fatalf("invalid JSON: %s", got)
	}
	uid := gjson.GetBytes(got, "metadata.user_id").String()
	if !strings.HasPrefix(uid, "magpie_u_") || strings.Contains(uid, "12345") || strings.Contains(uid, "abcd") {
		t.Fatalf("expected opaque anonymized user_id, got %q", uid)
	}
	// Deterministic and idempotent.
	if got2 := anonymizeThirdPartyUserID(got); !bytes.Equal(got2, got) {
		t.Fatalf("anonymizeThirdPartyUserID should be idempotent")
	}

	// Modern Claude Code JSON user_id with device_id/account_uuid is anonymized too.
	modern := []byte(`{"model":"deepseek-chat","metadata":{"user_id":"{\"device_id\":\"dev1\",\"account_uuid\":\"acct1\",\"session_id\":\"s1\"}"},"messages":[]}`)
	gotModern := anonymizeThirdPartyUserID(modern)
	uidModern := gjson.GetBytes(gotModern, "metadata.user_id").String()
	if !strings.HasPrefix(uidModern, "magpie_u_") || strings.Contains(uidModern, "acct1") {
		t.Fatalf("expected modern Claude Code user_id anonymized, got %q", uidModern)
	}

	// Plain caller-supplied user_id stays verbatim.
	plain := []byte(`{"model":"m1","metadata":{"user_id":"u1"},"messages":[]}`)
	if gotPlain := anonymizeThirdPartyUserID(plain); !bytes.Equal(gotPlain, plain) {
		t.Fatalf("plain caller user_id should stay verbatim, got %s", gotPlain)
	}
}

func TestHourMarksRefusalDetectsExtendedBeta(t *testing.T) {
	if !hourMarksRefusal(400, []byte(`{"error":{"message":"invalid beta flag: extended-cache-ttl-2025-04-11"}}`)) {
		t.Fatal("expected hourMarksRefusal to match extended-cache-ttl 400")
	}
	if !hourMarksRefusal(400, []byte(`{"error":{"message":"invalid beta flag"}}`)) {
		t.Fatal("expected hourMarksRefusal to match invalid beta flag 400")
	}
}
