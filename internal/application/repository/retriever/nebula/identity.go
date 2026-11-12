package nebula

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/primind/primind/internal/types"
)

// Storage layout v2.
//
// Identity: VID = "ng2_" + base64url(SHA-256(JSON [role, KB, doc, parts...])[:20]).
// It is a fixed 31 bytes whatever the names are, derived only from content
// (same content, same VID) and only uses [A-Za-z0-9_-], so it is the one thing
// ever spliced into statement text (always via literal). It cannot be decoded:
// everything about a record is read from its plaintext properties, and a VID
// read back from the store must pass safeVID before it is spliced again.
//
// Properties: stored as plaintext. Every user-derived value travels as a
// statement parameter ($name), never as statement text, so quotes, backslashes,
// control characters and NUL cannot reach the nGQL lexer. NebulaGraph 3.8
// rejects parameters in VID positions, which is why VIDs stay literal.
//
// Integrity: on every read the VID is recomputed from the plaintext properties
// and must match; this catches tampering, partial writes and namespace leakage.
// The 160-bit digest makes an accidental collision negligible; a collision is
// not detectable on read (INSERT IF NOT EXISTS keeps the first record).

const (
	vidPrefix = "ng2_"
	vidDigest = 20                  // bytes of SHA-256 kept (160 bits)
	vidLength = len(vidPrefix) + 27 // base64url of 20 bytes is 27 characters
)

func literal(vid string) string { return "\"" + vid + "\"" } // VIDs only: safe charset by construction

// safeVID reports whether id has exactly the shape identity produces. Every
// VID that comes back from the store is checked with it before being spliced
// into a statement.
func safeVID(id string) bool {
	if len(id) != vidLength || !strings.HasPrefix(id, vidPrefix) {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func identity(cfg Config, role string, ns types.NameSpace, parts ...string) (string, error) {
	fields := append([]string{role, ns.KnowledgeBase, ns.Knowledge}, parts...)
	for _, v := range fields {
		if !utf8.ValidString(v) {
			return "", fmt.Errorf("%w: UTF-8", ErrInvalid)
		}
	}
	raw, _ := json.Marshal(fields)
	sum := sha256.Sum256(raw)
	if vidLength > cfg.VIDBytes {
		return "", fmt.Errorf("%w: VID byte limit", ErrBudget)
	}
	return vidPrefix + base64.RawURLEncoding.EncodeToString(sum[:vidDigest]), nil
}

// nameKey is the search key stored next to each entity name and indexed with
// the KB. It is part of the storage format: changing it invalidates every
// stored name_key (Snapshot and DelGraph then fail) and requires a migration.
//
//  1. NFKC (folds full-width letters/digits and compatibility forms);
//  2. lower case;
//  3. split on white space (strings.Fields);
//  4. rejoin: two neighbouring pieces are joined directly when one side of the
//     boundary is a Han character and the other a Latin letter or digit
//     (either order), otherwise with one ASCII space.
//
// Only the two characters at the boundary are looked at. The character
// classes are fixed code point ranges (isHan, isLatinAlnum) rather than the
// standard library's Unicode tables, so the key cannot change with the Go
// version.
func nameKey(name string) string {
	parts := strings.Fields(strings.ToLower(norm.NFKC.String(name)))
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			last, _ := utf8.DecodeLastRuneInString(parts[i-1])
			first, _ := utf8.DecodeRuneInString(p)
			joined := (isHan(last) && isLatinAlnum(first)) || (isLatinAlnum(last) && isHan(first))
			if !joined {
				b.WriteByte(' ')
			}
		}
		b.WriteString(p)
	}
	return b.String()
}

// isLatinAlnum reports a-z or 0-9; it is applied after NFKC and lower case,
// so full-width and upper-case forms have already been folded into it.
func isLatinAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// isHan reports CJK Unified Ideographs and their extensions as fixed ranges:
// Extension A (U+3400-U+4DBF), the URO block (U+4E00-U+9FFF), planes 2-3
// (U+20000-U+3FFFF), and U+3007 IDEOGRAPHIC NUMBER ZERO.
func isHan(r rune) bool {
	return r == 0x3007 || (r >= 0x3400 && r <= 0x4DBF) || (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x20000 && r <= 0x3FFFF)
}

func validateNamespace(ns types.NameSpace, doc bool) error {
	if ns.KnowledgeBase == "" || (doc && ns.Knowledge == "") || len(ns.KnowledgeBase) > 96 || len(ns.Knowledge) > 96 ||
		!utf8.ValidString(ns.KnowledgeBase) || !utf8.ValidString(ns.Knowledge) {
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

// scopeWhere returns the namespace predicate for tag/edge `name` and its
// parameters. Values are parameters; only schema names are spliced.
func scopeWhere(name string, ns types.NameSpace) (string, map[string]string) {
	where := name + ".kb == $kb"
	params := map[string]string{"kb": ns.KnowledgeBase}
	if ns.Knowledge != "" {
		where += " AND " + name + ".doc == $doc"
		params["doc"] = ns.Knowledge
	}
	return where, params
}
