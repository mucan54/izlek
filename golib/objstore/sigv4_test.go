package objstore

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Known-answer tests from the AWS documentation ("Signature Calculations for
// the Authorization Header: Transferring Payload in a Single Chunk",
// Amazon S3 API Reference). devs3 does not verify signatures, so these are
// what pins the signer to the real algorithm.
func TestSigV4AWSExamples(t *testing.T) {
	const (
		accessKey = "AKIAIOSFODNN7EXAMPLE"
		secretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		host      = "examplebucket.s3.amazonaws.com"
	)
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name, method, url, payloadHash string
		headers                        map[string]string
		wantSigned, wantSig            string
	}{
		{
			name: "get object with range", method: "GET", url: "https://" + host + "/test.txt",
			payloadHash: emptySHA256,
			headers:     map[string]string{"Range": "bytes=0-9"},
			wantSigned:  "host;range;x-amz-content-sha256;x-amz-date",
			wantSig:     "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "put object", method: "PUT", url: "https://" + host + "/test%24file.text",
			payloadHash: sha256Hex([]byte("Welcome to Amazon S3.")),
			headers:     map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			wantSigned:  "date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			wantSig:     "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name: "get bucket lifecycle", method: "GET", url: "https://" + host + "/?lifecycle",
			payloadHash: emptySHA256,
			wantSigned:  "host;x-amz-content-sha256;x-amz-date",
			wantSig:     "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "list objects", method: "GET", url: "https://" + host + "/?max-keys=2&prefix=J",
			payloadHash: emptySHA256,
			wantSigned:  "host;x-amz-content-sha256;x-amz-date",
			wantSig:     "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, c.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		signV4(req, c.payloadHash, accessKey, secretKey, "us-east-1", now)
		auth := req.Header.Get("Authorization")
		want := "AWS4-HMAC-SHA256 Credential=" + accessKey + "/20130524/us-east-1/s3/aws4_request, SignedHeaders=" +
			c.wantSigned + ", Signature=" + c.wantSig
		if auth != want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, auth, want)
		}
	}
}

func TestAWSEscape(t *testing.T) {
	if got := awsEscape("data/p=1017/d=2026-11-18/a b+c~_.-é", true); got != "data/p%3D1017/d%3D2026-11-18/a%20b%2Bc~_.-%C3%A9" {
		t.Fatalf("path escape: %s", got)
	}
	if got := awsEscape("a/b", false); got != "a%2Fb" {
		t.Fatalf("query escape: %s", got)
	}
}

// The request path on the wire must be the same strict encoding that is
// signed (a mismatch is a SignatureDoesNotMatch on real S3).
func TestS3RequestEncoding(t *testing.T) {
	s, err := NewS3Store("bkt", "pre", S3Config{Endpoint: "http://127.0.0.1:9", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.newRequest(t.Context(), "GET", s.fullKey("data/p=1/d=2026-11-18/x y.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.URL.EscapedPath(); got != "/bkt/pre/data/p%3D1/d%3D2026-11-18/x%20y.json" {
		t.Fatalf("escaped path %s", got)
	}
	if !strings.HasSuffix(req.URL.String(), "/bkt/pre/data/p%3D1/d%3D2026-11-18/x%20y.json") {
		t.Fatalf("url %s", req.URL)
	}
}
