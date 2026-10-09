package proto

// Cost report group keys that no account, issue, principal or model can
// be, since none of those holds parentheses: the tokens no issue was held
// for, the issues under no epic, and the groups past a report's limit,
// summed (OtherModels, "(other)").
const (
	CostUnattributed = "(unattributed)"
	CostNoEpic       = "(no epic)"
)
