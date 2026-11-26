package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaudeCodeSessionFromUserID(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"s-1\"}"}}`, "s-1"},
		{`{"metadata":{"user_id":"user_abc_account_11111111-2222_session_33333333-4444"}}`, "33333333-4444"},
		{`{"metadata":{"user_id":"alice"}}`, ""},
		{`{"metadata":{"user_id":"x_session_y"}}`, ""},
		{`{"messages":[]}`, ""},
	} {
		if got := claudeCodeSession([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %q want %q", c.body, got, c.want)
		}
	}
}

// A relay drops session headers; two Claude Code sessions with the same
// opening must still get different affinity ids.
func TestAffinityIDReadsClaudeCodeSession(t *testing.T) {
	mk := func(s string) []byte {
		return []byte(`{"metadata":{"user_id":"{\"session_id\":\"` + s + `\"}"},"messages":[{"role":"user","content":"hi"}]}`)
	}
	h := http.Header{}
	a, b := affinityID(h, mk("one")), affinityID(h, mk("two"))
	if a == b || a != "cc:one" {
		t.Fatalf("affinity ids %q %q", a, b)
	}
}

func TestCallerScopeSeparatesRelayUsers(t *testing.T) {
	r1 := httptest.NewRequest("POST", "/v1/messages", nil)
	r2 := httptest.NewRequest("POST", "/v1/messages", nil)
	r1.Header.Set(SessionHeader, "u1")
	r2.Header.Set(SessionHeader, "u2")
	if s1, s2 := callerScope(r1), callerScope(r2); s1 == "" || s1 == s2 {
		t.Fatalf("scopes %q %q", s1, s2)
	}
}
