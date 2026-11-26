package service

import (
	"context"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
)

// AgentReadinessIssue is the issue type CheckAgentReadiness reports.
type AgentReadinessIssue = types.AgentReadinessIssue

// ModelLookup resolves a model ID within one workspace. It is the read method
// of interfaces.ModelRepository, so the repository satisfies it directly.
type ModelLookup interface {
	GetByID(ctx context.Context, tenantID uint64, id string) (*types.Model, error)
}

// CheckAgentReadiness reports why an agent cannot serve the workspaces it is
// shared with. It reads the saved configuration only, so the same verdict
// serves the share request, the save-time warning and the receiver list:
//
//   - no chat model configured: AgentIssueMissingChatModel;
//   - the configuration runs search_knowledge (agentRequiresRerankModel) but
//     names no rerank model: AgentIssueMissingRerankModel;
//   - a configured model no longer exists in the agent's workspace, or is not
//     active: AgentIssueChatModelUnavailable / AgentIssueRerankModelUnavailable.
//
// models is optional. Pass nil where only stored data may be read (list
// endpoints); the two "missing" checks still apply. A lookup that fails is
// logged and treated as unknown, so a database hiccup never blocks sharing.
// The result is empty when the agent is ready.
func CheckAgentReadiness(
	ctx context.Context, agent *types.CustomAgent, models ModelLookup,
) []types.AgentReadinessIssue {
	if agent == nil {
		return []types.AgentReadinessIssue{
			{Code: types.AgentIssueMissingChatModel, Field: types.AgentFieldChatModel},
		}
	}

	var issues []types.AgentReadinessIssue
	if agent.Config.ModelID == "" {
		issues = append(issues, types.AgentReadinessIssue{
			Code: types.AgentIssueMissingChatModel, Field: types.AgentFieldChatModel,
		})
	} else if !modelIsActiveForAgent(ctx, agent, models, agent.Config.ModelID) {
		issues = append(issues, types.AgentReadinessIssue{
			Code: types.AgentIssueChatModelUnavailable, Field: types.AgentFieldChatModel,
		})
	}

	// A rerank model matters only where the reranker runs. Elsewhere it is
	// optional, so neither a missing nor a stale ID is an issue.
	if agentRequiresRerankModel(agent) {
		if agent.Config.RerankModelID == "" {
			issues = append(issues, types.AgentReadinessIssue{
				Code: types.AgentIssueMissingRerankModel, Field: types.AgentFieldRerankModel,
			})
		} else if !modelIsActiveForAgent(ctx, agent, models, agent.Config.RerankModelID) {
			issues = append(issues, types.AgentReadinessIssue{
				Code: types.AgentIssueRerankModelUnavailable, Field: types.AgentFieldRerankModel,
			})
		}
	}
	return issues
}

// modelIsActiveForAgent reports whether modelID names an active model of the
// agent's own workspace. Without a lookup, or when the lookup fails, the
// answer is "not known to be unavailable".
func modelIsActiveForAgent(
	ctx context.Context, agent *types.CustomAgent, models ModelLookup, modelID string,
) bool {
	if models == nil {
		return true
	}
	model, err := models.GetByID(ctx, agent.TenantID, modelID)
	if err != nil {
		logger.Warnf(ctx, "Agent readiness: model lookup failed for %s in tenant %d: %v",
			modelID, agent.TenantID, err)
		return true
	}
	return model != nil && model.Status == types.ModelStatusActive
}

// AgentNotReadyError is returned by ShareAgent when the agent's saved
// configuration cannot serve receivers. It wraps ErrAgentNotConfigured, so
// errors.Is callers keep their existing handling, and carries the issues so
// the handler can name what is missing.
type AgentNotReadyError struct {
	Issues []types.AgentReadinessIssue
}

func (e *AgentNotReadyError) Error() string {
	return ErrAgentNotConfigured.Error()
}

func (e *AgentNotReadyError) Unwrap() error {
	return ErrAgentNotConfigured
}
