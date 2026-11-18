// Package devs3 is a small S3-compatible server for development and tests.
//
// It implements the subset the platform uses, path-style only:
// PutObject (incl. If-None-Match: * -> 412), GetObject (Range, ETag,
// If-None-Match -> 304), HeadObject, DeleteObject, DeleteObjects,
// CopyObject, ListObjectsV2 (prefix, delimiter, start-after, continuation)
// and multipart uploads. Buckets are created implicitly. Request signatures
// are NOT verified. Objects live in memory and are optionally written
// through to a directory.
//
// A fault switch (SetFault or POST /_devs3/fault?mode=none|fail|pause) makes
// every S3 request fail with 503 or hang, for outage tests.
package devs3

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Fault is the fault-injection mode.
type Fault int32

const (
	FaultNone  Fault = iota // normal operation
	FaultFail               // every request answers 503 ServiceUnavailable
	FaultPause              // every request blocks until the mode changes
)

const maxObjectSize = 5 << 30

type object struct {
	data []byte
	etag string // quoted
	mod  time.Time
	ctyp string
}

type upload struct {
	bucket, key string
	parts       map[int]*object
}

// Server is an http.Handler speaking the S3 REST protocol.
type Server struct {
	mu      sync.RWMutex
	buckets map[string]map[string]*object
	uploads map[string]*upload
	dir     string

	fault    atomic.Int32
	faultMu  sync.Mutex
	faultCh  chan struct{} // closed whenever the fault mode changes
	requests atomic.Int64
}

// New returns an in-memory server.
func New() *Server {
	return &Server{
		buckets: map[string]map[string]*object{},
		uploads: map[string]*upload{},
		faultCh: make(chan struct{}),
	}
}

// NewDir returns a server that loads its objects from dir
// (<dir>/<bucket>/<key>) and writes every change through to it.
func NewDir(dir string) (*Server, error) {
	s := New()
	s.dir = dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		bucket, key, ok := strings.Cut(filepath.ToSlash(rel), "/")
		if !ok || strings.HasSuffix(key, ".devs3tmp") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		o := newObject(data, "")
		o.mod = fi.ModTime().UTC()
		s.bucket(bucket)[key] = o
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SetFault switches the fault-injection mode.
func (s *Server) SetFault(f Fault) {
	s.faultMu.Lock()
	s.fault.Store(int32(f))
	close(s.faultCh)
	s.faultCh = make(chan struct{})
	s.faultMu.Unlock()
}

// Requests returns the number of S3 requests received so far (including
// failed and paused ones).
func (s *Server) Requests() int64 { return s.requests.Load() }

// Keys returns the sorted keys of a bucket (test helper).
func (s *Server) Keys(bucket string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.buckets[bucket]))
	for k := range s.buckets[bucket] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func newObject(data []byte, ctype string) *object {
	sum := md5.Sum(data)
	return &object{data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`, mod: time.Now().UTC(), ctyp: ctype}
}

// bucket returns (creating) the bucket map. Caller holds s.mu for writing.
func (s *Server) bucket(name string) map[string]*object {
	b := s.buckets[name]
	if b == nil {
		b = map[string]*object{}
		s.buckets[name] = b
	}
	return b
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
	Key     string   `xml:"Key,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg, key string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if status != http.StatusNotModified {
		xml.NewEncoder(w).Encode(s3Error{Code: code, Message: msg, Key: key})
	}
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, xml.Header)
	xml.NewEncoder(w).Encode(v)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_devs3/fault" {
		s.handleFaultControl(w, r)
		return
	}
	s.requests.Add(1)
	for {
		s.faultMu.Lock()
		mode, ch := Fault(s.fault.Load()), s.faultCh
		s.faultMu.Unlock()
		if mode == FaultNone {
			break
		}
		if mode == FaultFail {
			io.Copy(io.Discard, r.Body)
			writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "devs3 fault injection", "")
			return
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
	}

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket == "" {
		if r.Method == http.MethodGet {
			writeXML(w, struct {
				XMLName xml.Name `xml:"ListAllMyBucketsResult"`
			}{})
			return
		}
		writeError(w, http.StatusBadRequest, "InvalidRequest", "missing bucket", "")
		return
	}
	q := r.URL.Query()
	if key == "" {
		s.handleBucket(w, r, bucket, q)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.getObject(w, r, bucket, key)
	case http.MethodPut:
		switch {
		case q.Has("partNumber") && q.Has("uploadId"):
			s.uploadPart(w, r, q)
		case r.Header.Get("X-Amz-Copy-Source") != "":
			s.copyObject(w, r, bucket, key)
		default:
			s.putObject(w, r, bucket, key)
		}
	case http.MethodDelete:
		if q.Has("uploadId") {
			s.mu.Lock()
			delete(s.uploads, q.Get("uploadId"))
			s.mu.Unlock()
		} else if err := s.remove(bucket, key); err != nil {
			writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			s.createUpload(w, bucket, key)
		case q.Has("uploadId"):
			s.completeUpload(w, r, bucket, key, q.Get("uploadId"))
		default:
			writeError(w, http.StatusNotImplemented, "NotImplemented", "unsupported POST", key)
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method, key)
	}
}

func (s *Server) handleFaultControl(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		switch r.URL.Query().Get("mode") {
		case "none":
			s.SetFault(FaultNone)
		case "fail":
			s.SetFault(FaultFail)
		case "pause":
			s.SetFault(FaultPause)
		default:
			http.Error(w, "mode must be none|fail|pause", http.StatusBadRequest)
			return
		}
	}
	fmt.Fprintln(w, [...]string{"none", "fail", "pause"}[s.fault.Load()])
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	switch r.Method {
	case http.MethodPut:
		s.mu.Lock()
		s.bucket(bucket)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodHead, http.MethodDelete:
		w.WriteHeader(map[string]int{http.MethodHead: http.StatusOK, http.MethodDelete: http.StatusNoContent}[r.Method])
	case http.MethodGet:
		if q.Has("location") {
			writeXML(w, struct {
				XMLName xml.Name `xml:"LocationConstraint"`
			}{})
			return
		}
		s.listObjects(w, bucket, q)
	case http.MethodPost:
		if q.Has("delete") {
			s.deleteObjects(w, r, bucket)
			return
		}
		writeError(w, http.StatusNotImplemented, "NotImplemented", "unsupported bucket POST", "")
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method, "")
	}
}

// readBody reads the request payload, undoing aws-chunked framing
// (STREAMING-* payloads sent by the AWS SDKs) when present.
func readBody(r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(nil, r.Body, maxObjectSize)
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		br := bufio.NewReader(src)
		var out bytes.Buffer
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return nil, fmt.Errorf("aws-chunked: %w", err)
			}
			sizeHex, _, _ := strings.Cut(strings.TrimSpace(line), ";")
			n, err := strconv.ParseInt(sizeHex, 16, 64)
			if err != nil || n < 0 {
				return nil, errors.New("aws-chunked: bad chunk size")
			}
			if n == 0 {
				io.Copy(io.Discard, br) // trailers
				return out.Bytes(), nil
			}
			if _, err := io.CopyN(&out, br, n); err != nil {
				return nil, fmt.Errorf("aws-chunked: %w", err)
			}
			if _, err := br.Discard(2); err != nil {
				return nil, fmt.Errorf("aws-chunked: %w", err)
			}
		}
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if r.ContentLength >= 0 && int64(len(data)) != r.ContentLength {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

// store publishes obj under bucket/key. With ifAbsent it fails (false) when
// the key exists; the check and the insert are one critical section, which is
// what makes concurrent conditional PUTs have exactly one winner.
func (s *Server) store(bucket, key string, obj *object, ifAbsent bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bucket(bucket)
	if _, exists := b[key]; exists && ifAbsent {
		return false, nil
	}
	if s.dir != "" {
		p := filepath.Join(s.dir, bucket, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return false, err
		}
		tmp := p + ".devs3tmp"
		if err := os.WriteFile(tmp, obj.data, 0o644); err != nil {
			return false, err
		}
		if err := os.Rename(tmp, p); err != nil {
			return false, err
		}
	}
	b[key] = obj
	return true, nil
}

func (s *Server) remove(bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		if err := os.Remove(filepath.Join(s.dir, bucket, filepath.FromSlash(key))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	delete(s.buckets[bucket], key)
	return nil
}

func (s *Server) lookup(bucket, key string) *object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buckets[bucket][key]
}

func ifNoneMatchStar(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("If-None-Match")) == "*"
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	data, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error(), key)
		return
	}
	obj := newObject(data, r.Header.Get("Content-Type"))
	ok, err := s.store(bucket, key, obj, ifNoneMatchStar(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	if !ok {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed",
			"At least one of the pre-conditions you specified did not hold", key)
		return
	}
	w.Header().Set("ETag", obj.etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	src, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "bad copy source", key)
		return
	}
	sb, sk, _ := strings.Cut(strings.TrimPrefix(src, "/"), "/")
	from := s.lookup(sb, sk)
	if from == nil {
		writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", sk)
		return
	}
	obj := &object{data: from.data, etag: from.etag, mod: time.Now().UTC(), ctyp: from.ctyp}
	ok, err := s.store(bucket, key, obj, ifNoneMatchStar(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	if !ok {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", "destination exists", key)
		return
	}
	writeXML(w, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
	}{ETag: obj.etag, LastModified: obj.mod.Format(timeFormat)})
}

const timeFormat = "2006-01-02T15:04:05.000Z"

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	obj := s.lookup(bucket, key)
	if obj == nil {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", key)
		return
	}
	ctype := obj.ctyp
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("ETag", obj.etag)
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent implements Range, If-None-Match (304), If-Match and HEAD.
	http.ServeContent(w, r, "", obj.mod, bytes.NewReader(obj.data))
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Quiet   bool `xml:"Quiet"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error(), "")
		return
	}
	type deleted struct {
		Key string `xml:"Key"`
	}
	var res struct {
		XMLName xml.Name  `xml:"DeleteResult"`
		Deleted []deleted `xml:"Deleted"`
	}
	for _, o := range req.Objects {
		if err := s.remove(bucket, o.Key); err != nil {
			writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), o.Key)
			return
		}
		if !req.Quiet {
			res.Deleted = append(res.Deleted, deleted{Key: o.Key})
		}
	}
	writeXML(w, res)
}

func (s *Server) listObjects(w http.ResponseWriter, bucket string, q url.Values) {
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	maxKeys := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v >= 0 && v < maxKeys {
		maxKeys = v
	}
	after := q.Get("start-after")
	if tok := q.Get("continuation-token"); tok != "" {
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidArgument", "bad continuation token", "")
			return
		}
		after = string(raw)
	}
	enc := func(v string) string { return v }
	if q.Get("encoding-type") == "url" {
		enc = func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }
	}

	type content struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int    `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	type commonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	res := struct {
		XMLName               xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name                  string         `xml:"Name"`
		Prefix                string         `xml:"Prefix"`
		Delimiter             string         `xml:"Delimiter,omitempty"`
		MaxKeys               int            `xml:"MaxKeys"`
		EncodingType          string         `xml:"EncodingType,omitempty"`
		KeyCount              int            `xml:"KeyCount"`
		IsTruncated           bool           `xml:"IsTruncated"`
		NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
		Contents              []content      `xml:"Contents"`
		CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
	}{Name: bucket, Prefix: enc(prefix), Delimiter: enc(delim), MaxKeys: maxKeys, EncodingType: q.Get("encoding-type")}

	s.mu.RLock()
	b := s.buckets[bucket]
	keys := make([]string, 0, len(b))
	for k := range b {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	last := ""
	for _, k := range keys {
		// A continuation resumes after the last returned item; for a common
		// prefix that item is the prefix itself, so skip everything under it.
		if after != "" && (k <= after || (delim != "" && strings.HasSuffix(after, delim) && strings.HasPrefix(k, after))) {
			continue
		}
		item := k
		isPrefix := false
		if delim != "" {
			if i := strings.Index(k[len(prefix):], delim); i >= 0 {
				item = k[:len(prefix)+i+len(delim)]
				isPrefix = true
			}
		}
		if isPrefix && len(res.CommonPrefixes) > 0 && res.CommonPrefixes[len(res.CommonPrefixes)-1].Prefix == enc(item) {
			continue
		}
		if res.KeyCount >= maxKeys {
			res.IsTruncated = true
			res.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(last))
			break
		}
		if isPrefix {
			res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{Prefix: enc(item)})
		} else {
			o := b[k]
			res.Contents = append(res.Contents, content{Key: enc(k), LastModified: o.mod.Format(timeFormat),
				ETag: o.etag, Size: len(o.data), StorageClass: "STANDARD"})
		}
		res.KeyCount++
		last = item
	}
	s.mu.RUnlock()
	writeXML(w, res)
}

func (s *Server) createUpload(w http.ResponseWriter, bucket, key string) {
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	s.mu.Lock()
	s.uploads[id] = &upload{bucket: bucket, key: key, parts: map[int]*object{}}
	s.mu.Unlock()
	writeXML(w, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}{Bucket: bucket, Key: key, UploadID: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, q url.Values) {
	num, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || num < 1 || num > 10000 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "bad partNumber", "")
		return
	}
	data, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error(), "")
		return
	}
	part := newObject(data, "")
	s.mu.Lock()
	up := s.uploads[q.Get("uploadId")]
	if up != nil {
		up.parts[num] = part
	}
	s.mu.Unlock()
	if up == nil {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.", "")
		return
	}
	w.Header().Set("ETag", part.etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, bucket, key, id string) {
	var req struct {
		Parts []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error(), key)
		return
	}
	s.mu.Lock()
	up := s.uploads[id]
	delete(s.uploads, id)
	s.mu.Unlock()
	if up == nil || up.bucket != bucket || up.key != key {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.", key)
		return
	}
	var data []byte
	sums := md5.New()
	for _, p := range req.Parts {
		part := up.parts[p.PartNumber]
		if part == nil {
			writeError(w, http.StatusBadRequest, "InvalidPart", "part not found", key)
			return
		}
		data = append(data, part.data...)
		raw, _ := hex.DecodeString(strings.Trim(part.etag, `"`))
		sums.Write(raw)
	}
	obj := newObject(data, "")
	obj.etag = fmt.Sprintf(`"%x-%d"`, sums.Sum(nil), len(req.Parts))
	ok, err := s.store(bucket, key, obj, ifNoneMatchStar(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	if !ok {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed",
			"At least one of the pre-conditions you specified did not hold", key)
		return
	}
	writeXML(w, struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string   `xml:"Bucket"`
		Key     string   `xml:"Key"`
		ETag    string   `xml:"ETag"`
	}{Bucket: bucket, Key: key, ETag: obj.etag})
}
