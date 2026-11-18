package wal

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned by Append after Close or Abort.
var ErrClosed = errors.New("wal: writer closed")

// File is the part of *os.File the writer uses; tests inject wrappers to
// observe or fail writes and fsyncs.
type File interface {
	io.Writer
	Sync() error
	Truncate(size int64) error
	Close() error
}

// Options configures a Writer.
type Options struct {
	Dir    string
	NodeID string
	// SegmentBytes / SegmentAge: rotate when the active segment reaches this
	// size or this age, whichever comes first (design §6.4: 64 MB / 10 s).
	SegmentBytes int64
	SegmentAge   time.Duration
	// FsyncInterval / FsyncBytes: group commit; write+fsync happens this long
	// after the first pending entry arrived, or as soon as this many bytes
	// are pending (design §6.3: 2 ms / 1 MB). Interval 0 = no added delay.
	FsyncInterval time.Duration
	FsyncBytes    int
	// StartSeq is the sequence number of the first segment (see StartSeq).
	StartSeq uint64
	// OnSealed is called on the writer goroutine for every closed segment; it
	// must not block.
	OnSealed func(Segment)
	// OnFsync reports the duration of every successful fsync.
	OnFsync func(d time.Duration, entries int)
	// LocalBytes, when set, is increased by every byte that becomes durable
	// in a local segment.
	LocalBytes *atomic.Int64
	Logger     *slog.Logger

	// OpenFile creates a new segment file (default: O_CREATE|O_EXCL).
	OpenFile func(path string) (File, error)
	// Now is the clock (default time.Now).
	Now func() time.Time
}

type request struct {
	entry []byte
	done  chan error
}

var reqPool = sync.Pool{New: func() any { return &request{done: make(chan error, 1)} }}

// Writer appends entries to the local WAL from a single goroutine with group
// fsync. Append returns only after the entry has been fsynced.
type Writer struct {
	opt Options
	in  chan *request

	mu      sync.RWMutex // guards closed and the close of in
	closed  bool
	aborted atomic.Bool
	done    chan struct{}

	// Owned by the writer goroutine.
	f         File
	name      string
	size      int64 // durable bytes in the active segment
	dirSynced bool
	nextSeq   uint64
	buf       []byte
	pending   []*request
	rotTimer  *time.Timer
}

// NewWriter starts the writer goroutine. No file is created until the first
// entry arrives, so an idle node produces no segments.
func NewWriter(opt Options) *Writer {
	if opt.SegmentBytes <= 0 {
		opt.SegmentBytes = 64 << 20
	}
	if opt.SegmentAge <= 0 {
		opt.SegmentAge = 10 * time.Second
	}
	if opt.FsyncBytes <= 0 {
		opt.FsyncBytes = 1 << 20
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.OpenFile == nil {
		opt.OpenFile = func(path string) (File, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o644)
		}
	}
	w := &Writer{
		opt:      opt,
		in:       make(chan *request, 1024),
		done:     make(chan struct{}),
		nextSeq:  opt.StartSeq,
		rotTimer: time.NewTimer(time.Hour),
	}
	w.rotTimer.Stop()
	go w.run()
	return w
}

// Append writes one complete entry (as built by FinishEntry/AppendEntry) and
// blocks until it is durable. A nil error means the entry was written and
// fsynced; any error means it must be treated as not written (the caller
// answers 503 and the client retries). entry must not be modified until
// Append returns.
func (w *Writer) Append(entry []byte) error {
	if len(entry) < EntryPrefixLen || len(entry) > EntryHeaderLen+MaxEntryLen {
		return ErrBodyTooLarge
	}
	r := reqPool.Get().(*request)
	r.entry = entry
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		r.entry = nil
		reqPool.Put(r)
		return ErrClosed
	}
	w.in <- r
	w.mu.RUnlock()
	err := <-r.done
	r.entry = nil
	reqPool.Put(r)
	return err
}

// Close flushes pending entries, seals the active segment (handing it to
// OnSealed) and stops the writer. Appends racing with Close either complete
// durably or fail with ErrClosed.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.in)
	}
	w.mu.Unlock()
	<-w.done
}

// Abort stops the writer the way a crash would: entries not yet fsynced
// fail, and the active segment is left on disk unsealed for Recover.
func (w *Writer) Abort() {
	w.aborted.Store(true)
	w.Close()
}

func (w *Writer) run() {
	defer close(w.done)
	flushTimer := time.NewTimer(time.Hour)
	flushTimer.Stop()
	var flushC <-chan time.Time
	stop := func() {
		w.flush()
		if w.aborted.Load() {
			if w.f != nil {
				w.f.Close()
				w.f = nil
			}
			return
		}
		w.seal()
	}
	for {
		select {
		case r, ok := <-w.in:
			if !ok {
				stop()
				return
			}
			w.add(r)
		drain: // take whatever else is already queued into the same group
			for len(w.buf) < w.opt.FsyncBytes {
				select {
				case r, ok := <-w.in:
					if !ok {
						stop()
						return
					}
					w.add(r)
				default:
					break drain
				}
			}
			if len(w.buf) >= w.opt.FsyncBytes || w.opt.FsyncInterval <= 0 {
				if flushC != nil {
					flushTimer.Stop()
					flushC = nil
				}
				w.flush()
			} else if flushC == nil {
				flushTimer.Reset(w.opt.FsyncInterval)
				flushC = flushTimer.C
			}
		case <-flushC:
			flushC = nil
			w.flush()
		case <-w.rotTimer.C:
			if flushC != nil {
				flushTimer.Stop()
				flushC = nil
			}
			w.flush()
			w.seal()
		}
	}
}

func (w *Writer) add(r *request) {
	w.buf = append(w.buf, r.entry...)
	w.pending = append(w.pending, r)
}

// flush writes and fsyncs the pending group, then acknowledges every request
// in it. Nothing is acknowledged before fsync has returned successfully.
func (w *Writer) flush() {
	if len(w.pending) == 0 {
		return
	}
	var err error
	if w.aborted.Load() {
		err = ErrClosed
	} else if err = w.writeAndSync(); err != nil {
		// Cut the unacknowledged tail and retire the segment before the
		// failure is reported.
		w.opt.Logger.Error("wal write failed; abandoning segment", "segment", w.name, "err", err)
		w.abandon()
	}
	for i, r := range w.pending {
		r.done <- err
		w.pending[i] = nil
	}
	w.pending = w.pending[:0]
	if cap(w.buf) > 2*w.opt.FsyncBytes+2*MaxEntryLen {
		w.buf = nil
	}
	w.buf = w.buf[:0]
	if err == nil && w.size >= w.opt.SegmentBytes {
		w.seal()
	}
}

func (w *Writer) writeAndSync() error {
	if w.f == nil {
		if err := w.open(); err != nil {
			return err
		}
	}
	if _, err := w.f.Write(w.buf); err != nil {
		return err
	}
	start := w.opt.Now()
	if err := w.f.Sync(); err != nil {
		return err
	}
	if !w.dirSynced {
		// Make the new file's directory entry (and the sequence high-water
		// mark) durable before the first acknowledgement out of it.
		if err := syncDir(w.opt.Dir); err != nil {
			return err
		}
		w.dirSynced = true
	}
	if w.opt.OnFsync != nil {
		w.opt.OnFsync(w.opt.Now().Sub(start), len(w.pending))
	}
	w.size += int64(len(w.buf))
	if w.opt.LocalBytes != nil {
		w.opt.LocalBytes.Add(int64(len(w.buf)))
	}
	return nil
}

func (w *Writer) open() error {
	seq := w.nextSeq
	if err := writeHWM(w.opt.Dir, seq); err != nil {
		return err
	}
	w.nextSeq++ // never reuse a number, even if creating the file fails
	name := SegmentName(w.opt.NodeID, seq)
	path := activePath(w.opt.Dir, name)
	f, err := w.opt.OpenFile(path)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, SegMagic); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	w.f, w.name, w.size, w.dirSynced = f, name, int64(len(SegMagic)), false
	if w.opt.LocalBytes != nil {
		w.opt.LocalBytes.Add(w.size)
	}
	w.rotTimer.Reset(w.opt.SegmentAge)
	return nil
}

// seal closes the active segment and hands it to the uploader.
func (w *Writer) seal() {
	if w.f == nil {
		return
	}
	w.rotTimer.Stop()
	f, name, size := w.f, w.name, w.size
	w.f = nil
	if err := f.Close(); err != nil {
		w.opt.Logger.Error("wal close failed", "segment", name, "err", err)
	}
	active := activePath(w.opt.Dir, name)
	if size <= int64(len(SegMagic)) { // no durable entry: nothing to ship
		os.Remove(active)
		if w.opt.LocalBytes != nil {
			w.opt.LocalBytes.Add(-size)
		}
		return
	}
	seg := Segment{Name: name, Size: size, ClosedAt: time.UnixMilli(w.opt.Now().UnixMilli())}
	seg.Path = sealedPath(w.opt.Dir, name, seg.ClosedAt)
	if err := os.Rename(active, seg.Path); err != nil {
		// Still ship it; only key stability across a crash is lost.
		w.opt.Logger.Error("wal seal rename failed", "segment", name, "err", err)
		seg.Path = active
	} else if err := syncDir(w.opt.Dir); err != nil {
		w.opt.Logger.Error("wal dir sync failed", "segment", name, "err", err)
	}
	if w.opt.OnSealed != nil {
		w.opt.OnSealed(seg)
	}
}

// abandon is called after a failed write or fsync. Bytes past the last
// successful fsync were never acknowledged; they are cut off (best effort:
// if the cut fails, a partial entry is a torn tail the reader stops at, and a
// complete one is a harmless duplicate of what the client will retry). The
// durable prefix is sealed and shipped, and the next entry starts a new
// segment, so an fsync is never retried on a file that already failed one.
func (w *Writer) abandon() {
	if w.f == nil {
		return
	}
	if err := w.f.Truncate(w.size); err != nil {
		w.opt.Logger.Error("wal truncate after failed write", "segment", w.name, "err", err)
	}
	w.seal()
}
