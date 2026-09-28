[English](README.md) | [ไทย](README.th.md)

# 03: Governance-as-Code

A platform team keeps its connectors, tools, contracts and agents in Git, reviews them like code and deploys them
like code. EACP accepts that kind of change, but never as a new authority. A bundle is only a plan. Every step runs
through the same registry rules as the API, and nothing becomes active until a second person approves it.

[`bundle/eacp.yml`](bundle/eacp.yml) declares a `ledger` connector (the Fake ERP) with one read-only tool,
`get_balance`, and its contract, plus an agent, `ledger-bot`, that is allowed to use that tool. The agent is owned
by sam and should be `ACTIVE`.

## Run it

You need Go and `jq`, the running stack and `examples/setup.sh` done once ([examples/README.md](../README.md)).
The script runs `eacpctl` from source with `go run`.

```bash
bash examples/03-governance-as-code/run.sh
```

## What you should see

On a fresh tenant (the change set id changes on every run):

```text
== 1. Validate the bundle (offline: eacpctl resolves the YAML, the target and its variables)
bundle ledger, target examples: valid

== 2. Plan: what would change (a dry run records nothing)
  submit   create connector.ledger
  submit   create tool.ledger.get_balance
  submit   propose contract.ledger.get_balance
  submit   create agent.ledger-bot
  submit   create version.ledger-bot
  submit   propose allowlist.ledger-bot
  approve  activate contract.ledger.get_balance
  approve  activate allowlist.ledger-bot
  approve  transition version.ledger-bot

== 3. Erin (registry editor) deploys: the submit stage runs, nothing is active yet
change set bdba9ca4-f58e-4177-a867-26172f2bef83 is SUBMITTED

== 4. Rita (registry approver, a second person) approves: the change set is applied
change set bdba9ca4-f58e-4177-a867-26172f2bef83 is APPLIED

== 5. Drift: the registry matches the bundle's last applied change set
  in_sync agent.ledger-bot
  in_sync connector.ledger
  in_sync tool.ledger.get_balance
  in_sync version.ledger-bot

Example 03 passed.
```

Run it again and the bundle already matches the registry: the plan says `nothing to change`, `deploy` records no
change set, step 4 is skipped and drift is still `in_sync`.

## What happened

1. **Validate.** `eacpctl bundle validate` reads the YAML, resolves the target (`examples`, bound to the examples
   tenant) and its variables, and prints the bundle as JSON, the only form the API accepts. It needs no server.
2. **Plan.** `eacpctl bundle plan --dry-run` sends the bundle to the API, which compares it with the tenant's
   registry in one snapshot and answers with the steps it would take. Each step has a stage. `submit` steps create
   objects and make proposals. `approve` steps activate the proposals and move the agent version to `ACTIVE`. A dry
   run records nothing.
3. **Deploy.** erin, a `registry_editor`, runs `eacpctl bundle deploy`. The API plans again, records the change set
   with digests of the desired state and of the registry it was planned against, and erin submits it. Submitting
   first checks that the registry still matches the plan's digest, then runs the `submit` stage as erin through
   the registry's own transactions. The new connector, tool and agent exist, but the contract and the allowlist
   are only proposals, and the agent version is not active. Submitting seals the change set with a digest of what
   it produced.
4. **Approve.** rita, a `registry_approver` and a different person, runs `eacpctl bundle approve`. EACP checks the
   sealed digest (if anything changed since the submit, the change set is stale and runs nothing), then runs the
   `approve` stage as rita. Each step still passes its own trigger: for example, an allowlist cannot be activated by
   its author. The change set is `APPLIED`.
5. **Drift.** `eacpctl bundle drift` compares the registry with the bundle's last applied change set. Every address
   is `in_sync`. A change made outside the bundle, through the API or the console, would show here as `modified`
   or `missing`.

## What it demonstrates

- Registry changes can be reviewed as code and still be two-person: the submitter is never the approver
  (ADR-026).
- The bundle adds no authority. The registry's PostgreSQL triggers decide every step exactly as they do for the
  API.
- A plan is pinned to the registry it was made from. A change set whose registry has moved on does not run.
- Drift is read-only. It reports and never repairs.
