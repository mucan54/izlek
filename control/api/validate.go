package api

import (
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// cleanText trims s and checks its length (in characters) and content.
func cleanText(field, s string, minChars, maxChars int) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return "", errBadRequest("%q geçerli bir metin olmalıdır", field)
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return "", errBadRequest("%q denetim karakteri içeremez", field)
		}
	}
	n := utf8.RuneCountInString(s)
	if n < minChars {
		if n == 0 {
			return "", errBadRequest("%q zorunludur", field)
		}
		return "", errBadRequest("%q en az %d karakter olmalıdır", field, minChars)
	}
	if n > maxChars {
		return "", errBadRequest("%q en çok %d karakter olabilir", field, maxChars)
	}
	return s, nil
}

// normalizeEmail returns the canonical (trimmed, lower-cased) address.
func normalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	bad := errCode(400, "invalid_email", "Geçerli bir e-posta adresi girin")
	if s == "" || len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>\"") {
		return "", bad
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return "", bad
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", bad
	}
	return s, nil
}

func intInRange(field string, v, lo, hi int64) error {
	if v < lo || v > hi {
		return errBadRequest("%q %d ile %d arasında bir tam sayı olmalıdır", field, lo, hi)
	}
	return nil
}

// cleanStrings trims, drops empties, de-duplicates (keeping order) and
// bounds a string list.
func cleanStrings(field string, in []string, maxItems, maxChars int) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxChars {
			return nil, errBadRequest("%q öğeleri en çok %d karakterlik metinler olmalıdır", field, maxChars)
		}
		for _, r := range s {
			if unicode.IsControl(r) {
				return nil, errBadRequest("%q denetim karakteri içeremez", field)
			}
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) > maxItems {
		return nil, errBadRequest("%q en çok %d öğe içerebilir", field, maxItems)
	}
	return out, nil
}

var schemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// cleanOrigins validates an Origin allow-list. Entries are origins exactly
// as browsers send them (scheme://host[:port]); ingestion compares them
// literally after lower-casing, so anything else could never match.
func cleanOrigins(in []string) ([]string, error) {
	items, err := cleanStrings("allowed_origins", in, 50, 200)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, raw := range items {
		o := strings.TrimRight(strings.ToLower(raw), "/")
		u, err := url.Parse(o)
		if err != nil || !schemeRE.MatchString(u.Scheme) || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			strings.ContainsAny(u.Host, "*, ") || o != u.Scheme+"://"+u.Host {
			return nil, errBadRequest(`"allowed_origins": %q bir köken değil (ör. https://app.example.com)`, raw)
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out, nil
}
