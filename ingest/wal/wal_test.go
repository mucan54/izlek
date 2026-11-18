package wal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/mucan54/izlek/golib/objstore"
)

func env(format uint8, project uint32) Envelope {
	return Envelope{Format: format, ProjectID: project, RecvTSMs: 1795046400123, Country: [2]byte{'T', 'R'}, Flags: FlagWeb}
}

func mustEntry(t testing.TB, e Envelope, body []byte) []byte {
	t.Helper()
	b, err := AppendEntry(nil, e, body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sealedCollector gathers segments handed out by the writer.
type sealedCollector struct {
	mu   sync.Mutex
	segs []Segment
}

func (c *sealedCollector) add(s Segment) {
	c.mu.Lock()
	c.segs = append(c.segs, s)
	c.mu.Unlock()
}

func (c *sealedCollector) list() []Segment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Segment(nil), c.segs...)
}

func readSegmentFile(t testing.TB, path string) ([]Entry, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, torn, err := ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return entries, torn
}

func dirFiles(t testing.TB, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.Name() != hwmFile {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestEntryFormatGolden(t *testing.T) {
	// Byte-exact layout check against spec/wal.md §1, computed by hand:
	// envelope = format 01 | project 1017 LE | recv_ts 1795046400123 LE | "TR" | flags 01
	e := Envelope{Format: FormatNative, ProjectID: 1017, RecvTSMs: 1795046400123, Country: [2]byte{'T', 'R'}, Flags: 1}
	got := mustEntry(t, e, []byte("LA1\x00body"))
	wantEnv := "01" + "f9030000" + "7b601af1a1010000" + "5452" + "01"
	if h := hex.EncodeToString(got[8:24]); h != wantEnv {
		t.Fatalf("envelope %s, want %s", h, wantEnv)
	}
	if n := binary.LittleEndian.Uint32(got[0:4]); n != 16+8 {
		t.Fatalf("len field %d, want 24 (envelope 16 + body 8)", n)
	}
	// CRC-32C known answer: crc32c("123456789") = 0xE3069283.
	if c := crcOf([]byte("123456789")); c != 0xE3069283 {
		t.Fatalf("crc32c check value %08x", c)
	}
	if c := binary.LittleEndian.Uint32(got[4:8]); c != crcOf(got[8:]) {
		t.Fatalf("crc field %08x does not cover envelope+body", c)
	}
	if string(got[24:]) != "LA1\x00body" {
		t.Fatalf("body %q", got[24:])
	}
	if _, err := AppendEntry(nil, e, make([]byte, MaxBodyLen+1)); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("oversized body: %v", err)
	}
	if _, err := AppendEntry(nil, e, make([]byte, MaxBodyLen)); err != nil {
		t.Fatalf("max body: %v", err)
	}
}

func crcOf(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

func TestWriterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	var sealed sealedCollector
	var local atomic.Int64
	var fsyncs atomic.Int64
	w := NewWriter(Options{Dir: dir, NodeID: "ing-01", StartSeq: 42, OnSealed: sealed.add, LocalBytes: &local,
		FsyncInterval: time.Millisecond, OnFsync: func(time.Duration, int) { fsyncs.Add(1) }})

	var want []Entry
	for i := 0; i < 200; i++ {
		e := Envelope{Format: uint8(1 + i%3), ProjectID: uint32(1000 + i), RecvTSMs: uint64(1795046400000 + i),
			Country: [2]byte{'A' + byte(i%26), 'Z'}, Flags: uint8(i % 2)}
		body := bytes.Repeat([]byte{byte(i)}, i*37%5000)
		want = append(want, Entry{Envelope: e, Body: body})
	}
	var wg sync.WaitGroup
	for i := range want {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := w.Append(mustEntry(t, want[i].Envelope, want[i].Body)); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if got := dirFiles(t, dir); len(got) != 1 || got[0] != "ing-01-000000000042.wal" {
		t.Fatalf("active files: %v", got)
	}
	fi, _ := os.Stat(filepath.Join(dir, "ing-01-000000000042.wal"))
	if local.Load() != fi.Size() {
		t.Fatalf("LocalBytes %d, file size %d", local.Load(), fi.Size())
	}
	w.Close()
	if err := w.Append(mustEntry(t, env(1, 1), nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close: %v", err)
	}

	segs := sealed.list()
	if len(segs) != 1 || segs[0].Name != "ing-01-000000000042" || segs[0].Size != fi.Size() {
		t.Fatalf("sealed: %+v", segs)
	}
	if !strings.HasSuffix(segs[0].Path, ".sealed") {
		t.Fatalf("sealed path %s", segs[0].Path)
	}
	got, torn := readSegmentFile(t, segs[0].Path)
	if torn || len(got) != len(want) {
		t.Fatalf("read back %d entries (torn=%v), want %d", len(got), torn, len(want))
	}
	// Concurrent appends land in arbitrary order; compare as multisets.
	key := func(e Entry) string { return fmt.Sprintf("%d/%x", e.ProjectID, e.Body) }
	seen := map[string]Entry{}
	for _, e := range got {
		seen[key(e)] = e
	}
	for _, e := range want {
		g, ok := seen[key(e)]
		if !ok || g.Envelope != e.Envelope || !bytes.Equal(g.Body, e.Body) {
			t.Fatalf("entry for project %d missing or different", e.ProjectID)
		}
	}
	if n := fsyncs.Load(); n < 1 || n >= 200 {
		t.Logf("fsync count %d (grouping is timing dependent)", n)
	}
	if hwm := readHWM(dir); hwm != 42 {
		t.Fatalf("hwm %d, want 42", hwm)
	}
}

// gateFile wraps a real file; Sync blocks until released and records order.
type gateFile struct {
	File
	mu       sync.Mutex
	writes   int
	syncs    int
	written  int
	synced   int // bytes covered by completed fsyncs
	gate     chan struct{}
	syncErr  error
	writeErr error
	entered  chan struct{}
}

func (g *gateFile) Write(p []byte) (int, error) {
	g.mu.Lock()
	err := g.writeErr
	g.mu.Unlock()
	if err != nil {
		// Simulate a short write that left garbage on disk.
		g.File.Write(p[:len(p)/2])
		return len(p) / 2, err
	}
	n, err := g.File.Write(p)
	g.mu.Lock()
	g.writes++
	g.written += n
	g.mu.Unlock()
	return n, err
}

func (g *gateFile) Sync() error {
	if g.entered != nil {
		g.entered <- struct{}{}
	}
	if g.gate != nil {
		<-g.gate
	}
	g.mu.Lock()
	err := g.syncErr
	g.mu.Unlock()
	if err != nil {
		return err
	}
	if err := g.File.Sync(); err != nil {
		return err
	}
	g.mu.Lock()
	g.syncs++
	g.synced = g.written
	g.mu.Unlock()
	return nil
}

func openGated(g **gateFile, mk func(f File) *gateFile) func(string) (File, error) {
	return func(path string) (File, error) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		*g = mk(f)
		return *g, nil
	}
}

// The durability contract: Append never returns before the fsync covering
// its entry has completed, and one fsync acknowledges a whole group.
func TestNoAckBeforeFsync(t *testing.T) {
	dir := t.TempDir()
	var g *gateFile
	gate := make(chan struct{})
	entered := make(chan struct{}, 16)
	w := NewWriter(Options{Dir: dir, NodeID: "n", StartSeq: 1, FsyncInterval: 5 * time.Millisecond,
		OpenFile: openGated(&g, func(f File) *gateFile { return &gateFile{File: f, gate: gate, entered: entered} })})
	defer w.Close()

	const n = 50
	var acked atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := w.Append(mustEntry(t, env(1, uint32(i+1)), []byte("payload"))); err != nil {
				t.Errorf("append: %v", err)
			}
			acked.Add(1)
			// At the moment of the ack the entry must be covered by a
			// completed fsync.
			g.mu.Lock()
			syncs, synced := g.syncs, g.synced
			g.mu.Unlock()
			if syncs == 0 || synced == 0 {
				t.Errorf("ack observed with syncs=%d synced=%d", syncs, synced)
			}
		}(i)
	}
	<-entered // the writer is now inside fsync, blocked on the gate
	time.Sleep(100 * time.Millisecond)
	if a := acked.Load(); a != 0 {
		t.Fatalf("%d appends acknowledged while fsync was still in progress", a)
	}
	g.mu.Lock()
	if g.written == 0 {
		t.Fatal("fsync entered before any write")
	}
	g.mu.Unlock()
	// Release fsyncs; drain 'entered' so later groups are not blocked on it.
	go func() {
		for range entered {
		}
	}()
	close(gate)
	wg.Wait()
	close(entered)
	g.mu.Lock()
	defer g.mu.Unlock()
	if acked.Load() != n {
		t.Fatalf("acked %d/%d", acked.Load(), n)
	}
	if g.syncs >= n {
		t.Fatalf("%d fsyncs for %d entries: no group commit", g.syncs, n)
	}
	t.Logf("%d entries acknowledged by %d fsyncs", n, g.syncs)
}

// A failed write or fsync fails exactly the entries of that group, the
// durable prefix of the segment is shipped, and the writer continues in a
// fresh segment.
func TestWriteAndFsyncFailure(t *testing.T) {
	for _, mode := range []string{"fsync", "write"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			var sealed sealedCollector
			var local atomic.Int64
			var g *gateFile
			w := NewWriter(Options{Dir: dir, NodeID: "n", StartSeq: 7, OnSealed: sealed.add, LocalBytes: &local,
				OpenFile: openGated(&g, func(f File) *gateFile { return &gateFile{File: f} })})

			if err := w.Append(mustEntry(t, env(1, 1), []byte("good-1"))); err != nil {
				t.Fatal(err)
			}
			first := g
			boom := errors.New("injected " + mode + " failure")
			first.mu.Lock()
			if mode == "fsync" {
				first.syncErr = boom
			} else {
				first.writeErr = boom
			}
			first.mu.Unlock()
			if err := w.Append(mustEntry(t, env(1, 2), []byte("lost-2"))); !errors.Is(err, boom) {
				t.Fatalf("append during failure: %v", err)
			}
			// The failed segment was sealed with only the acknowledged entry.
			segs := sealed.list()
			if len(segs) != 1 || segs[0].Name != "n-000000000007" {
				t.Fatalf("sealed after failure: %+v", segs)
			}
			entries, torn := readSegmentFile(t, segs[0].Path)
			if torn || len(entries) != 1 || string(entries[0].Body) != "good-1" {
				t.Fatalf("failed segment content: %d entries torn=%v", len(entries), torn)
			}
			// Next append succeeds in a new segment.
			if err := w.Append(mustEntry(t, env(1, 3), []byte("good-3"))); err != nil {
				t.Fatalf("append after failure: %v", err)
			}
			if g == first {
				t.Fatal("writer kept using the failed file")
			}
			w.Close()
			segs = sealed.list()
			if len(segs) != 2 || segs[1].Name != "n-000000000008" {
				t.Fatalf("sealed: %+v", segs)
			}
			entries, _ = readSegmentFile(t, segs[1].Path)
			if len(entries) != 1 || string(entries[0].Body) != "good-3" {
				t.Fatalf("second segment: %+v", entries)
			}
			if want := segs[0].Size + segs[1].Size; local.Load() != want {
				t.Fatalf("LocalBytes %d, want %d", local.Load(), want)
			}
		})
	}
}

// If the very first write of a segment fails nothing durable exists: the
// file is removed and no segment is handed out.
func TestFirstWriteFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	var sealed sealedCollector
	var local atomic.Int64
	fail := true
	w := NewWriter(Options{Dir: dir, NodeID: "n", StartSeq: 1, OnSealed: sealed.add, LocalBytes: &local,
		OpenFile: func(path string) (File, error) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o644)
			if err != nil {
				return nil, err
			}
			g := &gateFile{File: f}
			if fail {
				g.syncErr = errors.New("disk on fire")
			}
			return g, nil
		}})
	if err := w.Append(mustEntry(t, env(1, 1), []byte("x"))); err == nil {
		t.Fatal("append succeeded despite fsync failure")
	}
	if files := dirFiles(t, dir); len(files) != 0 || len(sealed.list()) != 0 || local.Load() != 0 {
		t.Fatalf("leftovers: files=%v sealed=%v local=%d", files, sealed.list(), local.Load())
	}
	fail = false
	if err := w.Append(mustEntry(t, env(1, 1), []byte("x"))); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if segs := sealed.list(); len(segs) != 1 || segs[0].Name != "n-000000000002" {
		t.Fatalf("sealed: %+v", segs)
	}
}

func TestRotationBySizeAndAge(t *testing.T) {
	dir := t.TempDir()
	var sealed sealedCollector
	w := NewWriter(Options{Dir: dir, NodeID: "ing-a", StartSeq: 100, OnSealed: sealed.add,
		SegmentBytes: 4096, SegmentAge: 150 * time.Millisecond})
	body := bytes.Repeat([]byte("x"), 1000)
	for i := 0; i < 10; i++ { // ~10 KB: at least two size rotations
		if err := w.Append(mustEntry(t, env(1, uint32(i+1)), body)); err != nil {
			t.Fatal(err)
		}
	}
	bySize := len(sealed.list())
	if bySize < 2 {
		t.Fatalf("expected >= 2 size-triggered rotations, got %d", bySize)
	}
	for _, s := range sealed.list() {
		if s.Size < 4096 || s.Size > 4096+1100 {
			t.Fatalf("size-rotated segment has %d bytes", s.Size)
		}
	}
	// Age rotation: one small entry, then wait.
	if err := w.Append(mustEntry(t, env(1, 99), []byte("tiny"))); err != nil {
		t.Fatal(err)
	}
	before := len(sealed.list())
	deadline := time.Now().Add(3 * time.Second)
	for len(sealed.list()) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(sealed.list()) != before+1 {
		t.Fatal("segment was not rotated by age")
	}
	// Idle writer: no further (empty) segments.
	time.Sleep(400 * time.Millisecond)
	if n := len(sealed.list()); n != before+1 {
		t.Fatalf("idle writer produced segments: %d", n)
	}
	w.Close()

	// Sequence numbers are consecutive and every entry is in exactly one segment.
	seen := map[uint32]int{}
	for i, s := range sealed.list() {
		if want := SegmentName("ing-a", uint64(100+i)); s.Name != want {
			t.Fatalf("segment %d named %s, want %s", i, s.Name, want)
		}
		entries, torn := readSegmentFile(t, s.Path)
		if torn {
			t.Fatalf("segment %s torn", s.Name)
		}
		for _, e := range entries {
			seen[e.ProjectID]++
		}
	}
	if len(seen) != 11 {
		t.Fatalf("entries found: %v", seen)
	}
	for p, n := range seen {
		if n != 1 {
			t.Fatalf("project %d appears %d times", p, n)
		}
	}
}

func TestObjectKeyAndNames(t *testing.T) {
	closed := time.Date(2026, 11, 18, 21, 59, 59, 999e6, time.UTC)
	if got := ObjectKey(SegmentName("ing-01", 1795046), closed); got != "wal/dt=2026-11-18/hh=21/ing-01-000001795046.wal.zst" {
		t.Fatalf("key %s", got)
	}
	// Close time in another zone still yields UTC partitions.
	ist := time.FixedZone("TRT", 3*3600)
	if got := ObjectKey("n-000000000001", time.Date(2026, 11, 19, 1, 30, 0, 0, ist)); got != "wal/dt=2026-11-18/hh=22/n-000000000001.wal.zst" {
		t.Fatalf("key %s", got)
	}
	p := sealedPath("/w", "ing-eu-1-000000000009", closed)
	name, seq, at, sealed, ok := parseLocal(filepath.Base(p))
	if !ok || !sealed || name != "ing-eu-1-000000000009" || seq != 9 || !at.Equal(closed) {
		t.Fatalf("parse sealed: %v %v %v %v %v", name, seq, at, sealed, ok)
	}
	name, seq, _, sealed, ok = parseLocal("ing-eu-1-000000000010.wal")
	if !ok || sealed || name != "ing-eu-1-000000000010" || seq != 10 {
		t.Fatalf("parse active: %v %v %v %v", name, seq, sealed, ok)
	}
	for _, bad := range []string{"tokens.json", "seq.hwm", "x.wal", "n-123.wal", "n-000000000001.sealed", "n-000000000001.abc.sealed", ".wal"} {
		if _, _, _, _, ok := parseLocal(bad); ok {
			t.Errorf("parseLocal accepted %q", bad)
		}
	}
	now := time.Unix(1795046400, 0)
	if StartSeq(0, now) != 1795046400 || StartSeq(1795046400, now) != 1795046401 || StartSeq(9e12, now) != 9e12+1 {
		t.Fatal("StartSeq rule")
	}
}

// crash writes entries, then stops the writer without sealing, returning the
// path of the segment left behind.
func crash(t *testing.T, dir string, seq uint64, bodies ...string) string {
	t.Helper()
	w := NewWriter(Options{Dir: dir, NodeID: "ing-01", StartSeq: seq})
	for i, b := range bodies {
		if err := w.Append(mustEntry(t, env(FormatNative, uint32(i+1)), []byte(b))); err != nil {
			t.Fatal(err)
		}
	}
	w.Abort()
	path := activePath(dir, SegmentName("ing-01", seq))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("crashed writer left no active segment: %v", err)
	}
	return path
}

func TestRecoverTornTail(t *testing.T) {
	tail := mustEntry(t, env(FormatNative, 77), []byte("never-acknowledged"))
	cases := []struct {
		name    string
		mangle  func(valid []byte) []byte
		wantCut bool
	}{
		{"clean", func(v []byte) []byte { return v }, false},
		{"partial header", func(v []byte) []byte { return append(v, tail[:5]...) }, true},
		{"partial body", func(v []byte) []byte { return append(v, tail[:len(tail)-3]...) }, true},
		{"crc mismatch", func(v []byte) []byte {
			bad := append([]byte(nil), tail...)
			bad[len(bad)-1] ^= 0xff
			return append(v, bad...)
		}, true},
		{"len too small", func(v []byte) []byte { return append(v, 5, 0, 0, 0, 1, 2, 3, 4, 9, 9, 9, 9, 9) }, true},
		{"len huge", func(v []byte) []byte { return append(v, 0xff, 0xff, 0xff, 0x7f, 1, 2, 3, 4) }, true},
		{"zero fill", func(v []byte) []byte { return append(v, make([]byte, 4096)...) }, true},
		{"garbage then valid entry", func(v []byte) []byte {
			// Reading stops at the first bad entry (spec/wal.md §1); what
			// follows is unreachable and is cut with it.
			return append(append(v, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24), tail...)
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := crash(t, dir, 5, "one", "two", "three")
			valid, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, c.mangle(append([]byte(nil), valid...)), 0o644); err != nil {
				t.Fatal(err)
			}

			var visited []string
			now := time.Date(2026, 11, 18, 21, 15, 0, 0, time.UTC)
			res, err := Recover(dir, now, func(e Envelope, body []byte) {
				visited = append(visited, string(body))
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Segments) != 1 || res.MaxSeq != 5 {
				t.Fatalf("recovered %+v", res)
			}
			seg := res.Segments[0]
			if seg.Name != "ing-01-000000000005" || seg.Size != int64(len(valid)) || res.Bytes != seg.Size {
				t.Fatalf("segment %+v, valid len %d", seg, len(valid))
			}
			if seg.Key() != "wal/dt=2026-11-18/hh=21/ing-01-000000000005.wal.zst" {
				t.Fatalf("key %s", seg.Key())
			}
			if (res.TruncatedTails == 1) != c.wantCut {
				t.Fatalf("TruncatedTails=%d, wantCut=%v", res.TruncatedTails, c.wantCut)
			}
			onDisk, err := os.ReadFile(seg.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(onDisk, valid) {
				t.Fatalf("recovered file differs from the valid prefix (%d vs %d bytes)", len(onDisk), len(valid))
			}
			if fmt.Sprint(visited) != "[one two three]" {
				t.Fatalf("visited %v", visited)
			}
			if files := dirFiles(t, dir); len(files) != 1 || !strings.HasSuffix(files[0], ".sealed") {
				t.Fatalf("dir after recovery: %v", files)
			}

			// Recovery is idempotent, and keeps the close time (and so the
			// object key) even when run again an hour later.
			res2, err := Recover(dir, now.Add(time.Hour), nil, nil)
			if err != nil || len(res2.Segments) != 1 || res2.Segments[0] != seg || res2.TruncatedTails != 0 {
				t.Fatalf("second recovery: %+v %v", res2, err)
			}
		})
	}
}

func TestRecoverEdgeCases(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Died while creating the file / before the first fsync.
	write("ing-01-000000000001.wal", nil)
	write("ing-01-000000000002.wal", []byte("LA"))
	write("ing-01-000000000003.wal", []byte(SegMagic))
	// Magic present, first entry torn: nothing was ever acknowledged.
	write("ing-01-000000000004.wal", append([]byte(SegMagic), 0x20, 0, 0, 0, 1, 2))
	// A healthy sealed segment from an earlier run (different node id).
	good := mustEntry(t, env(FormatPostHog, 9), []byte(`{"api_key":"x"}`))
	write("old-node-000000000007.1795046400000.sealed", append([]byte(SegMagic), good...))
	// Unknown head: must be kept, not deleted, and must not block startup.
	write("ing-01-000000000005.wal", []byte("XXXXsomething else entirely"))
	// Unrelated files are ignored.
	write("tokens.json", []byte("{}"))
	write(hwmFile, []byte("1234"))

	var visited int
	res, err := Recover(dir, time.Now(), func(Envelope, []byte) { visited++ }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 1 || res.Segments[0].Name != "old-node-000000000007" || visited != 1 {
		t.Fatalf("recovered %+v visited=%d", res, visited)
	}
	if !res.Segments[0].ClosedAt.Equal(time.UnixMilli(1795046400000)) {
		t.Fatalf("closed at %v", res.Segments[0].ClosedAt)
	}
	if res.MaxSeq != 1234 {
		t.Fatalf("MaxSeq %d, want high-water mark 1234", res.MaxSeq)
	}
	want := "[ing-01-000000000005.wal old-node-000000000007.1795046400000.sealed tokens.json]"
	if got := fmt.Sprint(dirFiles(t, dir)); got != want {
		t.Fatalf("dir after recovery: %s", got)
	}
}

// failingStore fails the first n Puts.
type failingStore struct {
	objstore.Store
	mu       sync.Mutex
	failures int
	puts     int
}

func (f *failingStore) Put(ctx context.Context, key string, data []byte) (objstore.Object, error) {
	f.mu.Lock()
	f.puts++
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	f.mu.Unlock()
	if fail {
		return objstore.Object{}, errors.New("injected store failure")
	}
	return f.Store.Put(ctx, key, data)
}

func decodeObject(t testing.TB, st objstore.Store, key string) []Entry {
	t.Helper()
	raw, _, err := st.Get(context.Background(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		t.Fatalf("%s is not a zstd frame", key)
	}
	zr, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	entries, torn, err := ReadAll(zr)
	if err != nil || torn {
		t.Fatalf("decode %s: torn=%v err=%v", key, torn, err)
	}
	return entries
}

// Full cycle: write, rotate, upload (with transient store failures), local
// deletion only after success, crash, restart recovery, re-upload.
func TestRotateUploadRecover(t *testing.T) {
	dir := t.TempDir()
	base, err := objstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &failingStore{Store: base, failures: 3}
	ctx := context.Background()
	var local atomic.Int64
	var uploaded atomic.Int32
	var uploadErrs atomic.Int32

	newUploader := func() *Uploader {
		u := NewUploader(UploaderOptions{Store: store, LocalBytes: &local, MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
			OnUploaded: func(Segment, int64) { uploaded.Add(1) }, OnError: func(Segment, error) { uploadErrs.Add(1) }})
		u.Start()
		return u
	}

	// --- first process -----------------------------------------------------
	u := newUploader()
	w := NewWriter(Options{Dir: dir, NodeID: "ing-01", StartSeq: StartSeq(0, time.Unix(1000, 0)), SegmentBytes: 2000,
		OnSealed: u.Enqueue, LocalBytes: &local})
	var want []string
	for i := 0; i < 12; i++ {
		body := fmt.Sprintf("batch-%02d-%s", i, strings.Repeat("p", 500))
		want = append(want, body)
		if err := w.Append(mustEntry(t, env(FormatNative, 1017), []byte(body))); err != nil {
			t.Fatal(err)
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := u.Drain(dctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	cancel()
	if uploadErrs.Load() != 3 {
		t.Fatalf("upload errors %d, want the 3 injected", uploadErrs.Load())
	}
	// The remaining entries sit in the active segment; crash now.
	if err := w.Append(mustEntry(t, env(FormatNative, 1017), []byte("in-active-segment"))); err != nil {
		t.Fatal(err)
	}
	want = append(want, "in-active-segment")
	w.Abort()
	u.Stop()
	files := dirFiles(t, dir)
	if len(files) != 1 || !strings.HasSuffix(files[0], ".wal") {
		t.Fatalf("after crash expected exactly the active segment, got %v", files)
	}
	uploadedBefore := uploaded.Load()

	// --- second process ----------------------------------------------------
	// Simulate "uploaded but crashed before deleting the local file": put a
	// sealed copy of an already uploaded segment back.
	objs, _ := base.List(ctx, "wal/")
	if len(objs) == 0 {
		t.Fatal("nothing uploaded by the first process")
	}
	redoKey := objs[0].Key
	redoEntries := decodeObject(t, base, redoKey)
	redo := []byte(SegMagic)
	for _, e := range redoEntries {
		redo, _ = AppendEntry(redo, e.Envelope, e.Body)
	}
	// Recover the original close time from the key's hour partition: use the
	// object's own name and a close time inside that hour.
	nameWithExt := filepath.Base(redoKey)
	redoName := strings.TrimSuffix(nameWithExt, ".wal.zst")
	var y, mo, d, h int
	fmt.Sscanf(redoKey, "wal/dt=%d-%d-%d/hh=%d/", &y, &mo, &d, &h)
	redoClosed := time.Date(y, time.Month(mo), d, h, 30, 0, 0, time.UTC)
	if err := os.WriteFile(sealedPath(dir, redoName, redoClosed), redo, 0o644); err != nil {
		t.Fatal(err)
	}

	var seeded []string
	res, err := Recover(dir, time.Now(), func(e Envelope, body []byte) { seeded = append(seeded, string(body)) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 2 {
		t.Fatalf("recovered %d segments, want 2", len(res.Segments))
	}
	local.Store(res.Bytes)
	u2 := newUploader()
	for _, s := range res.Segments {
		u2.Enqueue(s)
	}
	next := StartSeq(res.MaxSeq, time.Unix(1000, 0))
	w2 := NewWriter(Options{Dir: dir, NodeID: "ing-01", StartSeq: next, OnSealed: u2.Enqueue, LocalBytes: &local})
	if err := w2.Append(mustEntry(t, env(FormatNative, 1017), []byte("after-restart"))); err != nil {
		t.Fatal(err)
	}
	want = append(want, "after-restart")
	w2.Close()
	dctx, cancel = context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := u2.Drain(dctx); err != nil {
		t.Fatalf("drain after restart: %v", err)
	}
	u2.Stop()

	if files := dirFiles(t, dir); len(files) != 0 {
		t.Fatalf("local WAL not empty after upload: %v", files)
	}
	if local.Load() != 0 {
		t.Fatalf("LocalBytes %d after everything was uploaded", local.Load())
	}
	if got := uploaded.Load() - uploadedBefore; got != 3 {
		t.Fatalf("second process uploaded %d segments, want 3", got)
	}

	// Every acknowledged entry is in the store exactly once; the re-uploaded
	// segment overwrote its own key instead of creating a second object.
	objs, err = base.List(ctx, "wal/")
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	names := map[string]bool{}
	for _, o := range objs {
		base := filepath.Base(o.Key)
		if names[base] {
			t.Fatalf("segment %s uploaded under two keys", base)
		}
		names[base] = true
		if !strings.HasPrefix(base, "ing-01-") || !strings.HasSuffix(base, ".wal.zst") || len(base) != len("ing-01-000000000000.wal.zst") {
			t.Fatalf("bad object name %s", o.Key)
		}
	}
	for _, o := range objs {
		for _, e := range decodeObject(t, store.Store, o.Key) {
			count[string(e.Body)]++
		}
	}
	for _, b := range want {
		if count[b] != 1 {
			t.Fatalf("entry %.20q found %d times in the store", b, count[b])
		}
	}
	if len(count) != len(want) {
		t.Fatalf("store has %d distinct entries, want %d", len(count), len(want))
	}
	// Sequence numbers never went backwards or repeated across the restart.
	if res.MaxSeq < 1000 || next != res.MaxSeq+1 {
		t.Fatalf("restart seq: max=%d next=%d", res.MaxSeq, next)
	}
	// The dedupe seed saw the entries that were still local at the crash.
	if !containsAll(seeded, "in-active-segment") {
		t.Fatalf("recovery did not visit the crashed segment: %v", len(seeded))
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// Appends racing with Close: every Append that reported success must be in a
// sealed segment; every other one must have failed with ErrClosed.
func TestCloseRace(t *testing.T) {
	for round := 0; round < 20; round++ {
		dir := t.TempDir()
		var sealed sealedCollector
		w := NewWriter(Options{Dir: dir, NodeID: "n", StartSeq: 1, OnSealed: sealed.add, SegmentBytes: 8 << 10})
		var ok sync.Map
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; ; i++ {
					id := fmt.Sprintf("g%d-%d", g, i)
					err := w.Append(mustEntry(t, env(1, 1), []byte(id)))
					if errors.Is(err, ErrClosed) {
						return
					}
					if err != nil {
						t.Errorf("append: %v", err)
						return
					}
					ok.Store(id, true)
				}
			}(g)
		}
		time.Sleep(time.Duration(round) * time.Millisecond)
		w.Close()
		wg.Wait()
		found := map[string]int{}
		for _, s := range sealed.list() {
			entries, torn := readSegmentFile(t, s.Path)
			if torn {
				t.Fatalf("torn sealed segment %s", s.Name)
			}
			for _, e := range entries {
				found[string(e.Body)]++
			}
		}
		acked := 0
		ok.Range(func(k, _ any) bool {
			acked++
			if found[k.(string)] != 1 {
				t.Errorf("acknowledged entry %s found %d times", k, found[k.(string)])
			}
			return true
		})
		if len(found) != acked {
			t.Errorf("round %d: %d entries on disk, %d acknowledged", round, len(found), acked)
		}
	}
}

func TestUploaderStopKeepsSegments(t *testing.T) {
	dir := t.TempDir()
	base, _ := objstore.NewFileStore(t.TempDir())
	store := &failingStore{Store: base, failures: 1 << 30}
	u := NewUploader(UploaderOptions{Store: store, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	u.Start()
	w := NewWriter(Options{Dir: dir, NodeID: "n", StartSeq: 1, OnSealed: u.Enqueue})
	if err := w.Append(mustEntry(t, env(1, 1), []byte("keep me"))); err != nil {
		t.Fatal(err)
	}
	w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := u.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain during outage: %v", err)
	}
	if n, oldest := u.Pending(); n != 1 || oldest.IsZero() {
		t.Fatalf("pending %d %v", n, oldest)
	}
	u.Stop()
	if n, _ := u.Pending(); n != 1 {
		t.Fatalf("pending after stop: %d", n)
	}
	files := dirFiles(t, dir)
	if len(files) != 1 || !strings.HasSuffix(files[0], ".sealed") {
		t.Fatalf("segment lost during outage shutdown: %v", files)
	}
	store.mu.Lock()
	puts := store.puts
	store.mu.Unlock()
	if puts < 2 {
		t.Fatalf("uploader did not retry (puts=%d)", puts)
	}
}

// FuzzReader: arbitrary bytes never panic the reader, never make it allocate
// beyond the entry limit, and the entries it yields re-encode to exactly the
// prefix it reports as valid.
func FuzzReader(f *testing.F) {
	seg := []byte(SegMagic)
	seg, _ = AppendEntry(seg, env(FormatNative, 1017), []byte("LA1\x00body"))
	seg, _ = AppendEntry(seg, env(FormatPostHog, 7), []byte(`{"api_key":"pk","batch":[]}`))
	seg, _ = AppendEntry(seg, env(FormatReplay, 7), nil)
	f.Add(seg)
	f.Add(seg[:len(seg)-3])
	f.Add([]byte(SegMagic))
	f.Add([]byte{})
	f.Add(append(append([]byte(nil), seg...), 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0))
	if raw, err := os.ReadFile("../../spec/vectors/wire.json"); err == nil {
		// Real wire bodies as entry payloads.
		for _, h := range strings.Split(string(raw), `"hex": "`)[1:] {
			if i := strings.IndexByte(h, '"'); i > 0 && i < 4096 {
				if body, err := hex.DecodeString(h[:i]); err == nil {
					s, _ := AppendEntry([]byte(SegMagic), env(FormatNative, 1), body)
					f.Add(s)
				}
			}
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		rd, err := NewReader(bytes.NewReader(data))
		if err != nil {
			if !errors.Is(err, ErrBadMagic) {
				t.Fatalf("unexpected error %v", err)
			}
			if len(data) >= 4 && string(data[:4]) == SegMagic {
				t.Fatal("valid magic rejected")
			}
			return
		}
		rebuilt := []byte(SegMagic)
		for {
			e, body, err := rd.Next()
			if err == io.EOF || errors.Is(err, ErrTornTail) {
				if (err == io.EOF) != (rd.Offset() == int64(len(data))) {
					t.Fatalf("end state %v with offset %d of %d", err, rd.Offset(), len(data))
				}
				// The end state is sticky.
				if _, _, err2 := rd.Next(); err2 != err {
					t.Fatalf("Next after end: %v then %v", err, err2)
				}
				break
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if len(body) > MaxBodyLen {
				t.Fatalf("body of %d bytes exceeds limit", len(body))
			}
			var aerr error
			if rebuilt, aerr = AppendEntry(rebuilt, e, body); aerr != nil {
				t.Fatal(aerr)
			}
		}
		if !bytes.Equal(rebuilt, data[:rd.Offset()]) {
			t.Fatalf("re-encoded entries differ from the valid prefix (offset %d)", rd.Offset())
		}
	})
}

func BenchmarkAppendParallel(b *testing.B) {
	dir := b.TempDir()
	var sealed sealedCollector
	w := NewWriter(Options{Dir: dir, NodeID: "n", StartSeq: 1, OnSealed: func(s Segment) { os.Remove(s.Path); sealed.add(Segment{}) },
		FsyncInterval: 2 * time.Millisecond})
	defer w.Close()
	body := bytes.Repeat([]byte("e"), 2000)
	b.SetBytes(int64(len(body)))
	b.SetParallelism(64)
	b.RunParallel(func(pb *testing.PB) {
		entry := mustEntry(b, env(1, 1), body)
		for pb.Next() {
			if err := w.Append(entry); err != nil {
				b.Fatal(err)
			}
		}
	})
}
