package modelcontext

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// originalBracketRefRE is the matcher the first BF-01 patch shipped with,
// before the variants were gathered into the rule table. The table must keep
// accepting exactly this language, which is what makes T2 a refactor and not
// a behaviour change.
var originalBracketRefRE = regexp.MustCompile(
	`(?i)[\[【]\s*ref\s+id\s*=\s*["“”']?([cwdb][1-9][0-9]*)["“”']?\s*/?\s*[\]】]`,
)

func TestBracketRuleTableMatchesOriginalLanguage(t *testing.T) {
	for _, text := range []string{
		// Citations in every spelling the original matcher took.
		`有效期内[ref id="c2"]。`,
		"a【ref id=“c1”/]b",
		`[ref id='c2' / ]`,
		`[REF ID="C2"]`,
		`[ ref id = "c2" ]`,
		`x[ref id="c15"] [ref id="c22"]y`,
		// Tolerated oddities that must keep working.
		`[ref id="c2]`, `[ref id=c2"]`, `[ref id="c2']`, `[ref id="c2】`, `【ref id=c2]`,
		// Ordinary bracketed prose the table must leave alone (C2).
		`see [ref](https://example.com) and [1] and 【注】`,
		`[ref id="note"] stays`,
		`array[i] and [ref] and [reference]`,
		`[ref id="c0"] [ref id=c01] [refid="c2"] [ref-id="c2"]`,
		`unterminated [ref id="c2"`,
		`angle <ref id="c2"/> is not a bracket variant`,
	} {
		require.Equal(t, originalBracketRefRE.FindStringIndex(text), bracketRefRE.FindStringIndex(text), text)
	}
}

// TestBracketRuleTableCoversEveryCombination walks the whole cross product the
// original matcher accepted (bracket family x quote on each side x optional
// slash x closer) so no tolerated corner is lost by the per-variant split.
func TestBracketRuleTableCoversEveryCombination(t *testing.T) {
	quotes := []string{``, `"`, `'`, `“`, `”`}
	slashes := []string{``, `/`}
	for _, opener := range []string{`[`, `【`} {
		for _, closer := range []string{`]`, `】`} {
			for _, open := range quotes {
				for _, close := range quotes {
					for _, slash := range slashes {
						tag := opener + `ref id=` + open + `c2` + close + slash + closer
						require.Equal(t, originalBracketRefRE.FindStringIndex(tag),
							bracketRefRE.FindStringIndex(tag), tag)
						variant, handle := classifyBracketRef(tag)
						require.Equal(t, "c2", handle, tag)
						require.NotEqual(t, bracketVariantOther, variant, "%s is claimed by no rule-table row", tag)
					}
				}
			}
		}
	}
}

func TestExpandTextVariantTableCoversEveryRule(t *testing.T) {
	registry := bracketTestRegistry()
	names := make(map[string]bool, len(citationVariants))
	expected := make(map[string]int, len(citationVariants))

	for _, row := range citationVariants {
		require.False(t, names[row.name], "duplicate rule-table row %q", row.name)
		names[row.name] = true
		require.NotEmpty(t, row.example, "%s needs a positive example", row.name)

		// The shared matcher finds the example as one whole tag, the stream
		// expander's anchored copy accepts it, and it is filed under this row
		// (rows are ordered most specific first, so no earlier row may claim it).
		require.Equal(t, []int{1, 1 + len(row.example)},
			bracketRefRE.FindStringIndex("a"+row.example+"b"), row.name)
		require.Regexp(t, fullBracketRefRE, row.example, row.name)
		variant, handle := classifyBracketRef(row.example)
		require.Equal(t, row.name, variant)
		require.Equal(t, "c2", handle, row.name)

		// A registered handle expands to a card; an unregistered one vanishes.
		require.Equal(t, "a"+kbChunk2+"b", registry.ExpandText("a"+row.example+"b"), row.name)
		expected[row.name]++
		if row.model != "" {
			require.Regexp(t, regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`), row.date,
				"%s was observed on %q and needs its date", row.name, row.model)
		}
	}
	require.Equal(t, expected, registry.VariantStats())

	// The defect that opened BF-01 is the observed row.
	var observed int
	for _, row := range citationVariants {
		if row.model != "" {
			observed++
		}
	}
	require.Equal(t, 1, observed, "exactly one row is a sighting so far")
	require.True(t, names["square_double_quoted"])

	unknown := bracketTestRegistry()
	require.Equal(t, "a", unknown.ExpandText(`a[ref id="c99"]`), "unknown handles still fail closed")
	require.Equal(t, map[string]int{"square_double_quoted": 1}, unknown.VariantStats())
}

// TestCitationVariantMetrics is the counting half of spec 4.4: a text with
// three bracketed citations counts three, the protocol spelling counts zero.
func TestCitationVariantMetrics(t *testing.T) {
	registry := NewRegistry(true)
	for i := 1; i <= 3; i++ {
		registry.RegisterChunk(ChunkReference{ChunkID: fmt.Sprintf("chunk-%d", i), DocumentTitle: "Doc"})
	}

	got := registry.DecodeOutputText(`有效期[ref id="c1"]；见[ref id="c2"]；另见[ref id="c3"]。`)
	require.Equal(t,
		`有效期<kb doc="Doc" chunk_id="chunk-1" />；见<kb doc="Doc" chunk_id="chunk-2" />；`+
			`另见<kb doc="Doc" chunk_id="chunk-3" />。`,
		got)
	require.Equal(t, map[string]int{"square_double_quoted": 3}, registry.CitationVariantStats())

	mixed := NewRegistry(true)
	mixed.RegisterChunk(ChunkReference{ChunkID: "chunk-1", DocumentTitle: "Doc"})
	mixed.RegisterChunk(ChunkReference{ChunkID: "chunk-2", DocumentTitle: "Doc"})
	mixed.DecodeOutputText(`a[ref id="c1"] b[ref id=c2] c【ref id="c1】`)
	require.Equal(t, map[string]int{
		"square_double_quoted":       1,
		"square_bare":                1,
		"fullwidth_unbalanced_quote": 1,
	}, mixed.CitationVariantStats())

	canonical := NewRegistry(true)
	canonical.RegisterChunk(ChunkReference{ChunkID: "chunk-1", DocumentTitle: "Doc"})
	require.Equal(t, `a<kb doc="Doc" chunk_id="chunk-1" /> b`, canonical.DecodeOutputText(`a<ref id="c1"/> b`))
	require.Empty(t, canonical.CitationVariantStats(), "protocol spelling is not a variant")

	plain := NewRegistry(true)
	ordinary := `see [1] and [ref](https://example.com) and 【注】 and array[i]`
	require.Equal(t, ordinary, plain.DecodeOutputText(ordinary))
	require.Empty(t, plain.CitationVariantStats())
}

// TestStreamExpanderCountsRewritesAndTruncations covers the stream half: a
// tag split across chunks counts once, and a half tag dropped at the end of
// the answer is reported as truncated rather than silently lost.
func TestStreamExpanderCountsRewritesAndTruncations(t *testing.T) {
	registry := bracketTestRegistry()
	expander := newCitationStreamExpander(registry)

	require.Equal(t, "有效期内", expander.Feed(`有效期内[`))
	require.Empty(t, expander.Feed(`ref id="c`))
	require.Equal(t, kbChunk2+"。claim ", expander.Feed(`2"]。claim [ref id="c1"`))
	require.Empty(t, expander.Flush())
	require.Equal(t, map[string]int{"square_double_quoted": 1, "truncated_bracket": 1}, registry.VariantStats())

	angle := bracketTestRegistry()
	expander = newCitationStreamExpander(angle)
	require.Equal(t, "claim ", expander.Feed(`claim <ref id="c1"`))
	require.Empty(t, expander.Flush())
	require.Equal(t, map[string]int{"truncated_angle": 1}, angle.VariantStats())

	// Flushing with nothing pending is how every well-formed stream ends; it
	// is not a truncation and must stay out of the per-model counts.
	quiet := bracketTestRegistry()
	expander = newCitationStreamExpander(quiet)
	require.Equal(t, "plain text", expander.Feed("plain text"))
	require.Empty(t, expander.Flush())
	require.Empty(t, expander.Flush())
	require.Empty(t, quiet.VariantStats())
}

func TestStreamExpanderNormalizesWhitespaceAfterOpener(t *testing.T) {
	// The rule table tolerates whitespace around every piece, so the stream
	// expander must hold the same spellings instead of leaking them as text.
	expander := newCitationStreamExpander(bracketTestRegistry())
	require.Equal(t, "a"+kbChunk1+"b", feedAll(expander, "a[", "\n ref", ` id="c1"]b`))
	require.Equal(t, "x"+kbChunk1+"y",
		feedAll(newCitationStreamExpander(bracketTestRegistry()), "x【", "ref\tid=", "c1】y"))
	require.Equal(t, "[ ref=5] stays", feedAll(newCitationStreamExpander(bracketTestRegistry()), "[ ref=5] stays"))
	require.Equal(t, "[ r", feedAll(newCitationStreamExpander(bracketTestRegistry()), "[ r"),
		"ordinary prose that merely starts like a citation is kept")
}

func TestVariantStatsAreSafeUnderConcurrentExpand(t *testing.T) {
	registry := NewRegistry(true)
	registry.RegisterChunk(ChunkReference{ChunkID: "chunk-1", DocumentTitle: "Doc"})

	const workers, perWorker = 16, 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				registry.DecodeOutputText(`claim [ref id="c1"]`)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, map[string]int{"square_double_quoted": workers * perWorker}, registry.CitationVariantStats())
}

func TestProtocolPromptForbidsBracketVariants(t *testing.T) {
	prompt := sourceProtocolPrompt(true)
	require.True(t, strings.Contains(prompt, `Always write the angle-bracket form <ref id="cN"/>`))
	require.Contains(t, prompt, `never write [ref id="cN"]`)
}
