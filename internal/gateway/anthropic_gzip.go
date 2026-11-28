package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/provider"
)

// Conditional outbound gzip compression for Anthropic Messages requests.
//
// Modes via MAGPIE_ANTHROPIC_OUTBOUND_GZIP:
//   - "" / "auto" / "1" / "on" / "true" (default): compress requests >= minBytes
//     going to official Anthropic hosts (matching Claude Code 2.1.321+ wire
//     behavior) without mutating third-party/relay payloads unexpectedly.
//   - "all": also attempt gzip against third-party Anthropic-compatible relays.
//   - "off" / "0" / "false": disabled.
//
// Optional tuning:
//   - MAGPIE_ANTHROPIC_OUTBOUND_GZIP_LEVEL: gzip level 1..9 (default 6).
//   - MAGPIE_OUTBOUND_COMPRESS_MIN_BYTES: minimum raw body size in bytes (default 2048).
//
// If an upstream rejects a gzip-encoded body (HTTP 415, non-origin 403, or
// 400/501 mentioning encoding/gzip/JSON parse failure), the gateway retries
// once uncompressed, restores req for downstream retry wrappers, and latches
// the provider ID so later requests go uncompressed directly.
const defaultOutboundCompressMinBytes = 2048

var (
	anthropicGzipPoolsMu sync.Mutex
	anthropicGzipPools   [10]*sync.Pool // indexed by level 1..9
)

var anthropicGzipRefused sync.Map // providerID -> struct{}

func outboundCompressMinBytes() int {
	raw := strings.TrimSpace(os.Getenv("MAGPIE_OUTBOUND_COMPRESS_MIN_BYTES"))
	if raw == "" {
		return defaultOutboundCompressMinBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultOutboundCompressMinBytes
	}
	return n
}

func anthropicGzipLevel() int {
	raw := strings.TrimSpace(os.Getenv("MAGPIE_ANTHROPIC_OUTBOUND_GZIP_LEVEL"))
	if raw == "" {
		return gzip.DefaultCompression
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < gzip.BestSpeed || n > gzip.BestCompression {
		return gzip.DefaultCompression
	}
	return n
}

func anthropicGzipPoolFor(level int) *sync.Pool {
	idx := level
	if idx < gzip.BestSpeed || idx > gzip.BestCompression {
		idx = 6
	}
	anthropicGzipPoolsMu.Lock()
	p := anthropicGzipPools[idx]
	if p == nil {
		lvl := level
		p = &sync.Pool{
			New: func() any {
				w, _ := gzip.NewWriterLevel(io.Discard, lvl)
				return w
			},
		}
		anthropicGzipPools[idx] = p
	}
	anthropicGzipPoolsMu.Unlock()
	return p
}

func anthropicGzipMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_ANTHROPIC_OUTBOUND_GZIP")))
	switch v {
	case "off", "0", "false", "no":
		return "off"
	case "all":
		return "all"
	default:
		return "auto"
	}
}

func shouldAnthropicGzip(p provider.Provider, req *http.Request, body []byte) bool {
	if req == nil || req.Method != http.MethodPost || len(body) < outboundCompressMinBytes() {
		return false
	}
	if req.Header.Get("Content-Encoding") != "" {
		return false
	}
	// Never re-encode payloads signed with AWS SigV4 (Bedrock).
	if req.Header.Get("X-Amz-Date") != "" || req.Header.Get("X-Amz-Content-Sha256") != "" {
		return false
	}
	if p.IsRemoteMagpie() {
		return false
	}
	if _, refused := anthropicGzipRefused.Load(p.ID); refused {
		return false
	}
	switch anthropicGzipMode() {
	case "off":
		return false
	case "all":
		return true
	default:
		return provider.HostOf(p.Base(provider.Anthropic)) == "api.anthropic.com"
	}
}

func gzipAnthropicBody(body []byte) ([]byte, bool) {
	var buf bytes.Buffer
	buf.Grow(len(body) / 3)
	pool := anthropicGzipPoolFor(anthropicGzipLevel())
	zw := pool.Get().(*gzip.Writer)
	zw.Reset(&buf)
	_, werr := zw.Write(body)
	cerr := zw.Close()
	zw.Reset(io.Discard)
	pool.Put(zw)
	if werr != nil || cerr != nil || buf.Len() >= len(body) {
		return nil, false
	}
	return buf.Bytes(), true
}

type anthropicGzipAttempt struct {
	providerID string
	raw        []byte
}

func maybeCompressAnthropicRequest(p provider.Provider, req *http.Request, body []byte) (*anthropicGzipAttempt, int) {
	if !shouldAnthropicGzip(p, req, body) {
		return nil, len(body)
	}
	gz, ok := gzipAnthropicBody(body)
	if !ok {
		return nil, len(body)
	}
	req.Header.Set("Content-Encoding", "gzip")
	req.Body = io.NopCloser(bytes.NewReader(gz))
	req.ContentLength = int64(len(gz))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(gz)), nil
	}
	return &anthropicGzipAttempt{providerID: p.ID, raw: body}, len(gz)
}

func (a *anthropicGzipAttempt) after(req *http.Request, res *http.Response, err error, again func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if a == nil || err != nil || res == nil {
		return res, err
	}
	if res.StatusCode != http.StatusUnsupportedMediaType &&
		res.StatusCode != http.StatusBadRequest &&
		res.StatusCode != http.StatusForbidden &&
		res.StatusCode != http.StatusNotImplemented {
		return res, nil
	}
	origin := isAnthropicOriginResponse(res.Header)
	if res.StatusCode == http.StatusForbidden && origin {
		// Genuine Anthropic origin 403 (auth/permission) is not an encoding rejection.
		return res, nil
	}
	peek, rerr := io.ReadAll(io.LimitReader(res.Body, 4096))
	if rerr != nil {
		return res, nil
	}
	if !anthropicGzipRejected(res.StatusCode, peek, origin) {
		// Restore the peeked body so downstream error handling sees it intact.
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), res.Body), res.Body}
		return res, nil
	}
	res.Body.Close()
	if a.providerID != "" {
		anthropicGzipRefused.Store(a.providerID, struct{}{})
	}
	// Restore req itself so later retry helpers (e.g. provider.RetryOneAuth)
	// never resend gzip bytes to a provider that just rejected gzip.
	restorePlainRequest(req, a.raw)
	retryReq := req.Clone(req.Context())
	restorePlainRequest(retryReq, a.raw)
	return again(retryReq)
}

func restorePlainRequest(req *http.Request, raw []byte) {
	if req == nil {
		return
	}
	req.Header.Del("Content-Encoding")
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
}

// isAnthropicOriginResponse checks upstream transport headers for evidence that
// the response came from Anthropic's own origin rather than an intermediary/WAF.
func isAnthropicOriginResponse(h http.Header) bool {
	if h == nil {
		return false
	}
	if strings.TrimSpace(h.Get("Cf-Ray")) != "" {
		return true
	}
	for _, v := range h.Values("Request-Id") {
		if strings.HasPrefix(strings.TrimSpace(v), "req_") {
			return true
		}
	}
	return false
}

func anthropicGzipRejected(status int, body []byte, origin bool) bool {
	if status == http.StatusUnsupportedMediaType {
		return true
	}
	if status == http.StatusForbidden {
		return !origin
	}
	l := strings.ToLower(string(body))
	for _, needle := range []string{
		"content-encoding",
		"gzip",
		"unsupported encoding",
		"could not parse the json",
		"could not parse request body as json",
		"request body is not valid json",
		"bad json:",
		"invalid json",
		"unexpected token",
		"invalid character '\\x1f'",
	} {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}
