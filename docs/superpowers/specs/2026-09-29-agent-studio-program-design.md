# Agent Studio and Agent Hub, and the missing tool-calling paths (program design)

Date: 2026-09-29 · Status: design agreed with the owner in chat, section by section; awaiting the owner's review of this file
Reference study: `research/STUDIO_REFERENCES.md` (n8n, Copilot Studio, Dify, the MCP specification, Radix, shadcn, Primer)
Scope: a program of five phases, each with its own ADR, spec, plan and review. This document fixes the shared
decisions. It is not an implementation spec for any single phase.

## 1. Intent

Employees who are not developers should be able to build an agent for their own department (the pattern of
n8n and Copilot Studio) and share it through an **Agent Hub** at three levels: private, department, organisation.

The project also lists what it does not do yet (README, "not built yet"). Three of those items block or extend
this work and are in the program: executing MCP tools (`tools/call`), the `run` kill scope and inbound A2A.
Multi-region, bypass detection and personal-data classification are **not** in it.

Success:

- an HR employee builds an onboarding agent from a form, sends it for approval, and a department lead publishes
  it; a colleague in the department runs it; every side effect went through the Action API;
- the employee could not give the agent a tool, publish it to the organisation or approve their own request at
  any point, and PostgreSQL, not Go, refused each attempt;
- a clone of a published agent carries no permission with it.

## 2. Principles (non-negotiable)

1. **A client of EACP, never an authority** (ADR-001). Studio adds no way to reach an enterprise system. Agents it
   runs hold no enterprise credential; every side effect is an action submitted through `POST /v1/actions`.
   ADR-033 records that this layer is a governed builder on top of the control plane and does not turn EACP into
   a competing workflow engine (MASTER_PLAN §114 is about positioning, not about forbidding a builder).
2. **Rules live in PostgreSQL** (ADR-003 §8). Every guard below is a trigger, tested with raw SQL as `eacp_app`.
3. **Employees choose tools, never permissions.** The capability an agent needs is computed from its steps and
   goes to a registry approver as an allowlist request.
4. **No secret in a definition.** A definition that contains a value that looks like a secret is refused.
5. **Fail closed** at every step (section 5).
6. **Never claim exactly-once** (MASTER_PLAN §21). A replayed step is deduplicated by its idempotency key.
7. One phase at a time; stop and report after each (AGENTS.md).

## 3. Decisions made in the brainstorm

| Question | Decision |
|---|---|
| Builder style | A form (Copilot Studio style) first. The definition is one document, so a canvas is a later second view of it. |
| How an agent works | Hybrid. The author fixes the steps (n8n style); some steps call an LLM. The LLM never chooses a tool. |
| Publishing | Tiered: private is free; department needs a department lead; organisation needs an admin or a `registry_approver`. The proposer never approves their own listing. |
| Using a Hub agent | Both. **Run** uses the published version. **Clone** makes an editable copy with its own approvals. |
| Architecture | Data and rules in `controlplane-api` (`internal/studio`, migrations, triggers). A new binary, `agent-runtime`, runs agents. |
| Missing pieces in scope | `tools/call` (Phase 26), the `run` kill scope (after 27a), inbound A2A. |
| Order | A thin slice first (27a), a trial with departments (the gate), then the Hub and the full builder (section 8). |

## 4. The agent definition

One JSON document per version, immutable once saved. PostgreSQL computes its digest and the capability set.

```text
schema_version, kind: "agent"      (a newer major version is refused, never guessed)
name, description, department (an eacp group), owner
trigger:  manual | chat | schedule                 (webhook: later)
inputs:   a schema
steps:    an ordered list, four kinds in the first release
  tool_call   a registered tool, through the Action API only
  llm         instruction and inputs; the output must match a schema; no tools
  branch      a fixed condition on earlier outputs; the LLM never decides
  respond     the reply, and the end of the run
limits:   max steps, max tokens, timeout, budget account
```

- No loops or `foreach` in the first release. Each is a new step kind with a hard iteration cap, added later.
- **Derived capability.** The set of tools named by `tool_call` steps, and of models named by `llm` steps, is
  computed by PostgreSQL when a version is saved and becomes an allowlist request. Until a `registry_approver`
  who is not the owner approves it, the version cannot be ACTIVE, so it cannot call a tool or a model.
- An `llm` output that does not match its schema fails the run. It is data; it never picks the next tool.

## 5. The runtime (`agent-runtime`)

> **Dependency found 2026-09-29:** no connector returns a tool's output today (HTTP and A2A return only a reference), so the per-step results below need Phase 26b, the result channel, before 27a. **Delivered 2026-09-30 (ADR-034):** a contract may keep a success's output for the calling agent (`GET /v1/actions/{id}/result`).

A binary like `execution-worker`. It claims a run under a lease and a fence, executes steps in order and records
each step's result before the next, so a crash resumes at a step boundary.

- `tool_call`: `POST /v1/actions` as the agent, with an `Idempotency-Key` derived from `run_id` and `step_id`.
- `llm`: through the LLM gateway (ADR-031) as the same agent. The gateway already enforces the model allowlist, the
  budget, the model kill scope and the ledger. **The runtime holds no LLM provider key.**
- `branch`, `respond`: in process, no side effect.
- Run states: `QUEUED`, `RUNNING`, `WAITING_APPROVAL`, `SUCCEEDED`, `FAILED`, `CANCELLED`, `NEEDS_HUMAN`.
- Fail closed: an action `DENIED` fails the run at that step with the reason; `UNKNOWN_OUTCOME` stops the run at
  `NEEDS_HUMAN` and never continues past it; an action that needs approval makes the run `WAITING_APPROVAL`, and
  the run resumes when the action's state changes (a run never decides an approval).
- Free from what exists: the PDP, approvals, budgets, kill scopes, FinOps and incidents all see a Studio agent as
  an agent.
- **Schedule** runs are created by a system actor `studio_scheduler` that takes `storage.TryLoopLock` per tenant
  (ADR-029), only for a version that is ACTIVE.
- `started_by` is recorded on every run and attached to every action as evidence. Who may start a run comes from
  the Hub scope and is checked in PostgreSQL when the run is created.

### 5.1 The agent's credential (corrected after reading `credentials_guard`)

An agent authenticates with an `ak` key (`eacp_ak_<tenant>_<credential>_<secret>`). The registry rules are:

- the creator of an agent credential must be a `registry_editor` or `registry_approver`;
- a **second person**, a `registry_approver` other than the proposer, approves it;
- it expires within 90 days and its hash, subject and expiry never change.

Design that satisfies them and stores no key:

- The credential id is chosen by the key holder (bring your own key). The runtime chooses it and derives the
  secret as `HMAC-SHA-256(master_secret, tenant_id || credential_id)`, then registers only the hash.
  The master secret exists only in the runtime (env or file, through the ADR-019 providers) and is redacted through
  `logging.SecretSet`. No key is stored; no employee ever sees one.
- The runtime has a service principal with a single-purpose role, `studio_runtime` (corrected by ADR-033:
  `registry_editor` would let it change connectors, tools, contracts and releases). It proposes the
  credential when the capability request for a version is approved; the approver of that request, who is a
  different person, approves the credential. Both approvals are visible in one queue.
- **Rotation.** The runtime proposes a new credential at 60 days. If nobody approves it before the old one
  expires, the agent's runs fail closed with `credential_expired` and an alert is raised (open point 4).

## 6. The Hub

```text
draft (mutable, owner only)
  -> saved version (immutable; digest and capability set computed by PostgreSQL)
  -> allowlist request approved by a registry_approver who is not the owner -> ACTIVE
  -> proposed to the Hub -> approved by scope -> PUBLISHED
```

- **Scopes.** `PRIVATE` needs no listing. `DEPARTMENT` is bound to one group; its approver is a lead of that group.
  `ORG` is approved by an `admin` or `registry_approver`.
- The proposer never approves their own listing. Only an ACTIVE version can be proposed, so the Hub never shows
  something that cannot run.
- Listing states: `PROPOSED -> PUBLISHED -> WITHDRAWN | DEPRECATED`. `published_version_id` moves forward when a
  newer version's proposal is approved, and back under the same approver rule. A withdrawn listing creates no new
  run; a run in progress finishes, and the `agent` or `version` kill scope stops it at once when that is needed.
- **Visibility is enforced by PostgreSQL.** PRIVATE: the owner. DEPARTMENT: members of the group. ORG: the tenant.
- **Run** creates a run on `published_version_id`. The database checks that the caller can see a PUBLISHED listing.
- **Clone** copies the definition into a new draft owned by the cloner, records `cloned_from`, and carries no
  permission. Its tools need a new allowlist request, so a clone cannot avoid the original's approval. A clone also
  drops what is bound to the original's environment: the schedule trigger arrives **disabled with no schedule**
  and any webhook URL is cleared (Dify does the same on export; `research/STUDIO_REFERENCES.md` section 1).
- **Run is not edit.** A listing grants the right to run and nothing else (Copilot Studio separates chat from
  co-authoring; n8n's viewer cannot execute). Co-authoring is a later phase.
- **No overwriting a colleague.** A draft carries a `revision`; a save with a stale revision is refused and the
  builder offers "save as copy" (the Copilot Studio behaviour).
- **Templates** are listings that an admin publishes with the tag `template` and seeds with a bundle (ADR-026).
  There is no separate templates system.
- **Discovery** (first release): search by name, tag and department, and a run count. Cost comes from FinOps.
  No ratings.
- **Departments** are groups. A group has no notion of a lead today (membership changes are admin-only), so a
  membership gets a `lead` flag, set only when an admin adds the membership; changing it means removing and
  adding the membership. An admin can never be locked out: the guard reuses the existing `admin` role check.

## 7. The builder (`/studio/`)

An embedded page, served like the operator console and under the same rules (ADR-028): no build step, strict CSP,
DOM only through `dom.js`, the key in memory only, every write through `confirm.js`, every route in `api.js`
ROUTES with `TestEveryConsoleCallIsARealRoute`. Five screens:

1. **Basics:** name, department, description.
2. **Trigger and inputs.**
3. **Steps:** an editor with a form per kind. The tool picker lists only registry tools, with their risk and
   "needs approval" badges.
4. **Test:** a draft is not ACTIVE, so it cannot call a tool. A test runs `llm`, `branch` and `respond` for real
   and uses sample output, typed by the author, for each `tool_call`.
5. **Review and submit:** shows the derived capability request and sends it.

Employees sign in with their principal API key, held in session memory like the console. SSO and OIDC do not
exist yet and are a later phase (open point 2).

## 8. The phases

Revised 2026-09-29 (the thin slice). The first version of this plan built the Studio, the Hub and the full builder
in order, all from reading and no user feedback. The risk is a large build that departments then want done another
way. The plan now proves one narrow path end to end, puts it in front of real departments, and only then builds
the wider parts.

| Phase | Content | ADR | State |
|---|---|---|---|
| 26a | **MCP `tools/call`:** the execution worker calls a certified MCP tool at most once, checking the server's current definition against the certified one before every call; it returns a reference, never output ([design](2026-09-29-phase-26a-mcp-tools-call-design.md)) | 032 | done (`608c9a1`) |
| 26b | **The result channel:** a governed way for the calling agent to read a tool's output (bounded, retained for a limited time, never in logs or the audit journal), for HTTP, MCP and A2A alike. Added after the review found that no connector returns output and section 5's per-step results and the leave-balance template need it ([design](2026-09-30-phase-26b-result-channel-design.md)) | 034 | done (branch `phase-26b-result-channel`) |
| 26-UI | Console design system and a redesign of every existing view (section 8.2) | 028 Rev 1.1 | done (`e275c4a`) |
| 27-0 | **Credential ADR first:** how `agent-runtime` obtains an agent's credential (section 5.1), its threat model, custody of the master secret, rotation and the privilege it holds. Written and reviewed before any 27a code ([ADR-033](../../adr/ADR-033-agent-studio-and-runtime-credentials.md)) | 033 | done (accepted 2026-09-30) |
| 27a | **The thin slice** (section 8.3; its template reads a result, so it needs 26b): definition, immutable versions, derived capability, `agent-runtime`, agent credentials, and one form with one template, from creation through approval to a run | 033 Rev 1.1 | after 26b and 27-0 |
| **Gate** | Two or three departments try the thin slice (section 8.4). What they say decides what 27b and 27c contain | none | required |
| 27b | Agent Hub: listings, scopes, tiered approval, run, clone, department leads. Reordered or reshaped by the gate | 033 Rev 1.2 | after the gate |
| 27c | The full builder, more templates and the end-to-end demo on compose and Kubernetes | 033 Rev 1.3 | after the gate |
| 28 | The `run` kill scope, using the authenticated run binding a Studio run provides | 016 revision | after 27a |
| 29 | Inbound A2A: accept delegations from remote agents | 030 revision | independent, can move |

Phase 26a comes first because most enterprise integrations an employee will pick are MCP tools, and today an action
on one expires unclaimed. Phase 29 does not depend on Studio and can move. **The `global` scope** stays out: it
waits for platform authority, not for run bindings. Each phase still gets its own brainstorm, ADR and plan, and
the repository rule stands: stop and report after every phase.

### 8.3 What the thin slice contains

One path, complete, and nothing beside it:

- **Steps:** `tool_call` and `respond` only. The `llm` and `branch` kinds wait for the gate, so the first proof has
  no model in it and every step is deterministic and testable.
- **One form**, not the full builder: name, department, the tools it may use, the steps in order, and a Save that
  makes an immutable version. It lives at `/studio/` under the same rules as the console (ADR-028).
- **The approval path is part of the slice, not an afterthought.** After Save the page shows where the agent is:
  waiting for a capability request, waiting for a second approver (named by role), approved, active. A refusal shows
  the server's reason. The two-person rule stays; the employee is never left guessing (this is the largest UX risk
  the review found).
- **One template**, chosen for a real department with one MCP or HTTP tool (for example a leave-balance lookup for
  HR), used to run the slice in the demo.
- **Run and see:** start a run, follow its steps from the journal, see a `NEEDS_HUMAN` or `DENIED` outcome plainly.
- **Left out on purpose:** the Hub, listings, clone, department leads, the `llm` step, branches, schedules,
  webhooks, the canvas, and any second template.

### 8.4 The gate

After 27a, before 27b or 27c, run the slice with two or three departments the owner picks (the demo stack with
their own tool, or a staging tenant). It passes when:

1. an employee who did not build the platform creates an agent from the form without help;
2. they understand where it is in the approval path without asking;
3. the approver understands what they are approving (the derived capability list, in plain words);
4. we have written down what they wanted that the slice does not do.

The notes go into an amendment of this spec. If departments want a template gallery before sharing, 27b shrinks to
a curated list; if they want an LLM step first, that moves ahead of the Hub. Nothing after the gate is fixed until
this is done. If the owner cannot arrange a trial, the fallback is the demo scenario of section 9 run by the owner
alone, and the gate is recorded as not met.

### 8.1 Phase 26a constraints already known (ADR-023, AGENTS.md, `research/STUDIO_REFERENCES.md` section 2)

The MCP contract pins the tool's current `definition_id`; the worker must re-check the fingerprint before every
call and refuse a quarantined tool. An MCP call is **always** at-most-once: the protocol has no idempotency key and
`idempotentHint` is an untrusted hint (the specification says clients must not trust annotations), so a hint never
permits a retry. A transport failure after the request was sent is `UNKNOWN_OUTCOME` (ADR-004). A result with
`isError: true` is a completed, failed outcome. `input_required` and a `structuredContent` that breaks the
`outputSchema` are refused with a reason; the worker never answers a request for more input. The stdio transport
and OAuth authorization flows stay out of scope. Phase 26a has its own spec (linked in the table).

### 8.2 The console redesign (added after the reference study)

The console is plain today (`internal/ui/static/app.css` is 5.8 KB: no icons, no empty states, counts shown as
underlined links). Studio's builder will live beside it, so the two share one design system, built first:
semantic token pairs, the 12-step colour meaning, type by role, system fonts only, inline SVG icons through `dom.js`
(`research/STUDIO_REFERENCES.md` section 3). It changes no route, permission or security rule (ADR-028). It is its
own phase, **Phase 26-UI**, and is delivered (`e275c4a`, ADR-028 Rev 1.1).

## 9. Testing

Every rule is written as a failing test first and run with `-race`.

- **PostgreSQL guards, raw SQL as `eacp_app`:** the owner cannot approve their own capability request or listing;
  a proposal of a non-ACTIVE version is refused; a clone carries no allowlist entry; visibility per scope; a
  department listing needs a lead of that group; an admin-added membership is the only way to set `lead`.
- **Runtime:** a crash resumes at the same step with the same idempotency key; `UNKNOWN_OUTCOME` becomes
  `NEEDS_HUMAN`; an `llm` output outside its schema fails the run; a run never continues after `DENIED`; an
  expired credential fails closed.
- **Catalogues:** new tables, policies and SECURITY DEFINER functions go into `internal/storage/rls_catalog_test.go`
  and every new [A] invariant into `docs/INVARIANTS.md`.
- **UI:** the console's route, sink and file-list tests extended to `/studio/`.
- **Demo:** the HR scenario of section 1 with `fakellm`, `fakeerp` and `fakemcp`, on compose and on Kubernetes.
- **Deployment:** an `agent-runtime` Deployment in the Helm chart with a NetworkPolicy, tested with `test/helm`.

## 10. Out of scope for the first release

A canvas, loops, a webhook trigger, knowledge and retrieval, ratings, sharing across tenants, SSO, an LLM that
chooses tools, multi-region, bypass detection and personal-data classification.

## 11. Open points (each has a conservative default; the owner may overrule)

1. **Master secret custody.** Default: an env or file secret read by the runtime through the ADR-019 providers.
   Losing it means every Studio credential is re-issued (new ids, new approvals).
2. **Employee sign-in.** Default: a principal API key in session memory, as the console does. OIDC is later.
3. **Department lead.** Default: a `lead` flag on the membership, admin-set at insert.
4. **Credential rotation.** Default: human approval on every rotation and a fail-closed expiry with an alert. A
   standing pre-approval would weaken the two-person rule and is not proposed.
5. **`started_by` and authority.** The caller's identity is evidence and never widens what the agent may do; the
   agent's own allowlist is the only authority. Whether the policy (PDP) should also see the caller is decided in
   the phase 27a spec.
7. **Who joins the gate.** The owner names the two or three departments and the tool each will use. Default: HR
   with a leave-balance lookup, plus one department that uses an MCP tool. Without them the gate is recorded as not met.
6. **Numbers.** ADR-032 and ADR-033 are the next free numbers on `main` (ADR-031 is the LLM gateway; migrations
   run to 00024). They are assigned when the ADRs are written.
