package wal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/mucan54/izlek/golib/objstore"
)

// UploaderOptions configures an Uploader.
type UploaderOptions struct {
	Store   objstore.Store
	Workers int // concurrent uploads (default 2)
	// LocalBytes, when set, is decreased by the size of every segment whose
	// local file has been deleted after a successful upload.
	LocalBytes *atomic.Int64
	OnUploaded func(seg Segment, compressedBytes int64)
	OnError    func(seg Segment, err error)
	Logger     *slog.Logger
	// Retry backoff bounds (defaults 500 ms .. 30 s, doubled per failure,
	// with jitter). Retries never give up.
	MinBackoff, MaxBackoff time.Duration
	// AttemptTimeout bounds one PUT (default 5 min).
	AttemptTimeout time.Duration
}

// Uploader compresses sealed segments (zstd level 3) and uploads them with
// an unconditional PUT to their spec/wal.md §4 key. The local file is
// deleted only after the PUT succeeded. A failed upload is retried forever
// with exponential backoff; the same key always carries the same content, so
// re-uploading is safe.
type Uploader struct {
	opt    UploaderOptions
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []Segment
	inflight map[string]Segment
	stopped  bool
}

// NewUploader returns an Uploader; call Start to launch its workers.
func NewUploader(opt UploaderOptions) *Uploader {
	if opt.Workers <= 0 {
		opt.Workers = 2
	}
	if opt.MinBackoff <= 0 {
		opt.MinBackoff = 500 * time.Millisecond
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = 30 * time.Second
	}
	if opt.AttemptTimeout <= 0 {
		opt.AttemptTimeout = 5 * time.Minute
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	u := &Uploader{opt: opt, inflight: map[string]Segment{}}
	u.cond = sync.NewCond(&u.mu)
	u.ctx, u.cancel = context.WithCancel(context.Background())
	return u
}

// Start launches the upload workers.
func (u *Uploader) Start() {
	for range u.opt.Workers {
		u.wg.Add(1)
		go u.worker()
	}
}

// Enqueue schedules a sealed segment for upload. It never blocks.
func (u *Uploader) Enqueue(seg Segment) {
	u.mu.Lock()
	u.queue = append(u.queue, seg)
	u.mu.Unlock()
	u.cond.Signal()
}

// Pending returns the number of segments not yet uploaded and the closing
// time of the oldest one (zero time if none).
func (u *Uploader) Pending() (n int, oldest time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	consider := func(s Segment) {
		n++
		if oldest.IsZero() || s.ClosedAt.Before(oldest) {
			oldest = s.ClosedAt
		}
	}
	for _, s := range u.queue {
		consider(s)
	}
	for _, s := range u.inflight {
		consider(s)
	}
	return n, oldest
}

// Drain blocks until every enqueued segment has been uploaded, or ctx ends.
func (u *Uploader) Drain(ctx context.Context) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if n, _ := u.Pending(); n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Stop cancels in-flight attempts and stops the workers. Segments not yet
// uploaded stay on local disk and are picked up by Recover on next start.
func (u *Uploader) Stop() {
	u.mu.Lock()
	u.stopped = true
	u.mu.Unlock()
	u.cancel()
	u.cond.Broadcast()
	u.wg.Wait()
}

func (u *Uploader) worker() {
	defer u.wg.Done()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		panic(err) // static options; cannot fail
	}
	defer enc.Close()
	var buf bytes.Buffer
	for {
		u.mu.Lock()
		for len(u.queue) == 0 && !u.stopped {
			u.cond.Wait()
		}
		if u.stopped {
			u.mu.Unlock()
			return
		}
		seg := u.queue[0]
		u.queue = u.queue[1:]
		u.inflight[seg.Name] = seg
		u.mu.Unlock()

		done := u.uploadWithRetry(seg, enc, &buf)

		u.mu.Lock()
		delete(u.inflight, seg.Name)
		if !done { // stopped mid-retry: keep it accounted as pending
			u.queue = append([]Segment{seg}, u.queue...)
		}
		u.mu.Unlock()
		if buf.Cap() > 96<<20 {
			buf = bytes.Buffer{}
		}
	}
}

func (u *Uploader) uploadWithRetry(seg Segment, enc *zstd.Encoder, buf *bytes.Buffer) bool {
	backoff := u.opt.MinBackoff
	for {
		err := u.uploadOnce(seg, enc, buf)
		if err == nil {
			return true
		}
		if u.ctx.Err() != nil {
			return false
		}
		if errors.Is(err, fs.ErrNotExist) {
			// The local file vanished (operator action); nothing left to ship.
			u.opt.Logger.Error("wal upload: local segment disappeared", "segment", seg.Name, "err", err)
			if u.opt.LocalBytes != nil {
				u.opt.LocalBytes.Add(-seg.Size)
			}
			return true
		}
		if u.opt.OnError != nil {
			u.opt.OnError(seg, err)
		}
		u.opt.Logger.Warn("wal upload failed; will retry", "segment", seg.Name, "key", seg.Key(),
			"retry_in_ms", backoff.Milliseconds(), "err", err)
		sleep := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
		select {
		case <-u.ctx.Done():
			return false
		case <-time.After(sleep):
		}
		backoff = min(backoff*2, u.opt.MaxBackoff)
	}
}

func (u *Uploader) uploadOnce(seg Segment, enc *zstd.Encoder, buf *bytes.Buffer) error {
	f, err := os.Open(seg.Path)
	if err != nil {
		return err
	}
	buf.Reset()
	enc.Reset(buf)
	_, err = io.Copy(enc, f)
	f.Close()
	if cerr := enc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(u.ctx, u.opt.AttemptTimeout)
	defer cancel()
	if _, err := u.opt.Store.Put(ctx, seg.Key(), buf.Bytes()); err != nil {
		return err
	}
	// The object is durable in the store; only now may the local copy go.
	if err := os.Remove(seg.Path); err != nil {
		// Not fatal: the file is re-uploaded (same key, same content) after
		// the next restart.
		u.opt.Logger.Error("wal upload: cannot remove local segment", "segment", seg.Name, "err", err)
	} else if u.opt.LocalBytes != nil {
		u.opt.LocalBytes.Add(-seg.Size)
	}
	if u.opt.OnUploaded != nil {
		u.opt.OnUploaded(seg, int64(buf.Len()))
	}
	return nil
}
