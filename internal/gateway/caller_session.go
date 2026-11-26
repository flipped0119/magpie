package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// claudeCodeSession is the Claude Code session a request's body names in
// metadata.user_id: the JSON form's session_id, or the legacy
// user_<id>_account_<uuid>_session_<uuid>'s last part. A relay in front of
// magpie (New-API) drops the session headers but passes the body as sent,
// so this is what still tells its users' conversations apart. "" when the
// body carries no Claude Code identity; a caller's own plain user_id is
// not a session.
func claudeCodeSession(body []byte) string {
	if len(body) == 0 || !bodyHas(body, "user_id") {
		return ""
	}
	uid := gjson.GetBytes(body, "metadata.user_id")
	if uid.Type != gjson.String {
		return ""
	}
	v := strings.TrimSpace(uid.String())
	if strings.HasPrefix(v, "{") {
		return strings.TrimSpace(gjson.Get(v, "session_id").String())
	}
	if i := strings.LastIndex(v, "_session_"); i >= 0 && strings.Contains(v[:i], "_account_") {
		return strings.TrimSpace(v[i+len("_session_"):])
	}
	return ""
}

// callerScope keeps one caller's Claude Code runs and saved sessions apart
// from another's on the same account: the gateway key it came with and the
// X-Magpie-Session a relay sets per user. Without it, every user behind
// one relay key was one owner, and two whose conversations read the same
// (a "hi" answered the same words) could be handed each other's run. Only
// a digest is kept.
func callerScope(r *http.Request) string {
	if r == nil {
		return ""
	}
	key, sess := callerKey(r), strings.TrimSpace(r.Header.Get(SessionHeader))
	if key == "" && sess == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("magpie-caller\x00" + key + "\x00" + sess))
	return hex.EncodeToString(sum[:12])
}
