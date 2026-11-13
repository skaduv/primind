package nebula

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrefixRangeIndexLength(t *testing.T) {
	if !strings.Contains(entityKeyIndexDDL, fmt.Sprintf("name_key(%d)", nameKeyIndexBytes)) {
		t.Fatalf("nameKeyIndexBytes %d does not match %s", nameKeyIndexBytes, entityKeyIndexDDL)
	}
}

// u builds a string from code points, so the test tables show them exactly.
func u(rs ...rune) string { return string(rs) }

func TestNextPrefixCases(t *testing.T) {
	for _, c := range []struct {
		name, p, want string
		ok            bool
	}{
		{"ASCII", "ab", "ac", true},
		{"one byte", "a", "b", true},
		{"Han 字 U+5B57", u('汉', 0x5b57), u('汉', 0x5b58), true},
		{"U+007F: 1 to 2 bytes", u('a', 0x7f), u('a', 0x80), true},
		{"U+07FF: 2 to 3 bytes", u('a', 0x7ff), u('a', 0x800), true},
		{"U+FFFF: 3 to 4 bytes", u('a', 0xffff), u('a', 0x10000), true},
		{"U+D7FF skips the surrogates", u('a', 0xd7ff), u('a', 0xe000), true},
		{"4-byte Han U+20000", u('a', 0x20000), u('a', 0x20001), true},
		{"U+10FFFE", u('a', 0x10fffe), u('a', 0x10ffff), true},
		{"U+10FFFF dropped, carry", u('a', 0x10ffff), "b", true},
		{"two U+10FFFF dropped", u('a', 0x10ffff, 0x10ffff), "b", true},
		{"carry into U+007F", u(0x7f, 0x10ffff), u(0x80), true},
		{"only U+10FFFF: no bound", u(0x10ffff), "", false},
		{"only U+10FFFF twice: no bound", u(0x10ffff, 0x10ffff), "", false},
		{"empty: no bound", "", "", false},
		{"quote", `q"`, `q#`, true},
		{"backslash", `q\`, `q]`, true},
		{"NUL", u(0), u(1), true},
		{"Han 巴 U+5DF4", "长名-尾" + u(0x5df4), "长名-尾" + u(0x5df5), true},
		{"digits", "规模实体-0000-0000", "规模实体-0000-0001", true},
	} {
		got, ok := nextPrefix(c.p)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: nextPrefix(%q) = %q, %v; want %q, %v", c.name, c.p, got, ok, c.want, c.ok)
		}
	}
}

func TestPrefixRangeFitsIndex(t *testing.T) {
	k := func(n int) string { return strings.Repeat("k", n) }
	han := "长名" + strings.Repeat("甲", 100) + "-尾巴a" // 313 bytes
	for _, c := range []struct {
		name, p, lo, hi string
		hasHi           bool
	}{
		{"short prefix unchanged", "长名", "长名", u('长', 0x540e), true}, // 名 U+540D
		{"exactly 256 bytes", k(256), k(256), k(255) + "l", true},
		{"ASCII cut to 256 bytes", k(300), k(256), k(255) + "l", true},
		{"Han cut at a code point boundary", han, han[:255], han[:252] + u(0x7533), true}, // 甲 U+7532
		{"U+007F at byte 256 would grow hi", k(255) + u(0x7f), k(255), k(254) + "l", true},
		{"U+07FF ending at 256 would grow hi", k(254) + u(0x7ff), k(254), k(253) + "l", true},
		{"U+FFFF ending at 256 would grow hi", k(253) + u(0xffff), k(253), k(252) + "l", true},
		{"U+10FFFF ending at 256 shrinks hi", k(252) + u(0x10ffff), k(252) + u(0x10ffff), k(251) + "l", true},
		{"4-byte code point cut", k(254) + u(0x20000), k(254), k(253) + "l", true},
		{"all U+10FFFF: no upper bound", strings.Repeat(u(0x10ffff), 70), strings.Repeat(u(0x10ffff), 64), "", false},
	} {
		lo, hi, hasHi := prefixRange(c.p, nameKeyIndexBytes)
		if lo != c.lo || hi != c.hi || hasHi != c.hasHi {
			t.Errorf("%s: got lo %d bytes, hi %q (%d bytes), %v; want lo %d bytes, hi %q, %v", c.name,
				len(lo), tail(hi), len(hi), hasHi, len(c.lo), tail(c.hi), c.hasHi)
		}
	}
}

func tail(s string) string {
	if len(s) > 12 {
		return "…" + s[len(s)-12:]
	}
	return s
}

// boundaryRunes are the code points where the byte length of UTF-8 changes,
// the surrogate gap, the end of Unicode, quotes and Han.
var boundaryRunes = []rune{
	0, 'a', 'z', '"', '\\', 0x7e, 0x7f, 0x80, 0xe9, 0x7ff, 0x800, 0xd7ff, 0xe000, 0xfffe, 0xffff,
	0x10000, 0x20000, 0x20001, 0x10fffe, 0x10ffff, '甲', 0x7533, '字', 0x5b58,
}

func randomRunes(rng *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(boundaryRunes[rng.Intn(len(boundaryRunes))])
	}
	return b.String()
}

// neighbours returns strings close to p in byte order: p itself, p cut, p
// extended, and p with its last code point moved by one either way.
func neighbours(rng *rand.Rand, p string) []string {
	out := []string{p, p + randomRunes(rng, 1+rng.Intn(3))}
	if p == "" {
		return append(out, randomRunes(rng, 2))
	}
	r, size := utf8.DecodeLastRuneInString(p)
	head := p[:len(p)-size]
	out = append(out, head, head+randomRunes(rng, 1))
	for _, d := range []rune{-1, 1} {
		n := r + d
		if n >= 0xd800 && n <= 0xdfff {
			n += d * 0x800
		}
		if n >= 0 && n <= utf8.MaxRune {
			out = append(out, head+string(n), head+string(n)+randomRunes(rng, 1))
		}
	}
	return out
}

// Property: lo <= s < hi holds exactly for the valid UTF-8 strings that start
// with lo (byte order), lo is a prefix of p, and both bounds fit the index.
func TestPrefixRangeProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(20261111))
	for i := 0; i < 20000; i++ {
		n := rng.Intn(8)
		if i%10 == 0 {
			n = 60 + rng.Intn(60) // past 256 bytes
		}
		p := randomRunes(rng, n)
		lo, hi, hasHi := prefixRange(p, nameKeyIndexBytes)
		if !strings.HasPrefix(p, lo) || !utf8.ValidString(lo) || len(lo) > nameKeyIndexBytes ||
			len(hi) > nameKeyIndexBytes || (hasHi && !utf8.ValidString(hi)) {
			t.Fatalf("prefix %q: bounds lo %q hi %q", p, lo, hi)
		}
		if len(p) <= nameKeyIndexBytes-4 && lo != p {
			t.Fatalf("short prefix %q was cut to %q", p, lo)
		}
		in := func(s string) bool { return s >= lo && (!hasHi || s < hi) }
		for _, s := range append(neighbours(rng, p), neighbours(rng, lo)...) {
			if !utf8.ValidString(s) {
				t.Fatalf("test string %q is not valid UTF-8", s)
			}
			if in(s) != strings.HasPrefix(s, lo) {
				t.Fatalf("prefix %q (lo %q, hi %q, %v): %q in range %v, has prefix %v", p, lo, hi, hasHi, s,
					in(s), strings.HasPrefix(s, lo))
			}
			if strings.HasPrefix(s, p) && !in(s) {
				t.Fatalf("prefix %q: match %q outside [%q, %q)", p, s, lo, hi)
			}
		}
		if hasHi && in(hi) {
			t.Fatalf("prefix %q: hi %q inside its own range", p, hi)
		}
	}
}
