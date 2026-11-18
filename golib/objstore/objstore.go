// Package objstore is the object-store client shared by the Go components
// (ingestion, control plane, canary). It implements exactly the operations
// spec/manifest.md §0 requires: GET (optionally ranged / conditional), PUT,
// create-if-absent PUT, DELETE and LIST by prefix, over two backends:
//
//	s3://<bucket>[/<prefix>]   any S3-compatible endpoint, path-style, SigV4
//	file:///absolute/dir       object key = relative path under the directory
//
// The file backend shares its directory layout with Rust's
// object_store::local::LocalFileSystem, so both can work on the same tree.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned by Get when the key does not exist.
	ErrNotFound = errors.New("objstore: object not found")
	// ErrAlreadyExists is returned by PutIfAbsent when the key already exists.
	ErrAlreadyExists = errors.New("objstore: object already exists")
	// ErrNotModified is returned by Get when GetOptions.IfNoneMatch matches
	// the current ETag of the object.
	ErrNotModified = errors.New("objstore: object not modified")
)

// Object describes a stored object. Key is always relative to the store root
// (the s3:// prefix or the file:// directory).
type Object struct {
	Key          string
	Size         int64
	ETag         string // opaque; only comparable with ETags from the same backend
	LastModified time.Time
}

// Range selects a byte range. Length <= 0 means "to the end of the object".
type Range struct {
	Offset int64
	Length int64
}

// GetOptions are the optional arguments of Get. A nil *GetOptions is valid.
type GetOptions struct {
	// IfNoneMatch is an ETag from an earlier Get/Put/List. When it matches
	// the current object, Get returns ErrNotModified and no data.
	IfNoneMatch string
	// Range restricts the returned bytes.
	Range *Range
}

// Store is the object-store abstraction. All methods are safe for concurrent
// use. Implementations do not retry; callers own the retry policy.
type Store interface {
	// Get returns the object's bytes. Errors: ErrNotFound, ErrNotModified.
	Get(ctx context.Context, key string, opts *GetOptions) ([]byte, Object, error)
	// Put unconditionally creates or replaces the object.
	Put(ctx context.Context, key string, data []byte) (Object, error)
	// PutIfAbsent creates the object only if the key does not exist
	// (If-None-Match: *). Exactly one of several concurrent callers wins;
	// the others get ErrAlreadyExists.
	PutIfAbsent(ctx context.Context, key string, data []byte) (Object, error)
	// PutStream unconditionally writes the object from r. size is the exact
	// number of bytes r will yield, or -1 when unknown. With a known size the
	// body is streamed without buffering; a short or long stream is an error
	// and leaves no object (or the previous one) behind.
	PutStream(ctx context.Context, key string, r io.Reader, size int64) (Object, error)
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List returns every object whose key starts with prefix (plain string
	// prefix, S3 semantics), sorted by key.
	List(ctx context.Context, prefix string) ([]Object, error)
}

// S3Config holds the settings of the s3:// backend.
type S3Config struct {
	Endpoint  string // e.g. http://127.0.0.1:9000; empty = https://s3.<region>.amazonaws.com
	Region    string // default us-east-1
	AccessKey string
	SecretKey string
	// HTTPClient overrides the default client (60 s header timeout, no
	// overall deadline; use the context for that).
	HTTPClient *http.Client
}

// Open opens the store named by rawURL (s3://bucket[/prefix] or
// file:///abs/dir). cfg is only used by the s3 backend.
func Open(rawURL string, cfg S3Config) (Store, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("objstore: bad store url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("objstore: file url must be file:///absolute/dir, got %q", rawURL)
		}
		return NewFileStore(u.Path)
	case "s3":
		if u.Host == "" {
			return nil, fmt.Errorf("objstore: s3 url needs a bucket: %q", rawURL)
		}
		return NewS3Store(u.Host, strings.Trim(u.Path, "/"), cfg)
	default:
		return nil, fmt.Errorf("objstore: unsupported store url %q (want s3:// or file://)", rawURL)
	}
}

// FromEnv opens the store configured by the LA_STORE_URL / LA_S3_* process
// environment (spec/manifest.md §0).
func FromEnv() (Store, error) { return FromEnvFunc(os.Getenv) }

// FromEnvFunc is FromEnv with an injectable environment lookup.
func FromEnvFunc(getenv func(string) string) (Store, error) {
	storeURL := getenv("LA_STORE_URL")
	if storeURL == "" {
		bucket := getenv("LA_S3_BUCKET")
		if bucket == "" {
			return nil, errors.New("objstore: set LA_STORE_URL (s3://bucket[/prefix] or file:///abs/dir) or LA_S3_BUCKET")
		}
		storeURL = "s3://" + bucket
	}
	return Open(storeURL, S3Config{
		Endpoint:  getenv("LA_S3_ENDPOINT"),
		Region:    getenv("LA_S3_REGION"),
		AccessKey: getenv("LA_S3_ACCESS_KEY"),
		SecretKey: getenv("LA_S3_SECRET_KEY"),
	})
}

// checkKey rejects keys that cannot be represented identically on both
// backends (and that could escape the file root).
func checkKey(key string) error {
	if key == "" || len(key) > 1024 {
		return fmt.Errorf("objstore: invalid key %q", key)
	}
	if key[0] == '/' || key[len(key)-1] == '/' {
		return fmt.Errorf("objstore: invalid key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("objstore: invalid key %q", key)
		}
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return fmt.Errorf("objstore: invalid key %q", key)
		}
	}
	return nil
}

func checkRange(r *Range) error {
	if r != nil && r.Offset < 0 {
		return errors.New("objstore: negative range offset")
	}
	return nil
}
