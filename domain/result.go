package domain

type WriteResult struct {
	ID     string `json:"id"`
	Fields Fields `json:"fields,omitempty"`
}
