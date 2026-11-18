package flags

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/mucan54/izlek/golib/murmur3"
)

// Eval evaluates the flag for one subject (spec/flags.md §3). The result is
// nil (off / no match and no default), true (on, no variants) or a variant
// key. props is the flat property set the SDK knows; the virtual keys
// "user_id" and "anon_id" are supplied from the arguments and win over
// entries of the same name in props. An empty userID means "not identified".
func (f *Flag) Eval(props map[string]any, userID, anonID string) any {
	if !f.Enabled {
		return normDefault(f.Default)
	}
	lookup := func(key string) any {
		switch key {
		case "user_id":
			if userID == "" {
				return nil
			}
			return userID
		case "anon_id":
			return anonID
		}
		return props[key]
	}
	for i := range f.Rules {
		rule := &f.Rules[i]
		if rule.When != nil && !rule.When.match(lookup) {
			continue
		}
		id := anonID
		if rule.StableOn != "anon_id" && userID != "" {
			id = userID
		}
		rollout := 10000
		if rule.Rollout != nil {
			rollout = *rule.Rollout
		}
		b := int(murmur3.Sum32String(f.Salt+":"+f.Key+":"+id) % 10000)
		if b >= rollout {
			return normDefault(f.Default) // the first matching rule decides
		}
		if len(rule.Variants) == 0 {
			return true
		}
		v := int(murmur3.Sum32String(f.Salt+":v:"+f.Key+":"+id) % 10000)
		acc := 0
		for _, variant := range rule.Variants {
			acc += variant.W
			if v < acc {
				return variant.Key
			}
		}
		return rule.Variants[len(rule.Variants)-1].Key
	}
	return normDefault(f.Default)
}

func normDefault(d any) any {
	if b, ok := d.(bool); ok && !b {
		return nil
	}
	return d
}

// Match reports whether the condition holds for props (no virtual keys).
func (c *Cond) Match(props map[string]any) bool {
	return c.match(func(key string) any { return props[key] })
}

func (c *Cond) match(lookup func(string) any) bool {
	switch c.kind {
	case kindAnd:
		for i := range c.subs {
			if !c.subs[i].match(lookup) {
				return false
			}
		}
		return true
	case kindOr:
		for i := range c.subs {
			if c.subs[i].match(lookup) {
				return true
			}
		}
		return false
	case kindNot:
		return c.not != nil && !c.not.match(lookup)
	}
	p := normalize(lookup(c.prop))
	if p == nil {
		return c.op == "ne" || c.op == "not_in"
	}
	v := c.value
	switch c.op {
	case "is_set":
		return true
	case "eq":
		return looseEq(p, v)
	case "ne":
		return !looseEq(p, v)
	case "in":
		return inList(p, v)
	case "not_in":
		return !inList(p, v)
	case "gt", "gte", "lt", "lte":
		a, okA := toNum(p)
		b, okB := toNum(v)
		if !okA || !okB {
			return false
		}
		switch c.op {
		case "gt":
			return a > b
		case "gte":
			return a >= b
		case "lt":
			return a < b
		default:
			return a <= b
		}
	case "contains":
		return strings.Contains(toStr(p), toStr(v))
	case "regex":
		return c.re != nil && c.re.MatchString(toStr(p))
	case "semver_gte", "semver_lt":
		a, okA := parseSemver(p)
		b, okB := parseSemver(v)
		if !okA || !okB {
			return false
		}
		cmp := compareSemver(a, b)
		if c.op == "semver_gte" {
			return cmp >= 0
		}
		return cmp < 0
	}
	return false
}

// normalize maps the Go values a caller may put into props onto the JSON
// value space used by the algorithm: nil, bool, float64, string, or a
// composite ([]any / map[string]any / anything else, compared via its JSON).
func normalize(x any) any {
	switch t := x.(type) {
	case nil, bool, float64, string:
		return x
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int8:
		return float64(t)
	case int16:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint:
		return float64(t)
	case uint8:
		return float64(t)
	case uint16:
		return float64(t)
	case uint32:
		return float64(t)
	case uint64:
		return float64(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	}
	return x
}

func isNum(x any) (float64, bool) {
	f, ok := x.(float64)
	return f, ok
}

// toStr is str(x) of spec/flags.md §4.
func toStr(x any) string {
	switch t := normalize(x).(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return strconv.FormatFloat(t, 'g', -1, 64)
		}
		if t == math.Trunc(t) {
			return strconv.FormatFloat(t, 'f', 0, 64)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		raw, err := marshalNoEscape(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func looseEq(p, v any) bool {
	p, v = normalize(p), normalize(v)
	if a, ok := isNum(p); ok {
		if b, ok := isNum(v); ok {
			return a == b
		}
	}
	return toStr(p) == toStr(v)
}

func inList(p, v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if looseEq(p, item) {
			return true
		}
	}
	return false
}

// isNumericString reports whether s matches ^-?[0-9]+(\.[0-9]+)?$.
func isNumericString(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	digits := func() bool {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i > start
	}
	if !digits() {
		return false
	}
	if i == len(s) {
		return true
	}
	if s[i] != '.' {
		return false
	}
	i++
	return digits() && i == len(s)
}

// toNum converts a number or a plain decimal string; booleans are not numbers.
func toNum(x any) (float64, bool) {
	switch t := normalize(x).(type) {
	case float64:
		return t, true
	case string:
		if isNumericString(t) {
			// Only a range error is possible here; it yields ±Inf, which
			// is what the reference implementation compares with too.
			f, _ := strconv.ParseFloat(t, 64)
			return f, true
		}
	}
	return 0, false
}

// semver is MAJOR.MINOR.PATCH as digit strings without leading zeros, so
// arbitrarily long components compare correctly.
type semver [3]string

func parseSemver(x any) (semver, bool) {
	s := strings.TrimSpace(toStr(x))
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		s = s[1:]
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return semver{}, false
	}
	out := semver{"0", "0", "0"}
	for i, p := range parts {
		if p == "" {
			return semver{}, false
		}
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return semver{}, false
			}
		}
		p = strings.TrimLeft(p, "0")
		if p == "" {
			p = "0"
		}
		out[i] = p
	}
	return out, true
}

func compareSemver(a, b semver) int {
	for i := 0; i < 3; i++ {
		if len(a[i]) != len(b[i]) {
			if len(a[i]) < len(b[i]) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}
