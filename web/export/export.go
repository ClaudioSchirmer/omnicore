// Package export is the format-neutral tabular-export core shared by the
// framework's view-export wrappers. A view's queries.ExportPlan plus the
// documents a read returns are walked once into a stream of Row events; a
// pluggable Encoder (CSV today, XLSX later) turns that stream into the wire
// bytes. The plan walk, the per-level column offset ("espaçar uma coluna por
// nível"), the labelKey header resolution, and the `?fields=` pruning all live
// above this boundary — an encoder only decides how a Row materializes, so a new
// format is a new Encoder and nothing else changes.
package export

import "io"

// Cell is one rendered value. Value is left as `any` so encoders type it: the
// CSV encoder stringifies; a future XLSX encoder can write a typed
// number/date/string cell and style header rows.
type Cell struct {
	Value any
}

// Row is one tabular line the generator emits. Depth is the column offset of
// the owning tree level (root = 0, child = 1, grandchild = 2, …) — the encoder
// realizes it (CSV: that many leading empty fields; XLSX: start at column
// Depth). Header marks the per-group label row so an encoder may style it; the
// CSV encoder ignores the flag.
type Row struct {
	Depth  int
	Header bool
	Cells  []Cell
}

// Sink consumes the Row stream for one export, writing to the underlying
// io.Writer the Encoder opened over. Close flushes any buffered tail.
type Sink interface {
	Write(Row) error
	Close() error
}

// Encoder is the pluggable serialization format. ContentType + Extension drive
// the HTTP `Content-Type` / `Content-Disposition` headers; Open binds a Sink to
// the response writer. Encoder-specific configuration (CSV delimiter, future
// XLSX sheet name) lives on the concrete constructor, never on this interface.
type Encoder interface {
	ContentType() string
	Extension() string
	Open(w io.Writer) (Sink, error)
}
