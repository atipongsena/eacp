[English](KUBERNETES.md) | [ไทย](KUBERNETES.th.md)

# รัน EACP บน Kubernetes

Helm chart `deployments/helm/eacp` รัน API, execution worker และ AGT PDP เป็น Deployment หลาย replica
โดยรักษาขอบเขตเครือข่ายแบบ compose (ADR-001) และกฎ availability ของ ADR-029 PostgreSQL และ NATS
อยู่นอก chart ให้ใช้บริการ managed หรือ HA chart เก็บเพียงชื่อ Secret ไม่เก็บค่าของมัน Studio runtime และ
LLM gateway เปิดเพิ่มได้ตาม ADR-033 Rev 1.4 และ ADR-029 Rev 1.3

## สิ่งที่ต้องมี

- Kubernetes 1.30 ขึ้นไปและ CNI ที่บังคับ NetworkPolicy เช่น Calico/Cilium หากไม่มี CNI ดังกล่าว policy ไม่มีผล
- Helm **v4.3.0** ที่ pin ไว้ ใส่ใน `.tools/` ที่ git ignore สคริปต์/test ตรวจที่นี่ก่อน `PATH`
  Windows amd64 ใช้ `https://get.helm.sh/helm-v4.3.0-windows-amd64.zip` ตรวจ SHA-256
  `304ea163cce4d9ad14e189c01846c6a34de9cfdfe48536ae54b2e8ba7884e67c` แล้วแตก `helm.exe` ลง `.tools/`
  ระบบอื่นใช้ archive ของ release เดียวกันและตรวจ `.sha256sum`
- namespace เฉพาะติด label `pod-security.kubernetes.io/enforce=restricted` default-deny ครอบคลุมทุก pod ในนั้น
- image `eacp` จาก `deployments/docker/Dockerfile` และ `eacp-agt-pdp` จาก
  `sidecars/agt-pdp/Dockerfile --target runtime` ใน registry ที่ cluster เข้าถึงได้

## Secret

สร้าง Secret เองหรือผ่าน operator ของ secret manager Values ระบุชื่อ ส่วน key เป็นชื่อที่กำหนดตายตัว

| Value | Keys | Reaches |
|---|---|---|
| `database.appSecret` | `url` — DSN ของ `eacp_app` | api, worker, llm-gateway ที่เปิดเพิ่ม |
| `database.ownerSecret` | `url` — DSN ของ `eacp_owner` | migrate Job เท่านั้น |
| `pdp.tlsSecret` | `ca.pem`, `client.pem`, `client-key.pem`, `server.pem`, `server-key.pem` | api/gateway: CA และ client; pdp: CA และ server |
| `nats.relaySecret` | `url` — user `relay` | api เมื่อเปิด `nats.enabled` |
| `nats.workerSecret` | `url` — user `worker` | worker เมื่อเปิด `nats.enabled` |
| `worker.connectorSecrets` | `connector-secrets.json` | worker เท่านั้น |
| `studio.masterSecret` | `master` — Studio master สุ่มอย่างน้อย 32 ไบต์ | agent-runtime เมื่อเปิด `studio.enabled` เท่านั้น |
| `studio.runtimeKeySecret` | `keys` — `pk` ของ runtime หนึ่ง tenant ต่อบรรทัด | agent-runtime เมื่อเปิด `studio.enabled` เท่านั้น |
| `llmGateway.providerSecret` | `provider-secrets.json` | llm-gateway เมื่อเปิด `llmGateway.enabled` เท่านั้น |

certificate PDP ต้องมีชื่อ Service `<release>-pdp` และ `<release>-pdp.<namespace>.svc`
สำหรับ dev ใช้ `EACP_ENV=development eacpctl pdp-dev-certs --dir <dir> --name eacp-pdp --name
eacp-pdp.eacp.svc --name eacp-pdp.eacp.svc.cluster.local` สร้าง PKI ชั่วคราว

## Values ที่กำหนดขอบเขต

- `database.peers`, `nats.peers` ต้องระบุ peer ที่เลือก PostgreSQL/NATS
- `worker.connectorEgress` เป็นทางออกสู่ระบบองค์กรเพียงทางเดียว ใช้ `to` และ `ports` หากว่าง chart ปฏิเสธ
  เว้นแต่ระบุ `worker.allowNoConnectorEgress=true` token URL, token-exchange `impersonate.url` และ AWS STS
  ต้องอยู่ในกฎนี้ด้วย STS ปกติคือ `sts.<region>.amazonaws.com:443` หรือ `sts_endpoint` ที่ตั้งไว้
- Manifest ของ worker mount เพียงไฟล์เดียว ให้ใส่ `client_secret`, `key`, `certificate` inline ใน Secret
  ไม่ใช้ `client_secret_file`/`key_file` ที่ไม่มีใน pod HTTP token URL ใช้ได้เฉพาะ `development`/`test`
- `worker.workloadIdentity`: `enabled=false`, audience 1–256 ตัวไม่มี whitespace, `expirationSeconds` 600–86 400
  (ปกติ 3600) เปิดแล้ว project token ให้ worker เท่านั้นที่ `/run/secrets/eacp-identity/token`
  ใช้เป็น `client_assertion_file` ServiceAccount `<release>-worker` มี federated subject
  `system:serviceaccount:<namespace>:<release>-worker` สำหรับ Entra ใช้ issuer ของ cluster ที่ Entra เข้าถึงได้
  audience `api://AzureADTokenExchange` และ `https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token`
  chart project token เอง ไม่ต้องใช้ Azure Workload Identity webhook
- `private_key_jwt` ไม่ต้องมี chart value เพิ่ม ใส่ alg เช่น PS256, key และ certificate ใน oauth2 manifest
  และลงทะเบียน certificate/JWKS กับ IdP key อยู่เฉพาะ Secret worker สคริปต์ e2e สร้าง dev key ผ่าน
  `eacpctl dev-client-key` และให้ Fake ERP อ่าน public JWKS จาก ConfigMap `fakeerp-federation`
- `worker.vaultIdentity`: ปกติปิด audience `vault` และ expiry 3600 (ช่วง 600–86 400) เปิดแล้ว worker ได้ token
  อีกตัวที่ `/run/secrets/eacp-vault-identity/token` ใส่ใน `vault.auth.kubernetes.jwt_file` ผูก role กับ
  `<release>-worker`, namespace และ audience เดียวกัน token แยกจาก workload identity จึง replay ข้าม IdP/Vault
  ไม่ได้ ใส่ host Vault ใน connector egress Vault ภายนอกใช้ reviewer JWT/issuer ส่วนภายในต้องมี
  `system:auth-delegator` ตาม `deployments/k8s/dev/vault.yaml`
- `worker.spiffe`: ปกติปิด `csiDriver=csi.spiffe.io` ห้ามชื่อว่าง เปิดแล้ว worker เท่านั้นได้ read-only CSI socket
  `/spiffe-workload-api/spire-agent.sock` ตั้ง endpoint `unix:///spiffe-workload-api/spire-agent.sock`
  CSI ผ่านมาตรฐาน restricted โดยไม่ต้องใช้ hostPath/egress เพิ่ม ผู้ดูแลต้องรัน SPIRE/CSI และลงทะเบียน
  `k8s:ns:<namespace>`, `k8s:sa:<release>-worker` JWT-SVID TTL อย่างน้อย 2.5 × (call budget ที่ยาวสุด + 30 s)
- `api.ingress.from` จำกัดผู้เข้าถึง 8080 ปกติ `[]` รับทุก source เฉพาะ port นี้ และ API ยืนยันตัวตนทุกครั้ง
- `llmGateway.enabled=false` เปิดแล้วต้องระบุ `providerSecret`, `providerPeers`, `providerPort` (ปกติ 443)
  `ingress.from` เลือก agent เพิ่มเติม runtime ที่เปิดถูกอนุญาตให้อัตโนมัติ หากไม่เปิด Studio ต้องมี ingress peer
  provider peers ต้องตรง host จริง ทุก call ยังยืนยันตัวตนและ reserve ผ่าน PostgreSQL runtime ได้
  `EACP_RUNTIME_LLM_URL` จาก chart โดย override ไม่ได้ มีเพียง master/key ของตัวเอง mount อยู่
- `otel.peers`/`otel.ports` เป็น egress OTLP เพิ่มเติม
- `environment` ปกติ `production` ใช้ `development`, `test`, `staging`, `production` เท่านั้น นอก dev/test
  NATS ต้องใช้ `tls://` ต่างจาก binary เดี่ยวที่ default เป็น development
- `nats.caSecret`, `database.caSecret` ใช้ key `ca.pem` สำหรับ private CA NATS ให้ API/worker ผ่าน
  `EACP_NATS_CA_FILE` PostgreSQL mount ที่ `/run/secrets/eacp-db/ca.pem` ให้ API/worker/gateway/migrate
  ตั้ง DSN `sslrootcert=/run/secrets/eacp-db/ca.pem`

chart ปฏิเสธตอน render หากชื่อ Secret/peer/egress ที่จำเป็นหาย env ไม่ใช่ string หรือมี URL credential
หรือ override ตัวแปรที่ chart ถือ เช่น `EACP_DATABASE_URL`, `EACP_CONNECTOR_SECRETS_FILE`, `EACP_ENV`,
`EACP_HTTP_ADDR`, `EACP_LOG_FORMAT`, `EACP_WORKER_ID`, `EACP_GOVERNANCE_PROVIDER`, `EACP_NATS_*`,
`EACP_SHUTDOWN_*`, `EACP_AGT_PDP_*`, `AGT_PDP_*` shutdown delay ต้องเป็นวินาทีเต็ม 0–60 timeout เป็นบวก
`worker.maxCallSeconds` เป็นจำนวนเต็มอย่างน้อย 1 replicas อย่างน้อย 1 และ grace ต้องพอ provider governance
ต้องเป็น microsoft-agt หรือ local ที่เปิด `governance.allowLocal` สำหรับ test เมื่อเปิด Studio ต้องมี Secret
masterVersion เป็น `v` ตามด้วยเลข 1–4 หลัก และ env ห้ามตั้ง secret/database หรือค่าที่ chart ถือ รวม
`EACP_API_URL`, `EACP_STUDIO_MASTER_FILE`, `EACP_STUDIO_MASTER_VERSION`, `EACP_RUNTIME_KEY_FILE`,
`EACP_RUNTIME_LLM_URL` gateway ตรวจ Secret/peers/port/ingress และ env ที่สำคัญเช่นกัน

## ติดตั้งและอัปเกรด

```bash
kubectl create namespace eacp
kubectl label namespace eacp pod-security.kubernetes.io/enforce=restricted
# create the Secrets above, then:
.tools/helm upgrade --install eacp deployments/helm/eacp -n eacp -f my-values.yaml --wait
```

migration เป็น hook Job `pre-install,pre-upgrade` ด้วย owner DSN ก่อน pod ใหม่เริ่ม หาก migration ล้มเหลว
install/upgrade ล้มและไม่แตะ pod ที่กำลังรัน

## สิ่งที่ NetworkPolicy อนุญาต

| Pod | Ingress | Egress |
|---|---|---|
| ทุก pod ของ chart | — | DNS kube-system `k8s-app=kube-dns` 53 UDP/TCP |
| api | 8080 จาก `api.ingress.from` | PostgreSQL, NATS, PDP 8443, OTLP |
| worker | ไม่มี ยกเว้น probe ของ kubelet | PostgreSQL, NATS, connector egress, OTLP |
| pdp | 8443 จาก api/gateway | ไม่มี |
| migrate Job | ไม่มี | PostgreSQL |
| agent-runtime ที่เปิด | ไม่มี | api 8080 และ gateway ที่เปิด 8083 |
| llm-gateway ที่เปิด | 8083 จาก runtime/ingress peers | PostgreSQL, PDP, provider peers/port, OTLP |

runtime มี ServiceAccount ของตัวเองไม่มี token ไม่มี database URL และไม่มี secret connector/provider
mount Secret สองตัวอ่านอย่างเดียวที่ `/run/studio` ปกติปิด gateway มี ServiceAccount ไม่มี token และ manifest
provider แบบอ่านอย่างเดียว ทั้งสอง hardened และใช้ Go DNS retry default-deny ปิด pod อื่นทั้งขาเข้า/ออก
agent namespace เข้าถึง API และ gateway ที่ระบุชัด ระบบองค์กรควรรับจาก worker เท่านั้นเหมือน Fake ERP

## Availability

- ปกติ API/worker/PDP อย่างละ 2 replicas `EACP_WORKER_ID` คือชื่อ pod ไม่มี leader
- PodDisruptionBudget `maxUnavailable:1` สำหรับ api/worker/pdp rolling update `maxUnavailable:0`, `maxSurge:1`
  กระจาย node แบบ `ScheduleAnyway`
- shutdown delay ปกติ 10 s ทำให้ไม่ ready แต่ยังให้บริการระหว่าง endpoint เปลี่ยน timeout 15 s จำกัด graceful
  PDP ใช้ `AGT_PDP_SHUTDOWN_DELAY`/`AGT_PDP_SHUTDOWN_TIMEOUT` เช่นเดียวกัน grace ของ API/PDP/gateway
  ต้องไม่น้อยกว่า delay+timeout ของ worker ต้องบวก `worker.maxCallSeconds` (ปกติ 300 s; grace 330 s)
  grace เป็นเพดาน worker ที่ไม่มี call ค้างจบได้ทันที หากฆ่ากลาง call จะมี unknown outcome ให้ reconcile
- Go services ใช้ DNS `timeout:1 attempts:3` เพื่อไม่ให้ query หายกิน budget PDP 5 s ทั้งหมด
- API/worker/gateway probe HTTP `/readyz`, `/healthz` PDP ใช้ TCP 8443 เพราะ health HTTP ต้องมี certificate
  log `connection_error`/`SSLEOFError` จาก probe ไม่ใช่ client

## Autoscaling

`autoscaling.enabled=true` เพิ่ม HPA CPU ให้ API/worker ปกติ 2–6 และ 2–10 ที่ 70% ปกติปิด throughput ยังจำกัด
ด้วย PostgreSQL admission/`max_inflight` และ CPU ไม่สะท้อน queue จึงเลื่อน KEDA/external queue metrics ไว้

## End-to-end run สำหรับ development เท่านั้น

`scripts/k8s-e2e.sh` พิสูจน์ chart บน minikube 2 node ด้วย Calico ที่บังคับ policy

1. เริ่ม profile `eacp-e2e` ถ้ายังไม่มี Docker driver แต่ละ node 3 CPU และ 2 800 MB
2. build/load `eacp:dev`, `eacp-agt-pdp:dev` และ dev images เข้า cluster ไม่ pull ภายใน
3. ติดตั้ง SPIRE 1.15.3 สำหรับ dev ใน `spire`: server SQLite/emptyDir อยู่ control plane, trust domain
   `eacp.test`, agent ทุก node และ CSI ลงทะเบียน worker เป็น `spiffe://eacp.test/ns/eacp/sa/eacp-worker`
   JWT-SVID TTL 3600 แล้ว snapshot trust bundle ให้ Fake ERP
4. สร้าง namespace `eacp` แบบ restricted, `eacp-deps`, `agents` และ dev Secrets จาก
   `deployments/docker/secrets`/`eacpctl pdp-dev-certs` dependencies คือ PostgreSQL, NATS, Fake ERP/MCP,
   Vault dev/Kubernetes-auth init, HR Fake MCP, Fake LLM และ busybox agent พร้อม policy แยก
5. Helm install ด้วย `deployments/k8s/e2e-values.yaml` และ `deployments/k8s/studio-e2e-values.yaml`
6. เปิด tunnel ผ่าน API Service ที่ทน pod restart รัน `TestSliceADemo`, `TestKubernetesDisruption`,
   `TestJITDemo`, `TestFederatedJITDemo`, `TestPrivateKeyJWTDemo`, `TestVaultDemo`, `TestSPIFFEDemo`,
   `TestTokenExchangeDemo`, `TestAWSDemo` และ `TestStudioDemo` โดย `EACP_DEMO_PLATFORM=k8s`
7. ลบ profile

```bash
bash scripts/k8s-e2e.sh                      # full run, then delete the cluster
KEEP=1 bash scripts/k8s-e2e.sh               # leave it running
TESTS=TestSliceADemo bash scripts/k8s-e2e.sh # choose the tests
TESTS=NONE KEEP=1 bash scripts/k8s-e2e.sh    # install only
```

Disruption test ส่ง purchase 30 รายการ รวม call ช้า ขณะ cordon control plane แล้ว rollout API/PDP ไป node สอง
scale worker เป็น 3 และ drain node นั้น eviction ต้องผ่าน PDB ทีละตัว ทุก purchase ต้องรับครั้งแรกและจบ
SUCCEEDED พร้อม ERP record เดียว Service ต้องมี ready endpoint ตลอด health ทุก 100 ms ต้องไม่พลาด
agent เข้าถึง API เท่านั้น ยกเว้น gateway ที่กำหนดใหม่ เข้า PDP/DB/NATS/ERP/worker ไม่ได้ และ pod ทั้งหมดต้อง
non-root/read-only/restricted หากไม่มี PDB หรือ PDP shutdown delay จะขาด endpoint/ได้ 503 หากไม่มี DNS retry
query หายระหว่าง churn อาจกินเวลาเรียก PDP จนได้ `governance_unavailable`

Slice C ต้อง copy ไฟล์เข้า distroless Fake MCP จึงรันบน compose เท่านั้นและ skip บน Kubernetes

Studio demo สร้าง principal runtime ผ่าน public API ใส่ master สุ่มและ key ลง Secret สองตัวโดยไม่แสดงค่า
แล้วเปิด Studio ผ่าน Helm ใช้ fixture สามตัวเดียวกับ compose ทั้งสอง provider/branch ปฏิเสธ budget/model
output ผิด preview ส่ง tool ไม่ได้ กู้ call เดิม และ kill run/model probe busybox ไม่มี token ใต้ selector ของ runtime
เข้า API/gateway ได้แต่เข้า DB/PDP/NATS/HR/provider โดยตรงไม่ได้ ตรวจ prompt/key canary และล้าง typed output
เมื่อจบ render tests ไม่ต้องมี cluster: `EACP_HELM_REQUIRED=1 go test ./test/helm` ถ้าไม่ตั้งจะ skip เมื่อไม่มี Helm

## นอกขอบเขต

Queue-depth autoscaling, cert-manager, service mesh, PostgreSQL/NATS operator หรือ HA, Ingress/Gateway objects,
publishing images, multi-cluster และ Slice C บน Kubernetes
