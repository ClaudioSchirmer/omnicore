//go:build postgres && mysql

package core

// This file compiles ONLY when both the `postgres` and `mysql` build tags are
// set at once — an unsupported combination, since a binary must link exactly one
// relational engine. The duplicate declaration below is intentional: it fails the
// build with a clear "redeclared" error whose identifier names the fix. Build
// with exactly one of -tags postgres / -tags mysql, never both.

const omnicore_build_error_use_exactly_one_of_tags_postgres_or_mysql = 0
const omnicore_build_error_use_exactly_one_of_tags_postgres_or_mysql = 0
