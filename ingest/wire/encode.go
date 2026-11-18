package wire

import (
	"encoding/binary"
	"math"
)

// Record kinds.
const (
	KindEvent     = 0
	KindIdentify  = 1
	KindSetPerson = 2
	KindReset     = 3
)

// Property value tags.
const (
	TagNull   = 0
	TagFalse  = 1
	TagTrue   = 2
	TagInt    = 3
	TagFloat  = 4
	TagString = 5
	TagJSON   = 6
)

// Prop is one key/value of a PropList. Int is used by TagInt, Float by
// TagFloat, Str by TagString and TagJSON.
type Prop struct {
	Key   string
	Tag   uint8
	Int   int64
	Float float64
	Str   string
}

// Record is a logical record.
type Record struct {
	Kind  uint8
	UUID  [16]byte
	TS    uint64
	Name  string
	Props []Prop
}

// Batch is a logical batch.
type Batch struct {
	BatchID   [16]byte
	SentAt    uint64
	SDK       string
	AnonID    string
	UserID    string
	SessionID [16]byte
	Context   []Prop
	Records   []Record
}

// AppendUvarint appends the minimal LEB128 encoding of v.
func AppendUvarint(dst []byte, v uint64) []byte { return binary.AppendUvarint(dst, v) }

func appendString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func appendProps(dst []byte, props []Prop) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(props)))
	for i := range props {
		p := &props[i]
		dst = appendString(dst, p.Key)
		dst = append(dst, p.Tag)
		switch p.Tag {
		case TagInt:
			dst = binary.AppendUvarint(dst, uint64(p.Int<<1)^uint64(p.Int>>63))
		case TagFloat:
			bits := math.Float64bits(p.Float)
			if p.Float != p.Float {
				bits = 0x7ff8000000000000 // canonical NaN
			}
			dst = binary.LittleEndian.AppendUint64(dst, bits)
		case TagString, TagJSON:
			dst = appendString(dst, p.Str)
		}
	}
	return dst
}

// AppendHeader appends Magic + the framed header of b (no records).
func AppendHeader(dst []byte, b *Batch) []byte {
	dst = append(dst, 'L', 'A', '1', 0)
	var body []byte
	body = append(body, b.BatchID[:]...)
	body = binary.AppendUvarint(body, b.SentAt)
	body = appendString(body, b.SDK)
	body = appendString(body, b.AnonID)
	body = appendString(body, b.UserID)
	body = append(body, b.SessionID[:]...)
	body = appendProps(body, b.Context)
	dst = binary.AppendUvarint(dst, uint64(len(body)))
	return append(dst, body...)
}

// AppendRecord appends one framed record.
func AppendRecord(dst []byte, r *Record) []byte {
	body := make([]byte, 0, 64)
	body = append(body, r.Kind)
	body = append(body, r.UUID[:]...)
	body = binary.AppendUvarint(body, r.TS)
	body = appendString(body, r.Name)
	body = appendProps(body, r.Props)
	dst = binary.AppendUvarint(dst, uint64(len(body)))
	return append(dst, body...)
}

// AppendBatch appends the full encoding of b (shortest varints, as the spec
// requires of encoders).
func AppendBatch(dst []byte, b *Batch) []byte {
	dst = AppendHeader(dst, b)
	for i := range b.Records {
		dst = AppendRecord(dst, &b.Records[i])
	}
	return dst
}
