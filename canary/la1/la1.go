// Package la1 is a minimal encoder for the LA1 wire format (spec/wire.md),
// just enough for the canary to speak the native protocol. It is verified
// against spec/vectors/wire.json.
package la1

import (
	"encoding/binary"
	"math"
)

// Magic is the 4-byte batch prefix.
var Magic = []byte{'L', 'A', '1', 0}

// Kind of a property value.
type Kind uint8

const (
	Null Kind = iota
	Bool
	Int
	Float
	String
	JSON
)

// Prop is one key/value pair of a PropList.
type Prop struct {
	Key  string
	Kind Kind
	B    bool
	I    int64
	F    float64
	S    string // String and JSON
}

func S(k, v string) Prop         { return Prop{Key: k, Kind: String, S: v} }
func I(k string, v int64) Prop   { return Prop{Key: k, Kind: Int, I: v} }
func F(k string, v float64) Prop { return Prop{Key: k, Kind: Float, F: v} }
func B(k string, v bool) Prop    { return Prop{Key: k, Kind: Bool, B: v} }

// Header is the batch header (field order fixed by the spec).
type Header struct {
	BatchID   [16]byte
	SentAt    uint64
	SDK       string
	AnonID    string
	UserID    string
	SessionID [16]byte
	Context   []Prop
}

// Record is one event/identify/set_person/reset record.
type Record struct {
	Kind  uint8
	UUID  [16]byte
	TS    uint64
	Name  string
	Props []Prop
}

func putVarint(b []byte, n uint64) []byte { return binary.AppendUvarint(b, n) }

func putString(b []byte, s string) []byte {
	b = putVarint(b, uint64(len(s)))
	return append(b, s...)
}

func putProps(b []byte, props []Prop) []byte {
	b = putVarint(b, uint64(len(props)))
	for _, p := range props {
		b = putString(b, p.Key)
		switch p.Kind {
		case Null:
			b = append(b, 0)
		case Bool:
			if p.B {
				b = append(b, 2)
			} else {
				b = append(b, 1)
			}
		case Int:
			b = append(b, 3)
			b = putVarint(b, uint64(p.I<<1)^uint64(p.I>>63))
		case Float:
			b = append(b, 4)
			b = binary.LittleEndian.AppendUint64(b, math.Float64bits(p.F))
		case String:
			b = append(b, 5)
			b = putString(b, p.S)
		case JSON:
			b = append(b, 6)
			b = putString(b, p.S)
		}
	}
	return b
}

func headerBody(h *Header) []byte {
	b := make([]byte, 0, 128)
	b = append(b, h.BatchID[:]...)
	b = putVarint(b, h.SentAt)
	b = putString(b, h.SDK)
	b = putString(b, h.AnonID)
	b = putString(b, h.UserID)
	b = append(b, h.SessionID[:]...)
	return putProps(b, h.Context)
}

func recordBody(r *Record) []byte {
	b := make([]byte, 0, 64)
	b = append(b, r.Kind)
	b = append(b, r.UUID[:]...)
	b = putVarint(b, r.TS)
	b = putString(b, r.Name)
	return putProps(b, r.Props)
}

// Encode serializes a batch.
func Encode(h *Header, recs []Record) []byte {
	out := append(make([]byte, 0, 256), Magic...)
	hb := headerBody(h)
	out = putVarint(out, uint64(len(hb)))
	out = append(out, hb...)
	for i := range recs {
		rb := recordBody(&recs[i])
		out = putVarint(out, uint64(len(rb)))
		out = append(out, rb...)
	}
	return out
}
