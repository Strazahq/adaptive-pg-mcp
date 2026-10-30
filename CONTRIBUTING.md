# Contributing

Open an issue before a pull request that adds a tool, a config key or a
catalog key. Their names are public interface, so they are settled first.

## Setup

You need Go, Docker for the database tests, curl, and gitleaks, govulncheck
and osv-scanner for the push hook. go.mod pins the Go toolchain, and the go
command fetches it when the installed Go is older.

```sh
make hooks
go install golang.org/x/vuln/cmd/govulncheck@latest
go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest
```

gitleaks comes from its [releases](https://github.com/gitleaks/gitleaks/releases).

`make hooks` binds the git hooks in .githooks. The commit hook runs the
checks of `make check` and caps Go files at 600 lines. The push hook runs
the scanners, the database tests and tools/specgate.sh, which needs the
network. A pull request runs the same checks in CI, apart from the spec
gate.

## Tests

`make check` runs the text, state and Go gates, gofmt, go vet and the unit
tests. `make pgtest` starts Postgres 14 to 18 in Docker, runs every test
built with `-tags pg`, and removes the containers again. A database test
carries `//go:build pg` and gets its servers from internal/pgtest. Tests are
table-driven, and a test of a refusal quotes its sentence from
docs/DESIGN.md section 7 word for word.

## Commits and pull requests

- The subject is at most 72 characters, says in the imperative what
  changed, and has no em dash. The body says why.
- A commit carries no AI attribution trailer. The commit hook refuses one.
- A pull request makes one change, and every changed line traces to it.
- A change that users see gets a line in CHANGELOG.md under Unreleased.

## Writing

Comments, messages and docs use full sentences and plain words, one idea
per sentence. A doc comment starts with the name it documents. Every
message to an operator says what failed, why, and what to do next.
tools/textgate.sh refuses em dashes and a list of filler words.
