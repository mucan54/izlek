package objstore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/objstore"
)

type backend struct {
	name string
	open func(t *testing.T) objstore.Store
}

// realS3 adds a backend for a real S3 implementation when LA_TEST_S3_ENDPOINT
// is set (e.g. a local MinIO, which verifies SigV4 signatures; devs3 does
// not):
//
//	LA_TEST_S3_ENDPOINT=http://127.0.0.1:19000 LA_TEST_S3_ACCESS_KEY=... LA_TEST_S3_SECRET_KEY=... go test ./golib/objstore/
func realS3() []backend {
	endpoint := os.Getenv("LA_TEST_S3_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	return []backend{{"s3-real", func(t *testing.T) objstore.Store {
		bucket := fmt.Sprintf("la-test-%d", time.Now().UnixNano())
		st, err := objstore.Open("s3://"+bucket+"/pre fix/x=1", objstore.S3Config{Endpoint: endpoint, Region: os.Getenv("LA_TEST_S3_REGION"),
			AccessKey: os.Getenv("LA_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("LA_TEST_S3_SECRET_KEY")})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.(*objstore.S3Store).CreateBucketForTest(context.Background()); err != nil {
			t.Fatal(err)
		}
		return st
	}}}
}

func backends() []backend {
	return append(realS3(), []backend{
		{"file", func(t *testing.T) objstore.Store {
			st, err := objstore.Open("file://"+t.TempDir(), objstore.S3Config{})
			if err != nil {
				t.Fatal(err)
			}
			return st
		}},
		{"s3", func(t *testing.T) objstore.Store {
			ts := httptest.NewServer(devs3.New())
			t.Cleanup(ts.Close)
			st, err := objstore.Open("s3://bucket/some/prefix", objstore.S3Config{Endpoint: ts.URL, AccessKey: "ak", SecretKey: "sk"})
			if err != nil {
				t.Fatal(err)
			}
			return st
		}},
		{"s3-noprefix", func(t *testing.T) objstore.Store {
			ts := httptest.NewServer(devs3.New())
			t.Cleanup(ts.Close)
			st, err := objstore.FromEnvFunc(func(k string) string {
				return map[string]string{"LA_S3_BUCKET": "bucket", "LA_S3_ENDPOINT": ts.URL,
					"LA_S3_ACCESS_KEY": "ak", "LA_S3_SECRET_KEY": "sk"}[k]
			})
			if err != nil {
				t.Fatal(err)
			}
			return st
		}},
	}...)
}

// TestContract runs the same behavioural contract against every backend.
func TestContract(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			st := b.open(t)
			ctx := context.Background()

			// Missing object.
			if _, _, err := st.Get(ctx, "control/tokens.json", nil); !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("get missing: %v", err)
			}
			if err := st.Delete(ctx, "control/tokens.json"); err != nil {
				t.Fatalf("delete missing: %v", err)
			}
			if objs, err := st.List(ctx, "nothing/here/"); err != nil || len(objs) != 0 {
				t.Fatalf("list empty: %v %v", objs, err)
			}

			// Put / Get / ETag round trip. Keys use the real layout characters.
			key := "data/p=1017/d=2026-11-18/_m/00000001.json"
			put, err := st.Put(ctx, key, []byte(`{"version":1}`))
			if err != nil {
				t.Fatal(err)
			}
			data, obj, err := st.Get(ctx, key, nil)
			if err != nil || string(data) != `{"version":1}` {
				t.Fatalf("get: %q %v", data, err)
			}
			if obj.Key != key || obj.Size != 13 || obj.ETag == "" || obj.ETag != put.ETag {
				t.Fatalf("object %+v (put %+v)", obj, put)
			}

			// Conditional GET.
			if _, _, err := st.Get(ctx, key, &objstore.GetOptions{IfNoneMatch: obj.ETag}); !errors.Is(err, objstore.ErrNotModified) {
				t.Fatalf("if-none-match same etag: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
			if _, err := st.Put(ctx, key, []byte(`{"version":2}`)); err != nil {
				t.Fatal(err)
			}
			data, obj2, err := st.Get(ctx, key, &objstore.GetOptions{IfNoneMatch: obj.ETag})
			if err != nil || string(data) != `{"version":2}` || obj2.ETag == obj.ETag {
				t.Fatalf("if-none-match after overwrite: %q %v %+v", data, err, obj2)
			}

			// Ranges.
			if _, err := st.Put(ctx, "r/range.bin", []byte("0123456789")); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				off, n int64
				want   string
			}{{0, 4, "0123"}, {3, 3, "345"}, {7, 0, "789"}, {8, 100, "89"}, {10, 5, ""}} {
				got, o, err := st.Get(ctx, "r/range.bin", &objstore.GetOptions{Range: &objstore.Range{Offset: c.off, Length: c.n}})
				if err != nil || string(got) != c.want {
					t.Fatalf("range %+v: %q %v", c, got, err)
				}
				if c.want != "" && o.Size != 10 {
					t.Fatalf("range %+v: object size %d, want full size 10", c, o.Size)
				}
			}

			// PutIfAbsent.
			if _, err := st.PutIfAbsent(ctx, "lease/000000000001.json", []byte("a")); err != nil {
				t.Fatal(err)
			}
			if _, err := st.PutIfAbsent(ctx, "lease/000000000001.json", []byte("b")); !errors.Is(err, objstore.ErrAlreadyExists) {
				t.Fatalf("second PutIfAbsent: %v", err)
			}
			if data, _, _ := st.Get(ctx, "lease/000000000001.json", nil); string(data) != "a" {
				t.Fatalf("PutIfAbsent overwrote: %q", data)
			}
			if _, err := st.PutIfAbsent(ctx, key, []byte("x")); !errors.Is(err, objstore.ErrAlreadyExists) {
				t.Fatalf("PutIfAbsent over Put: %v", err)
			}

			// Streaming put: known and unknown length, and a lying length.
			payload := bytes.Repeat([]byte("replay-chunk "), 5000)
			so, err := st.PutStream(ctx, "replay/p=1/d=2026-11-18/00112233445566778899aabbccddeeff/000001.bin.zst", bytes.NewReader(payload), int64(len(payload)))
			if err != nil || so.Size != int64(len(payload)) {
				t.Fatalf("PutStream known: %+v %v", so, err)
			}
			so, err = st.PutStream(ctx, "replay/unknown.bin", io.MultiReader(bytes.NewReader(payload)), -1)
			if err != nil || so.Size != int64(len(payload)) {
				t.Fatalf("PutStream unknown: %+v %v", so, err)
			}
			for _, k := range []string{"replay/p=1/d=2026-11-18/00112233445566778899aabbccddeeff/000001.bin.zst", "replay/unknown.bin"} {
				if got, _, err := st.Get(ctx, k, nil); err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("stream round trip %s: %v", k, err)
				}
			}
			if _, err := st.PutStream(ctx, "replay/short.bin", bytes.NewReader(payload[:10]), 20); err == nil {
				t.Fatal("PutStream accepted a short stream")
			}
			if _, _, err := st.Get(ctx, "replay/short.bin", nil); !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("short stream left an object: %v", err)
			}
			if _, err := st.PutStream(ctx, "replay/empty.bin", strings.NewReader(""), 0); err != nil {
				t.Fatalf("empty stream: %v", err)
			}

			// List: string-prefix semantics, sorted, relative keys.
			for _, k := range []string{"wal/dt=2026-11-18/hh=21/ing-01-000000000002.wal.zst",
				"wal/dt=2026-11-18/hh=21/ing-01-000000000001.wal.zst", "wal/dt=2026-11-18/hh=22/ing-01-000000000003.wal.zst",
				"wal/_done/1-0/ing-01-000000000001.ok"} {
				if _, err := st.Put(ctx, k, []byte(k)); err != nil {
					t.Fatal(err)
				}
			}
			keys := func(prefix string) []string {
				objs, err := st.List(ctx, prefix)
				if err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, o := range objs {
					if o.Size == 0 && !strings.HasSuffix(o.Key, "empty.bin") {
						t.Errorf("listed %s with size 0", o.Key)
					}
					if o.ETag == "" {
						t.Errorf("listed %s without etag", o.Key)
					}
					out = append(out, o.Key)
				}
				return out
			}
			if got := keys("wal/dt=2026-11-18/hh=21/"); fmt.Sprint(got) != "[wal/dt=2026-11-18/hh=21/ing-01-000000000001.wal.zst wal/dt=2026-11-18/hh=21/ing-01-000000000002.wal.zst]" {
				t.Fatalf("list hh=21: %v", got)
			}
			if got := keys("wal/dt=2026-11-18/hh=2"); len(got) != 3 { // not a directory boundary
				t.Fatalf("list by partial segment: %v", got)
			}
			if got := keys("wal/"); len(got) != 4 || got[0] != "wal/_done/1-0/ing-01-000000000001.ok" {
				t.Fatalf("list wal/: %v", got)
			}
			if got := keys(""); len(got) != 10 {
				t.Fatalf("list all: %d %v", len(got), got)
			}

			// ETag from List is usable for conditional GET.
			objs, _ := st.List(ctx, "lease/")
			if _, _, err := st.Get(ctx, objs[0].Key, &objstore.GetOptions{IfNoneMatch: objs[0].ETag}); !errors.Is(err, objstore.ErrNotModified) {
				t.Fatalf("list etag not accepted by get: %v", err)
			}

			// Delete.
			if err := st.Delete(ctx, key); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.Get(ctx, key, nil); !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("get after delete: %v", err)
			}

			// Invalid keys never reach the backend.
			for _, bad := range []string{"", "/abs", "a//b", "a/../b", "../x", "a/", "a\\b"} {
				if _, err := st.Put(ctx, bad, []byte("x")); err == nil {
					t.Errorf("Put accepted key %q", bad)
				}
				if _, _, err := st.Get(ctx, bad, nil); err == nil {
					t.Errorf("Get accepted key %q", bad)
				}
			}
		})
	}
}

// TestPutIfAbsentRace: at most one of many concurrent creators wins, on every
// backend (file: link(2); s3: If-None-Match: *), and what is stored is one
// contender's value in full.
//
// Against a real server a request can also fail in transit (a connection the
// server closed under load): its outcome is then unknown to the caller — the
// object may or may not have been created by it. The test accepts that, since
// it is what callers must cope with, but it never accepts two winners.
func TestPutIfAbsentRace(t *testing.T) {
	all := backends()
	for _, b := range all[:len(all)-1] { // every backend once (the last is a second devs3 flavour)
		t.Run(b.name, func(t *testing.T) {
			st := b.open(t)
			ctx := context.Background()
			lost := 0
			for round := 0; round < 10; round++ {
				key := fmt.Sprintf("control/lease/shipper-1-0/%012d.json", round)
				const n = 24
				var wins, exists atomic.Int32
				var winner atomic.Int32
				var mu sync.Mutex
				unknown := map[string]bool{} // payloads of requests that failed in transit
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						payload := fmt.Sprintf("holder-%02d", i)
						_, err := st.PutIfAbsent(ctx, key, []byte(payload))
						var transit *url.Error
						switch {
						case err == nil:
							wins.Add(1)
							winner.Store(int32(i))
						case errors.Is(err, objstore.ErrAlreadyExists):
							exists.Add(1)
						case errors.As(err, &transit):
							mu.Lock()
							unknown[payload] = true
							mu.Unlock()
						default:
							t.Errorf("unexpected: %v", err)
						}
					}(i)
				}
				close(start)
				wg.Wait()
				lost += len(unknown)
				if wins.Load() > 1 || int(wins.Load()+exists.Load())+len(unknown) != n {
					t.Fatalf("wins=%d exists=%d unknown=%d", wins.Load(), exists.Load(), len(unknown))
				}
				got, _, err := st.Get(ctx, key, nil)
				if err != nil {
					t.Fatalf("after the race: %v", err)
				}
				switch {
				case wins.Load() == 1:
					if want := fmt.Sprintf("holder-%02d", winner.Load()); string(got) != want {
						t.Fatalf("content %q, winner %q", got, want)
					}
				case !unknown[string(got)]:
					// Nobody was told it won, so the object must come from a request whose answer was lost.
					t.Fatalf("content %q but no request reported success (unknown: %v)", got, unknown)
				}
			}
			if lost > 0 {
				t.Logf("%d of %d requests failed in transit (outcome unknown to the caller)", lost, 10*24)
			}
			if b.name != "s3-real" && lost > 0 {
				t.Fatalf("in-process backends must not lose requests")
			}
		})
	}
}

// The file backend must leave nothing but plain object files behind (it
// shares the tree with Rust's LocalFileSystem) and must hide in-progress
// uploads, its own and Rust's ("<name>#<digits>").
func TestFileLayout(t *testing.T) {
	root := t.TempDir()
	st, err := objstore.Open("file://"+root, objstore.S3Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.Put(ctx, "data/p=7/d=2026-11-18/L0-1-0-0000000001.parquet", []byte("PAR1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutIfAbsent(ctx, "data/p=7/d=2026-11-18/_m/00000001.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutIfAbsent(ctx, "data/p=7/d=2026-11-18/_m/00000001.json", []byte("{}")); !errors.Is(err, objstore.ErrAlreadyExists) {
		t.Fatal(err)
	}
	// A file written directly (as Rust would) is visible as an object.
	if err := os.WriteFile(filepath.Join(root, "data/p=7/d=2026-11-18/L0-1-0-0000000002.parquet"), []byte("PAR1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// In-progress uploads: a fresh one stays, a stale one is cleaned up.
	fresh := filepath.Join(root, "data/p=7/d=2026-11-18/L0-1-0-0000000003.parquet#17")
	stale := filepath.Join(root, "data/p=7/d=2026-11-18/L0-1-0-0000000004.parquet#18")
	for _, p := range []string{fresh, stale} {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	objs, err := st.List(ctx, "data/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	want := "[data/p=7/d=2026-11-18/L0-1-0-0000000001.parquet data/p=7/d=2026-11-18/L0-1-0-0000000002.parquet data/p=7/d=2026-11-18/_m/00000001.json]"
	if fmt.Sprint(keys) != want {
		t.Fatalf("list = %v", keys)
	}

	var onDisk []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			onDisk = append(onDisk, rel)
		}
		return nil
	})
	wantDisk := "[data/p=7/d=2026-11-18/L0-1-0-0000000001.parquet data/p=7/d=2026-11-18/L0-1-0-0000000002.parquet data/p=7/d=2026-11-18/L0-1-0-0000000003.parquet#17 data/p=7/d=2026-11-18/_m/00000001.json]"
	if fmt.Sprint(onDisk) != wantDisk {
		t.Fatalf("files on disk = %v", onDisk)
	}

	if _, err := objstore.Open("file://relative/path", objstore.S3Config{}); err == nil {
		t.Fatal("accepted non-absolute file url")
	}
	if _, err := objstore.Open("gs://bucket", objstore.S3Config{}); err == nil {
		t.Fatal("accepted unknown scheme")
	}
	if _, err := objstore.FromEnvFunc(func(string) string { return "" }); err == nil {
		t.Fatal("FromEnv without configuration should fail")
	}
}
