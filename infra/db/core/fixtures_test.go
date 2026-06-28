package core

// Shared test fixtures for the relational-model (db) package tests. These small
// value types mirror fixtures the infra/pg test suites also use; each package
// keeps its own copy since test fixtures do not cross a package boundary.

type embedFixture struct{ ID string }
