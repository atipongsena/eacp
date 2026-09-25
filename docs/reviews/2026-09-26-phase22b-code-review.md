# Phase 22b — code review (2026-09-26)

A fresh reviewer read the whole branch (c654423..9a20731): the spec, the plan, ADR-028, every console module and
view, the Go server and its guard tests. It traced every API string in the nine views and ran the node suite. It
found no Critical issue. Verdict: ready "with fixes".

## Fixed (each red, then green)

| Finding | Grade | Fix | Test |
|---|---|---|---|
| Writes without a dialog (note, link, assign) sent twice on a double click. "Link this to the incident" could be clicked again after it succeeded. Notes and links are insert-only journal rows. | Important | The incident page sends one write at a time (`act` guard, with errors rendered as a notice). "Link this…" sends once and stays disabled after it succeeds. | `incidents.test.mjs`: "a double submit of a note sends it once", "a double click on "Assign to me" assigns once", ""Link this to the incident" links once and then stays disabled" |
| Polling the incident list discarded a half-filled manual-incident form and the server's answer to it. | Important | The manual-incident form moved to its own page, `#/incidents/new`, which is not polled. | "the polled list holds no form to lose; a manual incident is opened on its own page" |
| `buildPath` accepted `.` and `..` as an agent `ref`. | Important | A `ref` is never `.` or `..`. | `api.test.mjs`: "buildPath substitutes and validates path parameters" |
| A danger dialog without a text field (fleet apply) focused its confirm button, so a stray Enter confirmed it. | Minor, re-graded Important (every write must be deliberate) | A dialog starts on its reason or typed field; a danger dialog with neither starts on Cancel. | `confirm.test.mjs`: "a danger dialog without a text field starts on Cancel…", "a dialog with a reason starts in the reason field" |

## Ruling

- **The browser may offer to save the key.** Browsers do not reliably honour `autocomplete="off"` on password
  fields. ADR-028 now records this as a residual risk: operators decline the prompt, and managed browsers disable the
  password manager for the console's origin. Masking a text field with `-webkit-text-security` would stop the prompt,
  but the key would be shown in clear where that property is unsupported.

## Deferred minors

- `aria-busy` stays on the sign-in page after a 401 during a render.
- Sign-out does not close an open confirm dialog (confirming it afterwards sends nothing).
- Security, fleet and approval write handlers do not catch exceptions (the incident page now does).
- The quarantine and kill dialogs could restate more of the target (connector name, tool id); the fleet dialog omits
  the skipped rows shown behind it.
- Views receive `session` and could call `authorization()`; the lint could ban it outside `api.js`.
- Lint gaps: double-quoted `"/v1/…"` literals, `XMLHttpRequest`/`sendBeacon`/`WebSocket`/`EventSource`, and
  `createElement`/`setAttribute` outside `dom.js`.
- The Assign form silently ignores a value that is not a UUID. A dependencies target outside the router's
  characters is dropped. The Security link is shown to registry approvers, who then see 403 notices for kills and
  circuits.

## Declined to judge (the reviewer's list), with rulings

- **Re-killing an already-killed scope** bumps the epoch and hands `killed_by` to the new operator. This is ADR-016
  server behaviour, and the console stays out of it.
- **No authenticated real-browser pass** (Task 8 ruling). Residual risk remains for browser-only behaviour after
  sign-in: `<dialog>` focus and password-manager prompts. It is for the owner's pass.
- **N+1 calls on the Security overview.** Accepted by spec §10 (at most 50 connectors).
- **`EACP_UI` is parsed by every service,** not only controlplane-api. A typo fails closed everywhere; harmless.
- **A non-GET request to `/ui/`** gets the mux's 405 without the console headers. It is a plain-text answer with no
  console content.
- **`TestJavaScriptUnitTests` skips without node** unless `EACP_UI_NODE_REQUIRED=1`, by design (spec §8). The final
  verification ran with it set.
