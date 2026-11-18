package devs3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/objstore"
)

func newStore(t *testing.T, srv *devs3.Server) (objstore.Store, string) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	st, err := objstore.Open("s3://bkt/pre/fix", objstore.S3Config{Endpoint: ts.URL, AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	return st, ts.URL
}

// Design §19 assumption 1: concurrent create-if-absent PUTs to one key have
// exactly one winner. This proves the client + wire protocol against devs3;
// the production provider must be re-verified separately.
func TestConditionalPutExactlyOneWinner(t *testing.T) {
	srv := devs3.New()
	st, _ := newStore(t, srv)
	ctx := context.Background()

	for round := 0; round < 20; round++ {
		key := fmt.Sprintf("data/p=1/d=2026-11-18/_m/%08d.json", round)
		const n = 32
		var wins, exists atomic.Int32
		var winner atomic.Int32
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := st.PutIfAbsent(ctx, key, []byte(fmt.Sprintf("writer-%d", i)))
				switch {
				case err == nil:
					wins.Add(1)
					winner.Store(int32(i))
				case errors.Is(err, objstore.ErrAlreadyExists):
					exists.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 || exists.Load() != n-1 {
			t.Fatalf("round %d: wins=%d exists=%d, want 1 and %d", round, wins.Load(), exists.Load(), n-1)
		}
		got, _, err := st.Get(ctx, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("writer-%d", winner.Load()); string(got) != want {
			t.Fatalf("round %d: stored %q, winner wrote %q", round, got, want)
		}
	}
}

func TestRawProtocol(t *testing.T) {
	srv := devs3.New()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	do := func(method, path string, body string, hdr ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	read := func(resp *http.Response) string {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	resp := do("PUT", "/b/dir/a.txt", "0123456789")
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || etag != `"781e5e245d69b566979b86e28d23f2c7"` {
		t.Fatalf("put: %d %s", resp.StatusCode, etag)
	}
	read(resp)

	if resp = do("PUT", "/b/dir/a.txt", "x", "If-None-Match", "*"); resp.StatusCode != 412 {
		t.Fatalf("conditional put on existing key: %d", resp.StatusCode)
	} else if body := read(resp); !strings.Contains(body, "<Code>PreconditionFailed</Code>") {
		t.Fatalf("412 body: %s", body)
	}

	resp = do("GET", "/b/dir/a.txt", "", "Range", "bytes=2-5")
	if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range: %d %q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
	if got := read(resp); got != "2345" {
		t.Fatalf("range body %q", got)
	}
	if resp = do("GET", "/b/dir/a.txt", "", "Range", "bytes=-3"); read(resp) != "789" {
		t.Fatal("suffix range")
	}
	if resp = do("GET", "/b/dir/a.txt", "", "If-None-Match", etag); resp.StatusCode != 304 {
		t.Fatalf("if-none-match: %d", resp.StatusCode)
	}
	read(resp)
	resp = do("HEAD", "/b/dir/a.txt", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Length") != "10" || resp.Header.Get("ETag") != etag {
		t.Fatalf("head: %d %v", resp.StatusCode, resp.Header)
	}
	read(resp)
	if resp = do("HEAD", "/b/missing", ""); resp.StatusCode != 404 {
		t.Fatalf("head missing: %d", resp.StatusCode)
	}
	if resp = do("GET", "/b/missing", ""); resp.StatusCode != 404 || !strings.Contains(read(resp), "NoSuchKey") {
		t.Fatalf("get missing: %d", resp.StatusCode)
	}

	// aws-chunked upload (AWS SDK streaming signature framing).
	chunked := "5;chunk-signature=abc\r\nhello\r\n6;chunk-signature=def\r\n world\r\n0;chunk-signature=fff\r\n\r\n"
	resp = do("PUT", "/b/dir/chunked.txt", chunked, "X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	read(resp)
	if got := read(do("GET", "/b/dir/chunked.txt", "")); got != "hello world" {
		t.Fatalf("aws-chunked body %q", got)
	}

	// Listing with delimiter and pagination.
	for _, k := range []string{"dir/b.txt", "dir/sub/c.txt", "dir/sub/d.txt", "other/e.txt"} {
		read(do("PUT", "/b/"+k, k))
	}
	body := read(do("GET", "/b?list-type=2&prefix=dir/&delimiter=/", ""))
	for _, want := range []string{"<Key>dir/a.txt</Key>", "<Key>dir/b.txt</Key>", "<Key>dir/chunked.txt</Key>",
		"<CommonPrefixes><Prefix>dir/sub/</Prefix></CommonPrefixes>", "<KeyCount>4</KeyCount>", "<IsTruncated>false</IsTruncated>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("list missing %s in %s", want, body)
		}
	}
	if strings.Contains(body, "dir/sub/c.txt") || strings.Contains(body, "other/") {
		t.Fatalf("list leaked keys: %s", body)
	}

	// Bulk delete + single delete.
	del := `<Delete><Object><Key>dir/b.txt</Key></Object><Object><Key>nope</Key></Object></Delete>`
	if body = read(do("POST", "/b?delete", del)); !strings.Contains(body, "<Deleted><Key>dir/b.txt</Key></Deleted>") {
		t.Fatalf("bulk delete: %s", body)
	}
	if resp = do("DELETE", "/b/dir/a.txt", ""); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp = do("DELETE", "/b/dir/a.txt", ""); resp.StatusCode != 204 {
		t.Fatalf("delete missing: %d", resp.StatusCode)
	}
	// Copy.
	if resp = do("PUT", "/b/copy.txt", "", "X-Amz-Copy-Source", "/b/dir/chunked.txt"); resp.StatusCode != 200 {
		t.Fatalf("copy: %d", resp.StatusCode)
	}
	read(resp)
	if got := read(do("GET", "/b/copy.txt", "")); got != "hello world" {
		t.Fatalf("copy body %q", got)
	}
}

func TestListPagination(t *testing.T) {
	srv := devs3.New()
	st, _ := newStore(t, srv)
	ctx := context.Background()
	const n = 2345 // > 2 pages of 1000
	for i := 0; i < n; i++ {
		if _, err := st.Put(ctx, fmt.Sprintf("wal/k%05d", i), []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	before := srv.Requests()
	objs, err := st.List(ctx, "wal/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != n {
		t.Fatalf("listed %d, want %d", len(objs), n)
	}
	for i, o := range objs {
		if o.Key != fmt.Sprintf("wal/k%05d", i) || o.Size != 1 || o.ETag == "" || o.LastModified.IsZero() {
			t.Fatalf("object %d: %+v", i, o)
		}
	}
	if pages := srv.Requests() - before; pages != 3 {
		t.Fatalf("list used %d requests, want 3 pages", pages)
	}
}

func TestMultipartStream(t *testing.T) {
	srv := devs3.New()
	st, _ := newStore(t, srv)
	ctx := context.Background()
	// Unknown length, larger than one part: goes through the multipart path.
	want := bytes.Repeat([]byte("0123456789abcdef"), (20<<20)/16)
	obj, err := st.PutStream(ctx, "replay/big.bin", io.MultiReader(bytes.NewReader(want)), -1)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(want)) || !strings.HasSuffix(obj.ETag, `-3"`) {
		t.Fatalf("multipart object %+v", obj)
	}
	got, _, err := st.Get(ctx, "replay/big.bin", nil)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("multipart round trip failed: %v (len %d)", err, len(got))
	}
}

func TestFaultInjection(t *testing.T) {
	srv := devs3.New()
	st, baseURL := newStore(t, srv)
	ctx := context.Background()
	if _, err := st.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}

	srv.SetFault(devs3.FaultFail)
	_, err := st.Put(ctx, "k2", []byte("v"))
	var se *objstore.S3Error
	if !errors.As(err, &se) || se.StatusCode != 503 || se.Code != "ServiceUnavailable" {
		t.Fatalf("fail mode: %v", err)
	}
	if _, _, err := st.Get(ctx, "k", nil); err == nil {
		t.Fatal("get succeeded in fail mode")
	}

	srv.SetFault(devs3.FaultPause)
	done := make(chan error, 1)
	go func() {
		_, err := st.Put(ctx, "k3", []byte("v"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("put returned during pause: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	// A paused request honours client cancellation.
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if _, _, err := st.Get(cctx, "k", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("paused get: %v", err)
	}
	cancel()

	// Switch off through the HTTP control endpoint (what other languages use).
	resp, err := http.Post(baseURL+"/_devs3/fault?mode=none", "", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("control endpoint: %v", err)
	}
	resp.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("paused put failed after resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("paused put never resumed")
	}
	if got := srv.Keys("bkt"); len(got) != 2 || got[0] != "pre/fix/k" || got[1] != "pre/fix/k3" {
		t.Fatalf("keys after outage: %v", got)
	}
}

func TestDirPersistence(t *testing.T) {
	dir := t.TempDir()
	srv, err := devs3.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := newStore(t, srv)
	ctx := context.Background()
	if _, err := st.Put(ctx, "a/b.txt", []byte("persisted")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, "a/gone.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "a/gone.txt"); err != nil {
		t.Fatal(err)
	}
	srv2, err := devs3.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	st2, _ := newStore(t, srv2)
	got, _, err := st2.Get(ctx, "a/b.txt", nil)
	if err != nil || string(got) != "persisted" {
		t.Fatalf("reload: %q %v", got, err)
	}
	if objs, _ := st2.List(ctx, ""); len(objs) != 1 {
		t.Fatalf("reload listed %v", objs)
	}
}
