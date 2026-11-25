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
