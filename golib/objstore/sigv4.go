package objstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	unsignedPayload = "UNSIGNED-PAYLOAD"
	emptySHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// signV4 signs req in place with AWS Signature Version 4 (service "s3").
// Every header present on the request at call time is signed, so the caller
// must set all headers first. payloadHash is the hex SHA-256 of the body or
// "UNSIGNED-PAYLOAD".
func signV4(req *http.Request, payloadHash, accessKey, secretKey, region string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	names := make([]string, 0, len(req.Header)+1)
	values := map[string]string{"host": host}
	names = append(names, "host")
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" || lk == "host" {
			continue
		}
		trimmed := make([]string, len(vs))
		for i, v := range vs {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		values[lk] = strings.Join(trimmed, ",")
		names = append(names, lk)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(values[n])
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonURI := req.URL.EscapedPath()
	if canonURI == "" {
		canonURI = "/"
	}
	canonRequest := strings.Join([]string{
		req.Method,
		canonURI,
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := date + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	k := hmacSHA256([]byte("AWS4"+secretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(q))
	for k, vs := range q {
		ek := awsEscape(k, false)
		for _, v := range vs {
			pairs = append(pairs, ek+"="+awsEscape(v, false))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// awsEscape percent-encodes s the way SigV4 requires: everything except the
// RFC 3986 unreserved characters, upper-case hex. With keepSlash, "/" is left
// alone (object key paths).
func awsEscape(s string, keepSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
		}
	}
	return b.String()
}
