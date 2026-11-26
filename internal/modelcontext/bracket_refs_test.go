package modelcontext

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func bracketTestRegistry() *sourceRegistry {
	registry := newSourceRegistry()
	registry.RegisterChunk(ChunkReference{ChunkID: "chunk-1", DocumentTitle: "Doc"})
	registry.RegisterChunk(ChunkReference{ChunkID: "chunk-2", DocumentTitle: "Doc"})
	return registry
}

const (
	kbChunk1 = `<kb doc="Doc" chunk_id="chunk-1" />`
	kbChunk2 = `<kb doc="Doc" chunk_id="chunk-2" />`
)

func TestExpandTextAcceptsBracketedCitationVariants(t *testing.T) {
	registry := bracketTestRegistry()

	require.Equal(t, "有效期内"+kbChunk2+"。", registry.ExpandText(`有效期内[ref id="c2"]。`))
	require.Equal(t, "a"+kbChunk1+" b"+kbChunk2, registry.ExpandText(`a[ref id="c1"] b[ref id="c2"]`))
	require.Equal(t, "a"+kbChunk1, registry.ExpandText(`a【ref id="c1"】`))
	require.Equal(t, "a"+kbChunk1, registry.ExpandText(`a[ref id=c1]`))
	require.Equal(t, "a"+kbChunk1, registry.ExpandText(`a[ref id=“c1”/]`))
	require.Equal(t, "a", registry.ExpandText(`a[ref id="c99"]`), "unknown handles fail closed")
	require.Equal(t, "a", registry.ExpandText(`a[ref id="d1"]`), "document handles are never citable")
}

func TestExpandTextKeepsOrdinaryBracketedText(t *testing.T) {
	registry := bracketTestRegistry()

	for _, text := range []string{
		`see [ref](https://example.com) and [1] and 【注】`,
		`[ref id="note"] stays`,
		`array[i] and [ref]`,
	} {
		require.Equal(t, text, registry.ExpandText(text))
	}
}

func TestExpandTextDropsBracketedCitationsWhenDisabled(t *testing.T) {
	registry := newSourceRegistry(false)
	registry.RegisterChunk(ChunkReference{ChunkID: "chunk-1", DocumentTitle: "Doc"})

	require.Equal(t, "claim ", registry.ExpandText(`claim [ref id="c1"]`))
}

func feedAll(expander *citationStreamExpander, chunks ...string) string {
	var out strings.Builder
	for _, chunk := range chunks {
		out.WriteString(expander.Feed(chunk))
	}
	out.WriteString(expander.Flush())
	return out.String()
}

func TestStreamExpanderHandlesSplitBracketedCitation(t *testing.T) {
	expander := newCitationStreamExpander(bracketTestRegistry())

	require.Equal(t, "有效期内", expander.Feed(`有效期内[`))
	require.Equal(t, "", expander.Feed(`ref id="c`))
	require.Equal(t, kbChunk2+"。", expander.Feed(`2"]。`))
	require.Empty(t, expander.Flush())

	expander = newCitationStreamExpander(bracketTestRegistry())
	require.Equal(t, "a"+kbChunk1+"b", feedAll(expander, "a【", "ref id=\"c1\"", "】b"))

	expander = newCitationStreamExpander(bracketTestRegistry())
	require.Equal(t, "a"+kbChunk1+" b"+kbChunk2, feedAll(expander, `a[ref id="c1"] b<ref id="c2"/>`))
}

func TestStreamExpanderPassesOrdinaryBracketsThrough(t *testing.T) {
	for _, chunks := range [][]string{
		{"see [", "ref](https://example.com)"},
		{"x [1] y"},
		{"tail ["},
		{"【注】正文"},
		{"[re", "member]"},
	} {
		expander := newCitationStreamExpander(bracketTestRegistry())
		require.Equal(t, strings.Join(chunks, ""), feedAll(expander, chunks...))
	}
}

func TestStreamExpanderReleasesUnclosedBracketAfterLimit(t *testing.T) {
	expander := newCitationStreamExpander(bracketTestRegistry())
	long := "[ref " + strings.Repeat("x", maxBracketRefLen)

	require.Equal(t, long+" end", feedAll(expander, long, " end"))
}

func TestStreamExpanderDropsTruncatedBracketedCitationAtEnd(t *testing.T) {
	expander := newCitationStreamExpander(bracketTestRegistry())

	require.Equal(t, "claim ", feedAll(expander, `claim [ref id="c1"`))
}
