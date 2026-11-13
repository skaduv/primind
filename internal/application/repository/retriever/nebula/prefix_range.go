package nebula

import "unicode/utf8"

// nameKeyIndexBytes is the length of name_key in primind_entity_key (see
// entityKeyIndexDDL).
const nameKeyIndexBytes = 256

// prefixRange returns bounds lo <= s < hi that contain every valid UTF-8
// string s starting with prefix, for the index range scan of the prefix
// fallback. NebulaGraph 3.8 does not turn STARTS WITH into a range scan; it
// reads every index entry of the KB. A range condition is scanned through the
// index instead.
//
// Strings compare by bytes in NebulaGraph, and byte order of valid UTF-8 is
// code point order. The smallest string after every string starting with p
// is therefore p with its last code point incremented (skipping the surrogate
// range), or, when that code point is U+10FFFF, the same for p without it.
// When every code point is U+10FFFF there is no upper bound and hasHi is
// false.
//
// The bounds are computed from the longest prefix of prefix, cut at a code
// point boundary, for which neither bound exceeds maxBytes, so that both fit
// the index without truncation. They may then be wider than the prefix; the
// caller keeps STARTS WITH on the full prefix as the exact test.
func prefixRange(prefix string, maxBytes int) (lo, hi string, hasHi bool) {
	lo = prefix
	for len(lo) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(lo)
		lo = lo[:len(lo)-size]
	}
	for {
		hi, hasHi = nextPrefix(lo)
		if !hasHi || len(hi) <= maxBytes {
			return lo, hi, hasHi
		}
		_, size := utf8.DecodeLastRuneInString(lo)
		lo = lo[:len(lo)-size]
	}
}

// nextPrefix returns the smallest string greater than every string that
// starts with p, in byte order over valid UTF-8, and false when none exists.
func nextPrefix(p string) (string, bool) {
	for p != "" {
		r, size := utf8.DecodeLastRuneInString(p)
		p = p[:len(p)-size]
		switch r {
		case utf8.MaxRune:
			continue
		case 0xD7FF: // U+D800–U+DFFF are surrogates, not valid in UTF-8
			r = 0xE000
		default:
			r++
		}
		return p + string(r), true
	}
	return "", false
}
