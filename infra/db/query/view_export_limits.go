package query

// DefaultMaxExportRows is the framework fallback ceiling on the number of rows a
// tabular export (CSV/XLSX) streams, used when neither the view
// (MaxExportRows) nor the yaml (query.maxExportRows) declares one. Bounds the
// memory + wall-clock cost of an unfiltered export of a large collection.
const DefaultMaxExportRows int64 = 10_000

// MaxExportRows overrides the per-view ceiling on the number of rows a tabular
// export of this projection streams. Resolution at request time: this value
// (when > 0) wins; otherwise the yaml default query.maxExportRows; otherwise
// DefaultMaxExportRows. Operational state — NOT part of RebuildHash /
// ArtifactHash (no Mongo rebuild, no Version bump on change), mirroring
// MaxLimit.
func (v *ViewDefinition) MaxExportRows(n int64) *ViewDefinition {
	v.maxExportRows = n
	return v
}

// MaxExportRowsValue returns the declared per-view export cap, or 0 when unset.
func (v *ViewDefinition) MaxExportRowsValue() int64 { return v.maxExportRows }

// ResolveMaxExportRows resolves the effective export-row ceiling: the per-view
// override when declared, else the supplied yaml default (query.maxExportRows)
// when positive, else DefaultMaxExportRows. The consumer passes the yaml value
// (cfg.Query.MaxExportRows) so this stays free of any bootstrap import.
// Together with Name, satisfies the web layer's ExportView surface — the
// export's COLUMN plan comes from the Response DTO, not from the view.
func (v *ViewDefinition) ResolveMaxExportRows(yamlDefault int64) int64 {
	if v.maxExportRows > 0 {
		return v.maxExportRows
	}
	if yamlDefault > 0 {
		return yamlDefault
	}
	return DefaultMaxExportRows
}
