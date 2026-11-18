package server

import (
	"bytes"
	"encoding/json"
)

// phInfo is what the hot path extracts from a PostHog JSON body without
// building a document tree: the shape, the event count and the token in the
// body (spec/wal.md §2.2).
type phInfo struct {
	ok     bool   // top level is an object or an array
	events int    // number of events carried
	token  string // api_key -> token -> first event's properties.token ("" if none)
}

// scanPostHog inspects b, which MUST already have passed json.Valid: the
// walkers below rely on well-formed input (and are bounds-safe regardless).
func scanPostHog(b []byte) phInfo {
	var info phInfo
	i := skipWS(b, 0)
	if i >= len(b) {
		return info
	}
	switch b[i] {
	case '[':
		info.ok = true
		first := [2]int{-1, -1}
		info.events = arrayEach(b, i, func(idx, vs, ve int) {
			if idx == 0 {
				first = [2]int{vs, ve}
			}
		})
		if first[0] >= 0 {
			info.token = eventPropsToken(b, first[0])
		}
	case '{':
		info.ok = true
		info.events = 1
		var apiKey, token, propsToken string
		batchFirst := [2]int{-1, -1}
		hasBatch := false
		objectEach(b, i, func(key []byte, vs, ve int) {
			switch {
			case keyIs(key, "api_key"):
				apiKey = stringAt(b, vs, ve)
			case keyIs(key, "token"):
				token = stringAt(b, vs, ve)
			case keyIs(key, "batch"):
				if vs < len(b) && b[vs] == '[' {
					hasBatch = true
					batchFirst = [2]int{-1, -1}
					info.events = arrayEach(b, vs, func(idx, es, ee int) {
						if idx == 0 {
							batchFirst = [2]int{es, ee}
						}
					})
				}
			case keyIs(key, "properties"):
				propsToken = objectString(b, vs, "token")
			}
		})
		switch {
		case apiKey != "":
			info.token = apiKey
		case token != "":
			info.token = token
		case hasBatch:
			if batchFirst[0] >= 0 {
				info.token = eventPropsToken(b, batchFirst[0])
			}
		default:
			info.token = propsToken
		}
	}
	return info
}

// eventPropsToken returns event.properties.token for the event object
// starting at i.
func eventPropsToken(b []byte, i int) string {
	if i >= len(b) || b[i] != '{' {
		return ""
	}
	tok := ""
	objectEach(b, i, func(key []byte, vs, ve int) {
		if keyIs(key, "properties") {
			tok = objectString(b, vs, "token")
		}
	})
	return tok
}

// objectString returns the string member name of the object starting at i
// ("" if absent or not a string). The last occurrence wins.
func objectString(b []byte, i int, name string) string {
	if i >= len(b) || b[i] != '{' {
		return ""
	}
	out := ""
	objectEach(b, i, func(key []byte, vs, ve int) {
		if keyIs(key, name) {
			out = stringAt(b, vs, ve)
		}
	})
	return out
}

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index just past the string starting at b[i] == '"'.
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(b)
}

// skipValue returns the index just past the JSON value starting at i.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(b)
	default:
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\n', '\t', '\r':
				return i
			}
			i++
		}
		return len(b)
	}
}

// objectEach calls fn for every member of the object starting at b[i] == '{'
// with the raw key (without quotes, escapes intact) and the value span.
func objectEach(b []byte, i int, fn func(key []byte, valStart, valEnd int)) {
	i = skipWS(b, i+1)
	for i < len(b) && b[i] == '"' {
		ke := skipString(b, i)
		if ke > len(b) || ke-1 <= i {
			return
		}
		key := b[i+1 : ke-1]
		i = skipWS(b, ke)
		if i >= len(b) || b[i] != ':' {
			return
		}
		vs := skipWS(b, i+1)
		ve := skipValue(b, vs)
		fn(key, vs, ve)
		i = skipWS(b, ve)
		if i >= len(b) || b[i] != ',' {
			return
		}
		i = skipWS(b, i+1)
	}
}

// arrayEach calls fn for every element of the array starting at b[i] == '['
// and returns the element count.
func arrayEach(b []byte, i int, fn func(idx, valStart, valEnd int)) int {
	n := 0
	i = skipWS(b, i+1)
	for i < len(b) && b[i] != ']' {
		ve := skipValue(b, i)
		if ve <= i {
			return n
		}
		fn(n, i, ve)
		n++
		i = skipWS(b, ve)
		if i >= len(b) || b[i] != ',' {
			return n
		}
		i = skipWS(b, i+1)
	}
	return n
}

// keyIs compares a raw object key with name, decoding escapes if present.
func keyIs(raw []byte, name string) bool {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw) == name
	}
	var s string
	quoted := make([]byte, 0, len(raw)+2)
	quoted = append(append(append(quoted, '"'), raw...), '"')
	return json.Unmarshal(quoted, &s) == nil && s == name
}

// stringAt decodes the JSON string spanning b[vs:ve] ("" if it is not one).
func stringAt(b []byte, vs, ve int) string {
	if vs >= len(b) || ve > len(b) || ve-vs < 2 || b[vs] != '"' {
		return ""
	}
	raw := b[vs+1 : ve-1]
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw)
	}
	var s string
	if json.Unmarshal(b[vs:ve], &s) != nil {
		return ""
	}
	return s
}
