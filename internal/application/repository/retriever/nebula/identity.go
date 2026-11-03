package nebula

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/primind/primind/internal/types"
	"strings"
	"unicode/utf8"
)

// Reversible tuple encoding avoids both separator ambiguity and hash collisions.
// Every user property is base64url UTF-8; nGQL literals never contain user syntax.
func encoded(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func decoded(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !utf8.Valid(b) || encoded(string(b)) != s {
		return "", fmt.Errorf("%w: encoded property", ErrInvalid)
	}
	return string(b), nil
}
func literal(s string) string { return "\"" + s + "\"" } // only encoded values/VIDs
func values(s ...string) string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = literal(encoded(v))
	}
	return "(" + strings.Join(out, ",") + ")"
}
func identity(cfg Config, role string, ns types.NameSpace, parts ...string) (string, error) {
	fields := append([]string{role, ns.KnowledgeBase, ns.Knowledge}, parts...)
	for _, v := range fields {
		if !utf8.ValidString(v) {
			return "", fmt.Errorf("%w: UTF-8", ErrInvalid)
		}
	}
	raw, _ := json.Marshal(fields)
	id := "ng1_" + base64.RawURLEncoding.EncodeToString(raw)
	if len(id) > cfg.VIDBytes {
		return "", fmt.Errorf("%w: VID byte limit", ErrBudget)
	}
	return id, nil
}
func validateNamespace(ns types.NameSpace, doc bool) error {
	if ns.KnowledgeBase == "" || (doc && ns.Knowledge == "") || len(ns.KnowledgeBase) > 96 || len(ns.Knowledge) > 96 || !utf8.ValidString(ns.KnowledgeBase) || !utf8.ValidString(ns.Knowledge) {
		return fmt.Errorf("%w: namespace requires KB and bounded UTF-8 identifiers", ErrInvalid)
	}
	return nil
}
func nonempty(s string) error {
	if s == "" || !utf8.ValidString(s) {
		return fmt.Errorf("%w: empty or invalid UTF-8 field", ErrInvalid)
	}
	return nil
}
