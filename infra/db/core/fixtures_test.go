package core

// Shared test fixtures for the relational-model (db) package tests. These small
// value types mirror fixtures the infra/pg test suites also use; each package
// keeps its own copy since test fixtures do not cross a package boundary.

type embedFixture struct{ ID string }

// otherFixture is a second, distinct entity type used where a test needs a
// sibling/child of a DIFFERENT type than the owner, with a non-reserved field to
// map ("ID"/"ParentID" are reserved Go names, so a fixture that only carries ID
// cannot supply a mappable filler field).
type otherFixture struct {
	ID  string
	Tag string
}
