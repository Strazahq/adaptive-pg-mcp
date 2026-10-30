.PHONY: hooks check gate text state gogate go spec security pgtest

# Bind the gate once per clone. The hooks live in the tree so they travel.
hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/* tools/*.sh
	@echo "hooks bound: pre-commit, commit-msg and pre-push run from .githooks"

# Most of the checks the commit gate runs, by hand. The hook also caps Go
# files at 600 lines.
check: gate

gate: text state gogate go

text:
	@sh tools/textgate.sh && echo "text gate: clean"

state:
	@sh tools/stategate.sh docs/STATE.md && echo "state gate: docs/STATE.md is on-grammar"

gogate:
	@sh tools/gogate.sh && echo "go gate: clean"

go:
	@if [ -f go.mod ]; then unformatted=$$(gofmt -l .); [ -z "$$unformatted" ] || { echo "$$unformatted"; echo "go: run gofmt -w on the files above"; exit 1; }; go vet ./... && go test ./... ; else echo "go: no module yet"; fi

# The database tests on Postgres 14 to 18 in local Docker, as the push gate runs them.
# The harness is always removed again, also when a test fails.
pgtest:
	@sh tools/pg.sh up || { sh tools/pg.sh down; exit 1; }; \
	status=0; go test -tags pg ./... || status=$$?; \
	sh tools/pg.sh down || status=1; \
	exit $$status

# The MCP specification revision and the go-sdk release, as the push gate checks them.
spec:
	@sh tools/specgate.sh && echo "spec gate: on the newest revision and SDK"

# What the push gate runs, by hand.
security:
	gitleaks git --no-banner --redact .
	@if [ -f go.mod ]; then govulncheck ./... && osv-scanner scan source -r . ; fi
	@sh tools/specgate.sh && echo "spec gate: on the newest revision and SDK"
