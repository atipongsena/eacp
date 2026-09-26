# Running EACP on Kubernetes

The Helm chart `deployments/helm/eacp` runs the control-plane API, the execution worker and the AGT PDP sidecar
as replicated Deployments, with the same network boundary as compose (ADR-001 §3/§3a) and the availability
rules of ADR-029 (Rev 1.0 for the binaries, Rev 1.1 for the chart). PostgreSQL and NATS are **not** part of the
chart: use managed or HA services. The chart never holds a secret value.

## Prerequisites

- Kubernetes 1.30 or later, with a CNI that enforces NetworkPolicies (Calico, Cilium, …). Without one the
  policies below are silently ignored and the boundary is gone.
- Helm, pinned for this repository to **v4.3.0**. Put it in the git-ignored `.tools/` directory; the scripts and
  tests look there first, then on `PATH`:
  - Windows amd64: `https://get.helm.sh/helm-v4.3.0-windows-amd64.zip`, sha256
    `304ea163cce4d9ad14e189c01846c6a34de9cfdfe48536ae54b2e8ba7884e67c`; extract `helm.exe` to `.tools/`.
  - Other platforms: the matching archive from the same release, checked against its published `.sha256sum`.
- A dedicated namespace labelled `pod-security.kubernetes.io/enforce=restricted`. The chart's default-deny
  NetworkPolicy covers **every** pod in it.
- The images: `eacp` (`deployments/docker/Dockerfile`, all Go binaries) and `eacp-agt-pdp`
  (`sidecars/agt-pdp/Dockerfile --target runtime`), in a registry the cluster can pull from.

## Secrets

Create these Secrets yourself (or with your secret manager's operator). Values name them; the keys are fixed.

| Value | Keys | Reaches |
|---|---|---|
| `database.appSecret` | `url` — the `eacp_app` DSN | api, worker |
| `database.ownerSecret` | `url` — the `eacp_owner` DSN | the migrate Job only |
| `pdp.tlsSecret` | `ca.pem`, `client.pem`, `client-key.pem`, `server.pem`, `server-key.pem` | api: CA and client pair; pdp: CA and server pair |
| `nats.relaySecret` | `url` — the `relay` user | api (only with `nats.enabled`) |
| `nats.workerSecret` | `url` — the `worker` user | worker (only with `nats.enabled`) |
| `worker.connectorSecrets` | `connector-secrets.json` | worker only |

The PDP certificate must name the PDP Service (`<release>-pdp`, `<release>-pdp.<namespace>.svc`). For
development, `EACP_ENV=development eacpctl pdp-dev-certs --dir <dir> --name eacp-pdp --name
eacp-pdp.eacp.svc --name eacp-pdp.eacp.svc.cluster.local` makes a throwaway PKI.

## Values that shape the boundary

- `database.peers`, `nats.peers` — NetworkPolicy peers selecting PostgreSQL and NATS (required).
- `worker.connectorEgress` — NetworkPolicy egress rules (`to` + `ports`) to the enterprise systems. It is the
  **only** way out to them. The chart refuses to render when it is empty, unless
  `worker.allowNoConnectorEgress=true`.
- `api.ingress.from` — who may reach the API on 8080. Default `[]`: any source, port 8080 only (the API
  authenticates every call). Narrow it to your ingress controller and agent namespaces.
- `otel.peers` / `otel.ports` — optional egress for the OTLP exporter.

The chart fails at render time (`helm template` / `install` stops with a message) when a required Secret name is
missing, when any `*.env` sets `EACP_CONNECTOR_SECRETS_FILE` (only `worker.connectorSecrets` provides it) or
`EACP_DATABASE_URL` or a URL with a password, when replicas are below 1, when a grace period is too short (below),
or when the governance provider is not `microsoft-agt` (or `local` with `governance.allowLocal`, for tests).

## Install and upgrade

```bash
kubectl create namespace eacp
kubectl label namespace eacp pod-security.kubernetes.io/enforce=restricted
# create the Secrets above, then:
.tools/helm upgrade --install eacp deployments/helm/eacp -n eacp -f my-values.yaml --wait
```

Migrations run as a Helm hook Job (`pre-install,pre-upgrade`) with the owner DSN, before any new pod starts.
A failed migration fails the install or upgrade and leaves the running pods untouched.

## What the NetworkPolicies allow

| Pod | Ingress | Egress |
|---|---|---|
| every chart pod | — | DNS (kube-system `k8s-app=kube-dns`, 53 UDP/TCP) |
| api | 8080 from `api.ingress.from` | PostgreSQL, NATS, PDP 8443, OTLP |
| worker | none (kubelet probes only) | PostgreSQL, NATS, `worker.connectorEgress`, OTLP |
| pdp | 8443 from api pods only | none |
| migrate Job | none | PostgreSQL |

Everything else in the namespace is denied both ways. An agent namespace reaches only the API; your enterprise
systems should also accept only the worker, as the dev Fake ERP does.

## Availability

- Replicas: 2 each by default; `EACP_WORKER_ID` is the pod name. No leader (ADR-029 §1).
- PodDisruptionBudgets: `maxUnavailable: 1` for api, worker and pdp.
- Rolling updates: `maxUnavailable: 0`, `maxSurge: 1`; pods spread across nodes (`ScheduleAnyway`).
- Shutdown: `EACP_SHUTDOWN_DELAY` (default 10 s) keeps a pod serving but not ready while endpoints update;
  `EACP_SHUTDOWN_TIMEOUT` (15 s) bounds the graceful HTTP shutdown. The chart refuses a worker
  `terminationGracePeriodSeconds` below delay + timeout + `worker.maxCallSeconds` (default 30 s, so 55 s; the
  default is 60 s) and an API grace below delay + timeout. Set `worker.maxCallSeconds` to your longest connector
  call timeout: a worker killed mid-call leaves an unknown outcome that reconciliation must settle.
- Probes: `/readyz` and `/healthz` for api and worker. The PDP's HTTP health needs a client certificate, so its
  probes are TCP connects on 8443; each one logs a `connection_error` (`SSLEOFError`) line in the PDP. That is
  the probe, not a client.

## Autoscaling

`autoscaling.enabled=true` adds CPU HorizontalPodAutoscalers for the API and the worker (defaults 2–6 and 2–10
at 70 %). They are off by default: throughput is bounded by PostgreSQL admission and `max_inflight`, and worker
CPU does not follow the queue. Queue-depth scaling (KEDA or an external-metrics adapter) is deferred (ADR-029
Rev 1.1).

## The end-to-end run (development only)

`scripts/k8s-e2e.sh` proves the chart on a real 2-node minikube cluster with Calico, so the NetworkPolicies are
enforced:

1. starts profile `eacp-e2e` (2 nodes, 3 CPUs and 2 800 MB each, Docker driver) unless it exists;
2. builds `eacp:dev` and `eacp-agt-pdp:dev` and loads them and the dev images into the cluster (nothing is
   pulled inside it);
3. creates namespaces `eacp` (restricted), `eacp-deps` and `agents`, the dev Secrets (from
   `deployments/docker/secrets` and `eacpctl pdp-dev-certs`) and the dev dependencies of
   `deployments/k8s/dev` — PostgreSQL, NATS, Fake ERP, Fake MCP and a busybox stand-in agent, each protecting
   itself with its own NetworkPolicies, all pinned to the control-plane node;
4. `helm upgrade --install eacp … -f deployments/k8s/e2e-values.yaml --wait`;
5. opens `minikube service eacp-api --url` (through the Service, so it survives pod restarts) and runs
   `TestSliceADemo` and `TestKubernetesDisruption` from `test/demo` with `EACP_DEMO_PLATFORM=k8s`;
6. deletes the profile.

```bash
bash scripts/k8s-e2e.sh                      # full run, then delete the cluster
KEEP=1 bash scripts/k8s-e2e.sh               # leave it running
TESTS=TestSliceADemo bash scripts/k8s-e2e.sh # choose the tests
TESTS=NONE KEEP=1 bash scripts/k8s-e2e.sh    # install only
```

`TestKubernetesDisruption` submits 30 purchases (half of them slow calls, in flight while pods move) while it
rolls the API, scales the workers to 3 and drains the second node. Every purchase must end `SUCCEEDED` with
exactly one ERP record, a `/healthz` probe every 100 ms through the Service must never fail, the stand-in agent
must reach the API and nothing else (PDP, PostgreSQL, NATS, Fake ERP, a worker pod), and every EACP pod must run
non-root with a read-only root filesystem in a namespace that enforces `restricted`.

The Slice C demo copies a file into the distroless Fake MCP pod, so it runs on compose only
(`scripts/demo.sh`); on Kubernetes it skips.

The render tests run without a cluster: `EACP_HELM_REQUIRED=1 go test ./test/helm` (without the variable they
skip when Helm is missing).

## Out of scope

Queue-depth autoscaling, cert-manager, a service mesh, PostgreSQL or NATS operators or HA, Ingress or Gateway
objects, publishing images, multi-cluster, and Slice C on Kubernetes.
