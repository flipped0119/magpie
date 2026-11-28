package gateway

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestAnthropicOutboundGzipAutoOfficialAndAllRelayFallback(t *testing.T) {
	t.Cleanup(func() {
		anthropicGzipRefused.Delete("relay-no-gz")
		anthropicGzipRefused.Delete("official-gz")
	})

	bigBody := []byte(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"` + strings.Repeat("hello world context ", 250) + `"}]}`)

	// 1. Default auto mode compresses official Anthropic host bodies >= 2 KiB.
	pOfficial := provider.Provider{ID: "official-gz", Anthropic: "https://api.anthropic.com"}
	reqOfficial := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	att, sent := maybeCompressAnthropicRequest(pOfficial, reqOfficial, bigBody)
	if att == nil || reqOfficial.Header.Get("Content-Encoding") != "gzip" || sent >= len(bigBody) {
		t.Fatalf("expected gzip on official Anthropic host: att=%v enc=%q sent=%d raw=%d", att, reqOfficial.Header.Get("Content-Encoding"), sent, len(bigBody))
	}
	gr, err := gzip.NewReader(reqOfficial.Body)
	if err != nil {
		t.Fatalf("invalid gzip payload: %v", err)
	}
	decoded, err := io.ReadAll(gr)
	gr.Close()
	if err != nil || string(decoded) != string(bigBody) {
		t.Fatalf("gzip roundtrip mismatch: err=%v len=%d/%d", err, len(decoded), len(bigBody))
	}

	// 2. Default auto mode leaves third-party relay hosts uncompressed.
	pRelay := provider.Provider{ID: "relay-no-gz", Anthropic: "https://relay.example.com"}
	reqRelay := httptest.NewRequest(http.MethodPost, "https://relay.example.com/v1/messages", nil)
	if att2, _ := maybeCompressAnthropicRequest(pRelay, reqRelay, bigBody); att2 != nil || reqRelay.Header.Get("Content-Encoding") != "" {
		t.Fatalf("auto mode must not gzip third-party host by default")
	}

	// 3. MAGPIE_ANTHROPIC_OUTBOUND_GZIP=all compresses relay, retries uncompressed on 415, and latches provider.
	t.Setenv("MAGPIE_ANTHROPIC_OUTBOUND_GZIP", "all")
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Content-Encoding") == "gzip" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported Content-Encoding: gzip"}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != string(bigBody) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	pRelay.Anthropic = up.URL
	req1, _ := http.NewRequest(http.MethodPost, up.URL+"/v1/messages", nil)
	att3, _ := maybeCompressAnthropicRequest(pRelay, req1, bigBody)
	if att3 == nil || req1.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("expected gzip in all mode before latch")
	}
	res1, err := up.Client().Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resFinal, err := att3.after(req1, res1, nil, up.Client().Do)
	if err != nil {
		t.Fatal(err)
	}
	defer resFinal.Body.Close()
	if resFinal.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("expected 200 after uncompressed retry (2 calls), got status=%d calls=%d", resFinal.StatusCode, calls.Load())
	}

	// Subsequent request to the latched provider skips gzip directly.
	req2, _ := http.NewRequest(http.MethodPost, up.URL+"/v1/messages", nil)
	if att4, _ := maybeCompressAnthropicRequest(pRelay, req2, bigBody); att4 != nil || req2.Header.Get("Content-Encoding") != "" {
		t.Fatal("latched provider should skip gzip directly")
	}

	// 4. SigV4 signed requests are never re-encoded.
	reqSig := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	reqSig.Header.Set("X-Amz-Date", "20261125T000000Z")
	if att5, _ := maybeCompressAnthropicRequest(pOfficial, reqSig, bigBody); att5 != nil {
		t.Fatal("SigV4 signed request must not be gzipped after signing")
	}
}

func TestAnthropicOutboundGzipLevelThresholdOrigin403AndReqRestore(t *testing.T) {
	t.Cleanup(func() {
		anthropicGzipRefused.Delete("p-403-waf")
		anthropicGzipRefused.Delete("p-403-origin")
		anthropicGzipRefused.Delete("p-restore")
	})

	bigBody := []byte(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"` + strings.Repeat("context block alpha beta gamma ", 300) + `"}]}`)

	// Custom min bytes skips compression when body is smaller.
	t.Setenv("MAGPIE_OUTBOUND_COMPRESS_MIN_BYTES", "999999")
	pOfficial := provider.Provider{ID: "p-level", Anthropic: "https://api.anthropic.com"}
	reqSmall := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if att, sent := maybeCompressAnthropicRequest(pOfficial, reqSmall, bigBody); att != nil || sent != len(bigBody) {
		t.Fatalf("expected skip below custom min bytes: att=%v sent=%d", att, sent)
	}

	// Level 9 vs level 1 both produce valid gzip payloads.
	t.Setenv("MAGPIE_OUTBOUND_COMPRESS_MIN_BYTES", "2048")
	t.Setenv("MAGPIE_ANTHROPIC_OUTBOUND_GZIP_LEVEL", "9")
	reqL9 := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	att9, sent9 := maybeCompressAnthropicRequest(pOfficial, reqL9, bigBody)
	if att9 == nil || sent9 >= len(bigBody) {
		t.Fatalf("expected level 9 gzip: att=%v sent=%d", att9, sent9)
	}
	gr, err := gzip.NewReader(reqL9.Body)
	if err != nil {
		t.Fatalf("invalid level 9 gzip: %v", err)
	}
	plain, _ := io.ReadAll(gr)
	gr.Close()
	if string(plain) != string(bigBody) {
		t.Fatalf("level 9 roundtrip mismatch")
	}

	// Origin 403 (with Cf-Ray / Request-Id: req_*) must NOT retry or latch.
	t.Setenv("MAGPIE_ANTHROPIC_OUTBOUND_GZIP", "all")
	var originCalls atomic.Int32
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		w.Header().Set("Cf-Ray", "9999-SJC")
		w.Header().Set("Request-Id", "req_abc123")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"forbidden"}}`))
	}))
	defer originSrv.Close()

	pOrig := provider.Provider{ID: "p-403-origin", Anthropic: originSrv.URL}
	reqOrig, _ := http.NewRequest(http.MethodPost, originSrv.URL+"/v1/messages", nil)
	attOrig, _ := maybeCompressAnthropicRequest(pOrig, reqOrig, bigBody)
	resOrig, err := originSrv.Client().Do(reqOrig)
	if err != nil {
		t.Fatal(err)
	}
	resOrig, err = attOrig.after(reqOrig, resOrig, nil, originSrv.Client().Do)
	if err != nil {
		t.Fatal(err)
	}
	resOrig.Body.Close()
	if resOrig.StatusCode != http.StatusForbidden || originCalls.Load() != 1 {
		t.Fatalf("origin 403 must not retry: status=%d calls=%d", resOrig.StatusCode, originCalls.Load())
	}
	if _, latched := anthropicGzipRefused.Load("p-403-origin"); latched {
		t.Fatal("origin 403 must not latch provider")
	}

	// Non-origin 403 (middlebox/WAF) retries uncompressed, restores req (including GetBody), and latches.
	var wafCalls atomic.Int32
	wafSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wafCalls.Add(1)
		if r.Header.Get("Content-Encoding") == "gzip" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("blocked compressed request"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer wafSrv.Close()

	pWaf := provider.Provider{ID: "p-403-waf", Anthropic: wafSrv.URL}
	reqWaf, _ := http.NewRequest(http.MethodPost, wafSrv.URL+"/v1/messages", nil)
	attWaf, _ := maybeCompressAnthropicRequest(pWaf, reqWaf, bigBody)
	resWaf, err := wafSrv.Client().Do(reqWaf)
	if err != nil {
		t.Fatal(err)
	}
	resWaf, err = attWaf.after(reqWaf, resWaf, nil, wafSrv.Client().Do)
	if err != nil {
		t.Fatal(err)
	}
	resWaf.Body.Close()
	if resWaf.StatusCode != http.StatusOK || wafCalls.Load() != 2 {
		t.Fatalf("non-origin 403 should retry uncompressed: status=%d calls=%d", resWaf.StatusCode, wafCalls.Load())
	}
	if reqWaf.Header.Get("Content-Encoding") != "" {
		t.Fatalf("fallback must strip Content-Encoding from original req, got %q", reqWaf.Header.Get("Content-Encoding"))
	}
	gb, err := reqWaf.GetBody()
	if err != nil {
		t.Fatalf("restored GetBody failed: %v", err)
	}
	gotPlain, _ := io.ReadAll(gb)
	gb.Close()
	if string(gotPlain) != string(bigBody) {
		t.Fatalf("restored GetBody must return uncompressed bytes")
	}
}
