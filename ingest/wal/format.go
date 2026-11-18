// Package wal implements the ingestion write-ahead log: the LAW1 segment
// format (spec/wal.md), a single-goroutine group-commit writer, crash
// recovery of local segments and the uploader that ships sealed segments to
// object storage.
package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Segment format constants (spec/wal.md §1).
const (
	SegMagic       = "LAW1"
	EntryHeaderLen = 8  // u32le(len) u32le(crc32c)
	EnvelopeLen    = 16 // format, project_id, recv_ts_ms, country, flags
	// EntryPrefixLen is the number of bytes that precede Body in an entry.
	// Handlers reserve this much at the front of their body buffer so the
	// entry can be completed in place without copying the body.
	EntryPrefixLen = EntryHeaderLen + EnvelopeLen
	// MaxBodyLen is the largest Body an entry may carry.
	MaxBodyLen  = 1<<20 + 4096
	MinEntryLen = EnvelopeLen
	MaxEntryLen = EnvelopeLen + MaxBodyLen
)

// Entry formats.
const (
	FormatNative  = 1 // LA1 batch
	FormatPostHog = 2 // PostHog JSON
	FormatReplay  = 3 // replay index record
)

// FlagWeb is envelope flags bit0: the request carried an Origin header.
const FlagWeb = 1

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Envelope is the fixed 16-byte entry envelope.
type Envelope struct {
	Format    uint8
	ProjectID uint32
	RecvTSMs  uint64
	Country   [2]byte
	Flags     uint8
}

// ErrBodyTooLarge is returned when a body exceeds MaxBodyLen.
var ErrBodyTooLarge = errors.New("wal: entry body too large")

// FinishEntry completes an entry in place. buf must be EntryPrefixLen bytes
// of scratch space followed by the body; on return buf is the full entry
// (length, CRC-32C, envelope, body) ready for Writer.Append.
func FinishEntry(buf []byte, env Envelope) error {
	if len(buf) < EntryPrefixLen || len(buf)-EntryPrefixLen > MaxBodyLen {
		return ErrBodyTooLarge
	}
	e := buf[EntryHeaderLen:]
	e[0] = env.Format
	binary.LittleEndian.PutUint32(e[1:5], env.ProjectID)
	binary.LittleEndian.PutUint64(e[5:13], env.RecvTSMs)
	e[13], e[14] = env.Country[0], env.Country[1]
	e[15] = env.Flags
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(e)))
	binary.LittleEndian.PutUint32(buf[4:8], crc32.Checksum(e, castagnoli))
	return nil
}

// AppendEntry appends a complete entry for body to dst.
func AppendEntry(dst []byte, env Envelope, body []byte) ([]byte, error) {
	if len(body) > MaxBodyLen {
		return dst, ErrBodyTooLarge
	}
	start := len(dst)
	dst = append(dst, make([]byte, EntryPrefixLen)...)
	dst = append(dst, body...)
	if err := FinishEntry(dst[start:], env); err != nil {
		return dst[:start], err
	}
	return dst, nil
}

func parseEnvelope(e []byte) Envelope {
	return Envelope{
		Format:    e[0],
		ProjectID: binary.LittleEndian.Uint32(e[1:5]),
		RecvTSMs:  binary.LittleEndian.Uint64(e[5:13]),
		Country:   [2]byte{e[13], e[14]},
		Flags:     e[15],
	}
}

// ErrBadMagic means the data does not start with the LAW1 segment magic.
var ErrBadMagic = errors.New("wal: bad segment magic")

// ErrTornTail is returned by Reader.Next when the remaining bytes are not a
// complete, CRC-valid entry. Per spec/wal.md §1 reading stops there:
// everything before Reader.Offset is valid, everything after is discarded.
var ErrTornTail = errors.New("wal: torn or corrupt tail")

// Reader iterates the entries of one (uncompressed) segment.
type Reader struct {
	br   *bufio.Reader
	off  int64 // offset just past the last valid entry
	buf  []byte
	done error
}

// NewReader checks the segment magic and returns a Reader positioned at the
// first entry.
func NewReader(r io.Reader) (*Reader, error) {
	br := bufio.NewReaderSize(r, 256<<10)
	var magic [len(SegMagic)]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, ErrBadMagic
		}
		return nil, err
	}
	if string(magic[:]) != SegMagic {
		return nil, ErrBadMagic
	}
	return &Reader{br: br, off: int64(len(SegMagic))}, nil
}

// Offset is the byte offset just past the last entry Next returned
// successfully; a local segment with a torn tail is truncated to it.
func (r *Reader) Offset() int64 { return r.off }

// Next returns the next entry. body is only valid until the following call.
// It returns io.EOF at a clean end of segment, ErrTornTail when the rest of
// the data is not a valid entry, or the underlying read error.
func (r *Reader) Next() (env Envelope, body []byte, err error) {
	if r.done != nil {
		return env, nil, r.done
	}
	var hdr [EntryHeaderLen]byte
	if _, err := io.ReadFull(r.br, hdr[:]); err != nil {
		switch err {
		case io.EOF:
			r.done = io.EOF
		case io.ErrUnexpectedEOF:
			r.done = ErrTornTail
		default:
			return env, nil, err
		}
		return env, nil, r.done
	}
	n := binary.LittleEndian.Uint32(hdr[0:4])
	if n < MinEntryLen || n > MaxEntryLen {
		r.done = ErrTornTail
		return env, nil, r.done
	}
	if cap(r.buf) < int(n) {
		r.buf = make([]byte, n, max(int(n), 64<<10))
	}
	r.buf = r.buf[:n]
	if _, err := io.ReadFull(r.br, r.buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			r.done = ErrTornTail
			return env, nil, r.done
		}
		return env, nil, err
	}
	if crc32.Checksum(r.buf, castagnoli) != binary.LittleEndian.Uint32(hdr[4:8]) {
		r.done = ErrTornTail
		return env, nil, r.done
	}
	r.off += EntryHeaderLen + int64(n)
	return parseEnvelope(r.buf), r.buf[EnvelopeLen:], nil
}

// Entry is a decoded entry with its own copy of the body.
type Entry struct {
	Envelope
	Body []byte
}

// ReadAll decodes every valid entry of a segment. torn reports whether the
// data ended in a torn/corrupt tail (which is not an error).
func ReadAll(r io.Reader) (entries []Entry, torn bool, err error) {
	rd, err := NewReader(r)
	if err != nil {
		return nil, false, err
	}
	for {
		env, body, err := rd.Next()
		switch err {
		case nil:
			entries = append(entries, Entry{Envelope: env, Body: append([]byte(nil), body...)})
		case io.EOF:
			return entries, false, nil
		case ErrTornTail:
			return entries, true, nil
		default:
			return entries, false, fmt.Errorf("wal: read: %w", err)
		}
	}
}
