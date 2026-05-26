package policy

// ModelRiskTier classifies the risk level of an AI model for governance purposes.
type ModelRiskTier string

const (
	ModelRiskTierLow      ModelRiskTier = "low"
	ModelRiskTierMedium   ModelRiskTier = "medium"
	ModelRiskTierHigh     ModelRiskTier = "high"
	ModelRiskTierCritical ModelRiskTier = "critical"
)

// ValidModelRiskTiers returns all valid risk tier values.
func ValidModelRiskTiers() []ModelRiskTier {
	return []ModelRiskTier{ModelRiskTierLow, ModelRiskTierMedium, ModelRiskTierHigh, ModelRiskTierCritical}
}

// ModelApprovalStatus tracks the approval workflow state of a model.
type ModelApprovalStatus string

const (
	ModelApprovalPending  ModelApprovalStatus = "pending"
	ModelApprovalApproved ModelApprovalStatus = "approved"
	ModelApprovalRejected ModelApprovalStatus = "rejected"
	ModelApprovalRetired  ModelApprovalStatus = "retired"
)

// ValidModelApprovalStatuses returns all valid approval status values.
func ValidModelApprovalStatuses() []ModelApprovalStatus {
	return []ModelApprovalStatus{ModelApprovalPending, ModelApprovalApproved, ModelApprovalRejected, ModelApprovalRetired}
}
