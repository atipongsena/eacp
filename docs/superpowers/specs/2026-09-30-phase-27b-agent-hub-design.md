# Phase 27b: the Agent Hub (design)

Date: 2026-09-30 · Status: the owner chose the gate's fallback and asked for 27b to be built (2026-09-30)
Program spec: `2026-09-29-agent-studio-program-design.md` sections 1, 6 and 8.4 · ADR: ADR-033 Rev 1.3

## 1. The gate, recorded as not met

No department tried the thin slice. The owner chose the fallback of program spec section 8.4: the Studio demo
(`DEMO=S`, `TestStudioDemo`) stands in for the trial, and the gate is recorded as **not met**. So nothing the
departments would have said reshapes 27b: it is built exactly as section 6 describes, with each open point at its
conservative default. The questions the trial should have answered stay open in the program spec's amendment
(template gallery before sharing, an `llm` step before the Hub, whether a department lead is the right approver).

## 2. What 27b adds

1. **Department leads.** `eacp.group_memberships.lead`, set only when an admin adds the membership (the API's
   `POST /v1/groups/{id}/members` gains `lead`). It never changes afterwards; changing it means removing and adding
   the membership. Only a human is a lead. `/v1/me` lists `lead` with each group.
2. **Listings and proposals.** An agent has at most one listing (`eacp.studio_listings`): its scope (`DEPARTMENT`,
   bound to the agent's own department, or `ORG`), its state (`PUBLISHED`, `DEPRECATED`, `WITHDRAWN`), the
   published version and its tags. A listing changes only through a proposal (`eacp.studio_listing_proposals`):
   the agent's owner proposes an `ACTIVE`, approved version with a scope and tags; one proposal per agent is open
   at a time; it is decided once (`approved`, `rejected`) or cancelled by its proposer.
3. **Tiered approval, in PostgreSQL.** A `DEPARTMENT` proposal is decided by a live lead of the agent's
   department; an `ORG` proposal by an `admin` or a `registry_approver`. Nobody decides a proposal they made or
   an agent they own. The tag `template` is allowed only on an `ORG` proposal, and only an `admin` approves it.
   Approval requires the version to be still `ACTIVE` and approved, then creates or updates the listing
   (`published_version_id` moves to the proposal's version).
4. **Deprecate and withdraw.** The owner, an admin, or whoever may approve the listing's current scope moves a
   listing `PUBLISHED -> DEPRECATED` or `PUBLISHED | DEPRECATED -> WITHDRAWN`, with a reason. Both only narrow, so
   they need no second person. A withdrawn listing is published again only through a new approved proposal.
5. **Visibility in PostgreSQL.** A listing is visible when it is `PUBLISHED` or `DEPRECATED` and the caller is an
   enabled human in its audience: a live member of the agent's department (`DEPARTMENT`) or anyone in the tenant
   (`ORG`). The owner always sees their own listing. The Hub reads only through `eacp.studio_hub_listings()` and
   `eacp.studio_hub_definition(listing)`, which return what the caller may see and nothing else (an invisible
   listing is "not found").
6. **Run from the Hub.** `eacp.studio_run_start` now allows two callers: the agent's owner, while a live member of
   its department, and anyone who sees a listing whose published version is the agent's `ACTIVE` version. If the
   published version is no longer `ACTIVE` (a newer version was approved), the run is refused until the owner
   proposes the new one. The run itself is unchanged: the same version, key, action path and failure reasons.
7. **Clone.** `eacp.studio_clone(listing, name, display name, department)` copies the published definition into a
   new Studio agent of the caller (a `studio_author` who sees a `PUBLISHED` listing and is a member of the
   department). It goes through the same save as a new agent, so its version is `REGISTERED` with an allowlist
   request a `registry_approver` must decide, and it carries no permission, key or listing.
   `eacp.studio_agents.cloned_from_version` records the source.
8. **Discovery.** The Hub lists visible listings with a search by name, tag and department, and a run count. There
   are no ratings.

## 3. Narrower than the program spec, for safety

- **Without a listing, only the owner runs an agent.** 27a let every live member of the department run it; that
  would make the lead's approval of a `DEPARTMENT` listing meaningless. The 27a tests that ran as another member now
  publish a department listing first.
- A `DEPARTMENT` listing is bound to the agent's own department, never to another group.
- Only the agent's owner proposes, and only while holding `studio_author`.
- A clone is made only from a `PUBLISHED` listing, never from a `DEPRECATED` one.
- Drafts with a `revision` are not built: a Studio version is already immutable and only its owner saves the next
  one, so there is nothing to overwrite. Seeding template listings from a Governance-as-Code bundle is not built;
  a bundle never sets `lead` (a bundle's memberships are never leads).

## 4. API

| Route | Who | Does |
|---|---|---|
| `GET /v1/studio/hub?q=&tag=&department=` | any principal | the listings the caller sees |
| `GET /v1/studio/hub/{id}` | any principal | one visible listing with its published definition and tools in plain words |
| `GET /v1/studio/agents/{id}/listing` | the owner, or a Studio reader who sees all | the agent's listing and its latest proposal |
| `POST /v1/studio/agents/{id}/listing` | `studio_author` | propose `{version_id, scope, tags, note}` |
| `GET /v1/studio/listing-requests` | any principal | the open proposals the caller may decide |
| `POST /v1/studio/listing-proposals/{id}/approve`, `/reject` | any principal (PostgreSQL decides) | decide with a reason |
| `POST /v1/studio/listing-proposals/{id}/cancel` | `studio_author` | the proposer cancels with a reason |
| `POST /v1/studio/listings/{id}/deprecate`, `/withdraw` | any principal (PostgreSQL decides) | narrow with a reason |
| `POST /v1/studio/listings/{id}/clone` | `studio_author` | `{name, display_name, department_id}`; returns the new version |

`POST /v1/groups/{id}/members` accepts `lead`; `/v1/me` groups carry `lead`.

## 5. The page

`/studio/` gains a **Hub** area for everyone (search, tag and department filters, a card per listing; a listing
page with its plain-words definition, its run form when runnable, and Clone for authors), a **Hub** section on the
owner's agent page (the listing, the open proposal, "Publish to the Hub…" with scope and tags, cancel), and a
**Hub listings** section in Requests for whoever may decide one; Requests is now shown to everyone, and its agent
and key sections still only to registry approvers. Every text in English and Thai; a screenshot of the Hub.

## 6. Tests (failing first)

- Raw SQL as `eacp_app` (`internal/studio/hub_schema_test.go`): `lead` only at insert and only for a human; the
  owner cannot propose a non-`ACTIVE` version, and nobody else can propose; one open proposal per agent; each scope's
  approver and nobody else (not the proposer, not a lead of another group, not a lead for `ORG`); `template` needs
  `ORG` and an admin; approval of a no-longer-`ACTIVE` version is refused; deprecate and withdraw by the allowed
  people only; visibility per scope and state; a member runs only through a visible listing whose version is
  `ACTIVE`; a withdrawn listing starts no run; a clone is `REGISTERED`, has no active allowlist, no key and no
  listing, and records its source; nothing here writes Studio tables directly.
- API (`internal/api/studio_hub_test.go`): each route, its role gate and its error mapping.
- Page (`jstest`): the Hub list and filters, a listing's run and clone, the owner's proposal, a lead's decision.
- Demo: `TestStudioDemo` gains the Hub: a colleague in HR cannot run the agent until an HR lead publishes it; the
  author cannot approve her own listing; a lead cannot publish to the organisation; a finance author's clone is
  `REGISTERED` and cannot run.
- Catalogues: the new tables and functions in `internal/storage/rls_catalog_test.go`.
