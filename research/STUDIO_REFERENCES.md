# Reference systems for Agent Studio, the Agent Hub and the console (verified)

Read on 2026-09-29 from the owners' own sources: the projects' repositories (through `gh api`), their documentation
repositories and the specification repository. Each fact names the file or page it came from. Nothing here is
quoted at length: these notes are ours.

**Licences decide what we may take.** We take ideas, never code or visual design, from the two workflow
products.

| Source | Licence (read from its `LICENSE`) | Consequence |
|---|---|---|
| n8n (`n8n-io/n8n`, `LICENSE.md`) | Sustainable Use License 1.0: use or modify only for your own internal business purposes or non-commercial use; distribute only free of charge for non-commercial purposes. Files with `.ee.` in the name are under a separate enterprise licence. | Ideas only. Copy no code. |
| Dify (`langgenius/dify`, `LICENSE`) | Modified Apache 2.0: no multi-tenant service without written permission; the logo and copyright in its `web/` frontend must stay; "the interactive design of this product is protected by appearance patent". | Ideas only. Copy no code and do not imitate its interface. |
| Radix Colors, shadcn/ui, Primer primitives | MIT (repository metadata) | Free to reuse values and conventions, with the notice. |
| Model Context Protocol specification | Read as the protocol owner's text | The rules Phase 26 must follow. |

## 1. How the products share and scope agents (input to the Hub, spec section 6)

### n8n: projects and roles

Source: `n8n-io/n8n-docs`, `docs/administer/manage-users-and-access/set-permissions-and-roles-rbac/`
(`see-available-roles.md`, `organize-work-in-projects.md`).

- Workflows and credentials are grouped in a **project**; a user has a role **per project**, so one person can be
  an admin in one project and a viewer in another.
- Project roles: Admin (manages members and settings, and everything below), Editor (view, create, update, delete,
  execute), Viewer (read only; **cannot execute** a workflow).
- Only instance owners and admins create projects; only project admins add or remove members.
- Deleting a project forces a choice: move its workflows and credentials to another project, or delete them.
- Sharing warning: an exported workflow JSON holds the **names and ids** of credentials, never the secrets, and the
  docs still tell users to remove them before sharing because a name can leak information
  (`docs/reusable-content/.gitbook/includes/workflows/sharing-credentials.md`).

### Copilot Studio: who may chat and who may author

Source: Microsoft Learn, "Share agents with other users" (`admin-share-bots`, updated 2026-07-30).

- Two separate grants. **Chat** can go to individuals, a security group or the whole organisation. **Co-author**
  can go only to individual users; a co-author can view, edit, configure, share and publish, but cannot delete.
  The owner always keeps access.
- Sharing an agent does **not** share what it uses: a flow used by the agent is not shared with it, and
  connections to external services are tied to the identity of each user, which is why "it works for me but not for
  them" happens. Microsoft's remedies: a service principal, environment-level connections, or making each user sign
  in.
- Concurrent editing: on a save conflict the editor offers **Discard changes** or **Save copy**; it never
  overwrites a colleague's work.
- Stopping a share takes effect after the current conversation ends (30 minutes idle).

### Dify: export, import and roles

Source: `langgenius/dify`: `api/services/app_dsl_service.py`, `api/constants/dsl_version.py`,
`api/models/account.py`, `api/core/workflow/nodes/`.

- An app is exported as a YAML document with a `version` (`CURRENT_APP_DSL_VERSION = "0.7.0"`) and a
  `kind: app`. On import a compatibility check runs; a major-version mismatch does not import silently: the import
  is held as **pending** for 10 minutes until the user confirms.
- The document lists its **dependencies** (plugins); an import reports the ones that are missing.
- **Export strips what is bound to one environment.** With `include_secret` false (the default), the tool and
  agent nodes lose their `credential_id`; a webhook trigger's URL is cleared; a schedule trigger's configuration is
  **reset to the default**; dataset ids can be encrypted.
- Workspace roles: `owner`, `admin`, `editor`, `normal`, `dataset_operator`; only owner and admin count as
  privileged.
- Node types include agent, human input, knowledge retrieval and the schedule and webhook triggers. A **human
  input** step is a first-class node.

### What we adopt

| Idea | Where it lands |
|---|---|
| A definition names things but never carries a secret (n8n, Dify) | Already principle 4. EACP goes further: no credential reference at all, because connectors are held by the worker (ADR-001). |
| Run and author are different rights (Copilot Studio chat vs co-author; n8n viewer cannot execute) | Hub: a listing gives **run** only. Editing stays with the owner; co-authoring is a later phase. |
| Sharing carries no connection (Copilot Studio) | Already a decision: a clone carries no allowlist. Runs use the agent's identity, so the "works for me, not for them" failure cannot occur; the caller is recorded as evidence. |
| Export and clone strip environment-bound parts (Dify) | **New:** a clone drops the schedule trigger (it arrives disabled, with no schedule), any webhook URL, and every capability. Only the definition's logic is copied. |
| A versioned document with a compatibility check on import (Dify) | **New:** the definition has `schema_version` and `kind`; a newer major version is refused, never guessed. |
| Never overwrite a colleague; offer a copy (Copilot Studio) | **New:** a draft carries a `revision`; a save with a stale revision is refused and the console offers "save as copy". |
| Withdrawal takes effect at the end of the current run (Copilot Studio) | Already the Hub rule; the kill scopes remain the immediate stop. |
| A human step is a node kind (Dify `human_input`) | Recorded as the next step kind after loops; in the first release a human decision is an EACP approval on an action. |
| Role per scope, not one global role (n8n) | The department lead is a per-group flag, not a new global role. |

### What we do not adopt

- Projects as a separate grouping. EACP groups already are the departments.
- n8n and Dify's in-product credential store. Custody stays in the worker (ADR-001, ADR-019).
- Dify's plugin dependency list. A definition's tools are registry tools, and a missing one is a capability
  request, not an install.

## 2. MCP `tools/call` (input to Phase 26)

Source: `modelcontextprotocol/modelcontextprotocol`, `docs/specification/2026-07-28/server/tools.mdx` (the revision
Phase 14 pinned; the legacy revisions are in `research/REFERENCES.md`, "Phase 14").

- The request is `tools/call` with the tool's `name` and `arguments`. Modern requests carry `_meta` with the
  protocol version and client capabilities, and the transport headers listed in the Phase 14 notes.
- A successful result has `resultType: "complete"` and `content`, may carry `structuredContent` (any JSON value that
  conforms to the tool's `outputSchema`; a tool that returns it should also return the serialised JSON as text) and
  `isError`. Clients **should** validate `structuredContent` against `outputSchema`.
- **Two kinds of error.** A protocol error is a JSON-RPC error (for example `-32602` for a bad request). A **tool
  execution error** is an ordinary result with `isError: true`: the tool ran and reports a failure the model may
  correct.
- A server **may** answer `tools/call` with `resultType: "input_required"` (a multi round trip request). The retry
  must use a **different** JSON-RPC id. A worker that cannot answer such a request must not loop.
- **Annotations are hints.** Clients **must** treat tool annotations as untrusted unless they come from a trusted
  server. The Phase 14 notes already record the defaults: `readOnlyHint` false, `destructiveHint` true,
  `idempotentHint` false, `openWorldHint` true.
- A property annotated `x-mcp-header` is mirrored into an `Mcp-Param-{name}` header on the call. The constraints are
  in the Phase 14 notes and are already enforced when a tool is discovered.
- The specification says there **should** be a human in the loop able to deny a tool invocation. In EACP that is
  the approval step (ADR-005).

### What this means for Phase 26

- An MCP call is at-most-once: the protocol offers no idempotency key, and `idempotentHint` is an untrusted hint,
  so it never permits a retry. A transport failure after the request was sent is `UNKNOWN_OUTCOME` (ADR-004).
- `isError: true` is a definite, completed outcome and is recorded as a failed result, not as unknown.
- A result of `input_required`, or one whose `structuredContent` does not match `outputSchema`, is refused and
  recorded as a failure with a reason. The worker never answers a request for more input.
- Before every call the worker re-checks that the tool's current fingerprint equals the contract's pinned
  `definition_id` and that the tool is not quarantined (ADR-023).

## 3. Design-system conventions for the console (input to the console redesign)

### Radix Colors: the meaning of each step

Source: Radix Colors documentation, "Understanding the scale". Twelve steps per hue:
1-2 app and subtle component backgrounds; 3, 4, 5 component background at rest, hover and pressed or selected;
6, 7, 8 borders (non-interactive, interactive, strong and focus rings); 9 the solid colour, with the highest chroma,
and 10 its hover; 11 low-contrast text and 12 high-contrast text. Steps 11 and 12 keep APCA contrast of Lc 60 and
Lc 90 on step 2 of the same scale. Dark mode uses the dark step 1 or 2 as the page background instead of white and
maps the same aliases to different steps.

### shadcn/ui: semantic token pairs

Source: shadcn/ui documentation, "Theming". Tokens come in pairs: a surface (`card`, `popover`, `primary`,
`secondary`, `muted`, `accent`) and its `-foreground`, plus `destructive`, `border`, `input`, `ring`, `radius` and
`chart-1` to `chart-5`. Dark mode redefines **the same names** in one selector. A view uses only these names.

### Primer: typography roles

Source: `primer/primitives` (its typography stories name display, title large/medium/small, subtitle, body
large/medium/small, caption, code block and inline code as separate roles). We use roles, not sizes, in the
stylesheet.

### What we adopt for the console

- A small set of **semantic tokens in pairs** (surface and its text) with light and dark values under the existing
  `prefers-color-scheme` rule, and a manual override, so a view never names a colour.
- The **12-step meaning** for the neutral and the accent scales, so hover, selected, border and focus colours are
  derived by rule and the contrast target is APCA-style as documented, checked by a test.
- Typography by **role**. System font stack only: ADR-028 and `TestConsoleUsesNoDangerousSinks` forbid other origins,
  so there is no web font and no CDN.
- Icons as inline SVG built through `dom.js` (`h` must gain an SVG namespace helper, tested), never as `innerHTML`.

Nothing in this section changes a route, a permission or a security rule. The console remains a client (ADR-028).
