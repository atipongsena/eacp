[English](README.md) | [ไทย](README.th.md)

# 02: LLM calls through the EACP gateway

Agents call language models as well as enterprise systems. The EACP LLM gateway puts those calls under the same
controls as any other action: the model must be on the agent's allowlist, the policy must allow it, the budget
must cover it, a kill switch can stop it, and every call is recorded in a ledger with its cost.

The agent here is ordinary Python using the official
[Anthropic SDK](https://github.com/anthropics/anthropic-sdk-python). Only two things change: the base URL points
at the gateway, and the API key is the agent's own EACP key. The provider key stays in the gateway. In this stack
the provider is the Fake LLM, which answers the Anthropic Messages API with fixed text and exact token counts.

## Run it

You need Python 3.10 or later (the SDK's minimum), the running stack and `examples/setup.sh` done once
([examples/README.md](../README.md)). The script makes a virtual environment in `examples/02-llm-gateway/.venv`
on its first run and installs the pinned SDK from `requirements.txt` into it.

```bash
bash examples/02-llm-gateway/run.sh
```

## What you should see

```text
== 1. The agent asks an allowlisted model (sonnet) through the gateway
answer: 'Hello from fakellm.'
usage: 21 input and 20 output tokens

== 2. The agent asks a model outside its allowlist (opus)
refused before anything reached the provider: HTTP 403, permission_error

== 3. An operator reads the gateway's ledger
opus: DENIED model_not_in_allowlist, no cost
sonnet: SETTLED succeeded, 0.000363 USD

Example 02 passed.
```

## What happened

1. **An allowed call.** `client.messages.create(model="sonnet", ...)` goes to the gateway. The gateway
   authenticates the agent's key and asks the policy decision point about the call's metadata only: the operation
   `llm.generate`, the model, whether it streams, the output cap and the request size. The PDP never sees the
   prompt. PostgreSQL then admits the call: the model is on the agent's allowlist, no kill switch covers it, and
   the agent's budget can reserve PostgreSQL's estimate of the cost. Only then does the gateway forward the request,
   once, with the provider key the agent never sees. The answer comes back unchanged, and the call is settled with
   the provider's real token counts.
2. **A refused call.** `opus` exists in the tenant but is not on this agent's allowlist. Admission refuses it, so
   nothing is sent to the provider, and the SDK raises `PermissionDeniedError` with the Anthropic error shape.
   The agent needs no special handling: it is the same error a provider would return.
3. **The ledger.** An operator lists `GET /v1/llm-calls?agent=...`. Each call has its state: `SETTLED` with the
   cost that PostgreSQL computed from the tenant's price card, or `DENIED` with the reason and no cost. The ledger
   holds no prompt, no response and no key.

## What it demonstrates

- An agent uses a standard SDK unchanged, apart from the base URL and its own key (ADR-031).
- The provider credential stays in the gateway. Agents hold only their EACP key.
- Allowlists, budgets and kill switches apply to model calls before anything leaves the gateway.
- Costs are computed in PostgreSQL from a price card, and content is never stored.
