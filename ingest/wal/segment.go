package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Local file naming:
//
//	<node>-<seq:012>.wal                    the segment being written
//	<node>-<seq:012>.<close_unix_ms>.sealed closed, waiting for upload
//	seq.hwm                                 highest sequence number ever used
//
// The close time is part of the sealed file name because the object key
// contains the closing hour (spec/wal.md §4): keeping it on disk makes every
// upload attempt of a segment, including one after a crash and restart,
// target the same key.
const (
	activeExt = ".wal"
	sealedExt = ".sealed"
	hwmFile   = "seq.hwm"
)

// Segment is a closed local segment waiting for upload.
type Segment struct {
	Path     string    // local file
	Name     string    // "<node>-<seq:012>"
	Size     int64     // bytes on disk
	ClosedAt time.Time // closing instant (millisecond precision)
}

// SegmentName returns "<node>-<seq:012>".
func SegmentName(node string, seq uint64) string {
	return fmt.Sprintf("%s-%012d", node, seq)
}

// ObjectKey returns the object-store key of a segment (spec/wal.md §4).
func ObjectKey(name string, closedAt time.Time) string {
	t := closedAt.UTC()
	return fmt.Sprintf("wal/dt=%s/hh=%02d/%s.wal.zst", t.Format("2006-01-02"), t.Hour(), name)
}

// Key is the segment's object-store key.
func (s Segment) Key() string { return ObjectKey(s.Name, s.ClosedAt) }

func activePath(dir, name string) string { return filepath.Join(dir, name+activeExt) }

func sealedPath(dir, name string, closedAt time.Time) string {
	return filepath.Join(dir, name+"."+strconv.FormatInt(closedAt.UnixMilli(), 10)+sealedExt)
}

// parseLocal decodes a WAL directory entry name.
func parseLocal(file string) (name string, seq uint64, closedAt time.Time, sealed, ok bool) {
	switch {
	case strings.HasSuffix(file, activeExt):
		name = strings.TrimSuffix(file, activeExt)
	case strings.HasSuffix(file, sealedExt):
		rest := strings.TrimSuffix(file, sealedExt)
		i := strings.LastIndexByte(rest, '.')
		if i < 0 {
			return "", 0, time.Time{}, false, false
		}
		ms, err := strconv.ParseInt(rest[i+1:], 10, 64)
		if err != nil || ms < 0 {
			return "", 0, time.Time{}, false, false
		}
		name, closedAt, sealed = rest[:i], time.UnixMilli(ms), true
	default:
		return "", 0, time.Time{}, false, false
	}
	i := strings.LastIndexByte(name, '-')
	if i <= 0 || len(name)-i-1 != 12 {
		return "", 0, time.Time{}, false, false
	}
	seq, err := strconv.ParseUint(name[i+1:], 10, 64)
	if err != nil {
		return "", 0, time.Time{}, false, false
	}
	return name, seq, closedAt, sealed, true
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeHWM durably records the highest sequence number handed out, so a
// restart with an empty WAL directory and a clock that is behind the
// sequence counter still cannot reuse a number.
func writeHWM(dir string, seq uint64) error {
	tmp := filepath.Join(dir, hwmFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.FormatUint(seq, 10))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, hwmFile))
}

func readHWM(dir string) uint64 {
	raw, err := os.ReadFile(filepath.Join(dir, hwmFile))
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return v
}

// StartSeq implements the sequence start rule of spec/wal.md §4:
// max(highest local seq + 1, unix seconds), where "highest local seq" also
// covers the persisted high-water mark.
func StartSeq(maxLocal uint64, now time.Time) uint64 {
	return max(maxLocal+1, uint64(now.Unix()))
}
