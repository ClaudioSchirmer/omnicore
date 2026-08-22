package queries

// ProjectionMode names what a field selection does to the document a read
// returns. The zero value is ProjectAll, so an undeclared Projection is the whole
// document — the default every surface produces when the request names no fields.
type ProjectionMode uint8

const (
	// ProjectAll returns the whole document: no selection was declared.
	ProjectAll ProjectionMode = iota
	// ProjectOnly returns ONLY the listed paths — the `?fields=` form. A path the
	// list does not name is absent from the result, the ROOT IDENTITY included: a
	// consumer that did not ask for the id does not get one.
	ProjectOnly
	// ProjectExcept returns everything BUT the listed paths. It is what a Query
	// produces by scrubbing a field the caller may not see (ReadCriteria.Restrict)
	// from a request that named no fields of its own.
	ProjectExcept
)

// Projection is the store-neutral field selection a read carries. Paths are GO
// FIELD PATHS — the same vocabulary the filter and the ordering speak, dotted
// into a segment (`Addresses.ZipCode`) exactly as `?fields=` spells them.
//
// It names no store and encodes no store's convention. Each read engine renders
// it its own way: the Mongo reader builds the include/exclude document its driver
// wants (and decides there, privately, what to do about `_id`); a relational
// reader prunes the served document in memory. Nothing above the read seam knows
// which of those happens — which is the point of the type existing at all.
type Projection struct {
	Mode ProjectionMode
	// Paths is the selection, keyed by Go field path. Empty in ProjectAll mode.
	Paths map[string]bool
}

// ProjectOnlyPaths builds an inclusion projection over the given Go field paths.
// An empty list yields ProjectAll — selecting nothing is not the same as
// selecting no fields, and the whole document is the honest reading.
func ProjectOnlyPaths(paths ...string) Projection {
	if len(paths) == 0 {
		return Projection{}
	}
	p := Projection{Mode: ProjectOnly, Paths: make(map[string]bool, len(paths))}
	for _, path := range paths {
		p.Paths[path] = true
	}
	return p
}

// Narrows reports whether the projection restricts the document at all. False for
// ProjectAll and for a mode whose path set ended up empty.
func (p Projection) Narrows() bool { return p.Mode != ProjectAll && len(p.Paths) > 0 }

// IsInclusion reports whether the projection is an include-list (ProjectOnly).
func (p Projection) IsInclusion() bool { return p.Mode == ProjectOnly && len(p.Paths) > 0 }

// Selects reports whether path is named in the selection. It answers membership
// only — whether that means kept or dropped depends on Mode.
func (p Projection) Selects(path string) bool { return p.Paths[path] }

// Keeps reports whether the projection would keep path in the result: everything
// in ProjectAll, the named paths in ProjectOnly, everything else in ProjectExcept.
func (p Projection) Keeps(path string) bool {
	switch {
	case !p.Narrows():
		return true
	case p.Mode == ProjectOnly:
		return p.Paths[path]
	default:
		return !p.Paths[path]
	}
}

// Include adds path to an inclusion projection, switching an undeclared
// projection into ProjectOnly. A no-op on an exclusion projection: the two forms
// do not mix, and an exclusion already keeps everything it does not name.
func (p *Projection) Include(path string) {
	if p.Mode == ProjectExcept {
		return
	}
	if p.Paths == nil {
		p.Paths = map[string]bool{}
	}
	p.Mode = ProjectOnly
	p.Paths[path] = true
}

// Drop removes path from the result, whichever mode the projection is in: an
// inclusion loses the entry, anything else gains an exclusion. It is what
// ReadCriteria.Restrict applies so a field the caller may not see reaches neither
// the store nor the wire.
func (p *Projection) Drop(path string) {
	if p.Mode == ProjectOnly {
		delete(p.Paths, path)
		if len(p.Paths) == 0 {
			// An inclusion that lost its last path selects nothing, which would
			// read as "the whole document". Keep the intent by inverting it: an
			// exclusion of the dropped path.
			p.Mode, p.Paths = ProjectExcept, map[string]bool{path: true}
		}
		return
	}
	if p.Paths == nil {
		p.Paths = map[string]bool{}
	}
	p.Mode = ProjectExcept
	p.Paths[path] = true
}
