# Phase 22b — operator console end-to-end check (2026-09-26)

Stack: the isolated demo stack (`eacp-demo`, API on 127.0.0.1:18080), rebuilt from this branch. A scratch harness
ran Slice C steps C0–C5 in tenant Globex (MCP drift, blast radius, kill of po-assistant's version) and stopped
before C7, so the drift and kill incidents stayed open. No key was printed or committed.

## Browser (built-in browser, no sign-in)

| Check | Result |
|---|---|
| `GET /ui/` headers | 200, the ADR-028 CSP, `nosniff`, `X-Frame-Options: DENY`, `text/html; charset=utf-8` |
| Sign-in page renders; console messages | Renders; no console messages, no CSP or Trusted Types violations |
| Trusted Types enforced | Assigning an HTML string to an element from the page context throws `TypeError` |
| Browser storage after load | `localStorage` 0, `sessionStorage` 0, `document.cookie` empty |
| Layout at 1280 px and 375 px | Sign-in form centred; no horizontal page scroll at either width |

The authenticated pages were not driven in the browser pane: typing an API key into the sign-in field is a step the
owner does themselves. The same view modules were driven against the live API instead (below).

## The console's view modules against the live API

A scratch Node harness imported the real `static/` modules (`session.js`, `api.js`, `router.js`, the nine views,
`confirm.js`) with a fake DOM that supports forms, selects and dialogs. It clicked the rendered buttons and completed
the rendered confirm dialogs. Every step passed:

1. otto signs in; the overview renders the §55 counters (1 active kill, 1 quarantined tool).
2. The incident list shows both critical incidents: "MCP tool sap-mcp.get_po quarantined: its definition changed"
   and the agent_version kill.
3. The drift incident shows po-assistant as affected and recommends
   `#/fleet?op=pause&tool=sap-mcp.get_po&incident=…`.
4. The pre-filled fleet form previews 1 version, the confirm dialog applies the pause, and "Link this to the
   incident" records the link.
5. otto acknowledges, then tries to resolve: the server's refusal is shown verbatim ("two-person rule: the actor
   cannot also be the acknowledger of a critical incident").
6. opal resolves it; the timeline shows opened, acknowledged, linked and resolved.
7. On the Security page otto cannot clear the kill they set ("the operator who killed the scope cannot resume it");
   opal clears it.
8. A `tenant` kill keeps its confirm button disabled until `tenant` is typed; cancelled.
9. A manual incident titled `<img src=x onerror=alert(1)>` with reason `<script>alert(2)</script>` renders as text;
   the page has no IMG or SCRIPT element.
10. audra (auditor) reads the kill incident with no lifecycle controls.
11. Twelve read-only pages render with no error notice: inventory (list, agent, connector, tool definitions),
    execution (list, filtered list, action detail with evidence), dependencies (the MCP blast radius), cost, fleet,
    and the security connector and tool pages.
12. carol (approver) sees the approvals queue.

## Defect found and fixed

A 403 notice led with "Your roles do not allow this." The two-person refusals in steps 5 and 7 are 403s from
operators who hold the role, so the lead misdirected them. It now reads "The server refused this.", followed by the
server's detail (`dom.test.mjs`: "a 403 says the server refused, not that a role is missing", red then green).

## Rerun after the final review's fixes

After the fixes in `6e9adc1`, the stack was rebuilt and the whole walk-through ran again. Every step passed, and
the refusals now read "The server refused this. forbidden: …". The run also checked two of the fixes live:
- the polled incident list holds no form, and manual incidents are opened at `#/incidents/new`;
- a note submitted twice in a row appears once on the timeline.
