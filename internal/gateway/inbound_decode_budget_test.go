package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/provider"
)

func withInboundDecodeBudget(t *testing.T, limit int64) {
	t.Helper()
	inboundDecodeBudget.mu.Lock()
	prevLimit := inboundDecodeBudget.testLimit
	prevInFlight := inboundDecodeBudget.inFlight
	inboundDecodeBudget.testLimit = limit
	inboundDecodeBudget.inFlight = 0
	inboundDecodeBudget.mu.Unlock()
	t.Cleanup(func() {
		inboundDecodeBudget.mu.Lock()
		inboundDecodeBudget.testLimit = prevLimit
		inboundDecodeBudget.inFlight = prevInFlight
		inboundDecodeBudget.mu.Unlock()
	})
}

func waitDecodeInFlight(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for inboundDecodeBudget.current() != want {
		if time.Now().After(deadline) {
			t.Fatalf("inboundDecodeBudget inFlight = %d, want %d", inboundDecodeBudget.current(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func gzipBytes(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, plain []byte, withFCS bool) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	if withFCS {
		return enc.EncodeAll(plain, nil)
	}
	var buf bytes.Buffer
	enc.Reset(&buf)
	if _, err := enc.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInboundDecodeBudgetDisabledByDefault(t *testing.T) {
	// Neither MAGPIE_LOW_MEMORY nor MAGPIE_INBOUND_DECODE_BUDGET_MB is set.
	if got := inboundDecodeBudgetLimit(); got != 0 {
		t.Fatalf("expected budget disabled by default, got %d", got)
	}
	payload := bytes.Repeat([]byte("a"), 256<<10)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(gzipBytes(t, payload)))
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	got, status, err := readBoundedRequestBody(w, req, requestLimits{})
	if err != nil || status != 0 || len(got) != len(payload) {
		t.Fatalf("default path failed: status=%d err=%v len=%d", status, err, len(got))
	}
}

func TestInboundDecodeBudgetEnvEnableAndPrechecks(t *testing.T) {
	// Explicit env works even when MAGPIE_LOW_MEMORY is off.
	t.Setenv("MAGPIE_INBOUND_DECODE_BUDGET_MB", "1")
	if got := inboundDecodeBudgetLimit(); got != 1<<20 {
		t.Fatalf("expected 1 MiB from env, got %d", got)
	}

	// 1. Plain Content-Length pre-check rejects with 429 before reading and releases count.
	reqPlain := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("ignored"))
	reqPlain.ContentLength = 2 << 20
	wPlain := httptest.NewRecorder()
	if _, status, err := readBoundedRequestBody(wPlain, reqPlain, requestLimits{}); status != http.StatusTooManyRequests || err == nil {
		t.Fatalf("expected 429 for oversized Content-Length, got status=%d err=%v", status, err)
	}
	if wPlain.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After header on 429, got %q", wPlain.Header().Get("Retry-After"))
	}
	waitDecodeInFlight(t, 0)

	// 2. Gzip ISIZE pre-check rejects oversized decompressed payload with 429 and releases count.
	bigPlain := bytes.Repeat([]byte("hello world json "), 100000) // ~1.7 MiB > 1 MiB budget
	gz := gzipBytes(t, bigPlain)
	if len(gz) >= 1<<20 {
		t.Fatalf("expected compressed gzip to be much smaller than 1 MiB, got %d", len(gz))
	}
	reqGz := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(gz))
	reqGz.Header.Set("Content-Encoding", "gzip")
	wGz := httptest.NewRecorder()
	if _, status, err := readBoundedRequestBody(wGz, reqGz, requestLimits{}); status != http.StatusTooManyRequests || err == nil {
		t.Fatalf("expected 429 for oversized gzip ISIZE, got status=%d err=%v", status, err)
	}
	waitDecodeInFlight(t, 0)

	// 3. Valid gzip within budget passes, strips Content-Encoding, and releases count.
	smallPlain := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"` + strings.Repeat("ok ", 2000) + `"}]}`)
	reqGzOK := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(gzipBytes(t, smallPlain)))
	reqGzOK.Header.Set("Content-Encoding", "gzip")
	wGzOK := httptest.NewRecorder()
	gotGz, status, err := readBoundedRequestBody(wGzOK, reqGzOK, requestLimits{})
	if err != nil || status != 0 || string(gotGz) != string(smallPlain) {
		t.Fatalf("valid gzip failed: status=%d err=%v len=%d", status, err, len(gotGz))
	}
	if reqGzOK.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding should be stripped after gzip decode")
	}
	waitDecodeInFlight(t, 0)

	// 4. Non-gzip garbage still returns 400 (not false-positive 429).
	reqBadGz := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("not valid gzip data at all 1234567890"))
	reqBadGz.Header.Set("Content-Encoding", "gzip")
	wBadGz := httptest.NewRecorder()
	if _, status, err := readBoundedRequestBody(wBadGz, reqBadGz, requestLimits{}); status != http.StatusBadRequest || err == nil {
		t.Fatalf("expected 400 for invalid gzip, got status=%d err=%v", status, err)
	}
	waitDecodeInFlight(t, 0)

	// 5. Zstd FrameContentSize pre-check rejects oversized frame with 429 before decoding.
	zsWithFCS := zstdBytes(t, bigPlain, true)
	reqZs := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zsWithFCS))
	reqZs.Header.Set("Content-Encoding", "zstd")
	wZs := httptest.NewRecorder()
	if _, status, err := readBoundedRequestBody(wZs, reqZs, requestLimits{}); status != http.StatusTooManyRequests || err == nil {
		t.Fatalf("expected 429 for oversized zstd FCS, got status=%d err=%v", status, err)
	}
	waitDecodeInFlight(t, 0)

	// 6. Streaming zstd without FrameContentSize is still caught mid-decode by budgetedReader.
	zsStream := zstdBytes(t, bigPlain, false)
	reqZsStream := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zsStream))
	reqZsStream.Header.Set("Content-Encoding", "zstd")
	wZsStream := httptest.NewRecorder()
	if _, status, err := readBoundedRequestBody(wZsStream, reqZsStream, requestLimits{}); status != http.StatusTooManyRequests || err == nil {
		t.Fatalf("expected 429 for streaming zstd without FCS, got status=%d err=%v", status, err)
	}
	waitDecodeInFlight(t, 0)

	// 7. Valid zstd within budget passes and releases count.
	reqZsOK := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zstdBytes(t, smallPlain, true)))
	reqZsOK.Header.Set("Content-Encoding", "zstd")
	wZsOK := httptest.NewRecorder()
	gotZs, status, err := readBoundedRequestBody(wZsOK, reqZsOK, requestLimits{})
	if err != nil || status != 0 || string(gotZs) != string(smallPlain) {
		t.Fatalf("valid zstd failed: status=%d err=%v len=%d", status, err, len(gotZs))
	}
	waitDecodeInFlight(t, 0)
}

func TestInboundDecodeBudgetMultistreamGzipAndConcurrent429(t *testing.T) {
	withInboundDecodeBudget(t, 128<<10) // 128 KiB budget

	// Multistream gzip: each member has small ISIZE (80 KiB), sum is 160 KiB > 128 KiB.
	part := bytes.Repeat([]byte("m"), 80<<10)
	multi := append(gzipBytes(t, part), gzipBytes(t, part)...)
	reqMulti := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(multi))
	reqMulti.Header.Set("Content-Encoding", "gzip")
	wMulti := httptest.NewRecorder()
	if _, status, err := readBoundedRequestBody(wMulti, reqMulti, requestLimits{}); status != http.StatusTooManyRequests || err == nil {
		t.Fatalf("expected 429 for multistream gzip exceeding budget, got status=%d err=%v", status, err)
	}
	waitDecodeInFlight(t, 0)

	// Concurrent live requests: Request A holds 80 KiB until its context ends;
	// Request B (80 KiB gzip) arrives concurrently and gets 429 immediately;
	// after Request A finishes, Request C succeeds and inFlight returns to 0.
	ctxA, cancelA := context.WithCancel(context.Background())
	reqA := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(gzipBytes(t, part))).WithContext(ctxA)
	reqA.Header.Set("Content-Encoding", "gzip")
	wA := httptest.NewRecorder()
	bodyA, statusA, errA := readBoundedRequestBody(wA, reqA, requestLimits{})
	if errA != nil || statusA != 0 || len(bodyA) != len(part) {
		cancelA()
		t.Fatalf("request A failed: status=%d err=%v", statusA, errA)
	}
	// Wire overhead was shrunk away; exactly len(part) remains held for ctxA's lifetime.
	waitDecodeInFlight(t, int64(len(part)))

	// Request B (80 KiB zstd) exceeds remaining 48 KiB -> 429, and does not leak A's count.
	reqB := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zstdBytes(t, part, true)))
	reqB.Header.Set("Content-Encoding", "zstd")
	wB := httptest.NewRecorder()
	if _, statusB, errB := readBoundedRequestBody(wB, reqB, requestLimits{}); statusB != http.StatusTooManyRequests || errB == nil {
		cancelA()
		t.Fatalf("expected 429 for concurrent request B, got status=%d err=%v", statusB, errB)
	}
	waitDecodeInFlight(t, int64(len(part)))

	// Server.readRequestBody also fails fast with 429 for oversized plain Content-Length.
	s := New()
	reqFast := httptest.NewRequest(http.MethodPost, "/v1/messages", io.NopCloser(bytes.NewReader(part)))
	reqFast.ContentLength = int64(len(part))
	wFast := httptest.NewRecorder()
	if _, ok := s.readRequestBody(wFast, reqFast, provider.Anthropic, 0); ok || wFast.Code != http.StatusTooManyRequests {
		cancelA()
		t.Fatalf("expected fast 429 in readRequestBody, got ok=%v code=%d", ok, wFast.Code)
	}

	// Cancel A -> budget released -> Request C succeeds.
	cancelA()
	waitDecodeInFlight(t, 0)

	reqC := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zstdBytes(t, part, true)))
	reqC.Header.Set("Content-Encoding", "zstd")
	wC := httptest.NewRecorder()
	bodyC, statusC, errC := readBoundedRequestBody(wC, reqC, requestLimits{})
	if errC != nil || statusC != 0 || len(bodyC) != len(part) {
		t.Fatalf("request C after release failed: status=%d err=%v", statusC, errC)
	}
	waitDecodeInFlight(t, 0)
}
