// citations.go owns the public-citation surface of the model context: the
// system protocol prompt, expansion of private <ref/> handles into canonical
// <kb/> / <web/> tags, re-compaction of canonical tags replayed from history,
// and the stream expander that keeps partial tags off the wire.
package modelcontext

import (
	"fmt"
	"html"
	"maps"
	"regexp"
	"strings"
)

const sourceHandleProtocolPrompt = `

## Source handling protocol (system-owned)
Retrieved content uses request-local source handles: cN identifies a knowledge chunk, wN a web page, dN a document, and bN a knowledge base.
- Use dN and bN only as tool arguments when a tool requests a document or knowledge base.
- Never reveal raw chunk IDs, knowledge IDs, knowledge-base IDs, or private source handles in user-visible output. This does not change separate instructions to preserve retrieved Markdown image URLs.`

const citationEnabledProtocolPrompt = `
- Source citations are enabled for this answer. Cite a knowledge chunk with exactly <ref id="cN"/> and a web page with exactly <ref id="wN"/>.
- Cite only cN/wN handles backed by tool results for the current task, and only when that source supports the adjacent
  claim. Never cite dN/bN.
- Each cN covers only its own supplied text or image evidence, not every chunk of the same document.
  For a direct quotation, verify that the quoted words occur in the cited cN. Use the later chunk's handle
  when the words occur there; never reuse a document's first handle for unrelated passages.
- OCR watermarks, timestamps and image captions are observations of an image. Do not turn a watermark slogan
  into a verified service commitment or a caption into a factual document statement without supporting text.
- Handles in historical answers, tool arguments, or the bound knowledge-base directory are for navigation, not current
  evidence. Retrieve the relevant source before citing it.
- MCP results are external sources. Use the wN handle for the matching URL in the system-provided
  external_source_candidates list; never substitute a knowledge-base cN handle for MCP content. Candidate URLs are links
  observed in the result, not proof that every linked page was read.
- If a source has no citation handle, use its exact supplied HTTP(S) URL as a Markdown link when available.
  If neither is available, omit the citation; never invent or borrow a source.
- Always write the angle-bracket form <ref id="cN"/>; never write [ref id="cN"],
  【ref id="cN"】 or other bracket variants.
- Never output <kb> or <web> tags yourself; the system expands valid <ref/> tags after generation.
- Keep each <ref/> inline on the same line as the claim it supports. Do not group citations ` +
	`at the end. For a requested exact output format, use citations only where the format ` +
	`permits them; do not break a required schema to add citations.
- These rules supersede earlier, saved, or custom prompt instructions about citation syntax.`

const citationDisabledProtocolPrompt = `
- Source citations are disabled for this answer. Do not add <ref>, <kb>, <web>, or source ` +
	`attribution links to the answer. This does not prohibit a URL explicitly requested by the ` +
	`user, Wiki navigation links, downloadable deliverables, or relevant image URLs.
- These rules supersede earlier, saved, or custom prompt instructions that require source citations.`

// ProtocolPrompt returns the internal, non-user-editable source protocol for a
// model call. Citation formatting stays out of custom and template prompts.
func sourceProtocolPrompt(citationsEnabled bool) string {
	if citationsEnabled {
		return sourceHandleProtocolPrompt + citationEnabledProtocolPrompt
	}
	return sourceHandleProtocolPrompt + citationDisabledProtocolPrompt
}

// ProtocolPrompt returns the source protocol configured for this registry.
// Request lifecycle code should normally call this through Registry.
func (r *sourceRegistry) ProtocolPrompt() string {
	if r == nil {
		return ""
	}
	return sourceProtocolPrompt(r.citationsEnabled)
}

var (
	publicKBTagRE        = regexp.MustCompile(`(?is)<kb\b[^>]*>`)
	publicWebTagRE       = regexp.MustCompile(`(?is)<web\b[^>]*>`)
	docAttrRE            = regexp.MustCompile(`(?i)\bdoc\s*=\s*"([^"]*)"`)
	chunkAttrRE          = regexp.MustCompile(`(?i)\bchunk_id\s*=\s*"([^"]+)"`)
	publicKBAttrRE       = regexp.MustCompile(`(?i)\bkb_id\s*=\s*"([^"]*)"`)
	urlAttrRE            = regexp.MustCompile(`(?i)\burl\s*=\s*"([^"]+)"`)
	titleAttrRE          = regexp.MustCompile(`(?i)\btitle\s*=\s*"([^"]*)"`)
	legacyChunkRE        = regexp.MustCompile(`(?is)<(?:chunk|faq)\b[^>]*>`)
	faqAttrRE            = regexp.MustCompile(`(?i)\bfaq_id\s*=\s*"([^"]+)"`)
	knowledgeTitleAttrRE = regexp.MustCompile(`(?i)\bknowledge_title\s*=\s*"([^"]*)"`)
)

func (r *sourceRegistry) registerLegacyToolReferences(text string, evidence bool) {
	if r == nil || text == "" {
		return
	}
	r.registerLabeledReferences(text)
	for _, tag := range legacyChunkRE.FindAllString(text, -1) {
		chunkID := firstNonEmpty(publicAttr(chunkAttrRE, tag), publicAttr(faqAttrRE, tag))
		if chunkID == "" {
			continue
		}
		r.registerChunk(ChunkReference{
			ChunkID:         chunkID,
			KnowledgeID:     publicAttr(documentAttrRE, tag),
			KnowledgeBaseID: firstNonEmpty(publicAttr(kbAttrRE, tag), publicAttr(publicKBAttrRE, tag)),
			DocumentTitle:   firstNonEmpty(publicAttr(knowledgeTitleAttrRE, tag), publicAttr(docAttrRE, tag)),
		}, evidence)
	}
}

// CompactPublicCitations folds canonical citations into the private protocol.
// Historical citations register navigation handles only; citations returned by
// a successful current source tool can also authorize evidence.
func (r *sourceRegistry) CompactPublicCitations(text string, evidence bool) string {
	if r == nil || text == "" {
		return text
	}
	text = publicKBTagRE.ReplaceAllStringFunc(text, func(tag string) string {
		chunkID := publicAttr(chunkAttrRE, tag)
		if chunkID == "" {
			return tag
		}
		handle := r.registerChunk(ChunkReference{
			ChunkID:         chunkID,
			KnowledgeBaseID: publicAttr(publicKBAttrRE, tag),
			DocumentTitle:   publicAttr(docAttrRE, tag),
		}, evidence)
		return `<ref id="` + handle + `"/>`
	})
	return publicWebTagRE.ReplaceAllStringFunc(text, func(tag string) string {
		rawURL := publicAttr(urlAttrRE, tag)
		if rawURL == "" {
			return tag
		}
		handle := r.registerWeb(rawURL, publicAttr(titleAttrRE, tag), evidence)
		return `<ref id="` + handle + `"/>`
	})
}

func publicAttr(expression *regexp.Regexp, tag string) string {
	match := expression.FindStringSubmatch(tag)
	if len(match) != 2 {
		return ""
	}
	return html.UnescapeString(match[1])
}

var (
	refTagRE       = regexp.MustCompile(`(?i)<ref\s+id\s*=\s*"([^"]+)"\s*/?>`)
	refCandidateRE = regexp.MustCompile(`(?is)<ref(?:\s|$)[^>]*(?:>|$)`)
	modelKBTagRE   = regexp.MustCompile(`(?is)<kb(?:\s|$)[^>]*(?:>|$)`)
	modelWebTagRE  = regexp.MustCompile(`(?is)<web(?:\s|$)[^>]*(?:>|$)`)
	// bracketRefRE matches a citation a model wrote with square or full-width
	// brackets instead of the protocol's angle brackets, e.g. [ref id="c2"].
	// It is the union of every row in citationVariants, so the rule table is
	// the only place a variant is defined.
	bracketRefRE = regexp.MustCompile(`(?:` + bracketVariantSources() + `)`)
	// fullBracketRefRE is bracketRefRE anchored to a whole candidate tag.
	fullBracketRefRE = regexp.MustCompile(`^(?:` + bracketVariantSources() + `)$`)
)

// Citation variants a model writes instead of the protocol's <ref id="cN"/>
// (BF-01 spec 4.2). A 27B local model drifted into Markdown-style square
// brackets for a whole answer, and the strict angle-bracket parser passed the
// markers through to the page as literal text.
//
// This table is the single place a variant is defined: bracketRefRE is built
// from its rows, ExpandText counts every hit under the row's name, and the
// corpus test (TestModelOutputCorpus) fails on a real sample no row covers.
// To register a new variant, add the sample under testdata/model_outputs/,
// then add one row here with the model and date it was first seen. Only
// handle-shaped ids ([cwdb] + digits, no leading zero) are accepted so
// ordinary bracketed prose such as [1], [text](url) or 【注】 is kept.
//
// Rows are tried in order and the last row of each bracket family is the
// catch-all for a quote on only one side or mismatched quote styles, so the
// union stays exactly the language the original lenient matcher accepted.
type citationVariant struct {
	name string
	// source is the row's whole regular expression body. bracketRefRE is the
	// alternation of every row's source.
	source string
	// full is source anchored to one whole tag; it also names the row for a
	// tag bracketRefRE found and captures the handle inside it.
	full    *regexp.Regexp
	example string // positive case for TestExpandTextVariantTableCoversEveryRule
	// model and date record where the variant was first seen in a real answer
	// (date in Beijing time). Empty when the row was registered preventively.
	model string
	date  string
}

const bracketHandle = `([cwdb][1-9][0-9]*)`

// newCitationVariant compiles one rule-table row. openQuote and closeQuote
// are the quote classes allowed on each side of the handle, "" for none. The
// other pattern pieces are shared so whitespace, the optional "/" before the
// closer and the closer itself (] or 】) behave the same for every variant.
func newCitationVariant(name, opener, openQuote, closeQuote, example, model, date string) citationVariant {
	source := `(?i)` + opener + `\s*ref\s+id\s*=\s*` + openQuote + bracketHandle +
		closeQuote + `\s*/?\s*[\]】]`
	return citationVariant{
		name:    name,
		source:  source,
		full:    regexp.MustCompile(`^(?:` + source + `)$`),
		example: example,
		model:   model,
		date:    date,
	}
}

// citationVariants lists every known bracket spelling, most specific first.
var citationVariants = []citationVariant{
	// Observed: the BF-01 defect. qwen3.8-27b-bf16, quick Q&A over a policy
	// knowledge base, wrote every citation of one answer this way.
	newCitationVariant("square_double_quoted", `\[`, `["“”]`, `["“”]`,
		`[ref id="c2"]`, "qwen3.8-27b-bf16", "2026-11-23"),
	newCitationVariant("square_single_quoted", `\[`, `'`, `'`, `[ref id='c2']`, "", ""),
	newCitationVariant("square_bare", `\[`, ``, ``, `[ref id=c2]`, "", ""),
	newCitationVariant("square_unbalanced_quote", `\[`, `["“”']?`, `["“”']?`, `[ref id="c2]`, "", ""),
	// Preventive: a CJK model may reach for the full-width bracket instead.
	newCitationVariant("fullwidth_double_quoted", `【`, `["“”]`, `["“”]`,
		`【ref id="c2"】`, "", ""),
	newCitationVariant("fullwidth_single_quoted", `【`, `'`, `'`, `【ref id='c2'】`, "", ""),
	newCitationVariant("fullwidth_bare", `【`, ``, ``, `【ref id=c2】`, "", ""),
	newCitationVariant("fullwidth_unbalanced_quote", `【`, `["“”']?`, `["“”']?`, `【ref id="c2】`, "", ""),
}

// bracketVariantOther names a match of bracketRefRE that no row claims. The
// table is a partition, so this is only a defensive label for the stats.
const bracketVariantOther = "bracket_other"

func bracketVariantSources() string {
	sources := make([]string, 0, len(citationVariants))
	for i := range citationVariants {
		sources = append(sources, citationVariants[i].source)
	}
	return strings.Join(sources, "|")
}

// classifyBracketRef returns the rule-table row that produced tag, which must
// be a whole match of bracketRefRE, together with the handle it carries.
func classifyBracketRef(tag string) (string, string) {
	for i := range citationVariants {
		row := &citationVariants[i]
		if match := row.full.FindStringSubmatch(tag); match != nil {
			return row.name, match[1]
		}
	}
	return bracketVariantOther, ""
}

// maxBracketRefLen bounds how long the stream expander buffers a possible
// bracketed citation before giving up and emitting it as plain text.
const maxBracketRefLen = 64

// Variants logged for citation tags the stream expander dropped because the
// answer ended (or stalled past maxBracketRefLen) before the tag was closed.
const (
	variantTruncatedAngle   = "truncated_angle"
	variantTruncatedBracket = "truncated_bracket"
)

// normalizeBracketRefs rewrites bracketed citation variants into the canonical
// private <ref/> form so they follow the same validation as protocol tags,
// counting each rewrite under its rule-table row.
func (r *sourceRegistry) normalizeBracketRefs(text string) string {
	if r == nil || text == "" {
		return text
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "ref") || !strings.ContainsAny(lower, "[【") {
		return text
	}
	return bracketRefRE.ReplaceAllStringFunc(text, func(tag string) string {
		variant, handle := classifyBracketRef(tag)
		if handle == "" {
			return tag
		}
		r.countVariant(variant)
		return `<ref id="` + handle + `"/>`
	})
}

// countVariant records one non-canonical citation under variant. Registration
// of tool results can run concurrently with stream decoding, so the count is
// mutex-guarded rather than assumed single-goroutine.
func (r *sourceRegistry) countVariant(variant string) {
	if r == nil || variant == "" {
		return
	}
	r.variantMu.Lock()
	defer r.variantMu.Unlock()
	if r.variantCounts == nil {
		r.variantCounts = make(map[string]int)
	}
	r.variantCounts[variant]++
}

// VariantStats returns a copy of the citation variants this registry has
// recorded: each rewritten tag under its rule-table row and each dropped
// unfinished tag under truncated_angle / truncated_bracket. It returns nil
// while the model kept to the protocol, so callers log only when needed.
func (r *sourceRegistry) VariantStats() map[string]int {
	if r == nil {
		return nil
	}
	r.variantMu.Lock()
	defer r.variantMu.Unlock()
	if len(r.variantCounts) == 0 {
		return nil
	}
	stats := make(map[string]int, len(r.variantCounts))
	maps.Copy(stats, r.variantCounts)
	return stats
}

var (
	documentAttrRE    = regexp.MustCompile(`(?i)\bknowledge_id\s*=\s*"([^"]+)"`)
	documentElementRE = regexp.MustCompile(`(?is)<knowledge_id>\s*([^<]+?)\s*</knowledge_id>`)
	kbAttrRE          = regexp.MustCompile(`(?i)\b(?:knowledge_base_id|kb_id)\s*=\s*"([^"]+)"`)
	kbElementRE       = regexp.MustCompile(`(?is)<(?:knowledge_base_id|kb_id)>\s*([^<]+?)\s*</(?:knowledge_base_id|kb_id)>`)
)

// registerLabeledReferences covers metadata-oriented tools that do not have a
// dedicated compact renderer. Only explicit ID labels are recognized; UUID-like
// text in retrieved content is never guessed to be a source identifier.
func (r *sourceRegistry) registerLabeledReferences(text string) {
	if r == nil || text == "" {
		return
	}
	for _, expression := range []*regexp.Regexp{documentAttrRE, documentElementRE} {
		for _, match := range expression.FindAllStringSubmatch(text, -1) {
			if len(match) == 2 {
				r.RegisterDocument(strings.TrimSpace(match[1]))
			}
		}
	}
	for _, expression := range []*regexp.Regexp{kbAttrRE, kbElementRE} {
		for _, match := range expression.FindAllStringSubmatch(text, -1) {
			if len(match) == 2 {
				r.RegisterKnowledgeBase(strings.TrimSpace(match[1]))
			}
		}
	}
}

// ExpandText converts the private model protocol into the existing public
// <kb/> / <web/> contract. Unknown handles fail closed and disappear.
func (r *sourceRegistry) ExpandText(text string) string {
	if r == nil || text == "" {
		return text
	}
	text = r.normalizeBracketRefs(text)
	// Public citation tags are output-only. Drop any instance written directly
	// by the model, then create canonical tags solely from registered handles.
	text = modelKBTagRE.ReplaceAllString(text, "")
	text = modelWebTagRE.ReplaceAllString(text, "")
	if !r.citationsEnabled {
		return refCandidateRE.ReplaceAllString(text, "")
	}
	return refCandidateRE.ReplaceAllStringFunc(text, func(tag string) string {
		match := refTagRE.FindStringSubmatch(tag)
		if len(match) != 2 {
			return ""
		}
		handle := strings.ToLower(match[1])
		if _, citable := r.citable.Load(handle); !citable {
			return ""
		}
		if chunkID, chunkRef, ok := r.chunks.resolve(handle); ok {
			attrs := fmt.Sprintf(`doc="%s" chunk_id="%s"`, escapeAttr(chunkRef.DocumentTitle), escapeAttr(chunkID))
			if chunkRef.KnowledgeBaseID != "" {
				attrs += fmt.Sprintf(` kb_id="%s"`, escapeAttr(chunkRef.KnowledgeBaseID))
			}
			return "<kb " + attrs + " />"
		}
		if rawURL, web, ok := r.webs.resolve(handle); ok {
			return fmt.Sprintf(`<web url="%s" title="%s" />`, escapeAttr(rawURL), escapeAttr(web.title))
		}
		return ""
	})
}

func escapeAttr(value string) string { return html.EscapeString(value) }

// citationStreamExpander prevents partial private <ref/> tags from reaching SSE while
// preserving normal streaming for all other content.
type citationStreamExpander struct {
	registry *sourceRegistry
	pending  string
}

func newCitationStreamExpander(registry *sourceRegistry) *citationStreamExpander {
	return &citationStreamExpander{registry: registry}
}

func (d *citationStreamExpander) Feed(chunk string) string {
	if d == nil || d.registry == nil {
		return chunk
	}
	data := d.pending + chunk
	d.pending = ""
	var out strings.Builder
	for data != "" {
		idx := strings.IndexAny(data, "<[【")
		if idx < 0 {
			out.WriteString(data)
			break
		}
		out.WriteString(data[:idx])
		data = data[idx:]
		lower := strings.ToLower(data)
		if data[0] != '<' {
			opener := bracketOpenerLen(data)
			if isBracketRefPending(lower) {
				if len(data) <= maxBracketRefLen {
					d.pending = data
					break
				}
				out.WriteString(data[:opener])
				data = data[opener:]
				continue
			}
			if isBracketRefStart(lower) {
				end, closeLen := bracketCloseIndex(data)
				if end >= 0 && end+closeLen <= maxBracketRefLen {
					tag := data[:end+closeLen]
					if fullBracketRefRE.MatchString(tag) {
						out.WriteString(d.registry.ExpandText(tag))
						data = data[end+closeLen:]
						continue
					}
				}
			}
			out.WriteString(data[:opener])
			data = data[opener:]
			continue
		}
		if isSourceTagPending(lower) && !strings.Contains(data, ">") {
			d.pending = data
			break
		}
		if isRefTagStart(lower) {
			end := strings.IndexByte(data, '>')
			if end < 0 {
				d.pending = data
				break
			}
			tag := data[:end+1]
			if refTagRE.MatchString(tag) {
				out.WriteString(d.registry.ExpandText(tag))
			}
			data = data[end+1:]
			continue
		}
		if isNamedTagStart(lower, "kb") || isNamedTagStart(lower, "web") {
			end := strings.IndexByte(data, '>')
			if end < 0 {
				d.pending = data
				break
			}
			data = data[end+1:]
			continue
		}
		out.WriteByte('<')
		data = data[1:]
	}
	return out.String()
}

func isRefTagStart(value string) bool {
	return isNamedTagStart(value, "ref")
}

func isNamedTagStart(value, name string) bool {
	prefix := "<" + name
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	if len(value) == len(prefix) {
		return true
	}
	next := value[len(prefix)]
	return next == ' ' || next == '\t' || next == '\r' || next == '\n' || next == '>'
}

func isSourceTagPending(value string) bool {
	for _, name := range []string{"ref", "kb", "web"} {
		prefix := "<" + name
		if (len(value) <= len(prefix) && strings.HasPrefix(prefix, value)) || isNamedTagStart(value, name) {
			return true
		}
	}
	return false
}

func (d *citationStreamExpander) Flush() string {
	if d == nil {
		return ""
	}
	pending := d.pending
	d.pending = ""
	if pending == "" {
		return ""
	}
	lower := strings.ToLower(pending)
	// An answer that stops inside a citation tag must not surface the
	// protocol fragment. Record which kind was dropped so the per-model
	// citation stats can tell a cut-off answer from a formatting drift.
	if isBracketRefStart(lower) {
		d.registry.countVariant(variantTruncatedBracket)
		return ""
	}
	if isSourceTagPending(lower) {
		d.registry.countVariant(variantTruncatedAngle)
		return ""
	}
	return d.registry.ExpandText(pending)
}

// bracketOpenerLen returns the byte length of the leading "[" or "【", or 0
// when value does not start with either.
func bracketOpenerLen(value string) int {
	if strings.HasPrefix(value, "【") {
		return len("【")
	}
	if strings.HasPrefix(value, "[") {
		return 1
	}
	return 0
}

// bracketRest returns value without its leading "[" or "【" and without any
// whitespace that follows the opener, which the rule table tolerates. Any
// other leading character yields "" so callers treat the text as not a
// bracketed citation.
func bracketRest(value string) string {
	opener := bracketOpenerLen(value)
	if opener == 0 {
		return ""
	}
	return strings.TrimLeft(value[opener:], " \t\r\n")
}

func isSpaceByte(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}

// isBracketRefStart reports whether value begins with "[ref" or "【ref"
// followed by whitespace, i.e. a bracketed citation the model may still be
// writing.
func isBracketRefStart(value string) bool {
	rest := bracketRest(value)
	if rest == "" || !strings.HasPrefix(rest, "ref") || len(rest) == len("ref") {
		return false
	}
	return isSpaceByte(rest[len("ref")])
}

// isBracketRefPending reports whether more stream data is needed to decide if
// value is a bracketed citation: either an incomplete "[ref" prefix or a
// started citation whose closing bracket has not arrived yet.
func isBracketRefPending(value string) bool {
	if bracketOpenerLen(value) == 0 {
		return false
	}
	rest := bracketRest(value)
	if len(rest) <= len("ref") && strings.HasPrefix("ref", rest) {
		return true
	}
	if !isBracketRefStart(value) {
		return false
	}
	end, _ := bracketCloseIndex(value)
	return end < 0
}

// bracketCloseIndex finds the first "]" or "】" and its byte length.
func bracketCloseIndex(value string) (int, int) {
	ascii := strings.IndexByte(value, ']')
	wide := strings.Index(value, "】")
	switch {
	case ascii < 0 && wide < 0:
		return -1, 0
	case wide < 0 || (ascii >= 0 && ascii < wide):
		return ascii, 1
	default:
		return wide, len("】")
	}
}
