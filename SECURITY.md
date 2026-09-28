[English](SECURITY.md) | [ไทย](SECURITY.th.md)

# Security policy

EACP is a security boundary: it decides whether an AI agent's action may touch an enterprise system. A flaw that
lets an action bypass governance, run twice, run without its approval, or leak a credential is exactly what we want
to hear about.

## Reporting a vulnerability

Please report privately through GitHub's **private vulnerability reporting**: open the repository's **Security**
tab and choose **Report a vulnerability**. Do not open a public issue, pull request or discussion for a suspected
vulnerability.

A useful report says:

- what an attacker can do, and from which position (an agent with its own key, an operator, a network neighbour);
- the steps or a test that reproduces it, against `docker compose up` or the examples;
- the commit or release you tested.

You can expect an acknowledgement within a week. We will agree a disclosure date with you, credit you in the
release notes unless you prefer otherwise, and publish a GitHub security advisory with the fix.

## Supported versions

| Version | Supported |
|---|---|
| `main` | yes |
| The latest release | yes |
| Older releases | no; upgrade to the latest release |

## What is in scope

- The control plane, the execution worker, the LLM gateway, `eacpctl` and the AGT sidecar PDP.
- The database schema, its triggers and Row-Level Security policies (`migrations/`).
- The Helm chart and its network policies (`deployments/helm/`).
- The guarantees in the [threat model](docs/security/THREAT_MODEL.md) and [ADR-001](docs/adr/ADR-001-product-boundary-and-enforcement-point.md),
  for a conforming deployment as ADR-001 §3a defines it.

## What is not a vulnerability

- **The development secrets in the repository.** The files under `deployments/docker/secrets/` and the credential
  manifests under `deployments/k8s/` hold values for the local fake services (Fake ERP, Fake LLM, Fake A2A, the dev
  Vault). They are never valid anywhere else. Never reuse them.
- **The fake services themselves** (`cmd/fakeerp`, `cmd/fakellm`, `cmd/fakea2a`, `cmd/fakemcp`). They exist for
  tests and demos and are not shipped in releases.
- **A deployment that is not conforming,** for example one where an agent holds a connector credential or has a
  network route to the target system. EACP cannot enforce its boundary there; ADR-001 §3a explains why.
