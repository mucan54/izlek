package flags

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mucan54/izlek/golib/murmur3"
)

type vectorFile struct {
	Ruleset json.RawMessage `json:"ruleset"`
	Cases   []struct {
		Flag   string         `json:"flag"`
		Props  map[string]any `json:"props"`
		UserID string         `json:"user_id"`
		AnonID string         `json:"anon_id"`
		Expect any            `json:"expect"`
	} `json:"cases"`
}

func loadVectors(t testing.TB) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("../../spec/vectors/flags.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if len(vf.Cases) < 50 {
		t.Fatalf("suspiciously few cases: %d", len(vf.Cases))
	}
	return vf
}

func TestVectors(t *testing.T) {
	vf := loadVectors(t)
	rs, err := ParseRuleset(vf.Ruleset)
	if err != nil {
		t.Fatal(err)
	}
	if rs.V != 31 {
		t.Fatalf("v = %d", rs.V)
	}
	for i, c := range vf.Cases {
		f := rs.Find(c.Flag)
		if f == nil {
			t.Fatalf("case %d: flag %q not in ruleset", i, c.Flag)
		}
		got := f.Eval(c.Props, c.UserID, c.AnonID)
		if !reflect.DeepEqual(got, c.Expect) {
			t.Errorf("case %d (%s, props=%v, user=%q, anon=%q): got %#v, want %#v",
				i, c.Flag, c.Props, c.UserID, c.AnonID, got, c.Expect)
		}
	}
}

// The ruleset must survive decode → encode → decode with identical
// evaluation results, and every flag of the vector file must pass the
// control plane's validation (the vectors only use the portable subset).
func TestVectorsRoundTripAndValidate(t *testing.T) {
	vf := loadVectors(t)
	rs, err := ParseRuleset(vf.Ruleset)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	rs2, err := ParseRuleset(enc)
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := json.Marshal(rs2)
	if err != nil {
		t.Fatal(err)
	}
	if string(enc) != string(enc2) {
		t.Fatalf("encoding is not stable:\n%s\n%s", enc, enc2)
	}
	for i, c := range vf.Cases {
		a := rs.Find(c.Flag).Eval(c.Props, c.UserID, c.AnonID)
		b := rs2.Find(c.Flag).Eval(c.Props, c.UserID, c.AnonID)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("case %d: round trip changed the result: %#v vs %#v", i, a, b)
		}
	}
	for _, f := range rs.Flags {
		if !ValidKey(f.Key) {
			t.Errorf("flag %q: key rejected", f.Key)
		}
		// "short_weights" deliberately has weights that do not sum to 10000:
		// SDKs must tolerate it, the control plane must not accept it.
		err := ValidateRules(f.Rules)
		if f.Key == "short_weights" {
			if err == nil {
				t.Errorf("short_weights passed validation")
			}
			continue
		}
		if err != nil {
			t.Errorf("flag %q: %v", f.Key, err)
		}
		if err := ValidateDefault(f.Default); err != nil {
			t.Errorf("flag %q: %v", f.Key, err)
		}
	}
}

func TestMurmurVectors(t *testing.T) {
	raw, err := os.ReadFile("../../spec/vectors/murmur3.json")
	if err != nil {
		t.Fatal(err)
	}
	var mv struct {
		Cases []struct {
			Input  string `json:"input"`
			Hash   uint32 `json:"hash"`
			Bucket uint32 `json:"bucket"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &mv); err != nil {
		t.Fatal(err)
	}
	if len(mv.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range mv.Cases {
		if h := murmur3.Sum32String(c.Input); h != c.Hash || h%10000 != c.Bucket {
			t.Errorf("murmur3(%q) = %d (bucket %d), want %d (%d)", c.Input, h, h%10000, c.Hash, c.Bucket)
		}
	}
}

func TestToStr(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"x", "x"}, {true, "true"}, {false, "false"},
		{3.0, "3"}, {-2.0, "-2"}, {10.5, "10.5"}, {1e21, "1000000000000000000000"},
		{0.1, "0.1"}, {int64(7), "7"}, {json.Number("12"), "12"}, {json.Number("1.50"), "1.5"},
		{[]any{"a", 1.0}, `["a",1]`}, {map[string]any{"k": "<v>"}, `{"k":"<v>"}`}, {nil, "null"},
	}
	for _, c := range cases {
		if got := toStr(c.in); got != c.want {
			t.Errorf("toStr(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOperators(t *testing.T) {
	type tc struct {
		cond  string
		props string
		want  bool
	}
	cases := []tc{
		// missing / null property
		{`{"prop":"a","op":"is_set"}`, `{}`, false},
		{`{"prop":"a","op":"is_set"}`, `{"a":null}`, false},
		{`{"prop":"a","op":"is_set"}`, `{"a":false}`, true},
		{`{"prop":"a","op":"ne","value":1}`, `{}`, true},
		{`{"prop":"a","op":"not_in","value":[1]}`, `{"a":null}`, true},
		{`{"prop":"a","op":"eq","value":"null"}`, `{}`, false},
		{`{"prop":"a","op":"contains","value":""}`, `{}`, false},
		// loose equality
		{`{"prop":"a","op":"eq","value":3}`, `{"a":3.0}`, true},
		{`{"prop":"a","op":"eq","value":"3"}`, `{"a":3}`, true},
		{`{"prop":"a","op":"eq","value":3}`, `{"a":"3.0"}`, false},
		{`{"prop":"a","op":"eq","value":true}`, `{"a":"true"}`, true},
		{`{"prop":"a","op":"eq","value":1}`, `{"a":true}`, false},
		{`{"prop":"a","op":"in","value":["TR",7]}`, `{"a":"7"}`, true},
		{`{"prop":"a","op":"in","value":"TR"}`, `{"a":"TR"}`, false},
		{`{"prop":"a","op":"not_in","value":"TR"}`, `{"a":"TR"}`, true},
		// ordering
		{`{"prop":"a","op":"gt","value":"10"}`, `{"a":11}`, true},
		{`{"prop":"a","op":"gte","value":10}`, `{"a":"10.0"}`, true},
		{`{"prop":"a","op":"lt","value":10}`, `{"a":"1e0"}`, false},
		{`{"prop":"a","op":"lt","value":10}`, `{"a":true}`, false},
		{`{"prop":"a","op":"lte","value":-1.5}`, `{"a":"-1.5"}`, true},
		{`{"prop":"a","op":"gt","value":1}`, `{"a":" 2"}`, false},
		// contains / regex
		{`{"prop":"a","op":"contains","value":"li"}`, `{"a":"Ali"}`, true},
		{`{"prop":"a","op":"contains","value":"LI"}`, `{"a":"Ali"}`, false},
		{`{"prop":"a","op":"contains","value":2}`, `{"a":123}`, true},
		{`{"prop":"a","op":"regex","value":"^a.c$"}`, `{"a":"abc"}`, true},
		{`{"prop":"a","op":"regex","value":"b"}`, `{"a":"abc"}`, true},
		{`{"prop":"a","op":"regex","value":"("}`, `{"a":"("}`, false},
		// semver
		{`{"prop":"a","op":"semver_gte","value":"3.2.0"}`, `{"a":"3.10"}`, true},
		{`{"prop":"a","op":"semver_gte","value":"3.2"}`, `{"a":"v3.2.0-rc.1"}`, true},
		{`{"prop":"a","op":"semver_lt","value":"v4"}`, `{"a":"3.99.99"}`, true},
		{`{"prop":"a","op":"semver_lt","value":"4"}`, `{"a":"4.0.0"}`, false},
		{`{"prop":"a","op":"semver_gte","value":"1"}`, `{"a":"1.2.3.4"}`, false},
		{`{"prop":"a","op":"semver_gte","value":"1"}`, `{"a":"1..2"}`, false},
		{`{"prop":"a","op":"semver_gte","value":"99999999999999999999999.0.0"}`, `{"a":"100000000000000000000000"}`, true},
		{`{"prop":"a","op":"semver_gte","value":3}`, `{"a":3}`, true},
		// structure
		{`{"and":[]}`, `{}`, true},
		{`{"or":[]}`, `{}`, false},
		{`{"not":{"or":[]}}`, `{}`, true},
		{`{"prop":"a","op":"bogus","value":1}`, `{"a":1}`, false},
		{`{}`, `{}`, false},
	}
	for _, c := range cases {
		var cond Cond
		if err := json.Unmarshal([]byte(c.cond), &cond); err != nil {
			t.Errorf("%s: %v", c.cond, err)
			continue
		}
		var props map[string]any
		if err := json.Unmarshal([]byte(c.props), &props); err != nil {
			t.Fatal(err)
		}
		if got := cond.Match(props); got != c.want {
			t.Errorf("%s on %s = %v, want %v", c.cond, c.props, got, c.want)
		}
	}
}

func TestVirtualKeys(t *testing.T) {
	f := Flag{Key: "k", Enabled: true, Salt: "s", Rules: []Rule{
		{When: ptr(Leaf("user_id", "is_set", nil))},
	}, Default: "anon"}
	if got := f.Eval(map[string]any{"user_id": "spoofed"}, "", "a1"); got != "anon" {
		t.Errorf("anonymous subject matched user_id is_set: %#v", got)
	}
	if got := f.Eval(nil, "u1", "a1"); got != true {
		t.Errorf("identified subject: %#v", got)
	}
	g := Flag{Key: "k", Enabled: true, Salt: "s", Rules: []Rule{
		{When: ptr(Leaf("anon_id", "eq", "a1"))},
	}}
	if got := g.Eval(nil, "", "a1"); got != true {
		t.Errorf("anon_id eq: %#v", got)
	}
	if got := g.Eval(nil, "", "a2"); got != nil {
		t.Errorf("anon_id ne: %#v", got)
	}
}

func ptr[T any](v T) *T { return &v }

func TestParseRulesValidation(t *testing.T) {
	ok := []string{
		``, `null`, `[]`,
		`[{}]`,
		`[{"rollout":0},{"rollout":10000}]`,
		`[{"when":{"and":[{"prop":"platform","op":"eq","value":"ios"},{"prop":"v","op":"semver_gte","value":"3.2.0"}]},"rollout":2500,"variants":[{"key":"control","w":5000},{"key":"b","w":5000}],"stable_on":"anon_id"}]`,
		`[{"when":{"not":{"prop":"plan","op":"is_set"}}}]`,
		`[{"when":{"prop":"plan","op":"is_set","value":null}}]`,
		`[{"when":{"prop":"c","op":"in","value":["TR","DE",1,true]}}]`,
		`[{"when":{"prop":"c","op":"not_in","value":[]}}]`,
		`[{"when":{"prop":"age","op":"gte","value":18}}]`,
		`[{"when":{"prop":"age","op":"lt","value":"10.5"}}]`,
		`[{"when":{"prop":"email","op":"regex","value":"@example\\.com$"}}]`,
		`[{"when":{"prop":"email","op":"regex","value":"^(?:a|b)[0-9]{2,3}\\d+\\s*$"}}]`,
		`[{"when":{"prop":"v","op":"semver_lt","value":"v4"}}]`,
		`[{"when":{"prop":"v","op":"semver_lt","value":4}}]`,
		`[{"when":{"or":[]}}]`,
		`[{"variants":[]}]`,
		`[{"variants":[{"key":"only","w":10000}]}]`,
	}
	for _, raw := range ok {
		rules, err := ParseRules([]byte(raw))
		if err != nil {
			t.Errorf("ParseRules(%s): unexpected error %v", raw, err)
			continue
		}
		if rules == nil {
			t.Errorf("ParseRules(%s): nil slice (must encode as [])", raw)
		}
	}
	bad := []struct{ raw, wantPath string }{
		{`{}`, "rules"},
		{`"x"`, "rules"},
		{`[1]`, "rules"},
		{`[{"rolout":5}]`, "rules"},
		{`[{"rollout":-1}]`, "rules[0].rollout"},
		{`[{"rollout":10001}]`, "rules[0].rollout"},
		{`[{"rollout":2.5}]`, "rules"},
		{`[{"rollout":"50"}]`, "rules"},
		{`[{},{"stable_on":"user_id"}]`, "rules[1].stable_on"},
		{`[{"variants":[{"key":"a","w":5000}]}]`, "rules[0].variants"},
		{`[{"variants":[{"key":"a","w":5000},{"key":"b","w":5001}]}]`, "rules[0].variants"},
		{`[{"variants":[{"key":"a","w":5000},{"key":"a","w":5000}]}]`, "rules[0].variants[1].key"},
		{`[{"variants":[{"key":"","w":10000}]}]`, "rules[0].variants[0].key"},
		{`[{"variants":[{"key":"a","w":-1},{"key":"b","w":10001}]}]`, "rules[0].variants[0].w"},
		{`[{"variants":[{"key":"a","w":10000,"weight":1}]}]`, "rules"},
		{`[{"when":[]}]`, "rules"},
		{`[{"when":{}}]`, "rules[0].when"},
		{`[{"when":{"prop":"a"}}]`, "rules[0].when"},
		{`[{"when":{"op":"eq","value":1}}]`, "rules[0].when"},
		{`[{"when":{"prop":"","op":"eq","value":1}}]`, "rules[0].when.prop"},
		{`[{"when":{"prop":"a","op":"equals","value":1}}]`, "rules[0].when.op"},
		{`[{"when":{"prop":"a","op":"eq","value":1,"extra":true}}]`, "rules[0].when"},
		{`[{"when":{"and":[],"or":[]}}]`, "rules[0].when"},
		{`[{"when":{"and":{}}}]`, "rules"},
		{`[{"when":{"not":[]}}]`, "rules"},
		{`[{"when":{"prop":"a","op":"eq"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"eq","value":null}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"eq","value":{"x":1}}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"in","value":"TR"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"in","value":[["x"]]}}]`, "rules[0].when.value[0]"},
		{`[{"when":{"prop":"a","op":"gt","value":"abc"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"gt","value":true}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"regex","value":"("}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"regex","value":5}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"regex","value":"(?i)x"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"regex","value":"(?=x)"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"regex","value":"\\pL"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"regex","value":"[[:alpha:]]"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"semver_gte","value":"abc"}}]`, "rules[0].when.value"},
		{`[{"when":{"prop":"a","op":"semver_gte","value":true}}]`, "rules[0].when.value"},
		{`[{"when":{"or":[{"prop":"a","op":"is_set"},{"and":[{"prop":"b","op":"nope"}]}]}}]`, "rules[0].when.or[1].and[0].op"},
		{`[{}] trailing`, "rules"},
	}
	for _, c := range bad {
		_, err := ParseRules([]byte(c.raw))
		if err == nil {
			t.Errorf("ParseRules(%s): accepted", c.raw)
			continue
		}
		verr, ok := err.(*Error)
		if !ok {
			t.Errorf("ParseRules(%s): error type %T", c.raw, err)
			continue
		}
		if verr.Path != c.wantPath {
			t.Errorf("ParseRules(%s): path %q (%s), want %q", c.raw, verr.Path, verr.Msg, c.wantPath)
		}
	}
}

func TestLimits(t *testing.T) {
	deep := strings.Repeat(`{"not":`, MaxCondDepth) + `{"prop":"a","op":"is_set"}` + strings.Repeat(`}`, MaxCondDepth)
	if _, err := ParseRules([]byte(`[{"when":` + deep + `}]`)); err == nil {
		t.Error("over-deep condition accepted")
	}
	okDeep := strings.Repeat(`{"not":`, MaxCondDepth-1) + `{"prop":"a","op":"is_set"}` + strings.Repeat(`}`, MaxCondDepth-1)
	if _, err := ParseRules([]byte(`[{"when":` + okDeep + `}]`)); err != nil {
		t.Errorf("condition at the depth limit rejected: %v", err)
	}
	many := `[` + strings.Repeat(`{},`, MaxRules) + `{}]`
	if _, err := ParseRules([]byte(many)); err == nil {
		t.Error("too many rules accepted")
	}
	leaves := strings.Repeat(`{"prop":"a","op":"is_set"},`, MaxCondNodes)
	wide := `[{"when":{"and":[` + leaves[:len(leaves)-1] + `]}}]`
	if _, err := ParseRules([]byte(wide)); err == nil {
		t.Error("too many condition nodes accepted")
	}
}

func TestParseDefault(t *testing.T) {
	for _, raw := range []string{``, `null`, `true`, `false`, `"legacy"`} {
		if _, err := ParseDefault([]byte(raw)); err != nil {
			t.Errorf("ParseDefault(%s): %v", raw, err)
		}
	}
	for _, raw := range []string{`1`, `{}`, `[]`, `""`, `"` + strings.Repeat("x", MaxDefaultLen+1) + `"`, `nope`} {
		if _, err := ParseDefault([]byte(raw)); err == nil {
			t.Errorf("ParseDefault(%s): accepted", raw)
		}
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"a", "0", "new_checkout", "a-b_c9", strings.Repeat("a", 64)} {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false", k)
		}
	}
	for _, k := range []string{"", "_a", "-a", "A", "a b", "a.b", "ş", strings.Repeat("a", 65), "a\n"} {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true", k)
		}
	}
}

// FuzzParseRules feeds arbitrary bytes to the strict rule decoder. Whatever
// it accepts must re-encode to something it accepts again with the same
// canonical form, and evaluation must never panic.
func FuzzParseRules(f *testing.F) {
	vf := loadVectors(f)
	var rs struct {
		Flags []struct {
			Rules json.RawMessage `json:"rules"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(vf.Ruleset, &rs); err != nil {
		f.Fatal(err)
	}
	for _, fl := range rs.Flags {
		f.Add([]byte(fl.Rules))
	}
	f.Add([]byte(`[{"when":{"prop":"a","op":"regex","value":"^[a-z]+$"}}]`))
	f.Add([]byte(`[{"when":{"not":{"or":[{"prop":"a","op":"in","value":[1,"2",true]}]}},"rollout":1}]`))
	f.Add([]byte(`[{"when":{"and":[],"x":1}}]`))
	props := map[string]any{"a": "abc", "platform": "ios", "age": 18.0, "plan": nil, "tier": true, "app_version": "3.2.1"}
	f.Fuzz(func(t *testing.T, data []byte) {
		rules, err := ParseRules(data)
		if err != nil {
			return
		}
		enc, err := json.Marshal(rules)
		if err != nil {
			t.Fatalf("accepted rules do not encode: %v", err)
		}
		again, err := ParseRules(enc)
		if err != nil {
			t.Fatalf("canonical form rejected: %v\n%s", err, enc)
		}
		enc2, err := json.Marshal(again)
		if err != nil {
			t.Fatal(err)
		}
		if string(enc) != string(enc2) {
			t.Fatalf("canonical form is not stable:\n%s\n%s", enc, enc2)
		}
		fl := Flag{Key: "k", Enabled: true, Salt: "s", Rules: rules}
		fl2 := Flag{Key: "k", Enabled: true, Salt: "s", Rules: again}
		for _, id := range []string{"", "u1", "u2"} {
			a := fl.Eval(props, id, "anon")
			b := fl2.Eval(props, id, "anon")
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("round trip changed evaluation: %#v vs %#v", a, b)
			}
		}
	})
}

// FuzzCond checks the tolerant decoder used for published rulesets.
func FuzzCond(f *testing.F) {
	f.Add([]byte(`{"prop":"a","op":"eq","value":1}`))
	f.Add([]byte(`{"and":[{"or":[]},{"not":{"prop":"a","op":"semver_lt","value":"1.2"}}]}`))
	f.Add([]byte(`{"prop":"a","op":"regex","value":"(("}`))
	props := map[string]any{"a": "1.0.0", "b": 2.0, "c": []any{"x"}, "d": map[string]any{"k": 1.0}}
	f.Fuzz(func(t *testing.T, data []byte) {
		var c Cond
		if err := json.Unmarshal(data, &c); err != nil {
			return
		}
		_ = c.Match(props)
		if _, err := json.Marshal(c); err != nil && c.problem == "" {
			t.Fatalf("decoded condition does not encode: %v", err)
		}
	})
}
