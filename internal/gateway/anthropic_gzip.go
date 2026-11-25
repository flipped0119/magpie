package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/provider"
)

// Conditional outbound gzip compression for Anthropic Messages requests.
//
// Modes via MAGPIE_ANTHROPIC_OUTBOUND_GZIP:
//   - "" / "auto" / "1" / "on" / "true" (default): compress requests >= 2 KiB
//     going to official Anthropic hosts (matching Claude Code 2.1.321+ wire
//     behavior) without mutating third-party/relay payloads unexpectedly.
//   - "all": also attempt gzip against third-party Anthropic-compatible relays.
//   - "off" / "0" / "false": disabled.
//
// If an upstream rejects a gzip-encoded body (HTTP 400/415/501 mentioning
// encoding/gzip/JSON parse failure), the gateway retries once uncompressed and
// latches the provider ID so later requests go uncompressed directly.
const anthropicGzipMinBytes = 2048

var anthropicGzipWriters = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return w
	},
}

var anthropicGzipRefused sync.Map // providerID -> struct{}

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
	if len(body) < anthropicGzipMinBytes || req == nil {
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
	zw := anthropicGzipWriters.Get().(*gzip.Writer)
	zw.Reset(&buf)
	_, werr := zw.Write(body)
	cerr := zw.Close()
	zw.Reset(io.Discard)
	anthropicGzipWriters.Put(zw)
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
		res.StatusCode != http.StatusNotImplemented {
		return res, nil
	}
	peek, rerr := io.ReadAll(io.LimitReader(res.Body, 4096))
	if rerr != nil {
		return res, nil
	}
	if !anthropicGzipRejected(res.StatusCode, peek) {
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
	retryReq := req.Clone(req.Context())
	retryReq.Header.Del("Content-Encoding")
	retryReq.Body = io.NopCloser(bytes.NewReader(a.raw))
	retryReq.ContentLength = int64(len(a.raw))
	retryReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(a.raw)), nil
	}
	return again(retryReq)
}

func anthropicGzipRejected(status int, body []byte) bool {
	if status == http.StatusUnsupportedMediaType {
		return true
	}
	l := strings.ToLower(string(body))
	for _, needle := range []string{
		"content-encoding",
		"gzip",
		"unsupported encoding",
		"could not parse the json",
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
