# Contributing to omnicore

Thanks for your interest in omnicore — a DDD + CQRS infrastructure framework for Go
microservices. This document explains how to propose changes and what to expect.

## Before you start: this is a maintainer-gated framework

omnicore is a framework other services depend on, so its public surface is guarded
deliberately. **Open an issue or a discussion before writing code that changes the
framework** (public functions, endpoints, YAML fields, flags, defaults, struct
fields, behavior). Describe the change, the motivation, and the impact, and wait for
a maintainer's go-ahead. This is not bureaucracy for its own sake — an approved
design lands in one clean round instead of a long review of an approach that may not
fit the architecture.

Changes that generally do **not** need prior sign-off: fixing typos, tightening docs,
and clearly-scoped bug fixes with a failing test. When in doubt, open an issue first.

Good first contributions: reproducible bug reports, documentation fixes, and new test
cases that pin down existing behavior.

## Development setup

- **Go**: see the `go` directive in [`go.mod`](go.mod) for the pinned toolchain
  (1.26.x); the framework targets Go 1.21+.
- **Workspace**: omnicore is developed alongside its reference consumer,
  [`omnicore-example-users`](https://github.com/ClaudioSchirmer/omnicore-example-users),
  via a local `go.work` (gitignored — each developer keeps their own). The example is
  the sandbox and the home of the end-to-end QA suites.
- **Build tags are mandatory.** Every build links **two** tags — a relational engine
  (`postgres`, `mysql` or `sqlserver`) **and** a message transport (`kafka` or `nats`). A tagless
  build aborts at boot. The [`Makefile`](Makefile) encapsulates the matrix so you
  don't have to memorize the flags:

  ```sh
  make build          # go build -tags 'postgres kafka' ./...
  make vet
  make test           # unit suite
  make test TAGS='mysql nats'
  make matrix         # build + vet + test across the full engine×transport matrix
  make lint           # golangci-lint (install once with: make tools)
  make check          # the pre-push gate: fmt-check + vet + test
  ```

- **Never run `go mod tidy`** — it prunes the tag-gated engine/transport dependencies
  and breaks the build.

## Testing

- **Every change ships with tests.** The project holds a **95% unit-coverage floor**;
  do not lower it. `make cover` prints the total.
- Tests sit beside the file under test (`foo.go` ↔ `foo_test.go`).
- Integration tests opt in via `//go:build integration` and need the example's Docker
  bench up (`docker compose up` in `../omnicore-example-users/devops`), then
  `make integration`.
- The end-to-end QA suites live in the example (`omnicore-example-users/qa/`) and are
  an oracle — don't edit an expectation to make a suite pass; investigate the cause.

## Coding standards

- **English everywhere** — code, comments, docs, identifiers, tests, logs, error
  strings. The only sanctioned non-English text is the seven translation catalogs in
  `application/translation/`.
- **Respect the DDD layer boundaries** (`domain` → `application` → `infra`/`web`); the
  dependency rules are enforced and described in `CLAUDE.md` and the architecture
  docs. `domain` has zero IO.
- **Canonical and manual paths stay feature-equivalent** — a feature must work the
  same through the convention/Auto path and the hand-wired path.
- Keep the diff idiomatic: match the surrounding code's naming, comment density, and
  patterns.

## Submitting a change

Work on a topic branch (`feature|fix|docs|refactor/<kebab-outcome>`) off `main`. An
approved change to the public surface lands **in one round** with:

1. the code change,
2. tests (green `make check`),
3. a `CHANGELOG.md` `[Unreleased]` entry (public-surface changes only; breaking
   changes flagged under **Changed**),
4. the matching update to the docs site under `docs/content/sections/` — the manual is
   the source of truth for the public surface, and a `changelog.html` entry.

Open the PR against `main` with a clear description linking the approving issue.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), the same license that covers the project.
