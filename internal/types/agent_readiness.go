package types

// AgentReadinessIssue names one reason an agent's saved configuration cannot
// serve the workspaces it is shared with. Code is a stable value clients branch
// on; Field is the agent config path to fix.
type AgentReadinessIssue struct {
	Code  string `json:"code"`
	Field string `json:"field"`
}

// Readiness issue codes. Clients add one translation per code, so a new code
// is additive: an older client falls back to its generic message.
const (
	AgentIssueMissingChatModel       = "missing_chat_model"
	AgentIssueMissingRerankModel     = "missing_rerank_model"
	AgentIssueChatModelUnavailable   = "chat_model_unavailable"
	AgentIssueRerankModelUnavailable = "rerank_model_unavailable"
)

// Agent config paths reported in AgentReadinessIssue.Field.
const (
	AgentFieldChatModel   = "config.model_id"
	AgentFieldRerankModel = "config.rerank_model_id"
)

// AgentSaveWarning is a non-blocking problem reported next to an agent that
// was saved successfully.
type AgentSaveWarning struct {
	Code   string                `json:"code"`
	Issues []AgentReadinessIssue `json:"issues,omitempty"`
}

// AgentWarningSharedNotReady means the saved agent is shared, and the saved
// configuration leaves the receiving workspaces unable to use it.
const AgentWarningSharedNotReady = "shared_agent_not_ready"
