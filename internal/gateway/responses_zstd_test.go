package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/provider"
)

func TestResponsesOutboundZstdAutoOfficialAndRelayFallback(t *testing.T) {
	t.Cleanup(func() {
		responsesZstdRefused.Delete("resp-relay-no-zstd")
		responsesZstdRefused.Delete("resp-origin-403")
	})

	bigBody := []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"` + strings.Repeat("responses context payload ", 250) + `"}]}`)

	// 1. Default auto mode compresses official api.openai.com /v1/responses >= minBytes.
	pOfficial := provider.Provider{ID: "openai-official", Responses: "https://api.openai.com/v1"}
	reqOfficial := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	att, sent := maybeCompressResponsesRequest(pOfficial, reqOfficial, bigBody)
	if att == nil || reqOfficial.Header.Get("Content-Encoding") != "zstd" || sent >= len(bigBody) {
		t.Fatalf("expected zstd on official OpenAI Responses: att=%v enc=%q sent=%d raw=%d", att, reqOfficial.Header.Get("Content-Encoding"), sent, len(bigBody))
	}
	zr, err := zstd.NewReader(reqOfficial.Body)
	if err != nil {
		t.Fatalf("invalid zstd payload: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	zr.Close()
	if err != nil || string(decoded) != string(bigBody) {
		t.Fatalf("zstd roundtrip mismatch: err=%v len=%d/%d", err, len(decoded), len(bigBody))
	}

	// Non-turn paths (e.g. /v1/responses/compact) stay plain.
	reqCompact := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses/compact", nil)
	if attC, _ := maybeCompressResponsesRequest(pOfficial, reqCompact, bigBody); attC != nil {
		t.Fatal("/v1/responses/compact must not be zstd-compressed")
	}

	// Already compressed requests (e.g. CodexCompressed) are never double-encoded.
	reqAlready := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	reqAlready.Header.Set("Content-Encoding", "zstd")
	if attA, _ := maybeCompressResponsesRequest(pOfficial, reqAlready, bigBody); attA != nil {
		t.Fatal("already zstd-encoded request must not be re-encoded")
	}

	// 2. Default auto mode leaves third-party relay hosts uncompressed.
	pRelay := provider.Provider{ID: "resp-relay-no-zstd", Responses: "https://relay.example.com/v1"}
	reqRelay := httptest.NewRequest(http.MethodPost, "https://relay.example.com/v1/responses", nil)
	if att2, _ := maybeCompressResponsesRequest(pRelay, reqRelay, bigBody); att2 != nil || reqRelay.Header.Get("Content-Encoding") != "" {
		t.Fatal("auto mode must not zstd third-party host by default")
	}

	// 3. MAGPIE_RESPONSES_OUTBOUND_ZSTD=all compresses relay, retries uncompressed on 415, restores req, and latches.
	t.Setenv("MAGPIE_RESPONSES_OUTBOUND_ZSTD", "all")
	t.Setenv("MAGPIE_RESPONSES_OUTBOUND_ZSTD_LEVEL", "better")
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Content-Encoding") == "zstd" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported Content-Encoding: zstd"}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != string(bigBody) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	pRelay.Responses = up.URL + "/v1"
	req1, _ := http.NewRequest(http.MethodPost, up.URL+"/v1/responses", nil)
	att3, _ := maybeCompressResponsesRequest(pRelay, req1, bigBody)
	if att3 == nil || req1.Header.Get("Content-Encoding") != "zstd" {
		t.Fatal("expected zstd in all mode before latch")
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
	if req1.Header.Get("Content-Encoding") != "" {
		t.Fatalf("fallback must strip Content-Encoding from req")
	}

	// Subsequent request to the latched provider skips zstd directly.
	req2, _ := http.NewRequest(http.MethodPost, up.URL+"/v1/responses", nil)
	if att4, _ := maybeCompressResponsesRequest(pRelay, req2, bigBody); att4 != nil || req2.Header.Get("Content-Encoding") != "" {
		t.Fatal("latched provider should skip zstd directly")
	}

	// 4. Origin 403 (with Openai-Version header) must NOT retry or latch.
	var originCalls atomic.Int32
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		w.Header().Set("Openai-Version", "2020-10-01")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"permission denied"}}`))
	}))
	defer originSrv.Close()

	pOrig := provider.Provider{ID: "resp-origin-403", Responses: originSrv.URL + "/v1"}
	reqOrig, _ := http.NewRequest(http.MethodPost, originSrv.URL+"/v1/responses", nil)
	attOrig, _ := maybeCompressResponsesRequest(pOrig, reqOrig, bigBody)
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
}
