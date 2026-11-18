package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/wal"
	"github.com/mucan54/izlek/ingest/wire"
)

const testTokens = `{"version": 1, "generated_at": 1795046400000, "tokens": {
  "pk_test": {"project_id": 1017, "status": "active", "replay": true},
  "pk_other": {"project_id": 1018, "status": "active"},
  "pk_web": {"project_id": 2000, "status": "active", "allowed_origins": ["https://app.example.com"]},
  "pk_off": {"project_id": 3000, "status": "disabled", "replay": true},
  "pk_slow": {"project_id": 4000, "status": "active", "rate_limit": 5}
}}`

// harness is one ingestion node wired to a devs3 object store.
type harness struct {
	t      *testing.T
	s3     *devs3.Server
	s3URL  string
	store  objstore.Store
	cfg    Config
	srv    *Server
	ts     *httptest.Server
	client *http.Client
	logs   *syncBuffer
}

// newHarness starts a node. mutate may adjust the configuration; passing
// tokens == "" uploads no token file at all.
func newHarness(t *testing.T, tokens string, mutate func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, s3: devs3.New(), client: &http.Client{Timeout: 20 * time.Second}}
	s3ts := httptest.NewServer(h.s3)
	t.Cleanup(s3ts.Close)
	h.s3URL = s3ts.URL
	st, err := objstore.Open("s3://la-test", objstore.S3Config{Endpoint: s3ts.URL, AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	h.store = st
	if tokens != "" {
		if _, err := st.Put(context.Background(), "control/tokens.json", []byte(tokens)); err != nil {
			t.Fatal(err)
		}
	}
	h.cfg = DefaultConfig()
	h.cfg.NodeID = "ing-t1"
	h.cfg.WALDir = filepath.Join(t.TempDir(), "wal")
	h.cfg.SegmentAge = 40 * time.Millisecond // ship quickly so tests can read the store
	h.cfg.FsyncInterval = time.Millisecond
	h.cfg.WALMaxLocalBytes = 1 << 30
	h.cfg.RateIP = 0 // every test request comes from 127.0.0.1
	h.cfg.DrainDelay = 0
	h.cfg.ShutdownUploadTimeout = 10 * time.Second
	h.cfg.TokensPoll = time.Hour
	h.cfg.LogLevel = slog.LevelDebug
	if mutate != nil {
		mutate(&h.cfg)
	}
	h.start()
	t.Cleanup(func() {
		if h.ts != nil {
			h.stop()
		}
	})
	return h
}

// start launches (or relaunches, on the same WAL directory) the node.
func (h *harness) start() {
	h.t.Helper()
	h.logs = &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: h.cfg.LogLevel}))
	srv, err := New(h.cfg, h.store, log)
	if err != nil {
		h.t.Fatal(err)
	}
	h.srv = srv
	h.ts = httptest.NewServer(srv.Handler())
}

// stop shuts the node down gracefully (seal + upload).
func (h *harness) stop() error {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := h.srv.Shutdown(ctx)
	h.ts.Close()
	h.ts = nil
	return err
}

// kill simulates a process crash: no flush, no seal, no upload.
func (h *harness) kill() {
	h.srv.crash()
	h.ts.CloseClientConnections()
	h.ts.Close()
	h.ts = nil
}

func (h *harness) do(method, path string, body []byte, hdr ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// status sends a request and returns only the status code.
func (h *harness) status(method, path string, body []byte, hdr ...string) int {
	h.t.Helper()
	resp := h.do(method, path, body, hdr...)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// send posts a native batch with the default test token.
func (h *harness) send(body []byte) int {
	h.t.Helper()
	return h.status("POST", "/v1/e", body, "Authorization", "Bearer pk_test")
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// waitShipped blocks until nothing is left on local disk.
func (h *harness) waitShipped() {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := h.srv.uploader.Pending(); n == 0 && h.srv.localBytes.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	n, _ := h.srv.uploader.Pending()
	h.t.Fatalf("WAL not shipped: pending=%d local_bytes=%d", n, h.srv.localBytes.Load())
}

// shipped returns every WAL entry in the object store, in key order, after
// waiting for the local WAL to drain.
func (h *harness) shipped() []wal.Entry {
	h.t.Helper()
	if h.ts != nil {
		h.waitShipped()
	}
	return storeEntries(h.t, h.store)
}

func storeEntries(t *testing.T, st objstore.Store) []wal.Entry {
	t.Helper()
	objs, err := st.List(context.Background(), "wal/")
	if err != nil {
		t.Fatal(err)
	}
	var out []wal.Entry
	seen := map[string]bool{}
	for _, o := range objs {
		base := filepath.Base(o.Key)
		if seen[base] {
			t.Fatalf("segment %s exists under two keys", base)
		}
		seen[base] = true
		raw, _, err := st.Get(context.Background(), o.Key, nil)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		entries, torn, err := wal.ReadAll(zr)
		zr.Close()
		if err != nil || torn {
			t.Fatalf("segment %s: torn=%v err=%v", o.Key, torn, err)
		}
		out = append(out, entries...)
	}
	return out
}

// localFiles lists WAL segment files still on local disk.
func (h *harness) localFiles() []string {
	ents, err := os.ReadDir(h.cfg.WALDir)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".wal") || strings.HasSuffix(e.Name(), ".sealed") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// metric returns the value of one exposition line, e.g.
// metric(`la_ingest_dup_batches_total`) or
// metric(`la_ingest_requests_total{code="200",format="native"}`); -1 if absent.
func (h *harness) metric(name string) float64 {
	h.t.Helper()
	var sb strings.Builder
	if err := h.srv.m.reg.Write(&sb); err != nil {
		h.t.Fatal(err)
	}
	for _, line := range strings.Split(sb.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, name+" "); ok {
			var v float64
			if err := json.Unmarshal([]byte(rest), &v); err != nil {
				h.t.Fatalf("metric %s: %q", name, rest)
			}
			return v
		}
	}
	return -1
}

// mkBatch builds a native batch whose batch_id is derived from id, with n
// records.
func mkBatch(id uint32, n int) []byte {
	b := &wire.Batch{SentAt: 1795046400123, SDK: "test/0.1", AnonID: "anon-1"}
	b.BatchID = [16]byte{0x01, 0x93, byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id), 0x70}
	for i := 0; i < n; i++ {
		r := wire.Record{Kind: wire.KindEvent, TS: 1795046400000 + uint64(i), Name: "evt",
			Props: []wire.Prop{{Key: "i", Tag: wire.TagInt, Int: int64(i)}, {Key: "secret_marker", Tag: wire.TagString, Str: "s3cr3t-payload-value"}}}
		r.UUID = [16]byte{byte(id >> 8), byte(id), byte(i >> 8), byte(i)}
		b.Records = append(b.Records, r)
	}
	return wire.AppendBatch(nil, b)
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
