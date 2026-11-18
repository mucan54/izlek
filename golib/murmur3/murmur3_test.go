package murmur3

import (
	"encoding/json"
	"os"
	"testing"
)

func TestVectors(t *testing.T) {
	raw, err := os.ReadFile("../../spec/vectors/murmur3.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Seed  uint32 `json:"seed"`
		Cases []struct {
			Input  string `json:"input"`
			Hash   uint32 `json:"hash"`
			Bucket uint32 `json:"bucket"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range v.Cases {
		got := Sum32Seed([]byte(c.Input), v.Seed)
		if got != c.Hash {
			t.Errorf("murmur3(%q) = %d, want %d", c.Input, got, c.Hash)
		}
		if got%10000 != c.Bucket {
			t.Errorf("bucket(%q) = %d, want %d", c.Input, got%10000, c.Bucket)
		}
		if Sum32String(c.Input) != got {
			t.Errorf("Sum32String mismatch for %q", c.Input)
		}
	}
}
