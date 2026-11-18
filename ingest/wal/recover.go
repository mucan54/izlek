package wal

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Recovered is the result of scanning the WAL directory at startup.
type Recovered struct {
	Segments       []Segment // sealed segments to upload, in sequence order
	MaxSeq         uint64    // highest sequence number seen (files and high-water mark)
	Bytes          int64     // total size of Segments
	TruncatedTails int       // segments whose torn tail was cut off
	TruncatedBytes int64
}

// Recover prepares every segment left in dir by a previous process for
// upload (design §6.4):
//
//   - an unsealed "*.wal" file (the segment that was active when the process
//     died) is scanned; a torn tail (incomplete or CRC-failing last entries,
//     which were never acknowledged) is truncated, then the file is sealed;
//   - an already sealed file is taken as is.
//
// visit, when non-nil, is called for every valid entry of every segment
// (the body is only valid during the call); ingestion uses it to re-seed
// its batch_id dedupe cache so that a client retrying a batch whose
// acknowledgement was lost in the crash is not written twice.
func Recover(dir string, now time.Time, visit func(Envelope, []byte), log *slog.Logger) (Recovered, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var res Recovered
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return res, err
	}
	res.MaxSeq = readHWM(dir)
	type item struct {
		seg Segment
		seq uint64
	}
	var items []item
	for _, e := range ents {
		name, seq, closedAt, sealed, ok := parseLocal(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		res.MaxSeq = max(res.MaxSeq, seq)
		path := filepath.Join(dir, e.Name())
		if sealed {
			size, err := scanSealed(path, visit)
			if err != nil {
				log.Error("wal recover: cannot read sealed segment; will still try to upload it", "file", e.Name(), "err", err)
			}
			items = append(items, item{Segment{Path: path, Name: name, Size: size, ClosedAt: closedAt}, seq})
			continue
		}
		size, cut, err := recoverActive(path, visit)
		if err != nil {
			// Leave the file alone: it is retried on the next start and stays
			// visible to the operator. Never delete data we could not read.
			log.Error("wal recover: skipping unreadable segment", "file", e.Name(), "err", err)
			continue
		}
		if cut > 0 {
			res.TruncatedTails++
			res.TruncatedBytes += cut
			log.Warn("wal recover: truncated torn tail", "file", e.Name(), "bytes", cut)
		}
		if size == 0 { // no valid entry: nothing was ever acknowledged from it
			if err := os.Remove(path); err != nil {
				log.Error("wal recover: remove empty segment", "file", e.Name(), "err", err)
			}
			continue
		}
		seg := Segment{Name: name, Size: size, ClosedAt: time.UnixMilli(now.UnixMilli())}
		seg.Path = sealedPath(dir, name, seg.ClosedAt)
		if err := os.Rename(path, seg.Path); err != nil {
			log.Error("wal recover: seal rename failed", "file", e.Name(), "err", err)
			seg.Path = path
		}
		items = append(items, item{seg, seq})
	}
	if err := syncDir(dir); err != nil {
		return res, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].seq != items[j].seq {
			return items[i].seq < items[j].seq
		}
		return items[i].seg.Name < items[j].seg.Name
	})
	for _, it := range items {
		res.Segments = append(res.Segments, it.seg)
		res.Bytes += it.seg.Size
	}
	return res, nil
}

// recoverActive validates an unsealed segment, truncating it to its last
// valid entry. It returns the resulting size (0 if the file holds no valid
// entry) and the number of bytes cut.
func recoverActive(path string, visit func(Envelope, []byte)) (size, cut int64, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	rd, err := NewReader(f)
	if errors.Is(err, ErrBadMagic) {
		if fi.Size() <= int64(len(SegMagic)) {
			return 0, 0, nil // died while creating the file
		}
		return 0, 0, err // not ours or damaged at the head: keep for inspection
	}
	if err != nil {
		return 0, 0, err
	}
	entries := 0
	for {
		env, body, err := rd.Next()
		if err == io.EOF || errors.Is(err, ErrTornTail) {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		entries++
		if visit != nil {
			visit(env, body)
		}
	}
	if entries == 0 {
		return 0, 0, nil
	}
	valid := rd.Offset()
	if valid < fi.Size() {
		if err := f.Truncate(valid); err != nil {
			return 0, 0, err
		}
		if err := f.Sync(); err != nil {
			return 0, 0, err
		}
	}
	return valid, fi.Size() - valid, nil
}

func scanSealed(path string, visit func(Envelope, []byte)) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if visit == nil {
		return fi.Size(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fi.Size(), err
	}
	defer f.Close()
	rd, err := NewReader(f)
	if err != nil {
		return fi.Size(), err
	}
	for {
		env, body, err := rd.Next()
		if err == io.EOF || errors.Is(err, ErrTornTail) {
			return fi.Size(), nil
		}
		if err != nil {
			return fi.Size(), err
		}
		visit(env, body)
	}
}
