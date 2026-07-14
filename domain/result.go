package domain

type WriteResult struct {
	ID     ID     `json:"id"`
	Fields Fields `json:"fields,omitempty"`
}
