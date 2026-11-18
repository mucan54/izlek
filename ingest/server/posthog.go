package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/mucan54/izlek/ingest/wal"
	"github.com/mucan54/izlek/ingest/wire"
)

// PostHog capture bodies arrive url- and base64-encoded in some modes; allow
// that much transport overhead on top of the 1 MiB decoded JSON limit.
const postHogMaxRawBody = 2 * wire.MaxBatchBytes

const (
	postHogOKBody = `{"status": 1}`
	// spec/flags.md §6: the fixed /decide and /flags response.
	postHogFlagsBody = `{"config": {"enable_collect_everything": false}, "toolbarParams": {}, "isAuthenticated": false, ` +
		`"supportedCompression": ["gzip", "gzip-js"], "featureFlags": {}, "featureFlagPayloads": {}, ` +
		`"flags": {}, "errorsWhileComputingFlags": false, "sessionRecording": false, ` +
		`"surveys": false, "capturePerformance": false, "autocapture_opt_out": true}`
)

// handleFlags answers /decide and /flags: any method, any query, fixed body.
func (s *Server) handleFlags(_ *http.Request, rc *reqCtx) reply {
	rc.cors = corsAllow // a constant, public document
	return reply{status: http.StatusOK, body: postHogFlagsBody, ctype: "application/json"}
}

// handlePostHog is the capture adapter for /batch, /i/v0/e, /e and /capture
// (design §3.7, spec/wal.md §2.2). Transport layers (gzip, form encoding,
// base64) are peeled off; the JSON itself is validated and stored unchanged
// as a format=2 entry. Mapping to events happens in the shipper.
func (s *Server) handlePostHog(r *http.Request, rc *reqCtx) reply {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		return reply{status: http.StatusMethodNotAllowed}
	}
	// The token lives in the body, so only the IP limit can run first.
	ip := s.clientIP(r)
	if s.ipLimited(ip, rc.recv) {
		return reply{status: http.StatusTooManyRequests, retryAfter: retryRateLimited}
	}
	if s.overloaded() {
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryBackpressure}
	}
	if r.ContentLength > postHogMaxRawBody {
		return reply{status: http.StatusRequestEntityTooLarge}
	}
	rp := getBuf()
	defer putBuf(rp)
	raw, err := readInto((*rp)[:0], r.Body, postHogMaxRawBody, r.ContentLength)
	*rp = raw
	if err != nil {
		return bodyErrorReply(err)
	}
	doc, err := decodePostHogBody(r, raw)
	if err != nil {
		return bodyErrorReply(err)
	}
	if !json.Valid(doc) {
		return reply{status: http.StatusBadRequest}
	}
	info := scanPostHog(doc)
	if !info.ok {
		return reply{status: http.StatusBadRequest}
	}

	// Token: body (api_key -> token -> first event's properties.token), then
	// Authorization: Bearer, then ?t=.
	tokenStr := info.token
	if tokenStr == "" {
		tokenStr = bearerToken(r)
	}
	tok, rep, ok := s.authenticate(tokenStr, rc)
	if !ok {
		return rep
	}
	if s.tokenLimited(tok, rc.recv) {
		return reply{status: http.StatusTooManyRequests, retryAfter: retryRateLimited}
	}
	okReply := reply{status: http.StatusOK, body: postHogOKBody, ctype: "application/json"}
	if info.events == 0 {
		return okReply // empty batch: nothing to write
	}

	ep := getBuf()
	defer putBuf(ep)
	entry, err := wal.AppendEntry((*ep)[:0], wal.Envelope{
		Format:    wal.FormatPostHog,
		ProjectID: tok.ProjectID,
		RecvTSMs:  rc.recvMs(),
		Country:   s.country(ip),
		Flags:     envFlags(rc),
	}, doc)
	*ep = entry
	if err != nil {
		return reply{status: http.StatusRequestEntityTooLarge}
	}
	if err := s.writer.Append(entry); err != nil {
		s.m.walWriteErrors.Inc()
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryTransient}
	}
	s.m.bytesIn.Add(uint64(len(doc)))
	s.m.recordsIn.Add(uint64(info.events))
	return okReply
}

// decodePostHogBody strips the transport encodings PostHog SDKs use and
// returns the JSON text:
//
//   - Content-Encoding: gzip, ?compression=gzip / gzip-js, or a body that
//     simply starts with the gzip magic;
//   - application/x-www-form-urlencoded "data=<base64 or JSON>" (legacy web,
//     sendBeacon), also as ?data= on a GET;
//   - bare base64 (?compression=base64);
//   - plain JSON.
//
// The decoded JSON is limited to 1 MiB (errTooLarge -> 413).
func decodePostHogBody(r *http.Request, raw []byte) ([]byte, error) {
	const limit = wire.MaxBatchBytes
	q := r.URL.Query()
	compression := q.Get("compression")
	data := raw
	fromForm := false
	if len(bytes.TrimSpace(data)) == 0 {
		if d := q.Get("data"); d != "" { // GET /e/?data=...
			data, fromForm = []byte(d), true
		} else {
			return nil, errBadBody
		}
	}

	ce := r.Header.Get("Content-Encoding")
	if strings.EqualFold(ce, "gzip") || compression == "gzip" || compression == "gzip-js" || hasGzipMagic(data) {
		if !hasGzipMagic(data) {
			// Declared but not actually compressed (some proxies decompress
			// and keep the header): fall through and treat as-is.
		} else {
			out, err := gunzipInto(nil, data, limit)
			if err != nil {
				return nil, err
			}
			data = out
		}
	} else if ce != "" && !strings.EqualFold(ce, "identity") {
		return nil, errBadBody
	}

	if !fromForm && (strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || bytes.HasPrefix(data, []byte("data="))) {
		if first := firstNonSpace(data); first != '{' && first != '[' {
			vals, err := url.ParseQuery(string(data))
			if err != nil || vals.Get("data") == "" {
				return nil, errBadBody
			}
			if c := vals.Get("compression"); c != "" {
				compression = c
			}
			data = []byte(vals.Get("data"))
		}
	}
	if compression == "lz64" {
		return nil, errBadBody // legacy lz-string: not supported
	}

	if first := firstNonSpace(data); first != '{' && first != '[' {
		out, err := decodeBase64(bytes.TrimSpace(data))
		if err != nil {
			return nil, err
		}
		data = out
		if hasGzipMagic(data) {
			if out, err = gunzipInto(nil, data, limit); err != nil {
				return nil, err
			}
			data = out
		}
	}
	if len(data) > limit {
		return nil, errTooLarge
	}
	if len(data) == 0 {
		return nil, errBadBody
	}
	return data, nil
}

func firstNonSpace(b []byte) byte {
	for _, c := range b {
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			return c
		}
	}
	return 0
}
