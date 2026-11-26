package modelcontext

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestModelOutputCorpus replays real model output (testdata/model_outputs,
// BF-01 spec 4.5) through the citation expander and checks the result against
// the expansion recorded next to it.
//
// Each *.sample.md holds one answer a model actually produced, de-identified
// and quoted verbatim:
//
//   - model:  chat model that wrote it
//   - date:   when it was written (Beijing time)
//   - source: where the output came from
//   - citable: handles the answer could legally cite, e.g. c1-c22 or c1, w1;
//     each cN registers chunk "chunk-N" titled "Doc N", each wN a web page
//   - variants: the counts the registry must report, e.g. square_double_quoted=4
//
// followed by two ```text blocks: the raw model output and its expected
// expansion. A new bracket spelling therefore fails here first; add the
// sample, then register the variant in the citationVariants rule table.
func TestModelOutputCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "model_outputs", "*.sample.md"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "the regression corpus needs at least one real sample")

	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".sample.md"), func(t *testing.T) {
			sample := loadCorpusSample(t, file)
			require.NotEmpty(t, sample.meta["model"], "model:")
			require.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, sample.meta["date"], "date:")
			require.NotEmpty(t, sample.raw, "raw model output block")
			require.NotContains(t, sample.expected, "ref id=", "expected expansion must not keep a marker")

			registry := NewRegistry(true)
			for _, handle := range expandHandleList(t, sample.meta["citable"]) {
				n, err := strconv.Atoi(handle[1:])
				require.NoError(t, err)
				switch handle[0] {
				case 'c':
					registry.RegisterChunk(ChunkReference{
						ChunkID:       fmt.Sprintf("chunk-%d", n),
						DocumentTitle: fmt.Sprintf("Doc %d", n),
					})
				case 'w':
					registry.RegisterWeb(fmt.Sprintf("https://example.com/%d", n), fmt.Sprintf("Example %d", n))
				}
			}

			got := registry.DecodeOutputText(sample.raw)
			require.NotContains(t, got, "ref id=",
				"a marker survived expansion; register its spelling in the citationVariants rule table")
			require.Equal(t, sample.expected, got)
			require.Equal(t, parseVariantCounts(t, sample.meta["variants"]), registry.CitationVariantStats())
		})
	}
}

type corpusSample struct {
	meta     map[string]string
	raw      string
	expected string
}

func loadCorpusSample(t *testing.T, path string) corpusSample {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := strings.ReplaceAll(string(data), "\r\n", "\n")

	sample := corpusSample{meta: map[string]string{}}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "## ") {
			break
		}
		item, ok := strings.CutPrefix(line, "- ")
		if !ok {
			continue
		}
		if key, value, found := strings.Cut(item, ": "); found {
			sample.meta[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}

	blocks := fencedTextBlocks(content)
	require.Len(t, blocks, 2, "a sample needs a raw and an expected ```text block")
	sample.raw, sample.expected = blocks[0], blocks[1]
	return sample
}

// fencedTextBlocks returns the contents of every ```text fence, in order.
func fencedTextBlocks(content string) []string {
	var blocks []string
	for rest := content; ; {
		start := strings.Index(rest, "```text\n")
		if start < 0 {
			return blocks
		}
		rest = rest[start+len("```text\n"):]
		end := strings.Index(rest, "\n```")
		if end < 0 {
			return blocks
		}
		blocks = append(blocks, rest[:end])
		rest = rest[end+len("\n```"):]
	}
}

func splitHandleList(value string) []string {
	var handles []string
	for _, item := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '，' }) {
		if item = strings.TrimSpace(item); item != "" {
			handles = append(handles, item)
		}
	}
	return handles
}

// handleItemRE matches one citable entry: c3, w2, or an inclusive range such
// as c1-c22 or c1-22.
var handleItemRE = regexp.MustCompile(`^([cw])([1-9][0-9]*)(?:-([cw])?([1-9][0-9]*))?$`)

// expandHandleList turns "c1-c22, w1" into every handle it names, in order.
func expandHandleList(t *testing.T, value string) []string {
	t.Helper()
	var handles []string
	for _, item := range splitHandleList(value) {
		match := handleItemRE.FindStringSubmatch(item)
		require.NotNil(t, match, "citable handle %q", item)
		start, err := strconv.Atoi(match[2])
		require.NoError(t, err)
		if match[4] == "" {
			handles = append(handles, item)
			continue
		}
		require.True(t, match[3] == "" || match[3] == match[1], "range %q mixes handle kinds", item)
		end, err := strconv.Atoi(match[4])
		require.NoError(t, err)
		require.LessOrEqual(t, start, end, "range %q", item)
		for n := start; n <= end; n++ {
			handles = append(handles, fmt.Sprintf("%c%d", match[1][0], n))
		}
	}
	return handles
}

func parseVariantCounts(t *testing.T, value string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, item := range splitHandleList(value) {
		name, count, found := strings.Cut(item, "=")
		require.True(t, found, "variants entry %q must be name=count", item)
		n, err := strconv.Atoi(strings.TrimSpace(count))
		require.NoError(t, err)
		counts[strings.TrimSpace(name)] = n
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}
