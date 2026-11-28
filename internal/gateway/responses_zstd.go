package gateway

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/provider"
)

// Conditional outbound zstd compression for OpenAI Responses API requests
// (POST .../responses) outside the ChatGPT Codex account path (which is
// already zstd-compressed by provider.CodexCompressed).
//
// Modes via MAGPIE_RESPONSES_OUTBOUND_ZSTD:
//   - "" / "auto" / "1" / "on" / "true" (default): compress POST .../responses
//     bodies >= minBytes going to official OpenAI hosts (api.openai.com).
//   - "all": also attempt zstd against third-party Responses relays (e.g.
//     AsterGate, which accepts Content-Encoding: zstd on /v1/responses).
//   - "off" / "0" / "false": disabled.
//
// Optional tuning:
//   - MAGPIE_RESPONSES_OUTBOUND_ZSTD_LEVEL: fastest|default|better|best or 1..19
//     (default: default / level 3, matching Codex CLI & AsterGate).
//   - MAGPIE_OUTBOUND_COMPRESS_MIN_BYTES: minimum raw body size in bytes (default 2048).
//
// On 415, non-origin 403, or 400/501 encoding/JSON rejection, retries once
// uncompressed, restores req for downstream retry helpers, and latches the
// provider ID so subsequent requests go uncompressed directly.
var (
	responsesZstdEncodersMu sync.Mutex
	responsesZstdEncoders   = map[zstd.EncoderLevel]*zstd.Encoder{}
)

var responsesZstdRefused sync.Map // providerID -> struct{}

func responsesZstdMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_RESPONSES_OUTBOUND_ZSTD")))
	switch v {
	case "off", "0", "false", "no":
		return "off"
	case "all":
		return "all"
	default:
		return "auto"
	}
}

func responsesZstdLevel() zstd.EncoderLevel {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("MAGPIE_RESPONSES_OUTBOUND_ZSTD_LEVEL")))
	switch raw {
	case "", "default":
		return zstd.SpeedDefault
	case "fastest":
		return zstd.SpeedFastest
	case "better":
		return zstd.SpeedBetterCompression
	case "best":
		return zstd.SpeedBestCompression
	}
	if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 19 {
		return zstd.EncoderLevelFromZstd(n)
	}
	return zstd.SpeedDefault
}

func responsesZstdEncoderFor(level zstd.EncoderLevel) *zstd.Encoder {
	responsesZstdEncodersMu.Lock()
	enc := responsesZstdEncoders[level]
	if enc == nil {
		enc, _ = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(level),
			zstd.WithEncoderCRC(false),
			zstd.WithWindowSize(2<<20),
			zstd.WithEncoderConcurrency(2),
			zstd.WithLowerEncoderMem(true),
		)
		responsesZstdEncoders[level] = enc
	}
	responsesZstdEncodersMu.Unlock()
	return enc
}

func isResponsesTurnPath(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	p := strings.TrimRight(req.URL.Path, "/")
	return p == "/responses" || strings.HasSuffix(p, "/responses")
}

func shouldResponsesZstd(p provider.Provider, req *http.Request, body []byte) bool {
	if req == nil || req.Method != http.MethodPost || len(body) < outboundCompressMinBytes() {
		return false
	}
	if !isResponsesTurnPath(req) {
		return false
	}
	if enc := strings.TrimSpace(req.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return false
	}
	if req.Header.Get("X-Amz-Date") != "" || req.Header.Get("X-Amz-Content-Sha256") != "" {
		return false
	}
	if p.IsRemoteMagpie() {
		return false
	}
	if _, refused := responsesZstdRefused.Load(p.ID); refused {
		return false
	}
	switch responsesZstdMode() {
	case "off":
		return false
	case "all":
		return true
	default:
		return provider.HostOf(p.Base(provider.Responses)) == "api.openai.com"
	}
}

func zstdResponsesBody(body []byte) ([]byte, bool) {
	enc := responsesZstdEncoderFor(responsesZstdLevel())
	if enc == nil {
		return nil, false
	}
	z := enc.EncodeAll(body, make([]byte, 0, len(body)/4+64))
	if len(z) == 0 || len(z) >= len(body) {
		return nil, false
	}
	return z, true
}

type responsesZstdAttempt struct {
	providerID string
	raw        []byte
}

func maybeCompressResponsesRequest(p provider.Provider, req *http.Request, body []byte) (*responsesZstdAttempt, int) {
	if !shouldResponsesZstd(p, req, body) {
		return nil, len(body)
	}
	z, ok := zstdResponsesBody(body)
	if !ok {
		return nil, len(body)
	}
	req.Header.Set("Content-Encoding", "zstd")
	req.Body = io.NopCloser(bytes.NewReader(z))
	req.ContentLength = int64(len(z))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(z)), nil
	}
	return &responsesZstdAttempt{providerID: p.ID, raw: body}, len(z)
}

func (a *responsesZstdAttempt) after(req *http.Request, res *http.Response, err error, again func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if a == nil || err != nil || res == nil {
		return res, err
	}
	if res.StatusCode != http.StatusUnsupportedMediaType &&
		res.StatusCode != http.StatusBadRequest &&
		res.StatusCode != http.StatusForbidden &&
		res.StatusCode != http.StatusNotImplemented {
		return res, nil
	}
	origin := isOpenAIOriginResponse(res.Header)
	if res.StatusCode == http.StatusForbidden && origin {
		return res, nil
	}
	peek, rerr := io.ReadAll(io.LimitReader(res.Body, 4096))
	if rerr != nil {
		return res, nil
	}
	if !responsesZstdRejected(res.StatusCode, peek, origin) {
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), res.Body), res.Body}
		return res, nil
	}
	res.Body.Close()
	if a.providerID != "" {
		responsesZstdRefused.Store(a.providerID, struct{}{})
	}
	restorePlainRequest(req, a.raw)
	retryReq := req.Clone(req.Context())
	restorePlainRequest(retryReq, a.raw)
	return again(retryReq)
}

func isOpenAIOriginResponse(h http.Header) bool {
	if h == nil {
		return false
	}
	if strings.TrimSpace(h.Get("Openai-Version")) != "" || strings.TrimSpace(h.Get("Openai-Organization")) != "" {
		return true
	}
	if strings.TrimSpace(h.Get("Cf-Ray")) != "" && strings.HasPrefix(strings.TrimSpace(h.Get("X-Request-Id")), "req_") {
		return true
	}
	return false
}

func responsesZstdRejected(status int, body []byte, origin bool) bool {
	if status == http.StatusUnsupportedMediaType {
		return true
	}
	if status == http.StatusForbidden {
		return !origin
	}
	l := strings.ToLower(string(body))
	for _, needle := range []string{
		"content-encoding",
		"zstd",
		"unsupported encoding",
		"could not parse the json",
		"could not parse request body as json",
		"request body is not valid json",
		"bad json:",
		"invalid json",
		"unexpected token",
		"invalid character",
	} {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}
