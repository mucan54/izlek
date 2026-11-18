package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// multipartPartSize is the buffer / part size used by PutStream when the
// stream length is unknown (S3 minimum part size is 5 MiB).
const multipartPartSize = 8 << 20

// S3Error is a non-success response from the S3 endpoint.
type S3Error struct {
	Op         string
	Key        string
	StatusCode int
	Code       string // S3 error code from the XML body, when present
	Message    string
}

func (e *S3Error) Error() string {
	return fmt.Sprintf("objstore: s3 %s %s: http %d %s %s", e.Op, e.Key, e.StatusCode, e.Code, e.Message)
}

// S3Store is the s3:// backend: path-style addressing, SigV4, no SDK.
type S3Store struct {
	base   *url.URL
	bucket string
	prefix string // without leading/trailing slash; "" for none
	cfg    S3Config
	client *http.Client
	now    func() time.Time
}

// NewS3Store returns a store for bucket (keys optionally nested under prefix).
func NewS3Store(bucket, prefix string, cfg S3Config) (*S3Store, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	base, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("objstore: bad s3 endpoint %q", cfg.Endpoint)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
		}}
	}
	return &S3Store{
		base:   base,
		bucket: bucket,
		prefix: strings.Trim(prefix, "/"),
		cfg:    cfg,
		client: client,
		now:    time.Now,
	}, nil
}

func (s *S3Store) fullKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// newRequest builds an (unsigned) request for the bucket ("" key) or object.
func (s *S3Store) newRequest(ctx context.Context, method, fullKey string, query url.Values, body io.Reader) (*http.Request, error) {
	u := *s.base
	p := u.Path + "/" + s.bucket
	if fullKey != "" {
		p += "/" + fullKey
	}
	u.Path = p
	u.RawPath = awsEscape(p, true)
	u.RawQuery = query.Encode()
	return http.NewRequestWithContext(ctx, method, u.String(), body)
}

// do signs and sends the request and maps error statuses. On success the
// caller owns resp.Body.
func (s *S3Store) do(req *http.Request, payloadHash, op, key string, okStatus ...int) (*http.Response, error) {
	signV4(req, payloadHash, s.cfg.AccessKey, s.cfg.SecretKey, s.cfg.Region, s.now())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("objstore: s3 %s %s: %w", op, key, err)
	}
	for _, ok := range okStatus {
		if resp.StatusCode == ok {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	se := &S3Error{Op: op, Key: key, StatusCode: resp.StatusCode}
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(raw, &x) == nil {
		se.Code, se.Message = x.Code, x.Message
	}
	return nil, se
}

func statusOf(err error) int {
	var se *S3Error
	if errors.As(err, &se) {
		return se.StatusCode
	}
	return 0
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *S3Store) Get(ctx context.Context, key string, opts *GetOptions) ([]byte, Object, error) {
	if err := checkKey(key); err != nil {
		return nil, Object{}, err
	}
	req, err := s.newRequest(ctx, http.MethodGet, s.fullKey(key), nil, nil)
	if err != nil {
		return nil, Object{}, err
	}
	if opts != nil {
		if opts.IfNoneMatch != "" {
			req.Header.Set("If-None-Match", opts.IfNoneMatch)
		}
		if r := opts.Range; r != nil {
			if err := checkRange(r); err != nil {
				return nil, Object{}, err
			}
			if r.Length > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.Offset, r.Offset+r.Length-1))
			} else {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.Offset))
			}
		}
	}
	resp, err := s.do(req, emptySHA256, "get", key, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		switch statusOf(err) {
		case http.StatusNotFound:
			return nil, Object{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
		case http.StatusNotModified:
			return nil, Object{Key: key, ETag: req.Header.Get("If-None-Match")}, ErrNotModified
		case http.StatusRequestedRangeNotSatisfiable:
			// Range starts at or past the end: same as the file backend.
			return []byte{}, Object{Key: key}, nil
		}
		return nil, Object{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, Object{}, fmt.Errorf("objstore: s3 get %s: %w", key, err)
	}
	obj := Object{Key: key, Size: int64(len(data)), ETag: resp.Header.Get("ETag")}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if total, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				obj.Size = total
			}
		}
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		obj.LastModified = t
	}
	return data, obj, nil
}

func (s *S3Store) Put(ctx context.Context, key string, data []byte) (Object, error) {
	return s.putBytes(ctx, key, data, false)
}

func (s *S3Store) PutIfAbsent(ctx context.Context, key string, data []byte) (Object, error) {
	// AWS answers 409 ConditionalRequestConflict while another conditional
	// write to the same key is in flight; the documented reaction is to
	// retry, after which the outcome is a clean 200 or 412.
	backoff := 50 * time.Millisecond
	for attempt := 0; ; attempt++ {
		obj, err := s.putBytes(ctx, key, data, true)
		if err == nil {
			return obj, nil
		}
		switch statusOf(err) {
		case http.StatusPreconditionFailed:
			return Object{}, fmt.Errorf("put %s: %w", key, ErrAlreadyExists)
		case http.StatusConflict:
			if attempt < 5 {
				select {
				case <-ctx.Done():
					return Object{}, ctx.Err()
				case <-time.After(backoff):
				}
				backoff *= 2
				continue
			}
		}
		return Object{}, err
	}
}

func (s *S3Store) putBytes(ctx context.Context, key string, data []byte, ifAbsent bool) (Object, error) {
	if err := checkKey(key); err != nil {
		return Object{}, err
	}
	req, err := s.newRequest(ctx, http.MethodPut, s.fullKey(key), nil, bytes.NewReader(data))
	if err != nil {
		return Object{}, err
	}
	if ifAbsent {
		req.Header.Set("If-None-Match", "*")
	}
	resp, err := s.do(req, sha256Hex(data), "put", key, http.StatusOK, http.StatusCreated)
	if err != nil {
		return Object{}, err
	}
	drain(resp)
	return Object{Key: key, Size: int64(len(data)), ETag: resp.Header.Get("ETag")}, nil
}

func (s *S3Store) PutStream(ctx context.Context, key string, r io.Reader, size int64) (Object, error) {
	if err := checkKey(key); err != nil {
		return Object{}, err
	}
	if size >= 0 {
		// Known length: one PUT, body streamed straight through. The payload
		// cannot be hashed up front, so it is sent unsigned (headers are
		// still signed; integrity in transit is TLS's job).
		req, err := s.newRequest(ctx, http.MethodPut, s.fullKey(key), nil, io.NopCloser(r))
		if err != nil {
			return Object{}, err
		}
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
		resp, err := s.do(req, unsignedPayload, "put", key, http.StatusOK, http.StatusCreated)
		if err != nil {
			return Object{}, err
		}
		drain(resp)
		return Object{Key: key, Size: size, ETag: resp.Header.Get("ETag")}, nil
	}

	// Unknown length: buffer one part; a stream that fits is a plain signed
	// PUT, a longer one becomes a multipart upload.
	buf := make([]byte, multipartPartSize)
	n, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return s.putBytes(ctx, key, buf[:n], false)
	}
	if err != nil {
		return Object{}, fmt.Errorf("objstore: s3 put %s: %w", key, err)
	}
	return s.putMultipart(ctx, key, buf, r)
}

func (s *S3Store) putMultipart(ctx context.Context, key string, first []byte, rest io.Reader) (obj Object, err error) {
	fk := s.fullKey(key)
	req, err := s.newRequest(ctx, http.MethodPost, fk, url.Values{"uploads": {""}}, nil)
	if err != nil {
		return Object{}, err
	}
	resp, err := s.do(req, emptySHA256, "create-multipart", key, http.StatusOK)
	if err != nil {
		return Object{}, err
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadID == "" {
		return Object{}, fmt.Errorf("objstore: s3 create-multipart %s: bad response", key)
	}
	defer func() {
		if err != nil {
			// Best effort; use a fresh context so a cancelled upload is still aborted.
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if areq, aerr := s.newRequest(actx, http.MethodDelete, fk, url.Values{"uploadId": {init.UploadID}}, nil); aerr == nil {
				if aresp, aerr := s.do(areq, emptySHA256, "abort-multipart", key, http.StatusNoContent, http.StatusOK); aerr == nil {
					drain(aresp)
				}
			}
		}
	}()

	type part struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	var parts []part
	var total int64
	buf := first
	for {
		num := len(parts) + 1
		q := url.Values{"partNumber": {strconv.Itoa(num)}, "uploadId": {init.UploadID}}
		preq, err := s.newRequest(ctx, http.MethodPut, fk, q, bytes.NewReader(buf))
		if err != nil {
			return Object{}, err
		}
		presp, err := s.do(preq, sha256Hex(buf), "upload-part", key, http.StatusOK)
		if err != nil {
			return Object{}, err
		}
		drain(presp)
		parts = append(parts, part{PartNumber: num, ETag: presp.Header.Get("ETag")})
		total += int64(len(buf))

		buf = buf[:cap(buf)]
		n, rerr := io.ReadFull(rest, buf)
		buf = buf[:n]
		if rerr == io.EOF {
			break
		}
		if rerr != nil && rerr != io.ErrUnexpectedEOF {
			return Object{}, fmt.Errorf("objstore: s3 put %s: %w", key, rerr)
		}
		if rerr == io.ErrUnexpectedEOF {
			rest = eofReader{} // this short part is the last one
		}
	}

	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{Parts: parts})
	creq, err := s.newRequest(ctx, http.MethodPost, fk, url.Values{"uploadId": {init.UploadID}}, bytes.NewReader(body))
	if err != nil {
		return Object{}, err
	}
	cresp, err := s.do(creq, sha256Hex(body), "complete-multipart", key, http.StatusOK)
	if err != nil {
		return Object{}, err
	}
	raw, _ := io.ReadAll(io.LimitReader(cresp.Body, 64<<10))
	cresp.Body.Close()
	// S3 can answer 200 and still carry an <Error> document.
	var result struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
		ETag    string `xml:"ETag"`
	}
	if xerr := xml.Unmarshal(raw, &result); xerr != nil || result.XMLName.Local == "Error" {
		return Object{}, &S3Error{Op: "complete-multipart", Key: key, StatusCode: cresp.StatusCode, Code: result.Code, Message: result.Message}
	}
	return Object{Key: key, Size: total, ETag: result.ETag}, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	req, err := s.newRequest(ctx, http.MethodDelete, s.fullKey(key), nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req, emptySHA256, "delete", key, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]Object, error) {
	fullPrefix := prefix
	if s.prefix != "" {
		fullPrefix = s.prefix + "/" + prefix
	}
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {fullPrefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := s.newRequest(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.do(req, emptySHA256, "list", prefix, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var page struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				ETag         string `xml:"ETag"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("objstore: s3 list %q: %w", prefix, err)
		}
		for _, c := range page.Contents {
			key := c.Key
			if s.prefix != "" {
				key = strings.TrimPrefix(key, s.prefix+"/")
			}
			o := Object{Key: key, Size: c.Size, ETag: c.ETag}
			if t, err := time.Parse(time.RFC3339Nano, c.LastModified); err == nil {
				o.LastModified = t
			}
			out = append(out, o)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	return out, nil
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
