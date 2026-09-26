# Phase 23b — Kubernetes deployment (design)

Date: 2026-09-26 · Status: approved by the owner (design and downloads in chat: "Approve both")
Scope: MASTER_PLAN §95 (Phase 23), second half. 23a (ADR-029 Rev 1.0) made the binaries safe as N replicas;
23b ships them to Kubernetes and proves it on a real cluster. ADR: **ADR-029 Rev 1.1** (extends 23a).

## 1. Intent

Operators need to run EACP on Kubernetes with the same boundary compose enforces today (ADR-001 §3/§3a:
agents reach only the API, only the worker holds connector credentials and reaches enterprise systems, only
the API reaches the PDP) and with the availability §95 asks for: several API and worker replicas, disruption
budgets, rolling updates that lose nothing, and a way to autoscale.

Success:

- `helm lint` and `helm template` of `deployments/helm/eacp` pass, and a Go test checks every hardening,
  secret-custody, network and availability rule in the rendered YAML, and that bad values are refused.
- On a 2-node minikube cluster with Calico (so NetworkPolicies are enforced), the chart installs under the
  `restricted` Pod Security Standard, the Slice A demo passes unchanged, and a disruption run (rolling API
  restart, workers scaled 2→3, a node drained) settles every action `SUCCEEDED` with one ERP record per
  operation key while the API never loses all its ready endpoints and the stand-in agent reaches only the API.

## 2. Principles (non-negotiable)

1. **Same boundary as compose.** Nothing the chart adds lets an agent reach anything but the API, lets the
   API or PDP hold connector secrets, or lets anything but the API reach the PDP.
2. **The chart never holds a secret value.** Values name existing Secrets; the chart only references them.
   Dev secrets are created by the e2e script from the local dev files and `eacpctl pdp-dev-certs`.
3. **Fail closed at render time.** Wrong or dangerous values stop `helm template` with a message (`fail`),
   never render something weaker.
4. **No new authority, no cluster API access.** No RBAC objects, no service-account tokens mounted, no
   operator or controller. The binaries are unchanged by 23b except where a test proves a need.
5. **External state.** PostgreSQL and NATS are not part of the chart (production uses managed/HA services);
   the e2e deploys dev-only instances from plain manifests.
6. **Pinned tools.** Helm v4.3.0 (sha256 `304ea163cce4d9ad14e189c01846c6a34de9cfdfe48536ae54b2e8ba7884e67c`
   for the Windows amd64 zip) in git-ignored `.tools/`; minikube v1.38.1 as installed.

## 3. Design

### 3.1 Chart `deployments/helm/eacp`

`Chart.yaml` apiVersion v2, `kubeVersion: ">=1.30.0-0"`. Templates (names prefixed with the release name):

| Object | Notes |
|---|---|
| `Deployment` api | `/controlplane-api`, port 8080, replicas 2 |
| `Service` api | ClusterIP by default (`api.service.type`), port 8080 |
| `Deployment` worker | `/execution-worker`, port 8081 (probes only), replicas 2; `EACP_WORKER_ID` from the pod name |
| `Deployment` pdp | the AGT sidecar image, port 8443 (mTLS), replicas 2; `AGT_PDP_INSTANCE_ID` from the pod name |
| `Service` pdp | port 8443 |
| `Job` migrate | `/eacpctl migrate up` with the owner DSN; Helm hook `pre-install,pre-upgrade`, `hook-delete-policy: before-hook-creation,hook-succeeded` |
| `ServiceAccount` | one, `automountServiceAccountToken: false` |
| `PodDisruptionBudget` ×3 | api, worker, pdp: `maxUnavailable: 1` |
| `NetworkPolicy` | see 3.3 |
| `HorizontalPodAutoscaler` ×2 | api, worker; `autoscaling.enabled: false` by default |

Every pod:

- `securityContext`: `runAsNonRoot: true`, `runAsUser/Group: 65532`, `fsGroup: 65532`,
  `seccompProfile: RuntimeDefault`; container `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem:
  true`, `capabilities.drop: [ALL]`. An `emptyDir` at `/tmp`.
- `automountServiceAccountToken: false`, `enableServiceLinks: false`.
- Resource requests and limits from values (defaults set for all).
- Readiness `GET /readyz`, liveness `GET /healthz` (pdp: TCP 8443 for both, since its health needs a client
  certificate), a startup probe on the same endpoint.
- API and worker: `EACP_SHUTDOWN_DELAY` (default `10s`), `EACP_SHUTDOWN_TIMEOUT` (default `15s`),
  `terminationGracePeriodSeconds` from values; the template fails when grace < delay + timeout +
  `worker.maxCallSeconds` (default 30; for the API the call term is 0).
- Rolling update `maxUnavailable: 0`, `maxSurge: 1`; `topologySpreadConstraints` on
  `kubernetes.io/hostname`, `maxSkew: 1`, `whenUnsatisfiable: ScheduleAnyway`.
- Images `image.repository:image.tag` (EACP binaries) and `pdp.image.*`; `imagePullPolicy` from values.

Secrets, all referenced by name (`existingSecret` style, keys fixed):

| Value | Keys | Mounted into |
|---|---|---|
| `database.appSecret` | `url` | api, worker (as `EACP_DATABASE_URL`) |
| `database.ownerSecret` | `url` | migrate Job only |
| `pdp.tlsSecret` | `ca.pem`, `client.pem`, `client-key.pem`, `server.pem`, `server-key.pem` | api gets `ca`+`client*`; pdp gets `ca`+`server*` (projected, per-key items) |
| `nats.relaySecret` / `nats.workerSecret` | `url` | api / worker; optional (`nats.enabled`) |
| `worker.connectorSecrets` | `connector-secrets.json` | worker only (`EACP_CONNECTOR_SECRETS_FILE`) |

Render-time failures: a missing required secret name; `api.env` or `pdp.env` containing
`EACP_CONNECTOR_SECRETS_FILE`; any `*.env` entry containing `EACP_DATABASE_URL` or a value that looks like a
DSN with a password; replicas < 1; the grace-period rule; `governance.provider` other than `microsoft-agt`
or `local` (and `local` requires `governance.allowLocal: true`, for tests only).

### 3.2 Values that shape the boundary

- `api.ingress.from` (NetworkPolicy peers; default `[]` = any source, port 8080 only) — the API is the
  authenticated front door; narrowing it is deployment-specific and documented.
- `postgres.peers`, `nats.peers` (NetworkPolicy peers + ports) for egress to the external services.
- `worker.connectorEgress` (NetworkPolicy peers + ports; default none) — the only egress to enterprise
  systems. The chart fails when it is empty and `worker.allowNoConnectorEgress` is not true.
- `otel.peers` (optional egress for the OTLP exporter).

### 3.3 NetworkPolicies (release namespace; install into a dedicated namespace)

1. `default-deny`: all pods in the namespace, ingress and egress.
2. `dns`: egress from chart pods to `kube-system` `k8s-app=kube-dns` on 53/UDP and 53/TCP.
3. `api`: ingress on 8080 from `api.ingress.from`; egress to postgres, nats, pdp:8443, otel.
4. `worker`: ingress none except kubelet probes (host traffic is allowed by the CNI); egress to postgres,
   nats, `worker.connectorEgress`, otel.
5. `pdp`: ingress on 8443 from api pods only; no egress.
6. `migrate`: egress to postgres only.

Dev dependencies protect themselves (3.5), so the agent's reach is decided by every target, as a real ERP
must accept only the worker.

### 3.4 Autoscaling

HPA (`autoscaling/v2`) templates for api and worker on CPU utilisation, `min`/`max` from values, disabled by
default: throughput is bounded by PostgreSQL admission limits and `max_inflight`, and worker CPU does not track
the queue. Queue-depth scaling (KEDA PostgreSQL scaler or an external-metrics adapter) is recorded as deferred
in ADR-029 Rev 1.1: it needs a new dependency and a read-only queue metric, which is its own decision. The HPA
is exercised by the render test only.

### 3.5 Dev dependencies (`deployments/k8s/dev/`, plain manifests, namespace `eacp-deps`)

PostgreSQL 18 (StatefulSet, initdb from a ConfigMap copying `01-roles.sh`), NATS 2.15.0 (Deployment,
`nats.conf` ConfigMap, same users), Fake ERP and Fake MCP (Deployments, tokens from Secrets), and in namespace
`agents` a busybox stand-in agent. NetworkPolicies in `eacp-deps`: postgres ingress only from `eacp`
namespace pods labelled api, worker or migrate (and the e2e toolbox, 3.6); nats ingress only from api and
worker; fakeerp/fakemcp ingress only from worker pods. Labelled clearly DEVELOPMENT ONLY.

ExternalName alias Services keep the demo's host names: `fakeerp` and `fakemcp` in `eacp` and `agents`
(to `*.eacp-deps.svc.cluster.local`), and `controlplane-api` in `agents` (to the release's API Service). The
dev connector-secrets manifest (`host: fakeerp:8090`) and the demo's URLs stay unchanged, and an agent probe
that fails is refused by a NetworkPolicy, not by a DNS miss. The manifest gains the disruption test's tenant
(`00000000-0000-4000-8000-0000000000e2`) bound to the Fake ERP credential.

### 3.6 The real run

`scripts/k8s-e2e.sh` (Git Bash and Linux):

1. `minikube start -p eacp-e2e --nodes 2 --cni calico --driver docker` (unless it exists).
2. `docker build` the EACP image and the sidecar runtime image locally, `minikube image load` them and the
   dev images (postgres, nats, busybox), so nothing is pulled inside the cluster.
3. Namespaces `eacp` (label `pod-security.kubernetes.io/enforce=restricted`), `eacp-deps`, `agents`.
4. Dev secrets from `deployments/docker/secrets` and `eacpctl pdp-dev-certs --name <release>-pdp`.
5. `kubectl apply -f deployments/k8s/dev`, wait; `helm upgrade --install eacp deployments/helm/eacp -n eacp
   -f deployments/k8s/e2e-values.yaml --wait`.
6. `minikube service --url` for the API and Fake ERP (through the Service, so it survives pod restarts), then
   `EACP_DEMO=1 EACP_DEMO_PLATFORM=k8s go test ./test/demo -run 'TestSliceADemo|TestKubernetesDisruption'`.
7. Delete the profile unless `KEEP=1`.

`test/demo` gains a `platform` interface for the demo's non-HTTP operations (tenant bootstrap through
`eacpctl`, restart/kill/stop/start of a service, exec in the agent, psql, logs, dump, copy). `compose` is the
current behaviour, unchanged; `k8s` maps them to `kubectl` (bootstrap: a one-shot `eacpctl` pod with the owner
DSN, in the migrate pod's network identity; kill: force-delete the pods; stop/start: scale to 0 and back;
restart: `rollout restart` and wait). Slice C stays compose-only on this platform (it copies a file into the
distroless Fake MCP pod; `kubectl cp` needs `tar`) and skips with that reason.

`TestKubernetesDisruption` (k8s platform only, its own tenant bound in the dev connector-secrets manifest):
submits 30 actions across the Fake ERP scenarios while it runs `rollout restart` of the API, scales workers
2→3, and drains the second node (`kubectl drain --ignore-daemonsets --delete-emptydir-data`), then uncordons
it. It asserts: every action `SUCCEEDED` with one ERP record per operation key; the API Service had at least
one ready endpoint at every poll (100 ms) during the drain and restart; the stand-in agent reaches the API and
cannot reach the worker, the PDP, PostgreSQL, NATS or Fake ERP; no pod ran as root and every EACP pod has a
read-only root filesystem (from the pod specs).

### 3.7 Tests without a cluster

`deployments/helm/eacp` render test in a Go package `test/helm` (`TestChart…`), using `.tools/helm` or `helm`
on PATH; skipped without helm unless `EACP_HELM_REQUIRED=1` (then it fails), like the console's node tests.
It renders the default e2e values and bad-value fixtures and parses the YAML (`go.yaml.in/yaml/v3`):

- `helm lint --strict` passes;
- every Deployment/Job pod has the 3.1 security context, no SA token, `/tmp` emptyDir, probes;
- no rendered manifest contains a secret value (the fixture's secret names only; no `postgres://…:…@`);
- the connector-secret Secret is referenced by the worker only; PDP client key only in api; server key only in
  pdp; owner DSN only in the migrate Job;
- the NetworkPolicy set of 3.3, edge by edge;
- PDBs `maxUnavailable: 1`; rolling update `maxUnavailable: 0`; grace-period rule;
- HPAs absent by default, present when enabled;
- each bad-value fixture fails with its message.

### 3.8 Documentation

ADR-029 Rev 1.1 (deployment decisions, unresolved assumptions), `docs/KUBERNETES.md` (install, values,
secrets, boundary, the e2e run), MASTER_PLAN §95 status, AGENTS.md (status, commands, a rule line: the chart
never carries a secret value and keeps the compose boundary; change it only with the render test), README.
`.gitignore` gains `/.tools/`.

## 4. Out of scope

Queue-depth autoscaling (KEDA/metrics adapter), cert-manager, a service mesh, PostgreSQL/NATS operators or HA,
Ingress/Gateway objects, image publishing to a registry, multi-cluster, and Slice C on the k8s platform.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-029 Rev 1.1)

| Assumption | Choice |
|---|---|
| PostgreSQL / NATS in the chart | No: external; dev-only manifests for the e2e. |
| Secrets | Referenced by name only; never rendered from values. |
| API ingress default | Any source, port 8080 only; narrowing is per deployment. |
| Worker egress default | None; the chart refuses to render without an explicit allow-list or opt-out. |
| Autoscaling | CPU HPAs, off by default; queue-depth scaling deferred. |
| PDB | `maxUnavailable: 1` for api, worker, pdp. |
| Shutdown timing | delay 10 s, timeout 15 s, worker grace ≥ delay + timeout + max call (default 30 s). |
| PDP probes | TCP, since its HTTP health needs a client certificate. |
| Namespace | Dedicated, `restricted` Pod Security Standard; the default-deny policy covers the whole namespace. |
| Slice C on Kubernetes | Not run (file copy into a distroless pod); compose keeps it. |
