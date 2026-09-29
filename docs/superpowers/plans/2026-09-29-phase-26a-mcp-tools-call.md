# Phase 26a: MCP `tools/call` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The execution worker calls a certified MCP tool at most once, after checking the server's current definition against the certified one, and records a reference (never output).

**Architecture:** `mcp.Client` (today discovery only) gains `Execute` and `Lookup` so it satisfies `worker.Connector`, and `cmd/execution-worker` registers it under `mcp`. The job carries the tool's `remote_name` and the certified canonical definition; PostgreSQL gains contract rules that make every MCP contract single-attempt. The output canary rule (nothing of a tool's output is stored, journaled or logged) is tested end to end.

**Tech Stack:** Go, PostgreSQL triggers (goose migration 00025), `github.com/google/jsonschema-go` v0.4.3 (already an indirect dependency; `Schema.Resolve(nil)` then `Resolved.Validate(any)` verified in the module source), the official `modelcontextprotocol/go-sdk` v1.8.0 for interoperability.

**Spec:** [docs/superpowers/specs/2026-09-29-phase-26a-mcp-tools-call-design.md](../specs/2026-09-29-phase-26a-mcp-tools-call-design.md) (approved 2026-09-29). Read it and ADR-023 and ADR-030 first. Section numbers below (S3.4 and so on) refer to the spec.

## Global Constraints

- **At most one send.** Every MCP contract has `idempotency_mode = none` and `max_attempts = 1`, even `READ_ONLY`. A hint never permits a retry.
- **Certifiable no-effect classes only:** `connection_refused_before_send`, `unauthorized`, `invalid_payload`, `definition_changed`, `tool_missing`, `definition_unverified`, `unsupported_header_mirroring`, `mcp_rpc_32700`, `mcp_rpc_32600`, `mcp_rpc_32601`, `mcp_rpc_32602`, plus `mcp_tool_error` as a per-contract human choice. Anything else is `Ambiguous`.
- **External reference of a success:** `mcp:sha256:<64 hex>`, the SHA-256 of the result in RFC 8785 form (raw bytes if not canonicalizable). A digest, never content.
- **Output never leaves the worker:** no part of `content`, `structuredContent` or a server's error text in any log line, database column, journal entry or error string.
- **Transport:** Bearer only (an AWS-signing credential is refused, `unsupported_credential`); no redirect, no proxy, the token goes only to the connector's endpoint; revisions `2026-07-28` (`mcp.Modern`) and the legacy `initialize` fallback; discovery limits (50 pages, 500 tools, 4 MiB, 64 KiB per definition) apply to the pre-call listing.
- **Stay out of scope:** returning or storing output (26b), stdio, OAuth, sampling, `notifications/cancelled`, retries, a definition cache, `x-mcp-header` mirroring (the tool is refused, see Task 4).
- **Repository rules** (`AGENTS.md`): failing test first; `go vet ./... && go test -race ./...`; PostgreSQL suites need `EACP_TEST_ADMIN_DSN` (`docker compose up -d postgres`; skipped tests are not passes); never weaken a test to pass; commit as the user only, **no Co-Authored-By trailer**; files use LF and `gofmt -l` is clean; Thai text only in `*.th.md`; do not start the next phase.

## Review Focus

Inputs the spec implies but its rows do not spell out (each has a test in the task named):

1. A tool result larger than the response budget: `Ambiguous` `response_too_large`, no crash, no partial output kept (Task 3).
2. An enforced payload that is JSON `null`, an array or a string, not an object: nothing sent, `NoEffect` `invalid_payload` (Task 3).
3. A tool whose server name needs a derived EACP name (`Create.PO`): the call must use `remote_name`, not the EACP name (Task 6).
4. A reply delivered as a Server-Sent Events stream with unrelated notifications before it (`mcptest` `SetSSE`) (Task 3).
5. A legacy (`initialize`) server end to end, including closing its session, and a server that lists the certified definition with a different key order (must not count as drift) (Tasks 3 and 4).

---

### Task 1: ADR-032 and the verification note

**Files:**
- Create: `docs/adr/ADR-032-mcp-tools-call.md`
- Modify: `docs/adr/README.md` (index row), `research/REFERENCES.md` (a "Phase 26a" section)

**Interfaces:**
- Produces: the ADR number 032 and the recorded fact "the `x-mcp-header` value encoding is not specified in `docs/specification/2026-07-28/server/tools.mdx`", which Task 4 relies on.

- [ ] **Step 1: Write ADR-032** from the spec (Status: Accepted, Rev 1.0, 2026-09-29; Scope Phase 26a; Related ADR-001, 003, 004, 019, 023, 030). Sections: Context, Decision (S3.1 to S3.7, the classification table verbatim), Consequences, Unresolved assumptions (the spec's five open points and their defaults), Out of scope, Verification (the test names of Tasks 2 to 8). Amend ADR-023's "Out of scope for Phase 14" paragraph with one sentence pointing to ADR-032.
- [ ] **Step 2: Record the header finding** in `research/REFERENCES.md`: the specification page states the constraints on `x-mcp-header` but not how a value is encoded into `Mcp-Param-{name}`, nor when the header is required, nor the mismatch error; therefore Phase 26a refuses such tools. (Verified 2026-09-29 against the raw file on the `modelcontextprotocol` repository's main branch; `basic/transports.mdx` was not found at that path.)
- [ ] **Step 3: Verify** `go test -count=1 ./test/opensource/` passes (links and translation guards).
- [ ] **Step 4: Commit** `docs: ADR-032 MCP tools/call and the header-encoding finding`.

### Task 2: Single-attempt MCP contracts in PostgreSQL

**Files:**
- Create: `migrations/00025_mcp_call_contracts.sql`
- Test: `internal/registry/mcp_schema_test.go` (add `TestMCPContractsAreAtMostOnce`); fixtures in every test that certifies an MCP contract with `max_attempts > 1`

**Interfaces:**
- Produces: for an `mcp` connector's tool, `tool_contracts` inserts are refused (`23514`) unless `idempotency_mode = 'none'`, `max_attempts = 1` and every `no_effect_errors` element is in the certifiable list of Global Constraints (or matches `^mcp_rpc_(32700|32600|32601|32602)$`).

- [ ] **Step 1: Write the failing test** `TestMCPContractsAreAtMostOnce` (raw SQL as `eacp_app`, style of `TestMCPContractRules`): with an `mcp` connector and a scanned tool, an insert is refused (`wantState(t, err, sqlCheck)`) for each of: `max_attempts = 2`; `READ_ONLY` with `max_attempts = 3`; `idempotency_mode = 'correlation_only'`; `no_effect_errors = '{a2a_rejected}'`; `no_effect_errors = '{mcp_rpc_32603}'`. It is accepted for `'{IRREVERSIBLE_WRITE,FINANCIAL}'`, `'none'`, max 1, `no_effect_errors = '{definition_changed,mcp_rpc_32602,mcp_tool_error}'`. An HTTP tool's contract with `max_attempts = 3` and `idempotency_mode = 'native'` is still accepted, and so is an `a2a` `delegate` contract from its existing test.
- [ ] **Step 2: Run** `EACP_TEST_ADMIN_DSN=... go test -race -run TestMCPContractsAreAtMostOnce ./internal/registry/` and see it fail (the first refusal case is accepted).
- [ ] **Step 3: Implement** the migration: `CREATE OR REPLACE FUNCTION eacp.tool_contracts_guard()` as a copy of the current body (migration 00023, the second definition) with the three rules added inside `IF proto = 'mcp'`, error code `23514`, messages naming the rule. The migration is forward-only (`-- +goose Up`, `-- +goose StatementBegin/End` like 00023); `migrations_test.go` derives the latest version from the file count.
- [ ] **Step 4: Fix the fixtures** the rule now breaks (run `go test -race ./internal/registry/... ./internal/api/... ./internal/action/... ./internal/bundle/... ./test/demo/... ./cmd/eacpctl/...` with the DSN). Change the certified MCP contract's `max_attempts` to 1 and drop any `no_effect_errors` outside the list (`mcpReadContractSQL` uses 3 today). Change a fixture only where it is legitimately affected; never loosen the new rule.
- [ ] **Step 5: Run** the same suites; all pass.
- [ ] **Step 6: Commit** `feat(registry): MCP contracts are single-attempt with certifiable classes (ADR-032)`.

### Task 3: `mcp.Client.Execute` sends one `tools/call` and classifies it

**Files:**
- Create: `internal/connector/mcp/execute.go`, `internal/connector/mcp/execute_test.go`
- Modify: `internal/connector/mcp/mcptest/server.go` (serve `tools/call`), `internal/connector/mcp/client.go` (a field `Log *slog.Logger`; distinguish connection refused), `internal/worker/connector.go` (two `Call` fields)
- Depends on: none. The pre-call definition check is Task 4, so this task's tests pass a `Definition` equal to what the fake server lists, and `Execute` does not yet compare it.

**Interfaces:**
- Produces, in `worker.Call`: `RemoteName string` (the server's exact tool name) and `Definition string` (the certified canonical definition, RFC 8785 text).
- Produces: `func (c *Client) Execute(ctx context.Context, call worker.Call) worker.Result` and `func (c *Client) Lookup(context.Context, worker.LookupCall) worker.LookupResult` (always `LookupUnknown`), so `*Client` satisfies `worker.Connector`.
- Produces, in `mcptest`: `type Reply struct { Result any; RPCStatus int; RPCCode int; RPCMessage string; Delay time.Duration }`, `func (s *Server) OnCall(f func(name string, arguments json.RawMessage) Reply)`, and `func (s *Server) Calls() []CallRequest` where `CallRequest` has `Name string`, `Arguments json.RawMessage` and `Header http.Header`. `tools/call` is served by the modern and the legacy path, honours `SetSSE`, and validates the same headers as `tools/list`.

- [ ] **Step 1: Write the failing tests** in `execute_test.go` (package `mcp_test`, helpers `secret`, `token`, `mcptest.New` as in `client_test.go`). One table test `TestExecuteClassifiesEveryReply` runs each row of S3.4 against `mcptest` and asserts `Outcome` and `ErrorClass`: a success gives `Succeeded` with a reference matching `^mcp:sha256:[0-9a-f]{64}$` equal to the SHA-256 of `governance.Canonicalize(result)`; `isError: true` gives `Ambiguous` `mcp_tool_error`; `structuredContent` breaking the certified `outputSchema` gives `Ambiguous` `mcp_output_invalid`; `resultType: "input_required"` gives `Ambiguous` `mcp_input_required` and exactly one `tools/call` reached the server; JSON-RPC errors 32700, 32600, 32601 and 32602 give `NoEffect` `mcp_rpc_<code>`, while -32603 and -32020 give `Ambiguous`; 401 gives `NoEffect` `unauthorized`; a closed port gives `NoEffect` `connection_refused_before_send`; a handler that sleeps past the context deadline gives `Ambiguous` `timeout`; a malformed JSON body gives `Ambiguous` `invalid_response`; a body over the 4 MiB budget gives `Ambiguous` `response_too_large` (Review Focus 1). Separate tests: `TestExecuteRefusesAPayloadThatIsNotAnObject` (`null`, `[]`, `"x"`, `1`: `NoEffect` `invalid_payload`, `Calls()` empty; Focus 2); `TestExecuteRefusesAnUnsafeContract` (`IdempotencyMode` other than `none` or `MaxAttempts != 1`: `Ambiguous` `invalid_contract`, no request at all); `TestExecuteRefusesASigningCredential` (`unsupported_credential`); `TestExecuteWorksOverSSE` and `TestExecuteWorksOnALegacyServer` (Focus 4 and 5; the legacy session is closed afterwards: a `DELETE` reaches the server); `TestExecuteNeverLeaksOutput`: a canary string in `content`, `structuredContent` and the error message of a failing reply appears nowhere in the returned `Result` fields nor in a captured `slog` buffer at debug level, and the bearer token appears in no log line; `TestExecuteSendsOnceAndNeverFollowsARedirect` (a 307 is `Ambiguous`, the redirect target receives nothing, and `Calls()` has length 1 for every table row).
- [ ] **Step 2: Run** `go test -race ./internal/connector/mcp/ -run 'TestExecute'` and see them fail to compile or fail.
- [ ] **Step 3: Implement** `Execute` in `execute.go` on the existing `session` (`probe` then `initialize` fallback, `call`). Steps in order, each returning early: contract check; endpoint and credential checks as `Discover`; payload must decode to a JSON object; open the session; send `tools/call` with `params{name: call.RemoteName, arguments: <payload>}` (the modern `_meta` and headers come from `session.call`); map the result or error by S3.4. Decisions the tests do not fix: validate `structuredContent` with `jsonschema-go` (`Schema` unmarshalled from the certified definition's `outputSchema`, `Resolve(nil)`, `Validate` on the decoded value); an `outputSchema` that does not resolve is `mcp_output_invalid`; compute the reference from `governance.Canonicalize` of the raw result. Distinguish connection refused in `session.post` (`errors.Is(err, syscall.ECONNREFUSED)` with a live context) without changing the class or message discovery records today. Log only the host, action id, class and the reference; never a result or an error text. `Lookup` returns `worker.LookupResult{Status: worker.LookupUnknown}`.
- [ ] **Step 4: Extend `mcptest`** as in Interfaces; `tools/call` on an unknown tool answers -32602 (the specification's unknown-tool error), keeping the existing tests green.
- [ ] **Step 5: Run** `go test -race ./internal/connector/mcp/...`; all pass. Run `go mod tidy` so `github.com/google/jsonschema-go` becomes a direct requirement.
- [ ] **Step 6: Commit** `feat(mcp): call a tool once and classify the reply (ADR-032)`.

### Task 4: The definition check before every call, and the header refusal

**Files:**
- Modify: `internal/connector/mcp/execute.go`, `internal/connector/mcp/execute_test.go`

**Interfaces:**
- Consumes: `worker.Call.RemoteName`, `worker.Call.Definition` (Task 3); the existing `session.listTools(ctx, serverInfo) (worker.Discovery, error)` and `canonicalTools`.
- Produces: `Execute` runs the check (S3.5) after the session opens and before `tools/call`. Task 3's tests keep passing because they already supply a matching `Definition`.

- [ ] **Step 1: Write the failing tests:** `TestAChangedDefinitionIsNeverCalled` (the server lists the tool with a changed description, then with a changed `inputSchema`, then with a new `annotations`: `NoEffect` `definition_changed`, `Calls()` has no `tools/call`, a `slog` record with alert `worker.mcp_definition_changed` exists); `TestAMissingOrRejectedToolIsNeverCalled` (not listed, and listed with an invalid `x-mcp-header` so the scanner's rules reject it: `NoEffect` `tool_missing`); `TestAnUnverifiableListingIsNeverCalled` (a failing second page, a 4 MiB overrun, a hang: `NoEffect` `definition_unverified`, no call); `TestTheSameDefinitionInAnotherKeyOrderIsNotDrift` (the server lists the certified object with keys reordered and extra whitespace: the call is sent; Focus 5); `TestAToolOnALaterPageIsFound` (`SetPageSize(1)`, the tool is the third); `TestAnEmptyCertifiedDefinitionIsRefused` (`Definition == ""`: `NoEffect` `definition_unverified`, nothing sent); `TestAToolWithHeaderMirroringIsRefused` (a certified `inputSchema` with `x-mcp-header` on a string property: `NoEffect` `unsupported_header_mirroring`, no request reaches the server at all, checked before the session opens).
- [ ] **Step 2: Run** `go test -race ./internal/connector/mcp/ -run 'TestAChanged|TestAMissing|TestAnUnverifiable|TestTheSame|TestAToolOnALater|TestAnEmpty|TestAToolWithHeader'` and see them fail.
- [ ] **Step 3: Implement** the check with the same session that will call: list every page, find the tool by `RemoteName` in `Discovery.Tools` (rejected tools are not in it, so they are `tool_missing`), compare `DiscoveredTool.Definition` to `call.Definition` as strings. Refuse `x-mcp-header` by parsing `call.Definition` before any network use.
- [ ] **Step 4: Run** the whole package with `-race`; all pass.
- [ ] **Step 5: Commit** `feat(mcp): check the server's definition against the certified one before every call`.

### Task 5: The worker serves `mcp`

**Files:**
- Modify: `internal/worker/store.go` (`Job`, `Load`), `internal/worker/worker.go` (the `Call` literal), `cmd/execution-worker/main.go`
- Test: `internal/worker/store_mcp_test.go` (new, PostgreSQL)

**Interfaces:**
- Consumes: `worker.Call.RemoteName/Definition` (Task 3).
- Produces: `Job.RemoteName string` and `Job.Definition string`, both empty for a non-MCP tool; the `Call` handed to the connector carries them; `mcp` is in the worker's `Connectors` map and shares one `*mcp.Client` with the scanner's `Discoverers`.

- [ ] **Step 1: Write the failing test** `TestLoadCarriesTheCertifiedDefinition` (the `scanEnv`/`registrytest` style of `scanner_test.go`): after a scan and a certified contract for a tool named `Create.PO`, `Store.Load` returns `RemoteName == "Create.PO"` and `Definition` byte-equal to `eacp.tool_definitions.definition` of the contract's `definition_id`; for an HTTP tool both are empty.
- [ ] **Step 2: Run** it with the DSN and see it fail (fields missing).
- [ ] **Step 3: Implement** `Load` with `LEFT JOIN eacp.tool_definitions d ON d.tenant_id = k.tenant_id AND d.id = k.definition_id` and `COALESCE(t.remote_name, '')`, `COALESCE(d.definition, '')`; pass both into `Call` in `execute`. In `cmd/execution-worker/main.go` build `mcpClient := mcp.New(); mcpClient.Log = d.Log` and use it for `connectors["mcp"]` and `Discoverers["mcp"]`.
- [ ] **Step 4: Run** `go vet ./... && go test -race ./internal/worker/ ./cmd/...` with the DSN; all pass.
- [ ] **Step 5: Commit** `feat(worker): serve protocol mcp`.

### Task 6: The real worker end to end

**Files:**
- Create: `internal/worker/mcp_integration_test.go`

**Interfaces:**
- Consumes: everything above. Model it on `a2a_integration_test.go`: an `mcpEnv` (registrytest fixture, `mcptest` server, a `SecretStore` bound to the server's host, a `worker.Scanner` with `Discoverers{"mcp": client}`, a certified contract for the discovered tool with `no_effect_errors` and `max_attempts = 1`, an active agent, `action.Engine`, `worker.Worker` with `Connectors{"mcp": client}`), with a helper `call(args map[string]any) action.View` that submits and runs the worker once.

- [ ] **Step 1: Write the failing tests:** `TestTheWorkerCallsAnMCPTool` (a `Create.PO` tool, Focus 3: the server receives `name == "Create.PO"` with the arguments byte-equal to the enforced payload; the action is `SUCCEEDED` with an external reference `mcp:sha256:...`; one `tools/call` reached the server); `TestAChangedDefinitionBetweenScansIsNeverCalled` (change the server's tool after the scan, before the call: the attempt is `no_effect` `definition_changed`, no `tools/call` reached the server, and after the next scan the tool is quarantined); `TestAnMCPToolErrorIsUnknownUntilCertified` (`isError`: `UNKNOWN_OUTCOME` when the contract does not certify `mcp_tool_error`; a failed no-effect attempt when it does); `TestAnMCPActionIsNeverRetried` (a transport reset after send: exactly one `tools/call`, the action is `UNKNOWN_OUTCOME`, still exactly one after the worker runs again); `TestAKillDuringAnMCPCallIsUnknown` (the server holds the call, the tenant kill is set: the attempt is ambiguous and the action `UNKNOWN_OUTCOME`); `TestNothingOfTheOutputIsPersisted` (a canary in `content`, `structuredContent` and an error text: it appears in no log line and in no text column of any table, using the dump helper of `TestNothingSecretIsPersistedByADelegation`); `TestAWorkerWithoutMCPLeavesTheActionQueued`.
- [ ] **Step 2: Run** `go test -race -run 'MCP' ./internal/worker/` with the DSN and see the new tests fail.
- [ ] **Step 3: Fix** whatever they expose in the production code of Tasks 3 to 5 (do not weaken a test).
- [ ] **Step 4: Run** `go test -race ./internal/worker/...`; all pass.
- [ ] **Step 5: Commit** `test(worker): an MCP tool is called once, never on a changed definition, and its output is never stored`.

### Task 7: Interoperability with the official Go SDK server

**Files:**
- Modify: `internal/connector/mcp/interop_test.go`

- [ ] **Step 1: Read** `interop_test.go` for how it builds the SDK server, and read the SDK's tool registration and `CallToolResult` types in the module source (`go list -m -f '{{.Dir}}' github.com/modelcontextprotocol/go-sdk`); do not assume names.
- [ ] **Step 2: Write** `TestExecuteCallsAToolOfTheOfficialSDKServer`: discover the SDK server's tool, then `Execute` with the discovered definition and a valid argument object; expect `Succeeded` with an `mcp:sha256:` reference; a call with an invalid argument (wrong type) expects the SDK's error mapped by S3.4 (protocol error 32602 as `NoEffect`, or `isError` as `mcp_tool_error`, whichever the SDK returns; assert what it actually returns and record it in a comment).
- [ ] **Step 3: Run** `go test -race ./internal/connector/mcp/ -run Interop`; all pass.
- [ ] **Step 4: Commit** `test(mcp): tools/call against the official Go SDK server`.

### Task 8: Fake MCP `tools/call`, the Slice C demo and the network test

**Files:**
- Modify: `internal/fakemcp/fakemcp.go`, `internal/fakemcp/fakemcp_test.go`, `test/demo/slice_c_test.go`, `test/security/` (only if an isolation test names the MCP server)

**Interfaces:**
- Produces: `fakemcp` answers `tools/call` for `get_po` (arguments `{"id": string}`, result `content` with a text and an `isError: false`; an `id` of `"ERR"` gives `isError: true`) and keeps a durable call log like `fakea2a`, content-free (tool name, count and time only).

- [ ] **Step 1: Write the failing test** in `fakemcp_test.go` for `tools/call` (success, `isError`, unknown tool -32602, missing bearer 401, the log holds no argument value).
- [ ] **Step 2: Implement** it to pass.
- [ ] **Step 3: Extend the demo** (`TestSliceCDemo`, helpers of that file): before the drift, `erin` proposes and `rita` activates a contract for `sap-mcp.get_po` (`READ_ONLY`, `max_attempts` 1), `po-assistant` calls it and the action succeeds with a `mcp:sha256:` reference; after the drift but before the rescan (`d.drift` replaces the tool list), a second call is `no_effect` `definition_changed` and the fake server's call log shows one call in total. Log each step with `d.logf` in the file's style.
- [ ] **Step 4: Verify** `go test -race ./internal/fakemcp/`, then `DEMO=C bash scripts/demo.sh` if Docker is available (state plainly in the report if it was not run), and `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` against a running stack.
- [ ] **Step 5: Commit** `feat(demo): the Slice C demo calls an MCP tool and refuses a rug pull`.

### Task 9: Documents and the final check

**Files:**
- Modify: `AGENTS.md` (replace "No worker serves protocol `mcp` yet: do not add `tools/call` without an ADR." with the ADR-032 rule: single attempt, definition check before every call, reference only, output never stored), `docs/FEATURES.md` and `docs/FEATURES.th.md`, `docs/DEMO.md` and `docs/DEMO.th.md` (wherever they say MCP tools cannot be called; same headings and code blocks in both), `CHANGELOG.md` (Unreleased), `docs/MASTER_PLAN.md` (a status line for Phase 26a), `THIRD_PARTY_NOTICES.md` (only if the test guard asks: `jsonschema-go` is already listed)

- [ ] **Step 1: Update** the documents above; Thai only in the `.th.md` files.
- [ ] **Step 2: Run the whole verification:** `go vet ./... && EACP_TEST_ADMIN_DSN=... go test -race -count=1 -timeout 45m ./...`, `go test -count=1 ./test/opensource/ ./test/invariants/`, `gofmt -l .` (empty). Report every failure by name, including any you did not cause.
- [ ] **Step 3: Commit** `docs: Phase 26a, MCP tools/call (ADR-032)`. Then stop and report; do not start Phase 26b.
