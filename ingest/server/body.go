package server

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

var (
	errTooLarge   = errors.New("body too large")
	errBadBody    = errors.New("malformed body")
	errClientRead = errors.New("reading request body failed")
)

// bufPool recycles body buffers. Buffers grow to at most a little over the
// 1 MiB body limit, so the pool's footprint is bounded by MaxInflight.
var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 32<<10)
	return &b
}}

func getBuf() *[]byte { return bufPool.Get().(*[]byte) }

func putBuf(b *[]byte) {
	if cap(*b) <= 4<<20 {
		*b = (*b)[:0]
		bufPool.Put(b)
	}
}

// readInto appends everything r yields to dst, failing with errTooLarge as
// soon as more than limit bytes arrive. hint is the expected size or <= 0.
func readInto(dst []byte, r io.Reader, limit int, hint int64) ([]byte, error) {
	start := len(dst)
	if hint > 0 && hint <= int64(limit) {
		if need := start + int(hint) + 1; cap(dst) < need {
			grown := make([]byte, start, need)
			copy(grown, dst)
			dst = grown
		}
	}
	for {
		if len(dst) == cap(dst) {
			dst = append(dst, 0)[:len(dst)]
		}
		n, err := r.Read(dst[len(dst):cap(dst)])
		dst = dst[:len(dst)+n]
		if len(dst)-start > limit {
			return dst, errTooLarge
		}
		if err == io.EOF {
			return dst, nil
		}
		if err != nil {
			return dst, err
		}
	}
}

var gzipPool sync.Pool

// gunzipInto appends the decompressed content of src to dst, enforcing limit
// on the decompressed size (gzip-bomb guard).
func gunzipInto(dst, src []byte, limit int) ([]byte, error) {
	br := bytes.NewReader(src)
	zr, _ := gzipPool.Get().(*gzip.Reader)
	var err error
	if zr == nil {
		zr, err = gzip.NewReader(br)
	} else {
		err = zr.Reset(br)
	}
	if err != nil {
		if zr != nil {
			gzipPool.Put(zr)
		}
		return dst, errBadBody
	}
	defer gzipPool.Put(zr)
	dst, err = readInto(dst, zr, limit, 0)
	if err != nil && !errors.Is(err, errTooLarge) {
		return dst, errBadBody
	}
	return dst, err
}

func hasGzipMagic(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// readBody reads the (optionally gzip-encoded) request body into a pooled
// buffer, leaving reserve bytes free at the front. Both the encoded and the
// decoded size are limited to limit. The caller must putBuf the result.
func readBody(r *http.Request, reserve, limit int) (*[]byte, error) {
	if r.ContentLength > int64(limit) {
		return nil, errTooLarge
	}
	gz := false
	switch ce := r.Header.Get("Content-Encoding"); {
	case ce == "" || strings.EqualFold(ce, "identity"):
	case strings.EqualFold(ce, "gzip"):
		gz = true
	default:
		return nil, errBadBody
	}
	bp := getBuf()
	buf := (*bp)[:0]
	if cap(buf) < reserve {
		buf = make([]byte, 0, 32<<10)
	}
	buf = buf[:reserve]
	var err error
	if !gz {
		buf, err = readInto(buf, r.Body, limit, r.ContentLength)
	} else {
		cp := getBuf()
		var comp []byte
		comp, err = readInto((*cp)[:0], r.Body, limit, r.ContentLength)
		if err == nil {
			buf, err = gunzipInto(buf, comp, limit)
		}
		*cp = comp
		putBuf(cp)
	}
	*bp = buf
	if err != nil {
		putBuf(bp)
		if errors.Is(err, errTooLarge) || errors.Is(err, errBadBody) {
			return nil, err
		}
		return nil, errClientRead
	}
	return bp, nil
}

// decodeBase64 accepts the base64 flavours PostHog clients produce:
// standard or URL alphabet, with or without padding, and with '+' turned
// into ' ' by form decoding.
func decodeBase64(src []byte) ([]byte, error) {
	clean := make([]byte, 0, len(src))
	for _, c := range src {
		switch c {
		case ' ':
			clean = append(clean, '+')
		case '\r', '\n', '\t':
		case '-':
			clean = append(clean, '+')
		case '_':
			clean = append(clean, '/')
		case '=':
		default:
			clean = append(clean, c)
		}
	}
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, err := base64.RawStdEncoding.Decode(out, clean)
	if err != nil {
		return nil, errBadBody
	}
	return out[:n], nil
}
