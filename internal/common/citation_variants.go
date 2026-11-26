package common

import (
	"context"
	"sort"

	"github.com/primind/primind/internal/logger"
	secutils "github.com/primind/primind/internal/utils"
)

// CitationVariantLogMessage is the message on every citation-variant log
// line; the per-variant detail travels in the structured fields next to it.
const CitationVariantLogMessage = "model wrote a non-canonical citation"

// LogCitationVariants emits one structured log line per citation variant a
// model-context registry counted while decoding one answer (BF-01 spec 4.4).
//
// stats maps a variant to how often it occurred: a rule-table row name for a
// bracketed citation that was rewritten into a card, or truncated_angle /
// truncated_bracket for a half tag dropped at the end of the stream. An empty
// stats logs nothing, so a model that keeps to the <ref id="cN"/> protocol
// adds no noise. modelID is the chat model's ID and sessionID / messageID
// identify the assistant answer, which is what makes the lines groupable per
// model when a new model is evaluated.
func LogCitationVariants(ctx context.Context, stats map[string]int, modelID, sessionID, messageID string) {
	variants := make([]string, 0, len(stats))
	for variant, count := range stats {
		if count > 0 && variant != "" {
			variants = append(variants, variant)
		}
	}
	if len(variants) == 0 {
		return
	}
	sort.Strings(variants)

	ids := logger.Fields{
		"model_id":   secutils.SanitizeForLog(modelID),
		"session_id": secutils.SanitizeForLog(sessionID),
		"message_id": secutils.SanitizeForLog(messageID),
	}
	entry := logger.GetLogger(ctx).WithFields(ids)
	for _, variant := range variants {
		entry.WithFields(logger.Fields{
			"variant": secutils.SanitizeForLog(variant),
			"count":   stats[variant],
		}).Info(CitationVariantLogMessage)
	}
}
