package flags

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits enforced by Validate. They keep a published ruleset small enough to
// download and evaluate on a phone.
const (
	MaxRules      = 100
	MaxVariants   = 50
	MaxCondNodes  = 1000
	MaxListValues = 1000
	MaxValueLen   = 1024
	MaxPropLen    = 200
	MaxVariantKey = 64
	MaxDefaultLen = 200
)

var keyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Ops is the operator list of spec/flags.md §4.
var Ops = []string{"eq", "ne", "in", "not_in", "gt", "gte", "lt", "lte", "contains", "regex", "is_set", "semver_gte", "semver_lt"}

func knownOp(op string) bool {
	for _, o := range Ops {
		if o == op {
			return true
		}
	}
	return false
}

// Error is a validation failure. Path points into the flag definition
// (e.g. "rules[1].when.and[0].value").
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

func errAt(path, format string, args ...any) error {
	return &Error{Path: path, Msg: fmt.Sprintf(format, args...)}
}

// ValidKey reports whether key is a legal flag key.
func ValidKey(key string) bool { return keyRE.MatchString(key) }

// ParseRules strictly decodes and validates the "rules" array of a flag
// definition. Unknown fields anywhere inside a rule are errors: a misspelt
// "rollout" must not silently become "everyone". null and absent decode to
// an empty list.
func ParseRules(raw []byte) ([]Rule, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return []Rule{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	rules := []Rule{}
	if err := dec.Decode(&rules); err != nil {
		return nil, errAt("rules", "%s", cleanJSONError(err))
	}
	if dec.More() {
		return nil, errAt("rules", "unexpected data after the array")
	}
	if rules == nil {
		rules = []Rule{}
	}
	if err := ValidateRules(rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func cleanJSONError(err error) string {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		field := ute.Field
		if field == "" {
			return "must be an array of rule objects"
		}
		return fmt.Sprintf("field %q has the wrong type (%s)", field, ute.Value)
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "json: ")
	msg = strings.TrimPrefix(msg, "flags: ")
	return msg
}

// ParseDefault decodes and validates the "default" of a flag definition:
// null, a boolean or a string.
func ParseDefault(raw []byte) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errAt("default", "invalid JSON")
	}
	if err := ValidateDefault(v); err != nil {
		return nil, err
	}
	return v, nil
}

// ValidateDefault checks a flag default.
func ValidateDefault(v any) error {
	switch t := v.(type) {
	case nil, bool:
		return nil
	case string:
		if t == "" || len(t) > MaxDefaultLen || !utf8.ValidString(t) {
			return errAt("default", "a variant key must be 1–%d bytes of UTF-8", MaxDefaultLen)
		}
		return nil
	}
	return errAt("default", "must be null, a boolean or a string")
}

// ValidateRules applies the rules of spec/control.md §5 to a rule list.
func ValidateRules(rules []Rule) error {
	if len(rules) > MaxRules {
		return errAt("rules", "at most %d rules", MaxRules)
	}
	nodes := 0
	for i := range rules {
		r := &rules[i]
		path := fmt.Sprintf("rules[%d]", i)
		if r.Rollout != nil && (*r.Rollout < 0 || *r.Rollout > 10000) {
			return errAt(path+".rollout", "must be between 0 and 10000")
		}
		if r.StableOn != "" && r.StableOn != "anon_id" {
			return errAt(path+".stable_on", `only "anon_id" is supported`)
		}
		if len(r.Variants) > MaxVariants {
			return errAt(path+".variants", "at most %d variants", MaxVariants)
		}
		if len(r.Variants) > 0 {
			sum := 0
			seen := make(map[string]bool, len(r.Variants))
			for j, v := range r.Variants {
				vp := fmt.Sprintf("%s.variants[%d]", path, j)
				if v.Key == "" || len(v.Key) > MaxVariantKey || !utf8.ValidString(v.Key) {
					return errAt(vp+".key", "must be 1–%d bytes of UTF-8", MaxVariantKey)
				}
				if seen[v.Key] {
					return errAt(vp+".key", "duplicate variant key %q", v.Key)
				}
				seen[v.Key] = true
				if v.W < 0 || v.W > 10000 {
					return errAt(vp+".w", "must be between 0 and 10000")
				}
				sum += v.W
			}
			if sum != 10000 {
				return errAt(path+".variants", "variant weights must sum to 10000 (got %d)", sum)
			}
		}
		if r.When != nil {
			if err := validateCond(r.When, path+".when", &nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCond(c *Cond, path string, nodes *int) error {
	*nodes++
	if *nodes > MaxCondNodes {
		return errAt(path, "too many conditions (at most %d per flag)", MaxCondNodes)
	}
	if c.problem != "" {
		return errAt(path, "%s", c.problem)
	}
	switch c.kind {
	case kindAnd, kindOr:
		name := "and"
		if c.kind == kindOr {
			name = "or"
		}
		for i := range c.subs {
			if err := validateCond(&c.subs[i], fmt.Sprintf("%s.%s[%d]", path, name, i), nodes); err != nil {
				return err
			}
		}
		return nil
	case kindNot:
		if c.not == nil {
			return errAt(path+".not", "must be a condition")
		}
		return validateCond(c.not, path+".not", nodes)
	}
	if c.prop == "" || len(c.prop) > MaxPropLen || !utf8.ValidString(c.prop) {
		return errAt(path+".prop", "must be 1–%d bytes of UTF-8", MaxPropLen)
	}
	if !knownOp(c.op) {
		return errAt(path+".op", "unknown operator %q", c.op)
	}
	vp := path + ".value"
	v := c.value
	switch c.op {
	case "is_set":
		return nil
	case "eq", "ne", "contains":
		return validateScalar(v, vp)
	case "in", "not_in":
		list, ok := v.([]any)
		if !ok {
			return errAt(vp, "must be an array")
		}
		if len(list) > MaxListValues {
			return errAt(vp, "at most %d values", MaxListValues)
		}
		for i, item := range list {
			if err := validateScalar(item, fmt.Sprintf("%s[%d]", vp, i)); err != nil {
				return err
			}
		}
		return nil
	case "gt", "gte", "lt", "lte":
		if _, isBool := v.(bool); isBool {
			return errAt(vp, "must be a number")
		}
		if _, ok := toNum(v); !ok {
			return errAt(vp, "must be a number or a decimal string")
		}
		return nil
	case "regex":
		// The pattern is str(value): clients that turn numeric-looking input
		// into JSON numbers (or true/false into booleans) still mean text.
		if err := validateScalar(v, vp); err != nil {
			return err
		}
		if err := PortableRegex(toStr(v)); err != nil {
			return errAt(vp, "%s", err.Error())
		}
		return nil
	case "semver_gte", "semver_lt":
		if _, isBool := v.(bool); isBool || v == nil {
			return errAt(vp, "must be a version like 3.2.0")
		}
		if _, isList := v.([]any); isList {
			return errAt(vp, "must be a version like 3.2.0")
		}
		if _, isMap := v.(map[string]any); isMap {
			return errAt(vp, "must be a version like 3.2.0")
		}
		if _, ok := parseSemver(v); !ok {
			return errAt(vp, "must be a version like 3.2.0")
		}
		return nil
	}
	return nil
}

func validateScalar(v any, path string) error {
	switch t := v.(type) {
	case bool, float64:
		return nil
	case string:
		if len(t) > MaxValueLen || !utf8.ValidString(t) {
			return errAt(path, "string longer than %d bytes", MaxValueLen)
		}
		return nil
	}
	return errAt(path, "must be a string, a number or a boolean")
}

// PortableRegex checks that pattern compiles and stays inside the subset
// every SDK's regex engine understands the same way (spec/flags.md §4):
// literals, character classes, ^ $ . * + ? | ( ) (?: ) and {m,n}, plus the
// escapes \d \w \s \D \W \S \b \B \t \n \r \f \v and escaped punctuation.
func PortableRegex(pattern string) error {
	if !utf8.ValidString(pattern) {
		return errors.New("pattern is not valid UTF-8")
	}
	inClass := false
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch {
		case ch == '\\':
			if i+1 >= len(pattern) {
				return errors.New("pattern ends with a backslash")
			}
			i++
			n := pattern[i]
			if n >= utf8.RuneSelf {
				return errors.New("escaped non-ASCII character is not portable")
			}
			isAlnum := n >= '0' && n <= '9' || n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z'
			if isAlnum && !strings.ContainsRune("dDwWsSbBtnrfv", rune(n)) {
				return fmt.Errorf(`escape \%c is not portable`, n)
			}
			if inClass && (n == 'b' || n == 'B') {
				return fmt.Errorf(`escape \%c is not portable inside a character class`, n)
			}
		case inClass:
			if ch == '[' && i+1 < len(pattern) && (pattern[i+1] == ':' || pattern[i+1] == '=' || pattern[i+1] == '.') {
				return errors.New("POSIX character classes are not portable")
			}
			if ch == ']' {
				inClass = false
			}
		case ch == '[':
			inClass = true
			j := i + 1
			if j < len(pattern) && pattern[j] == '^' {
				j++
			}
			if j < len(pattern) && pattern[j] == ']' {
				return errors.New(`a character class must not start with "]"`)
			}
		case ch == '(':
			if i+1 < len(pattern) && pattern[i+1] == '?' {
				if i+2 >= len(pattern) || pattern[i+2] != ':' {
					return errors.New("only plain ( ) and (?: ) groups are portable")
				}
			}
		}
	}
	if _, err := regexp.Compile(pattern); err != nil {
		msg := err.Error()
		msg = strings.TrimPrefix(msg, "error parsing regexp: ")
		return fmt.Errorf("invalid regular expression: %s", msg)
	}
	return nil
}
