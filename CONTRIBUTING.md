# Contributing

Thanks for taking an interest. This is a working tool for a production platform,
so the bar is less about style than about the two properties it must never lose.

## The two rules

**Authorization is never the model's job.** Every control is deterministic and
lives outside the prompt: arguments are checked before a tool runs, results are
filtered before the model sees them, and a PromQL query is pinned to the
caller's namespaces before it leaves the bot. A change that moves any of that
into an instruction — "the model should only report namespaces the user owns" —
will not be merged, however well it works in testing.

**Fail closed.** If scope cannot be verified, withhold. If a region's
authorization service is unreachable, refuse. A guess that happens to be right
is still the wrong behaviour.

## Before you open a pull request

```sh
make test    # go test -race ./...
make lint    # golangci-lint run ./...
make build
```

CI runs the same, with `golangci-lint` at `latest` — a newer linter than yours
can fail a build that passed locally.

## Tests

Write the test that would have caught the bug. For anything touching
authorization, write it as an attempt to get around the control rather than a
demonstration that the happy path works: `internal/agent/promql_test.go` is the
model — every case there is a way to read another tenant's metrics.

If a test passes before your fix, it is not testing your fix. Check.

## Commit messages

Explain why, not what. The diff already says what changed; what it cannot say is
the constraint you were working under, the option you rejected, or the failure
that prompted it. A future reader deciding whether they may change your code
needs that, and it is usually the difference between a safe change and a
regression.
