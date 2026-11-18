// Package wire implements the ingestion side of the LA1 wire format
// (spec/wire.md): frame validation for the hot path, plus a small encoder
// used by tests and the load generator.
package wire

import "errors"

// Limits enforced by ingestion (spec/wire.md §3).
const (
	MaxBatchBytes = 1 << 20 // uncompressed and compressed body limit
	MaxRecords    = 500
	MinHeaderLen  = 16
	MaxHeaderLen  = 65536
	MinRecordLen  = 1
	MaxRecordLen  = 65536
	MagicLen      = 4
)

// Frame errors. All of them map to HTTP 400 except ErrTooManyRecords (413).
var (
	ErrMagic          = errors.New("wire: bad magic")
	ErrVarint         = errors.New("wire: bad varint")
	ErrHeaderLen      = errors.New("wire: header length out of range")
	ErrHeaderOverrun  = errors.New("wire: header exceeds body")
	ErrRecordLen      = errors.New("wire: record length out of range")
	ErrRecordOverrun  = errors.New("wire: record exceeds body")
	ErrTooManyRecords = errors.New("wire: too many records")
)

// Frame is the result of a successful frame walk.
type Frame struct {
	Records int
	BatchID [16]byte // HeaderBody[0..16]
}

// Validate walks the batch frame: magic, header length, record lengths
// summing exactly to the body, record count. Record contents are not
// decoded. It is O(records) and does not allocate.
//
// A structurally broken frame wins over the record limit: a batch with more
// than MaxRecords records is only reported as ErrTooManyRecords (-> 413,
// "split and retry") if the whole frame is otherwise well-formed.
func Validate(b []byte) (Frame, error) {
	var f Frame
	if len(b) < MagicLen || b[0] != 'L' || b[1] != 'A' || b[2] != '1' || b[3] != 0 {
		return f, ErrMagic
	}
	pos := MagicLen
	hlen, n := uvarint(b[pos:])
	if n <= 0 {
		return f, ErrVarint
	}
	pos += n
	if hlen < MinHeaderLen || hlen > MaxHeaderLen {
		return f, ErrHeaderLen
	}
	if uint64(len(b)-pos) < hlen {
		return f, ErrHeaderOverrun
	}
	copy(f.BatchID[:], b[pos:pos+16])
	pos += int(hlen)

	for pos < len(b) {
		rlen, n := uvarint(b[pos:])
		if n <= 0 {
			return f, ErrVarint
		}
		pos += n
		if rlen < MinRecordLen || rlen > MaxRecordLen {
			return f, ErrRecordLen
		}
		if uint64(len(b)-pos) < rlen {
			return f, ErrRecordOverrun
		}
		pos += int(rlen)
		f.Records++
	}
	if f.Records > MaxRecords {
		return f, ErrTooManyRecords
	}
	return f, nil
}

// uvarint decodes an unsigned LEB128 value of at most 10 bytes. Non-minimal
// encodings are accepted; a sequence longer than 10 bytes, one overflowing
// 64 bits, or one cut off by the end of b returns n <= 0.
func uvarint(b []byte) (v uint64, n int) {
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		if i == 9 {
			if c > 1 {
				return 0, -1 // overflows u64, or an 11th byte follows
			}
			return v | uint64(c)<<63, 10
		}
		if c < 0x80 {
			return v | uint64(c)<<shift, i + 1
		}
		v |= uint64(c&0x7f) << shift
		shift += 7
	}
	return 0, 0
}
