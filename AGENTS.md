# Buildkite CLI repository guide

Read the existing `AGENT.md` (singular) and `CONTRIBUTING.md`; their command layout, GraphQL, testing, and contribution rules remain applicable. `main.go` enters the CLI, `pkg/` contains command/client code, and the GraphQL schema/generation sources define API bindings. `.buildkite/` holds CI; release jobs are not ordinary validation.

`go.mod` declares Go 1.24 with toolchain 1.24.3, which is more current than the README's older minimum. Use that toolchain, `go mod download`, `go build ./...`, and focused `go test` packages or `go test ./...` as appropriate. The existing lint path runs golangci-lint through `.buildkite` Docker Compose; inspect its configuration and have Docker available before reproducing it. `go generate` can regenerate bindings and should be used only for relevant schema changes with generated diffs reviewed.

Offline build/tests and help should not need a real Buildkite token; live development commands need the documented GraphQL credential and authorized organization scope. Pipeline/build mutations and release/tag operations are external actions. Report mock/unit evidence separately from a real Buildkite API interaction, and preserve the upstream CI approval requirement for fork PRs.

## Completing work

Carry the authorized change through the relevant checks and repair failures it causes. Make routine, reversible implementation choices using existing patterns; ask only when missing information, a material product decision, or an authorization boundary prevents the next step. Existing authorization remains valid within its scope. If blocked, name the exact action and missing prerequisite, retain concise evidence, and continue independent work.

Choose verification proportional to the change. For instructions or prose, inspect changed paths, links, and local instruction precedence and run `git diff --check -- <changed-paths>`; don't install dependencies or run the application solely for a prose edit. For behavior changes, exercise the affected behavior and applicable checks below, then broaden only for failures or unresolved risk. Report files changed, checks actually run and their results, commands only inspected, and remaining limitations. A build or source inspection alone does not prove runtime behavior. Continue through already-authorized follow-through; stop at explicit review checkpoints or boundaries requiring new authorization.
