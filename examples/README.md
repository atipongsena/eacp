[English](README.md) | [ไทย](README.th.md)

# Examples

Three small programs that use EACP the way an agent, an approver, an operator and a platform team would. Each one
runs against the local `docker compose` stack, prints what happens step by step, and exits non-zero if EACP does not
behave as described. They run nightly in CI, so they stay true.

| Example | What it shows | Needs |
|---|---|---|
| [01-agent-action](01-agent-action/README.md) | An agent's high-value purchase order: governance, two approvals, execution against the Fake ERP, idempotent retries and the verified evidence | `curl`, `jq` |
| [02-llm-gateway](02-llm-gateway/README.md) | An LLM call through the EACP gateway with the official Anthropic SDK: an allowlisted model, a refused one, and the gateway's ledger | Python 3.10 or later |
| [03-governance-as-code](03-governance-as-code/README.md) | A YAML bundle that declares a connector and an agent, planned into a change set and applied only after a second person approves it | Go, `jq` |

## Before you start

You need Docker with Compose v2, Go (the version in `go.mod`), Python 3 and Bash. On Windows, use Git Bash. From
the repository root, generate the fake services' local secrets (Git-ignored files), then start the stack and wait
until every service is healthy:

```bash
python3 deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --build --wait
```

Then prepare the examples tenant, once:

```bash
bash examples/setup.sh
```

`setup.sh` creates a tenant called `examples` and fills it with everything the three examples need. It talks to
the API as a client would, except for the first step: creating a tenant and its two admins is a bootstrap command
(`eacpctl tenant create`) that runs inside the stack's `migrate` container.

| Who | Role | Used by |
|---|---|---|
| alice, bob | `admin` (a tenant always has two) | setup |
| erin | `registry_editor` | 03: submits the change set |
| rita, ravi | `registry_approver` | 03: rita approves the change set; rita also leads the group `hr` and publishes leave-bot in the Agent Hub (the screenshots) |
| otto, olga | `operator` | 01: reads the evidence; 02: reads the LLM ledger |
| amy, ben | `approver` | 01: the two approvals |
| sam | none | owns the agents; the subject of every action |
| procurement-bot | an agent | 01 and 02 |
| stella | `studio_author`, in the group `hr` | the Agent Studio screenshots |
| studio-runtime | `studio_runtime` (a service principal) | the agent runtime, for the Agent Studio screenshots |

It also adds a policy (high-value purchases need two approvers), the Fake ERP connector with two tools
(`create_po` for example 01, and `create_po_eventual`, whose orders the ERP shows only later, for the screenshot
scenarios), two fake LLM models (`sonnet`, which the agent may use, and `opus`, which it may not), a price for them and a
budget for the agent. For Agent Studio it registers the HR MCP server (`hr-mcp`) and certifies its read-only
`get_leave_balance` tool.

Every key goes to `examples/.env`. That file is git-ignored and `setup.sh` never prints a key, so keep it that way:
don't paste it anywhere. Running `setup.sh` again is safe. If `.env` still works, it says so and does nothing; if it was written by an older
setup (it lacks a key, or rita does not lead `hr`), it says how to start over.

## Run them

```bash
bash examples/01-agent-action/run.sh
bash examples/02-llm-gateway/run.sh
bash examples/03-governance-as-code/run.sh
```

Each example can run any number of times. When jq is not on your `PATH`, point `JQ` at it
(`JQ=/path/to/jq bash examples/01-agent-action/run.sh`).

## Clean up

```bash
docker compose down -v
rm examples/.env
```

`down -v` removes the database volume, and with it the examples tenant. Remove `examples/.env` too: its keys are
worthless once the tenant is gone, and the next `setup.sh` writes a new one.

Clean up the same way when `setup.sh` says so: when it stopped part way (it writes `examples/.env` only at the end,
so the tenant it left behind has no keys), or when a key in `examples/.env` no longer works. The keys last 90 days.
