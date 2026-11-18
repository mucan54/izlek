package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strconv"
	"testing"
)

type vecProp struct {
	K string          `json:"k"`
	T string          `json:"t"`
	V json.RawMessage `json:"v"`
}

type vecBatch struct {
	BatchID   string    `json:"batch_id"`
	SentAt    uint64    `json:"sent_at"`
	SDK       string    `json:"sdk"`
	AnonID    string    `json:"anon_id"`
	UserID    string    `json:"user_id"`
	SessionID string    `json:"session_id"`
	Context   []vecProp `json:"context"`
	Records   []struct {
		Kind  uint8     `json:"kind"`
		UUID  string    `json:"uuid"`
		TS    uint64    `json:"ts"`
		Name  string    `json:"name"`
		Props []vecProp `json:"props"`
	} `json:"records"`
}

// Vectors mirrors spec/vectors/wire.json.
type Vectors struct {
	Valid []struct {
		Name  string   `json:"name"`
		Batch vecBatch `json:"batch"`
		Hex   string   `json:"hex"`
	} `json:"valid"`
	Tolerant []struct {
		Name  string   `json:"name"`
		Hex   string   `json:"hex"`
		Batch vecBatch `json:"batch"`
	} `json:"tolerant"`
	Invalid []struct {
		Name       string `json:"name"`
		Hex        string `json:"hex"`
		FrameOK    bool   `json:"frame_ok"`
		HTTPStatus int    `json:"http_status"`
	} `json:"invalid"`
}

func loadVectors(tb testing.TB) *Vectors {
	tb.Helper()
	raw, err := os.ReadFile("../../spec/vectors/wire.json")
	if err != nil {
		tb.Fatal(err)
	}
	var v Vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		tb.Fatal(err)
	}
	if len(v.Valid) == 0 || len(v.Tolerant) == 0 || len(v.Invalid) == 0 {
		tb.Fatal("wire.json: empty vector set")
	}
	return &v
}

func unhex(tb testing.TB, s string) []byte {
	tb.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func id16(tb testing.TB, s string) (out [16]byte) {
	copy(out[:], unhex(tb, s))
	return
}

func convProps(tb testing.TB, in []vecProp) []Prop {
	out := make([]Prop, 0, len(in))
	for _, p := range in {
		q := Prop{Key: p.K}
		var s string
		switch p.T {
		case "null":
			q.Tag = TagNull
		case "bool":
			q.Tag = TagFalse
			if string(p.V) == "true" {
				q.Tag = TagTrue
			}
		case "int":
			json.Unmarshal(p.V, &s)
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				tb.Fatal(err)
			}
			q.Tag, q.Int = TagInt, n
		case "float":
			json.Unmarshal(p.V, &s)
			q.Tag = TagFloat
			switch s {
			case "NaN":
				q.Float = math.NaN()
			case "Infinity":
				q.Float = math.Inf(1)
			case "-Infinity":
				q.Float = math.Inf(-1)
			default:
				f, err := strconv.ParseFloat(s, 64)
				if err != nil {
					tb.Fatal(err)
				}
				q.Float = f
			}
		case "string":
			json.Unmarshal(p.V, &s)
			q.Tag, q.Str = TagString, s
		case "json":
			json.Unmarshal(p.V, &s)
			q.Tag, q.Str = TagJSON, s
		default:
			tb.Fatalf("unknown prop type %q", p.T)
		}
		out = append(out, q)
	}
	return out
}

func convBatch(tb testing.TB, v *vecBatch) *Batch {
	b := &Batch{BatchID: id16(tb, v.BatchID), SentAt: v.SentAt, SDK: v.SDK, AnonID: v.AnonID, UserID: v.UserID,
		SessionID: id16(tb, v.SessionID), Context: convProps(tb, v.Context)}
	for _, r := range v.Records {
		b.Records = append(b.Records, Record{Kind: r.Kind, UUID: id16(tb, r.UUID), TS: r.TS, Name: r.Name, Props: convProps(tb, r.Props)})
	}
	return b
}

// Status maps a Validate error to the HTTP status of spec/wire.md §3.
func status(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, ErrTooManyRecords):
		return 413
	default:
		return 400
	}
}

func TestVectorsValid(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Valid {
		want := unhex(t, c.Hex)
		f, err := Validate(want)
		if err != nil {
			t.Errorf("valid/%s: frame rejected: %v", c.Name, err)
			continue
		}
		if f.Records != len(c.Batch.Records) {
			t.Errorf("valid/%s: %d records, want %d", c.Name, f.Records, len(c.Batch.Records))
		}
		if hex.EncodeToString(f.BatchID[:]) != c.Batch.BatchID {
			t.Errorf("valid/%s: batch_id %x, want %s", c.Name, f.BatchID, c.Batch.BatchID)
		}
		// The test/loadgen encoder must reproduce the vector byte for byte.
		if got := AppendBatch(nil, convBatch(t, &c.Batch)); !bytes.Equal(got, want) {
			t.Errorf("valid/%s: encoder mismatch\n got  %x\n want %x", c.Name, got, want)
		}
	}
}

func TestVectorsTolerant(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Tolerant {
		f, err := Validate(unhex(t, c.Hex))
		if err != nil {
			t.Errorf("tolerant/%s: frame rejected: %v", c.Name, err)
			continue
		}
		if hex.EncodeToString(f.BatchID[:]) != c.Batch.BatchID {
			t.Errorf("tolerant/%s: batch_id %x, want %s", c.Name, f.BatchID, c.Batch.BatchID)
		}
		// The frame walker counts frames; unknown kinds are skipped later by
		// the full decoder, so the frame count can exceed the logical count.
		if f.Records < len(c.Batch.Records) {
			t.Errorf("tolerant/%s: %d frames < %d logical records", c.Name, f.Records, len(c.Batch.Records))
		}
	}
}

func TestVectorsInvalid(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Invalid {
		_, err := Validate(unhex(t, c.Hex))
		if (err == nil) != c.FrameOK {
			t.Errorf("invalid/%s: frame_ok = %v (err %v), want %v", c.Name, err == nil, err, c.FrameOK)
		}
		if got := status(err); got != c.HTTPStatus {
			t.Errorf("invalid/%s: status %d (err %v), want %d", c.Name, got, err, c.HTTPStatus)
		}
	}
}

func TestUvarint(t *testing.T) {
	cases := []struct {
		in   string
		v    uint64
		n    int
		desc string
	}{
		{"00", 0, 1, "zero"},
		{"7f", 127, 1, "one byte max"},
		{"8001", 128, 2, "two bytes"},
		{"8000", 0, 2, "non-minimal zero"},
		{"ffffffffffffffffff01", math.MaxUint64, 10, "u64 max"},
		{"ffffffffffffffffff02", 0, -1, "overflow"},
		{"80808080808080808080", 0, -1, "11th byte follows"},
		{"80808080808080808000", 0, 10, "non-minimal 10 bytes"},
		{"80", 0, 0, "unterminated"},
		{"", 0, 0, "empty"},
	}
	for _, c := range cases {
		v, n := uvarint(unhex(t, c.in))
		if v != c.v || n != c.n {
			t.Errorf("%s: uvarint(%s) = (%d, %d), want (%d, %d)", c.desc, c.in, v, n, c.v, c.n)
		}
	}
}

func TestRecordLimitBoundary(t *testing.T) {
	b := &Batch{SDK: "t"}
	rec := AppendRecord(nil, &Record{Name: "e"})
	buf := AppendHeader(nil, b)
	for i := 0; i < MaxRecords; i++ {
		buf = append(buf, rec...)
	}
	if f, err := Validate(buf); err != nil || f.Records != MaxRecords {
		t.Fatalf("500 records: %v %d", err, f.Records)
	}
	buf = append(buf, rec...)
	if _, err := Validate(buf); !errors.Is(err, ErrTooManyRecords) {
		t.Fatalf("501 records: %v", err)
	}
	// A broken frame is a 400 even when it also has too many records.
	if _, err := Validate(append(buf, 0x05, 0x01)); !errors.Is(err, ErrRecordOverrun) {
		t.Fatalf("501 records + truncated: %v", err)
	}
	// Header and record size limits.
	big := append([]byte{'L', 'A', '1', 0}, AppendUvarint(nil, MaxHeaderLen)...)
	big = append(big, make([]byte, MaxHeaderLen)...)
	if _, err := Validate(big); err != nil {
		t.Fatalf("max header: %v", err)
	}
	big = append(big, AppendUvarint(nil, MaxRecordLen)...)
	big = append(big, make([]byte, MaxRecordLen)...)
	if f, err := Validate(big); err != nil || f.Records != 1 {
		t.Fatalf("max record: %v", err)
	}
}

func TestValidateDoesNotAllocate(t *testing.T) {
	v := loadVectors(t)
	var bodies [][]byte
	for _, c := range v.Valid {
		bodies = append(bodies, unhex(t, c.Hex))
	}
	for _, c := range v.Invalid {
		bodies = append(bodies, unhex(t, c.Hex))
	}
	allocs := testing.AllocsPerRun(100, func() {
		for _, b := range bodies {
			Validate(b)
		}
	})
	if allocs != 0 {
		t.Fatalf("Validate allocates: %v allocs/run", allocs)
	}
}

func BenchmarkValidate(b *testing.B) {
	batch := &Batch{SDK: "bench/1", AnonID: "0193f1a2-0000-7000-8000-000000000001"}
	buf := AppendHeader(nil, batch)
	rec := AppendRecord(nil, &Record{Name: "page_view", Props: []Prop{{Key: "path", Tag: TagString, Str: "/pricing"}, {Key: "n", Tag: TagInt, Int: 42}}})
	for i := 0; i < 50; i++ {
		buf = append(buf, rec...)
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Validate(buf); err != nil {
			b.Fatal(err)
		}
	}
}

// FuzzValidate checks the frame walker against an independent, deliberately
// naive reference implementation.
func FuzzValidate(f *testing.F) {
	v := loadVectors(f)
	for _, c := range v.Valid {
		f.Add(unhex(f, c.Hex))
	}
	for _, c := range v.Tolerant {
		f.Add(unhex(f, c.Hex))
	}
	for _, c := range v.Invalid {
		f.Add(unhex(f, c.Hex))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Validate(data)
		wantRecords, wantID, wantStatus := refValidate(data)
		if status(err) != wantStatus {
			t.Fatalf("status %d (err %v), reference %d", status(err), err, wantStatus)
		}
		if err == nil {
			if got.Records != wantRecords || got.BatchID != wantID {
				t.Fatalf("frame %+v, reference records=%d id=%x", got, wantRecords, wantID)
			}
			if got.Records > MaxRecords || len(data) < MagicLen+1+MinHeaderLen {
				t.Fatalf("accepted impossible frame %+v (len %d)", got, len(data))
			}
		}
	})
}

// refVarint is a big-number-free reference LEB128 decoder: it collects the 7-bit
// groups and rejects > 10 bytes or a value above 2^64-1.
func refVarint(b []byte) (uint64, int, bool) {
	var groups []byte
	for i, c := range b {
		if i >= 10 {
			return 0, 0, false
		}
		groups = append(groups, c&0x7f)
		if c&0x80 == 0 {
			var v uint64
			for j := len(groups) - 1; j >= 0; j-- {
				if v>>57 != 0 {
					return 0, 0, false
				}
				v = v<<7 | uint64(groups[j])
			}
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

func refValidate(b []byte) (records int, id [16]byte, st int) {
	if len(b) < 4 || string(b[:4]) != "LA1\x00" {
		return 0, id, 400
	}
	rest := b[4:]
	hlen, n, ok := refVarint(rest)
	if !ok || hlen < 16 || hlen > 65536 || uint64(len(rest)-n) < hlen {
		return 0, id, 400
	}
	rest = rest[n:]
	copy(id[:], rest[:16])
	rest = rest[hlen:]
	for len(rest) > 0 {
		rlen, n, ok := refVarint(rest)
		if !ok || rlen < 1 || rlen > 65536 || uint64(len(rest)-n) < rlen {
			return 0, id, 400
		}
		rest = rest[uint64(n)+rlen:]
		records++
	}
	if records > 500 {
		return 0, id, 413
	}
	return records, id, 200
}
