package la1

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

type vprop struct {
	K string          `json:"k"`
	T string          `json:"t"`
	V json.RawMessage `json:"v"`
}

type vrec struct {
	Kind  uint8   `json:"kind"`
	UUID  string  `json:"uuid"`
	TS    uint64  `json:"ts"`
	Name  string  `json:"name"`
	Props []vprop `json:"props"`
}

type vbatch struct {
	BatchID   string  `json:"batch_id"`
	SentAt    uint64  `json:"sent_at"`
	SDK       string  `json:"sdk"`
	AnonID    string  `json:"anon_id"`
	UserID    string  `json:"user_id"`
	SessionID string  `json:"session_id"`
	Context   []vprop `json:"context"`
	Records   []vrec  `json:"records"`
}

func id16(t *testing.T, s string) (out [16]byte) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		t.Fatalf("bad id %q", s)
	}
	copy(out[:], b)
	return
}

func props(t *testing.T, in []vprop) []Prop {
	out := make([]Prop, 0, len(in))
	for _, p := range in {
		var str string
		_ = json.Unmarshal(p.V, &str)
		switch p.T {
		case "null":
			out = append(out, Prop{Key: p.K, Kind: Null})
		case "bool":
			out = append(out, B(p.K, string(p.V) == "true"))
		case "int":
			n, err := strconv.ParseInt(str, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, I(p.K, n))
		case "float":
			var f float64
			switch str {
			case "NaN":
				f = math.Float64frombits(0x7ff8000000000000) // canonical quiet NaN, as in the vectors
			case "Infinity":
				f = math.Inf(1)
			case "-Infinity":
				f = math.Inf(-1)
			default:
				var err error
				if f, err = strconv.ParseFloat(str, 64); err != nil {
					t.Fatal(err)
				}
			}
			out = append(out, F(p.K, f))
		case "string":
			out = append(out, S(p.K, str))
		case "json":
			out = append(out, Prop{Key: p.K, Kind: JSON, S: str})
		default:
			t.Fatalf("type %q", p.T)
		}
	}
	return out
}

func TestWireVectors(t *testing.T) {
	raw, err := os.ReadFile("../../spec/vectors/wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Valid []struct {
			Name  string `json:"name"`
			Batch vbatch `json:"batch"`
			Hex   string `json:"hex"`
		} `json:"valid"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Valid) < 6 {
		t.Fatalf("only %d vectors", len(v.Valid))
	}
	for _, c := range v.Valid {
		h := Header{BatchID: id16(t, c.Batch.BatchID), SentAt: c.Batch.SentAt, SDK: c.Batch.SDK,
			AnonID: c.Batch.AnonID, UserID: c.Batch.UserID, SessionID: id16(t, c.Batch.SessionID),
			Context: props(t, c.Batch.Context)}
		var recs []Record
		for _, r := range c.Batch.Records {
			recs = append(recs, Record{Kind: r.Kind, UUID: id16(t, r.UUID), TS: r.TS, Name: r.Name, Props: props(t, r.Props)})
		}
		if got := hex.EncodeToString(Encode(&h, recs)); got != c.Hex {
			t.Errorf("%s: encoding differs from vector", c.Name)
		}
	}
}
