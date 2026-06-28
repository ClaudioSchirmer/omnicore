package query

import "strings"

// DerivedIndexName returns the index name: the explicit .Name(...) when set,
// else Mongo's conventional "<field>_<token>" join across the key spec
// (1/-1/text/2d/2dsphere/hashed). Lives with IndexSpec so the Mongo adapter
// computes a stable name for conflict detection without reaching into the
// spec's unexported fields.
func (s *IndexSpec) DerivedIndexName() string {
	if s.name != "" {
		return s.name
	}
	parts := make([]string, 0, len(s.Keys))
	for _, k := range s.Keys {
		token := "1"
		switch k.Order {
		case IndexOrderDesc:
			token = "-1"
		case IndexOrderText:
			token = "text"
		case IndexOrderGeo2D:
			token = "2d"
		case IndexOrderGeo2DSph:
			token = "2dsphere"
		case IndexOrderHashed:
			token = "hashed"
		}
		parts = append(parts, k.Field+"_"+token)
	}
	return strings.Join(parts, "_")
}
