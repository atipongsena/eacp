# Phase 27c node builder — design

Status: accepted under the owner's 2026-10-01 instruction to continue with a connected node-based UI.

## Design posture

Inherit ADR-028's English/Thai console, semantic colour tokens, CSS glyph icons, safe text-only DOM and memory-only credentials. A diagram makes the existing bounded definition understandable; it creates no authority. No dependency, image, SVG, new route or persisted editor geometry is required.

## Layout and hierarchy

Keep Basics and Inputs above the builder, Test and Review below it. Schema-v2 definitions use a scrollable graph beside a selected-node inspector. At widths below 1000px the inspector follows the graph. Deterministic forward layers make the starting node, both branch arms and terminal answers visible. Show node id, kind and model/tool identifier; keep instructions, payloads and answers in the inspector. Do not preview private content in cards.

## Components and interaction states

- A selected card uses the accent border and a pressed native button. Selecting another node keeps draft edits.
- Tool/model nodes have one labelled output; branches have True and False outputs; answer nodes end a path.
- Click an output and then a following input, or drag between those ports. Escape or Cancel connection abandons the pending local edit. External drops do nothing.
- Forward edges have arrowheads and textual connection summaries. Invalid, duplicate or missing targets remain visible as errors and are never repaired silently.
- Provide 75%, 100% and 125% zoom and native scrolling. Auto-layout determines positions; free node positioning and persisted canvas state are outside this increment.
- Add/select/edit/remove/reorder nodes through existing form controls. Add controls stop at the server's 20-node limit. Reordering can invalidate edges; validation stays fail-closed.
- Saved version and approver views show the same graph read-only alongside full definition details.

## Accessibility and safety

Use native buttons with input labels and a polite connection status, keyboard click equivalents and explicit True/False text. Every visible sentence has a literal Thai catalogue entry. All colours use contrast-tested semantic tokens. DOM geometry accepts only bounded finite numbers for whitelisted CSS properties, never an arbitrary style string. Existing CSP, sink restrictions and confirmed writes remain unchanged.

## Verification

Test forward-port edits, rejected backward/ambiguous targets, ignored external drops, both branch arms, safe hostile text, bounded geometry, selected-node draft retention, v1 preservation, catalogue/translation/security rules and read-only graphs. Generate and inspect screenshots through the repository script.
