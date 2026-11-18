package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/golib/flags"
)

var flagSeeds = []string{
	`{}`,
	`{"key":"new_checkout","name":"New checkout","enabled":true,"rules":[{"when":{"and":[{"prop":"platform","op":"eq","value":"ios"},{"prop":"app_version","op":"semver_gte","value":"3.2.0"}]},"rollout":2500,"variants":[{"key":"control","w":5000},{"key":"b","w":5000}],"stable_on":"anon_id"}],"default":null}`,
	`{"key":"k","rules":[{"when":{"or":[{"prop":"age","op":"gte","value":18},{"prop":"country","op":"in","value":["TR","DE"]},{"prop":"email","op":"regex","value":"@example\\.com$"},{"not":{"prop":"plan","op":"is_set","value":true}}]},"rollout":10000,"variants":[{"key":"x","w":10000}]}],"default":"legacy"}`,
	`{"key":"k","rules":[{"rolout":1}]}`,
	`{"key":"k","rules":[{"when":{"prop":"a","op":"regex","value":"("}}]}`,
	`{"key":"k","rules":"x","default":{}}`,
	`{"key":"k","salt":"x","version":3,"updated_at":1,"updated_by":"key:1","rules":[],"default":false}`,
	`{"rules":[{"when":{"and":[],"or":[]}}]}`,
	`[]`, `null`, `{"rules":[{"when":null}]}`, `{"default":"` + strings.Repeat("x", 300) + `"}`,
}

// FuzzFlagDefinition: the decoder for user-supplied flag JSON never panics,
// and whatever it accepts is canonical, re-accepted unchanged, valid for the
// shared evaluator, and safe to evaluate.
func FuzzFlagDefinition(f *testing.F) {
	for _, s := range flagSeeds {
		f.Add([]byte(s))
	}
	props := map[string]any{"platform": "ios", "app_version": "3.2.1", "age": 18.0, "country": "TR", "email": "a@example.com", "plan": nil}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, fl, err := decodeFlagDefinition(data)
		if err != nil {
			if _, ok := err.(*apiError); !ok {
				t.Fatalf("non-API error %T: %v", err, err)
			}
			return
		}
		if !json.Valid(fl.Rules) || !json.Valid(fl.Default) {
			t.Fatalf("accepted definition is not valid JSON: %s / %s", fl.Rules, fl.Default)
		}
		if len(fl.Rules) > maxRulesBytes {
			t.Fatalf("rules of %d bytes accepted", len(fl.Rules))
		}
		// Canonical form: feeding the stored definition back changes nothing.
		again, err := json.Marshal(map[string]any{"name": fl.Name, "description": fl.Description, "enabled": fl.Enabled,
			"rules": json.RawMessage(fl.Rules), "default": json.RawMessage(fl.Default)})
		if err != nil {
			t.Fatal(err)
		}
		_, fl2, err := decodeFlagDefinition(again)
		if err != nil {
			t.Fatalf("canonical form rejected: %v\n%s", err, again)
		}
		if string(fl2.Rules) != string(fl.Rules) || string(fl2.Default) != string(fl.Default) || fl2.Name != fl.Name || fl2.Enabled != fl.Enabled {
			t.Fatalf("definition is not stable:\n%s\n%s", fl.Rules, fl2.Rules)
		}
		// What is stored passes the shared validator and evaluates.
		rules, err := flags.ParseRules(fl.Rules)
		if err != nil {
			t.Fatalf("stored rules fail validation: %v", err)
		}
		if err := flags.ValidateRules(rules); err != nil {
			t.Fatal(err)
		}
		stored := db.Flag{Key: "k", Salt: "s", Enabled: true, Rules: fl.Rules, Default: fl.Default}
		for _, user := range []string{"", "u1"} {
			v, err := EvaluateFlag(stored, props, user, "anon")
			if err != nil {
				t.Fatalf("stored flag does not evaluate: %v", err)
			}
			switch v.(type) {
			case nil, bool, string:
			default:
				t.Fatalf("evaluation produced %T", v)
			}
			if b, ok := v.(bool); ok && !b {
				t.Fatal("evaluation produced false (must be null)")
			}
		}
	})
}

// FuzzRequestBodies drives the other JSON request decoders with arbitrary
// input: they must reject or accept, never panic, and accepted values must
// be within their documented bounds.
func FuzzRequestBodies(f *testing.F) {
	for _, s := range []string{
		`{"name":"Overview","items":[{"insight_id":1,"x":0,"y":0,"w":8,"h":3},{"insight_id":"2","x":8,"y":0,"w":4,"h":3}]}`,
		`{"name":"Daily","kind":"trends","query":{"range":"30d","ui":{"chart":"bar"}}}`,
		`{"name":"x","kind":"sql","query":"SELECT"}`,
		`{"items":[{"insight_id":1e99}]}`, `{"items":[null]}`, `{"name":5}`, `{"query":{"a":[1,2,{"b":null}]}}`,
		`{"user_ids":["42",7],"anon_ids":[""],"note":"n"}`, `{"allowed_origins":["https://a.example","*"]}`,
		`{"email":"a@b.c","role":"owner"}`, `{`, `"x"`, ``,
	} {
		f.Add([]byte(s))
	}
	insights := map[int64]bool{1: true, 2: true}
	f.Fuzz(func(t *testing.T, data []byte) {
		var d dashboardInput
		if err := decodeJSON(data, &d); err == nil {
			if out, err := d.check(insights); err == nil {
				if len(out.Items) > maxDashboardItems || out.Name == "" {
					t.Fatalf("dashboard out of bounds: %+v", out)
				}
				for _, it := range out.Items {
					if !insights[it.InsightID] || it.X < 0 || it.Y < 0 || it.W < 0 || it.H < 0 {
						t.Fatalf("dashboard item out of bounds: %+v", it)
					}
				}
			}
		}
		var in insightInput
		if err := decodeJSON(data, &in); err == nil {
			if out, err := in.check(""); err == nil {
				if !validKind(out.Kind) || !json.Valid(out.Query) || len(out.Query) == 0 || out.Query[0] != '{' || len(out.Query) > maxQueryBytes {
					t.Fatalf("insight out of bounds: %+v", out)
				}
			}
		}
		var lists struct {
			UserIDs []flexString `json:"user_ids"`
			Origins []string     `json:"allowed_origins"`
			Email   string       `json:"email"`
			OrgID   flexInt      `json:"org_id"`
		}
		if err := decodeJSON(data, &lists); err == nil {
			raw := make([]string, len(lists.UserIDs))
			for i, s := range lists.UserIDs {
				raw[i] = string(s)
			}
			if out, err := cleanStrings("user_ids", raw, maxDeletionIDs, maxDeletionIDChars); err == nil {
				seen := map[string]bool{}
				for _, s := range out {
					if s == "" || s != strings.TrimSpace(s) || seen[s] {
						t.Fatalf("cleanStrings kept %q", s)
					}
					seen[s] = true
				}
			}
			if out, err := cleanOrigins(lists.Origins); err == nil {
				for _, o := range out {
					if !strings.Contains(o, "://") || strings.ContainsAny(o, "* ") || o != strings.ToLower(o) || strings.HasSuffix(o, "/") {
						t.Fatalf("cleanOrigins accepted %q", o)
					}
				}
			}
			if email, err := normalizeEmail(lists.Email); err == nil {
				if email != strings.ToLower(email) || strings.Count(email, "@") != 1 || strings.ContainsAny(email, " \t\n") {
					t.Fatalf("normalizeEmail accepted %q", email)
				}
			}
		}
	})
}

// The seeds as an ordinary table test, so plain `go test` covers them.
func TestFlagDefinitionDecoder(t *testing.T) {
	cases := []struct {
		body    string
		ok      bool
		rules   string
		deflt   string
		enabled bool
	}{
		{`{}`, true, `[]`, `null`, false},
		{`{"rules":null,"default":null}`, true, `[]`, `null`, false},
		{`{"enabled":true,"default":false}`, true, `[]`, `false`, true},
		{`{"default":"legacy","rules":[{}]}`, true, `[{}]`, `"legacy"`, false},
		// Whitespace, key order and number spelling are normalised; values are kept as written.
		{`{"rules":[ { "variants":[{"w":5000,"key":"a"},{"key":"b","w":5000}], "rollout": 2500 , "when": {"value": 18.0, "op":"gte", "prop":"age"} } ]}`, true,
			`[{"when":{"prop":"age","op":"gte","value":18.0},"rollout":2500,"variants":[{"key":"a","w":5000},{"key":"b","w":5000}]}]`, `null`, false},
		{`{"rules":[{"when":{"prop":"plan","op":"is_set","value":true}}]}`, true, `[{"when":{"prop":"plan","op":"is_set","value":true}}]`, `null`, false},
		// HTML-significant characters are stored escaped (same JSON value;
		// safe wherever the public ruleset ends up embedded).
		{`{"rules":[{"when":{"prop":"tag","op":"contains","value":"<b>&"}}]}`, true, `[{"when":{"prop":"tag","op":"contains","value":"\u003cb\u003e\u0026"}}]`, `null`, false},
		// Read-only fields echoed back by the UI are ignored.
		{`{"salt":"x","version":9,"updated_at":1,"updated_by":3,"rules":[]}`, true, `[]`, `null`, false},
		{`{"rules":[{"rolout":1}]}`, false, ``, ``, false},
		{`{"rules":{}}`, false, ``, ``, false},
		{`{"rules":[{"rollout":10001}]}`, false, ``, ``, false},
		{`{"default":1}`, false, ``, ``, false},
		{`{"enabled":"true"}`, false, ``, ``, false},
		{`{"name":["x"]}`, false, ``, ``, false},
		{`[]`, false, ``, ``, false},
		{`{"rules":[]} trailing`, false, ``, ``, false},
		{`{"rules":[` + strings.Repeat(`{"when":{"prop":"p","op":"eq","value":"`+strings.Repeat("v", 1000)+`"}},`, 40) + `{}]}`, false, ``, ``, false},
	}
	for _, c := range cases {
		_, fl, err := decodeFlagDefinition([]byte(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%.80s: err = %v, want ok = %v", c.body, err, c.ok)
			continue
		}
		if err != nil {
			ae, isAPI := err.(*apiError)
			if !isAPI || ae.Status != 400 {
				t.Errorf("%.80s: error %v is not a 400", c.body, err)
			}
			continue
		}
		if string(fl.Rules) != c.rules || string(fl.Default) != c.deflt || fl.Enabled != c.enabled {
			t.Errorf("%.80s:\n got rules %s default %s enabled %v\nwant rules %s default %s enabled %v", c.body, fl.Rules, fl.Default, fl.Enabled, c.rules, c.deflt, c.enabled)
		}
	}
	for _, s := range flagSeeds {
		_, _, _ = decodeFlagDefinition([]byte(s)) // must not panic
	}
}

func TestInputHelpers(t *testing.T) {
	for in, want := range map[string]string{" A@B.Example ": "a@b.example", "x+tag@example.com": "x+tag@example.com"} {
		if got, err := normalizeEmail(in); err != nil || got != want {
			t.Errorf("normalizeEmail(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "@x", "x@", "a b@example.com", "Name <a@example.com>", "a@b@c", strings.Repeat("a", 250) + "@example.com", "a@exam\nple.com"} {
		if got, err := normalizeEmail(bad); err == nil {
			t.Errorf("normalizeEmail(%q) accepted as %q", bad, got)
		}
	}
	got, err := cleanOrigins([]string{" HTTPS://App.Example.com/ ", "https://app.example.com", "http://localhost:5173", "capacitor://localhost", ""})
	if err != nil || !reflect.DeepEqual(got, []string{"https://app.example.com", "http://localhost:5173", "capacitor://localhost"}) {
		t.Errorf("cleanOrigins = %v, %v", got, err)
	}
	for _, bad := range []string{"app.example.com", "https://", "https://a.example/path", "https://a.example?x=1", "https://a.example#f", "https://u:p@a.example", "*", "https://*.example.com", "//a.example", "null"} {
		if got, err := cleanOrigins([]string{bad}); err == nil {
			t.Errorf("cleanOrigins(%q) accepted as %v", bad, got)
		}
	}
	if _, err := cleanText("name", "ok\x00", 1, 10); err == nil {
		t.Error("control character accepted")
	}
	if s, err := cleanText("name", "  şşş  ", 3, 3); err != nil || s != "şşş" {
		t.Errorf("cleanText counts bytes, not characters: %q, %v", s, err)
	}
	for in, want := range map[string]uint32{"1": 1, "4294967295": 4294967295} {
		if got, ok := parseProjectID(in); !ok || got != want {
			t.Errorf("parseProjectID(%q) = %d, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "0", "01", "-1", "4294967296", "1e3", "abc", " 1", "1 ", "+1", "99999999999"} {
		if got, ok := parseProjectID(bad); ok {
			t.Errorf("parseProjectID(%q) = %d", bad, got)
		}
	}
}
