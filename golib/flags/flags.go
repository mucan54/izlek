// Package flags implements the feature-flag ruleset of spec/flags.md: the
// JSON model of flags/<token>.json, the evaluation algorithm (§3, §4) and the
// validation the control plane applies before a ruleset is published
// (spec/control.md §5).
//
// Decoding is tolerant where the published file format allows it (unknown
// fields are ignored, as every reader of the file must do); Validate is the
// strict gate used on the write path.
package flags

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// Ruleset is the document published as flags/<token>.json.
type Ruleset struct {
	V     int64  `json:"v"`
	Flags []Flag `json:"flags"`
}

// Flag is one flag of a ruleset.
type Flag struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
	Salt    string `json:"salt"`
	Rules   []Rule `json:"rules"`
	// Default is null, a boolean or a variant key (string). false ≡ null.
	Default any `json:"default"`
}

// Rule is one targeting rule. The first rule whose condition matches decides.
type Rule struct {
	When *Cond `json:"when,omitempty"`
	// Rollout is in basis points (0–10000); nil means 10000.
	Rollout  *int      `json:"rollout,omitempty"`
	Variants []Variant `json:"variants,omitempty"`
	// StableOn is "" or "anon_id".
	StableOn string `json:"stable_on,omitempty"`
}

// Variant is one weighted variant of a rule; weights are basis points.
type Variant struct {
	Key string `json:"key"`
	W   int    `json:"w"`
}

// MaxCondDepth bounds the nesting of and/or/not. Deeper documents fail to
// decode, which also bounds the work an adversarial request body can cause.
const MaxCondDepth = 32

type condKind uint8

const (
	kindLeaf condKind = iota
	kindAnd
	kindOr
	kindNot
)

// Cond is a condition tree (spec/flags.md §4):
//
//	{"and": [Cond…]} | {"or": [Cond…]} | {"not": Cond} | {"prop", "op", "value"}
//
// The zero value is a leaf with an empty operator, which never matches.
type Cond struct {
	kind  condKind
	subs  []Cond // and / or
	not   *Cond
	prop  string
	op    string
	raw   json.RawMessage // leaf value as written (nil = absent)
	value any             // decoded raw
	re    *regexp.Regexp  // compiled value of a regex leaf; nil if invalid
	// problem is the first structural defect found while decoding (unknown
	// key, mixed forms, wrong type). Evaluation ignores it; Validate reports it.
	problem string
}

// And returns the conjunction of subs.
func And(subs ...Cond) Cond { return Cond{kind: kindAnd, subs: subs} }

// Or returns the disjunction of subs.
func Or(subs ...Cond) Cond { return Cond{kind: kindOr, subs: subs} }

// Not returns the negation of c.
func Not(c Cond) Cond { return Cond{kind: kindNot, not: &c} }

// Leaf returns the comparison {"prop": prop, "op": op, "value": value}.
// A nil value is encoded as an absent "value".
func Leaf(prop, op string, value any) Cond {
	c := Cond{kind: kindLeaf, prop: prop, op: op}
	if value != nil {
		raw, err := marshalNoEscape(value)
		if err != nil {
			c.problem = "value cannot be encoded: " + err.Error()
			return c
		}
		c.setValue(raw)
	}
	return c
}

func (c *Cond) setValue(raw json.RawMessage) {
	c.raw = raw
	c.value = nil
	c.re = nil
	if raw == nil {
		return
	}
	if err := json.Unmarshal(raw, &c.value); err != nil {
		c.problem = "invalid value"
		return
	}
	if c.op == "regex" {
		if re, err := regexp.Compile(toStr(c.value)); err == nil {
			c.re = re
		}
	}
}

// UnmarshalJSON decodes a condition. Like the reference implementation it
// looks for "and", then "or", then "not", and otherwise reads a leaf.
func (c *Cond) UnmarshalJSON(data []byte) error {
	parsed, err := parseCond(data, 1)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

func parseCond(data []byte, depth int) (Cond, error) {
	if depth > MaxCondDepth {
		return Cond{}, fmt.Errorf("flags: condition nested deeper than %d levels", MaxCondDepth)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return Cond{}, errors.New("flags: a condition must be a JSON object")
	}
	var c Cond
	list := func(key string, kind condKind) (Cond, error) {
		c.kind = kind
		var items []json.RawMessage
		if err := json.Unmarshal(m[key], &items); err != nil {
			return Cond{}, fmt.Errorf("flags: %q must be an array of conditions", key)
		}
		c.subs = make([]Cond, 0, len(items))
		for _, it := range items {
			sub, err := parseCond(it, depth+1)
			if err != nil {
				return Cond{}, err
			}
			c.subs = append(c.subs, sub)
		}
		c.problem = extraKeys(m, key)
		return c, nil
	}
	if _, ok := m["and"]; ok {
		return list("and", kindAnd)
	}
	if _, ok := m["or"]; ok {
		return list("or", kindOr)
	}
	if raw, ok := m["not"]; ok {
		sub, err := parseCond(raw, depth+1)
		if err != nil {
			return Cond{}, err
		}
		c.kind, c.not = kindNot, &sub
		c.problem = extraKeys(m, "not")
		return c, nil
	}
	c.kind = kindLeaf
	c.problem = extraKeys(m, "prop", "op", "value")
	if raw, ok := m["prop"]; ok {
		if err := json.Unmarshal(raw, &c.prop); err != nil && c.problem == "" {
			c.problem = `"prop" must be a string`
		}
	} else if c.problem == "" {
		c.problem = `missing "prop"`
	}
	if raw, ok := m["op"]; ok {
		if err := json.Unmarshal(raw, &c.op); err != nil && c.problem == "" {
			c.problem = `"op" must be a string`
		}
	} else if c.problem == "" {
		c.problem = `missing "op"`
	}
	if raw, ok := m["value"]; ok {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return Cond{}, errors.New("flags: invalid condition value")
		}
		problem := c.problem
		c.setValue(compact.Bytes())
		if problem != "" {
			c.problem = problem
		}
	}
	return c, nil
}

func extraKeys(m map[string]json.RawMessage, allowed ...string) string {
	var extra []string
outer:
	for k := range m {
		for _, a := range allowed {
			if k == a {
				continue outer
			}
		}
		extra = append(extra, k)
	}
	if len(extra) == 0 {
		return ""
	}
	sort.Strings(extra)
	return fmt.Sprintf("unexpected field %q", extra[0])
}

// MarshalJSON encodes the condition in its canonical form.
func (c Cond) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := c.encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *Cond) encode(buf *bytes.Buffer) error {
	switch c.kind {
	case kindAnd, kindOr:
		if c.kind == kindAnd {
			buf.WriteString(`{"and":[`)
		} else {
			buf.WriteString(`{"or":[`)
		}
		for i := range c.subs {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := c.subs[i].encode(buf); err != nil {
				return err
			}
		}
		buf.WriteString("]}")
	case kindNot:
		buf.WriteString(`{"not":`)
		if c.not == nil {
			return errors.New("flags: empty not")
		}
		if err := c.not.encode(buf); err != nil {
			return err
		}
		buf.WriteByte('}')
	default:
		prop, err := marshalNoEscape(c.prop)
		if err != nil {
			return err
		}
		op, err := marshalNoEscape(c.op)
		if err != nil {
			return err
		}
		buf.WriteString(`{"prop":`)
		buf.Write(prop)
		buf.WriteString(`,"op":`)
		buf.Write(op)
		if c.raw != nil {
			buf.WriteString(`,"value":`)
			buf.Write(c.raw)
		}
		buf.WriteByte('}')
	}
	return nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ParseRuleset decodes a published ruleset (tolerant of unknown fields).
func ParseRuleset(data []byte) (*Ruleset, error) {
	var rs Ruleset
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("flags: invalid ruleset: %w", err)
	}
	return &rs, nil
}

// Find returns the flag with the given key, or nil.
func (rs *Ruleset) Find(key string) *Flag {
	for i := range rs.Flags {
		if rs.Flags[i].Key == key {
			return &rs.Flags[i]
		}
	}
	return nil
}
