[English](FEATURES.md) | [ไทย](FEATURES.th.md)

# ความสามารถ

หน้านี้รวบรวมสิ่งที่ EACP ทำได้ในวันนี้ ทีละ phase พร้อมหลักฐานเบื้องหลังแต่ละความสามารถ คือ test ที่บังคับใช้, API route และคำสั่ง
ที่เปิดให้ใช้ และ ADR ที่ตัดสินเรื่องนั้น [README](../README.th.md) อธิบายแนวคิด ส่วนหน้านี้เป็นบัญชีรายการ และ [INVARIANTS.md](INVARIANTS.md)
จับคู่การรับประกันแต่ละข้อของ MASTER_PLAN §103 กับ test ของมัน

## ภาพรวม

| Phase | สิ่งที่เพิ่ม | การตัดสินใจ |
|---|---|---|
| 1–8 (Slice A) | registry, identity และ capability; governance และการอนุมัติที่คงทน; Action API และ release boundary แบบ atomic; lease, fencing และ dispatch intent; HTTP connector และ Fake ERP; `UNKNOWN_OUTCOME`, reconciliation และการตัดสินโดยคน; hardening และ demo | [ADR-001](adr/ADR-001-product-boundary-and-enforcement-point.md), [ADR-003](adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-004](adr/ADR-004-action-state-machine-and-execution-semantics.md), [ADR-005](adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) |
| 9 | AGT sidecar PDP | [ADR-002](adr/ADR-002-agt-integration-sidecar-pdp.md) |
| 10 | สัญญาณผ่าน NATS JetStream | [ADR-014](adr/ADR-014-postgresql-authority-nats-signals.md) |
| 11 | การจองงบประมาณแบบแข็ง | [ADR-012](adr/ADR-012-budget-reservation.md) |
| 12 | ตัวจัดคิวที่เป็นธรรม | [ADR-011](adr/ADR-011-scheduler-fairness.md) |
| 13 | backpressure, bulkhead, circuit breaker และ retry budget | [ADR-022](adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) |
| 14 | MCP registry และ tool fingerprint | [ADR-023](adr/ADR-023-mcp-registry-and-tool-fingerprint.md) |
| 15 | dependency graph และ blast radius | [ADR-015](adr/ADR-015-dependency-graph.md) |
| 16 | kill การทำงานแบบกระจาย | [ADR-016](adr/ADR-016-distributed-kill-switch.md) |
| 17 | fleet operation | [ADR-024](adr/ADR-024-fleet-operations.md) |
| 18 | Agent FinOps | [ADR-025](adr/ADR-025-agent-finops.md) |
| 19 | release และการประเมิน | [ADR-018](adr/ADR-018-release-and-evaluation.md) |
| 20–21 | Governance-as-Code | [ADR-026](adr/ADR-026-governance-as-code.md) |
| 22 | incident, Agent SOC และ operator console | [ADR-027](adr/ADR-027-incidents-and-agent-soc.md), [ADR-028](adr/ADR-028-operator-console.md) |
| 23 | high availability และ Kubernetes | [ADR-029](adr/ADR-029-high-availability.md) |
| 24a–24g | credential แบบ just-in-time | [ADR-019](adr/ADR-019-credential-custody.md) |
| 25a | A2A delegation ภายใต้ governance | [ADR-030](adr/ADR-030-a2a-delegation.md) |
| 25b | LLM gateway | [ADR-031](adr/ADR-031-llm-gateway.md) |
| §104 | load benchmark | [BENCHMARKS.md](BENCHMARKS.md) |

## Slice A (Phase 1–8): รากฐานและเส้นทางของ action

| ความสามารถ | หลักฐาน |
|---|---|
| service: `controlplane-api`, `execution-worker`, `fakeerp`, `eacpctl` | `docker compose up -d --build` |
| เริ่มทำงานแบบ fail closed: ไม่ยอมใช้ role ของฐานข้อมูลที่ bypass RLS ได้ | test ของ `internal/service`; container จบด้วย exit 1 เมื่อใช้ DSN ของ superuser |
| ธรรมเนียม Row-Level Security: ถ้าไม่มี tenant context จะไม่เห็นแถวใดเลย | test ของ `internal/storage` (ผ่าน mutation check) |
| schema migration (goose, ฝังในไบนารี, ใช้ advisory lock) | `eacpctl migrate up\|status` |
| health และ readiness (ฐานข้อมูล, ความปลอดภัยของ role, เวอร์ชัน schema) | `GET /healthz`, `GET /readyz` |
| ปิดบัง secret ใน log | test ของ `internal/logging`, `internal/service` |
| OpenTelemetry tracing พร้อม W3C propagation | test ของ `internal/telemetry` |
| ปิดระบบอย่างนุ่มนวล (graceful shutdown) | test ของ `internal/httpserver` |
| แยกเครือข่าย: agent เข้าถึง ERP หรือฐานข้อมูลไม่ได้ | `test/security` (ผ่าน mutation check) |
| registry: principal, การมอบ role แบบสองคน, group, agent, version, allowlist, connector, tool, contract ([ADR-003](adr/ADR-003-agent-registry-identity-and-capability.md)) | `internal/registry` (schema test รัน SQL ดิบด้วย role ของแอป และ trigger ผ่าน mutation check) |
| กฎสองคนที่บังคับโดย PostgreSQL: การมอบสิทธิ์, credential, การเปิดใช้ allowlist/contract/version, การปล่อยจากการกักกัน | `internal/registry/schema_test.go` |
| API key: ผู้ใช้สร้าง key เอง เก็บเฉพาะ hash ผูกกับ principal หรือ agent version หนึ่งตัว อายุไม่เกิน 90 วัน | `internal/identity` |
| capability check: tool ต้องอยู่ใน allowlist ของ version ที่ ACTIVE และมี contract ที่ active, ไม่ถูกเพิกถอน และ fingerprint ตรงกัน | `registry.CheckCapability`, `POST /v1/agent/capability-check` |
| audit journal แบบ hash chain เพิ่มได้อย่างเดียว เขียนใน transaction เดียวกับการเปลี่ยนแปลงแต่ละครั้ง | `internal/audit` (test การดัดแปลง) |
| governance provider ภายใน process พร้อมคำตัดสินห้าแบบ, policy version ที่แก้ไขไม่ได้ และการเปิดใช้แบบสองคน | `internal/governance`; `POST /v1/policies`, `POST /v1/policies/{id}/activate`, `GET /v1/policies/current` |
| digest แบบ JCS + SHA-256 ของ input และของ payload ที่บังคับใช้ และหลักฐานการตัดสินที่แก้ไขไม่ได้ | `internal/governance/digest.go`, `migrations/00004_governance_approvals.sql` |
| คำขออนุมัติที่คงทน, การโหวตของคน, quorum, การแบ่งแยกหน้าที่, การหมดอายุ และ grant ที่ใช้ได้ครั้งเดียวผูกกับ action | `internal/approval`; `GET /v1/approvals`, `GET /v1/approvals/{id}`, `POST /v1/approvals/{id}/votes` |
| Action API: ส่งคำขอแบบ idempotent (409 เมื่อ digest ต่าง), admission แบบคงที่ (429), governance ที่ fail closed ด้วย 503, `?wait=` | `internal/action`, `internal/api/actions.go`; `POST /v1/actions`, `GET /v1/actions/{id}` |
| state machine ก่อน dispatch ของ ADR-004 (T1–T13, T15) บังคับด้วย trigger ของ PostgreSQL แม้เป็น SQL ดิบ | `migrations/00005_actions.sql`, `internal/action/schema_test.go` (ผ่าน mutation check) |
| release boundary แบบ atomic: ตรวจซ้ำสดใหม่, ใช้ grant ครั้งเดียวที่ตรวจตอน commit, pin policy และ contract, journal และ outbox ใน transaction เดียว | test การปล่อยของ `internal/action` (ปล่อยขนาน, race ของการเปิดใช้, ความล้มเหลวที่ฉีดเข้าไป) |
| sweeper: กู้คืนหลังระบบล่ม, ปล่อยหลังได้รับอนุมัติ, การหมดอายุ และการยกเลิกที่ไม่ปรึกษา PDP | `internal/action/sweeper.go`; `POST /v1/actions/{id}/cancel` |
| worker claim (`FOR UPDATE SKIP LOCKED` ตามลำดับที่เป็นธรรมจาก Phase 12), heartbeat และ lease generation โดย PostgreSQL ปฏิเสธการเขียนของ worker ที่ล้าสมัย | `migrations/00006_execution.sql`, `migrations/00012_scheduler.sql`, `internal/worker/schema_test.go` (ผ่าน mutation check) |
| dispatch intent แบบ fenced ก่อนการเรียกใดๆ พร้อมตรวจ drift ซ้ำ (T16a/T16b) และแถว attempt ต่อการ dispatch แต่ละครั้ง ผลลัพธ์แบบ fenced และหลักฐานของผลที่มาช้า | `internal/worker` (lease race, worker ที่ล้าสมัยไม่มีทาง dispatch ซ้ำ) |
| เรียก lease คืน, retry ตาม contract และคำขอยกเลิกระหว่างกำลังทำงาน | `internal/action/sweeper.go`, `internal/action/execution_test.go` |
| credential ของ connector ใน worker แบ่งตาม tenant และผูกกับ host โดย agent ไม่ได้รับเลย | `internal/worker/secrets.go`, `test/security` |
| HTTP connector และ Fake ERP ที่ต้องใช้ credential พร้อมการค้นหาตาม operation key ที่คงทนและสถานการณ์จำลองความล้มเหลว | `internal/connector`, `internal/fakeerp`, `internal/worker/http_integration_test.go` |
| reconciler แบบ fenced: ค้นหาภายใต้ proof standard ที่ pin ไว้ มีเพียงการไม่พบที่ authoritative และยุติแล้วที่อนุญาตให้ retry ด้วย key เดิมหรือเป็น `FAILED` ส่วนความขัดแย้งและการหมดความพยายามส่งให้คน | `migrations/00007_reconciliation.sql`, `internal/worker/reconciler.go`, `internal/worker/reconcile_schema_test.go` |
| test หลักกับ Fake ERP: คำตอบหาย → มีรายการเดียว, มองเห็นช้า → ไม่ retry, worker ถูก kill → ไม่ dispatch ซ้ำแบบไม่ดูตาม้าตาเรือ | `internal/worker/reconcile_integration_test.go` |
| operator ตัดสินพร้อมการแบ่งแยกหน้าที่และการ retry แบบสองคน คิวและหลักฐานสำหรับ operator และ auditor | `internal/action/resolution.go`; `GET /v1/actions?state=`, `GET /v1/actions/{id}/evidence`, `POST /v1/actions/{id}/resolutions`; `eacpctl action` |
| สร้างหลักฐานคืนจาก `action_id`: คำตัดสิน, การอนุมัติพร้อมโหวตและ grant, attempt, การตรวจ, การตัดสิน และ journal พร้อมการตรวจสอบ chain | `internal/action/evidence.go`, `internal/worker/evidence_integration_test.go` |
| การแยก tenant: catalog test ของ RLS พร้อมเส้นทางข้าม tenant ที่ทบทวนแล้ว และกวาดตรวจทุกตารางหลังรันทั้ง flow | `internal/storage/rls_catalog_test.go`, `internal/worker/isolation_integration_test.go` |
| chaos: การเชื่อมต่อล่มถี่ขณะ loop ทำงาน, service รีสตาร์ต, worker ถูก kill, การส่งคำขอซ้ำ และ governance ล่มโดยไม่ขวางการยกเลิก | `internal/worker/chaos_integration_test.go` |
| แผนที่ invariant ของ Slice A ตรวจกับ MASTER_PLAN §103 | `docs/INVARIANTS.md`, `test/invariants` |
| demo ของ Slice A, Slice C, A2A, JIT credential และ LLM gateway บน stack ที่แยกออกมา (§111, §96) | `scripts/demo.sh`, [DEMO.md](DEMO.th.md) |

## Slice B (Phase 9): AGT sidecar PDP

| ความสามารถ | หลักฐาน |
|---|---|
| sidecar PDP ที่ห่อ AGT policy layer 5.0.0, ACS engine 0.3.1b1 และ OPA 1.20.2 ตามเวอร์ชันที่ pin ไว้ ไม่มี state และไม่เคยตัดสินการอนุมัติ | `sidecars/agt-pdp`, [research/REFERENCES.md](../research/REFERENCES.md) |
| Go client ที่ implement `GovernanceProvider`: loopback หรือ mutual TLS 1.3 เท่านั้น, pin เวอร์ชันในทุกคำตัดสิน, ตรวจคำตอบเข้มงวด, จำกัด clock skew | `integrations/governance/microsoftagt` |
| conformance: ชุดอ้างอิงเดียวพร้อม digest ซึ่ง provider ภายใน process, wire protocol และ sidecar (ผ่าน ACS และ OPA ใน image build ของมัน) ต้องให้ผลตรงกันทั้งหมด | `test/conformance/governance_reference.json`, `internal/governance/conformance_test.go`, `sidecars/agt-pdp/tests` |
| หลักฐานของ provider (identity ของ ACS, rule, digest ของ adapter, เวอร์ชันของ engine) เก็บและบันทึกลง journal พร้อมคำตัดสินแต่ละครั้ง | `migrations/00009_provider_evidence.sql`, `GET /v1/actions/{id}/evidence` |
| compose รัน `controlplane-api` ด้วย `EACP_GOVERNANCE_PROVIDER=microsoft-agt` บนเครือข่าย internal `pdp` มีเพียง API ที่เข้าถึง sidecar ได้ | `test/security/agt_pdp_test.go` |

## Slice B (Phase 10): สัญญาณผ่าน NATS JetStream

| ความสามารถ | หลักฐาน |
|---|---|
| outbox relay ใน `controlplane-api` publish หลัง transaction commit แล้ว lock แถวด้วย SKIP LOCKED และใช้ id ของแถวเป็น `Nats-Msg-Id` พร้อม `traceparent` ของแถว แถวถูกทำเครื่องหมายว่า publish แล้วหลังได้ PubAck เท่านั้น การส่งเป็นแบบ at least once | `internal/messaging/relay.go`, `internal/messaging/relay_test.go` |
| work hint มีเพียง `action_id` และปลุก claim loop ของ worker บน PostgreSQL การ poll ยังเปิดอยู่ ถ้าไม่มี NATS ระบบแค่ช้าลง ไม่ผิด | `internal/messaging/hints.go`, `TestHintedWorkerExecutesLongBeforeItsPollInterval`, `TestWithoutNATSTheWorkerStillExecutes` |
| inbox dedup (§63) ตารางของ tenant ภายใต้ RLS ที่เขียนได้เฉพาะ messaging actor `inbox` message ซ้ำถูก ACK และไม่ประมวลผลอีก | `migrations/00010_messaging.sql`, `TestHintWakesOnceAndADuplicateIsAckedWithoutWaking` |
| stream ของ dashboard event (`eacp.events.<tenant>.action.transition`) แต่ละ event มีเพียง id, สถานะ และเวลา ไม่เคยมีเหตุผลหรือ payload | `TestTransitionsWriteDashboardEvents`, `TestDashboardEventsArrivePerTenant` |
| compose รัน NATS บนเครือข่าย internal `bus` ผู้ใช้หนึ่งคนต่อบทบาท ผู้ใช้ของ worker publish ไม่ได้ และ agent ไม่มีเส้นทางไปถึง | `deployments/docker/nats/nats.conf`, `test/security/nats_test.go` |

## Slice B (Phase 11): การจองงบประมาณแบบแข็ง

| ความสามารถ | หลักฐาน |
|---|---|
| contract ของ connector ประกาศค่าใช้จ่ายของการเรียก คือหน่วย, ค่าคงที่ และ field จำนวนเงินใน payload PostgreSQL คำนวณค่าใช้จ่ายจาก payload ที่บังคับใช้ ซึ่งผูกกับ digest ของคำตัดสิน | `migrations/00011_budget.sql` (`eacp.action_cost`), `TestContractsDeclareACostOrNone` |
| transaction ของการปล่อยจองค่าใช้จ่ายในบัญชีงบประมาณระดับล่างสุดของ agent ก่อนเขียน journal ใดๆ และก่อนใช้ grant งบที่รับไม่ไหวจะปฏิเสธ action (`budget_exceeded`) การไม่มีบัญชีหรือค่าใช้จ่ายที่ไม่ถูกต้องก็ปฏิเสธเช่นกัน T10 ของ action ที่มีงบต้องมีการจอง | `eacp.budget_reserve`, `TestReservationsAreMadeOnlyByTheReleaseForTheActionsCost`, `TestABudgetDenialNeverSpendsTheApproval`, `TestBudgetFailuresDenyClosed` |
| ไม่จองเกิน (§103 invariant 3): ปล่อย 100 รายการพร้อมกันบนบัญชีที่รับได้ 37 จองได้ 37 พอดี `CHECK (allocated + reserved + committed <= hard_limit)` เป็นด่านสุดท้าย | `TestConcurrentReleasesNeverOversubscribeAHardBudget` (log p50/p99) |
| การเปลี่ยนสถานะของ action เองเป็นตัวปิดยอดการจอง: สำเร็จจะ commit, ไม่มีผลจะคืน, ผลที่ไม่รู้แน่ชัดจะถือไว้จนกว่าจะ reconcile หรือมีการตัดสิน และหมดอายุที่ `not_after` (TTL) จะคืน การปิดยอดไม่เคย lock บัญชี | `TestSettlementFollowsTheOutcome`, `TestAnExpiredReleaseGivesItsBudgetBack`, `TestSettlementNeverWaitsForTheAccount` |
| ต้นไม้ของบัญชีพร้อม escrow: วงเงินของบัญชีลูกถูกแบ่งจากบัญชีแม่ การจองจึง lock เฉพาะบัญชีล่างสุด การลดวงเงินใช้ admin คนเดียว การเพิ่มใช้สองคน | `TestEscrowBoundsChildrenByTheirParent`, `TestRaisingALimitIsTwoPersonAndLoweringIsNot`, `TestReleasesSettlementsAndLimitChangesDoNotDeadlock` |
| Budget API: `POST /v1/budgets`, `GET /v1/budgets[/{id}]`, `POST /v1/budgets/{id}/limit`, `POST /v1/budget-limit-changes/{id}/approve\|reject` หลักฐานของ action แสดงการจอง | `internal/api/budget.go`, `TestBudgetsThroughTheAPI` |

## Slice B (Phase 12): ตัวจัดคิวที่เป็นธรรม

| ความสามารถ | หลักฐาน |
|---|---|
| PostgreSQL เลือกคิวตามน้ำหนักของ tenant/team โดย team pin น้ำหนักของ group และ priority ของ contract ไว้ตอนปล่อย พร้อม aging และการเลื่อนตาม deadline ภายใน team NATS ยังเป็นเพียงสัญญาณปลุก | [ADR-011](adr/ADR-011-scheduler-fairness.md), `TestSchedulerServesTenantWithSmallerBacklog`, `TestSchedulerUsesPinnedTeamWeight`, `TestSchedulerPriorityAndAging` |
| T14 จัดลำดับและบังคับ `max_inflight` ของ connector หรือ group ที่ตั้งชื่อ รวมถึง SQL ดิบ, การ claim พร้อมกัน และ snapshot ของ transaction ที่ล้าสมัย | `TestConnectorCapacityIsEnforcedForRawClaims`, `TestConnectorCapacitySerializesConcurrentClaims`, `TestConnectorCapacityRejectsStaleRepeatableReadClaim`, `TestNamedCapacityGroupSpansConnectorsAndStateIsTenantIsolated` |
| เมื่อมีงานค้าง 10,000, 100 และ 100 รายการ แต่ละ team เล็กได้ 10 จาก 30 การ claim แรก | `BenchmarkSchedulerFairness` |

## Slice B (Phase 13): backpressure, bulkhead, circuit breaker และ retry budget

| ความสามารถ | หลักฐาน |
|---|---|
| admission ตอบ 429 พร้อม `scope` และไม่สร้างสิ่งใด: คิวระดับ global และ tenant, action ที่ยังไม่ถูกปล่อยของ tenant (`EACP_ACTION_MAX_PENDING_PER_TENANT`) และคิวของ connector group (`max_queued` ของ contract) คิวของ connector ที่เต็มยังรับ connector อื่นได้ | [ADR-022](adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) §1, `TestAdmissionBoundsPendingAndConnectorQueues`, `TestAdmissionLimitIs429` |
| connector ที่ล้มไม่ทำให้ pool อื่นขาดทรัพยากร (§103 invariant 9): worker ถือ action ได้ไม่เกิน `EACP_WORKER_GROUP_CONCURRENCY` ต่อ capacity group และ `max_inflight` จำกัด group ข้าม worker | `TestConnectorFailureDoesNotStarveUnrelatedConnectors` |
| breaker ต่อ worker และ connector เปิดหลังล้ม `EACP_WORKER_BREAKER_FAILURES` ครั้ง ทดลองหนึ่งครั้งหลัง cooldown และเพิ่ม cooldown เป็นสองเท่าทุกครั้งที่ตัดติดกัน breaker ยังเปิด circuit ที่ใช้ร่วมกันของ connector และบันทึกลง journal ระหว่างที่เปิด PostgreSQL ปฏิเสธการ claim (T14) และ dispatch intent (T16) operator ปิดหรือเปิด connector ได้ (`eacpctl connector disable\|enable`) | `TestBreakerOpensProbesAndCloses`, `TestLocalBreakerHoldsWithoutTheSharedCircuit`, `TestOpenCircuitRefusesClaimAndDispatch`, `TestDisableSerializesWithAStaleDispatchIntent`, `TestCircuitChangesAreGuardedAndJournaled` |
| retry ถอยแบบ exponential พร้อม jitter และอยู่ใน retry budget ของ contract คือจำนวนครั้ง, `retry_max_elapsed_ms` นับจาก dispatch แรก และ `retry_max_cost` PostgreSQL ตอบคำถามเรื่อง budget ให้ worker, sweeper, reconciler และการ retry ของ operator เหมือนกันหมด | `TestDefaultBackoffIsJitteredWithinBounds`, `TestRetryBudgetBoundsElapsedTime`, `TestRetryBudgetBoundsRetryCost` |

## Slice C (Phase 14): MCP registry และ tool fingerprint

| ความสามารถ | หลักฐาน |
|---|---|
| MCP server คือ connector ที่มี protocol `mcp` tool ของมันถูกค้นพบ ไม่ได้ถูกประกาศ scanner ของ execution worker ดึงรายการผ่าน Streamable HTTP (2026-07-28 และถอยไปใช้ revision ปี 2025 ที่ใช้ initialize ทั้ง JSON และ SSE) ด้วย credential ที่ worker ถือและผูกกับ host ทุก `EACP_MCP_SCAN_INTERVAL` และจำกัดจำนวนหน้า tool และ byte | [ADR-023](adr/ADR-023-mcp-registry-and-tool-fingerprint.md) §1–§2, `internal/connector/mcp` (ทำงานร่วมกับ MCP Go SDK ตัวทางการ), `TestScannerDiscoversToolsAndQuarantinesDrift` |
| การ scan ถูก fence ด้วย scan lease (worker id, generation, เวลาหมดอายุ) ใน PostgreSQL scanner ที่ล้าสมัยบันทึกอะไรไม่ได้ และการ scan ที่ล้มไม่เปลี่ยน tool ใด | `TestScanLeaseFencesStaleScanners`, `TestStaleScannerCannotRecord`, `TestScannerRecordsFailuresWithoutChangingTools` |
| PostgreSQL คำนวณ fingerprint ของนิยามแบบ canonical (RFC 8785) เก็บประวัติ และจำแนกการเปลี่ยนแปลง: เปลี่ยนเฉพาะการแสดงผล (`title`, `icons`) ถือว่าเสี่ยงต่ำ นอกนั้นเสี่ยงสูง annotation ของ server เป็นเพียงคำใบ้ที่ไม่น่าเชื่อถือซึ่งทำได้แค่เข้มงวดขึ้น | `TestScanRecordsDefinitionsWithDatabaseFingerprints`, `TestDefinitionChangesAreClassifiedAndInvalidateContracts`, `TestMCPContractRules` |
| contract pin นิยามที่ทบทวนแล้ว การเปลี่ยนที่เสี่ยงสูง หรือ tool ที่รับรองแล้วหายไป จะกักกัน tool และ contract จะไม่ตรงอีกต่อไป action ที่อนุมัติแล้วจะถูกปฏิเสธตอนปล่อย การกักกันคือ containment (operator หรือ registry approver) การปล่อยต้องใช้ registry approver คนที่สองและไม่รับรองใหม่ | `TestMCPDefinitionDriftBeforeReleaseDenies`, `TestMissingCertifiedToolIsQuarantined`, `TestToolQuarantineRules` |
| operator ดู tool, ประวัตินิยาม และการ scan ขอ scan ใหม่ และกักกันหรือปล่อย tool ได้ | `GET /v1/connectors/{id}/tools`, `GET /v1/connectors/{id}/mcp[/scans]`, `POST /v1/connectors/{id}/mcp/scan`, `GET /v1/tools/{id}[/definitions]`, `POST /v1/tools/{id}/quarantine\|release`; `eacpctl connector tools\|mcp\|scans\|scan`, `eacpctl tool` |

การเรียก MCP tool (`tools/call`) คือ Phase 26a ([ADR-032](adr/ADR-032-mcp-tools-call.md)) execution worker ส่ง `tools/call` ไม่เกินหนึ่งครั้งต่อ action หลังแสดงรายการ tool ของ server และพบว่านิยามของ tool ตรงกับที่รับรองไว้ทุกไบต์ (`definition_changed`, `tool_missing` และ `definition_unverified` จะไม่ส่งอะไรเลย) tool ที่ใช้ `x-mcp-header` ถูกปฏิเสธ (`unsupported_header_mirroring`) reference ของความสำเร็จคือ `mcp:sha256:<digest ของผลลัพธ์>` ผลลัพธ์ของ tool ไม่ถูกบันทึกลง journal หรือ log และจะถูกเก็บไว้ให้ agent ที่เรียกอ่านเฉพาะเมื่อ contract เลือกเปิด (Phase 26b ด้านล่าง) ผลลัพธ์ `isError`, output ที่ไม่ถูกต้อง, `input_required` และความล้มเหลวของการส่งหลังส่งไปแล้วจะเป็น `UNKNOWN_OUTCOME` เว้นแต่ class นั้นถูกรับรอง หลักฐาน: `TestTheWorkerCallsAnMCPTool`, `TestAChangedDefinitionBetweenScansIsNeverCalled`, `TestAnMCPActionIsNeverRetried`, `TestNothingOfTheOutputIsPersisted`, `TestAToolWithHeaderMirroringIsRefused`

## Slice C (Phase 15): dependency graph และ blast radius

| ความสามารถ | หลักฐาน |
|---|---|
| allowlist ที่ active ให้เส้นทาง Agent→Tool และ Agent→MCP และ tool เป็นของ MCP server editor บันทึกการสังเกต Agent→Model, Agent→MCP, Agent→Agent และ Tool→System พร้อมแหล่งที่มา วันหมดอายุ และความมั่นใจ ฐานข้อมูลคุมและบันทึก audit ทุกการเขียน | [ADR-015](adr/ADR-015-dependency-graph.md), `TestDependencyWritesAreGuardedAndTenantScoped` |
| recursive CTE ของ PostgreSQL คืน agent version ที่ได้รับผลกระทบแบบยืนยันแล้วและแบบเป็นไปได้ หลักฐานที่เก่าหรือไม่รู้ทำให้ชุดที่เป็นไปได้กว้างขึ้น และระบุ coverage ชัดเจนว่า `observed_only` | `TestBlastRadiusTraversesRegistryAndDeclaredDependencies`, `TestBlastRadiusWidensForUnknownOrStaleEvidence` |
| operator และ auditor query `GET /v1/dependencies/blast-radius?kind=mcp&id=<uuid>` หรือ `eacpctl dependency blast-radius mcp <uuid>` editor บันทึกด้วย `POST /v1/dependencies` และเพิกถอนด้วย `POST /v1/dependencies/{id}/revoke` สำหรับเป้าหมายที่เป็น model/system ให้ใช้ `name=<tenant-local-name>` แทน `id` | `TestDependencyAPIRecordsAndReportsBlastRadius`, `TestDependencyBlastRadiusCommand` |

รายงานนับ **action** ล่าสุด ไม่ใช่ workflow run เพราะ identity ของ workflow ยังไม่อยู่ใน registry หลักฐานในกราฟไม่เปลี่ยนการอนุญาตให้ทำงานและไม่สั่ง containment

## Slice C (Phase 16): kill การทำงานแบบกระจาย

operator ใช้ `POST /v1/killswitch` กับ `{"scope":"agent_version","target_id":"<uuid>","killed":true,"reason_code":"security_incident","reason":"<incident note>"}` ค่าเริ่มต้นของ `reason_code` คือ `operator_request` และรับ [reason code ของ AGT](adr/ADR-016-distributed-kill-switch.md) สี่แบบ operator อีกคนเป็นผู้ resume scope ด้วย `killed:false` operator และ auditor ดูรายการสถานะด้วย `GET /v1/killswitch` CLI มี `eacpctl kill activate|resume <scope> <uuid> --reason <text> [--code <reason-code>]` และ `eacpctl kill list`

scope ที่บังคับใช้ได้คือ `tenant`, `team`, `agent`, `agent_version`, `action`, `connector`, `tool` และตั้งแต่ Phase 25b คือ `model` ซึ่งระบุ LLM model ของ tenant และหยุดเฉพาะการเรียก LLM PostgreSQL ปฏิเสธ scope ที่ถูก kill ตอน claim และตอน dispatch intent worker ตรวจอีกครั้งก่อนเรียกภายนอกและระหว่างการเรียก NATS แค่ปลุกการตรวจนั้น kill ระหว่างการทำงานจะบันทึก `UNKNOWN_OUTCOME` เพื่อ reconcile เพราะการยกเลิกไม่ได้ย้อนผลภายนอก `global` และ `run` ถูกปฏิเสธจนกว่า EACP จะมี platform operator authority และการผูก run กับ action ที่ยืนยันตัวตนแล้ว ดู ADR-016

## Slice C (Phase 17): fleet operation

`GET /v1/fleet/health` คือ dashboard ของ fleet และ `GET /v1/fleet/agents` แสดงรายการ agent ทั้งสองรับ filter `environment`, `risk_class`, `owner_group_id`, `health` และ `window` agent แต่ละตัวแสดง version ที่ active, สถานะของเจ้าของ, kill ที่ตรงกัน, tool ใน allowlist ที่ฐานข้อมูลจะไม่ยอมให้ทำงาน, circuit ที่เปิด และ action ที่เปิดอยู่และล่าสุด health ที่ได้คือ `ok`, `degraded` หรือ `contained` พร้อมเหตุผล ทั้งหมดเป็นการสังเกตเท่านั้น ไม่เคยให้หรือขวางสิ่งใด

`POST /v1/fleet/operations` ทำ operation หนึ่งแบบ atomic คือทุก version เปลี่ยน หรือไม่มีอันไหนเปลี่ยนเลย dry run คืนแผนโดยไม่เปลี่ยนอะไร:

```json
{"kind":"pause","selector":{"environment":"production","tool":"erp.post"},"reason":"ERP incident 42","dry_run":true}
```

- `pause` ระงับ version ที่ active หลังจากนั้น action ที่อยู่ในคิวและ action ใหม่จะถูกปฏิเสธ ถ้าต้องการพักงานไว้ให้ใช้ kill (Phase 16) แทน
- `quarantine` กักกันทุก version ที่ยังใช้งานของ agent ที่เลือก
- `resume` และ `release` ย้อนเฉพาะ version ที่ `source_operation_id` ที่ระบุเป็นผู้เปลี่ยน
- `rollback` เปิดใช้ version เก่าที่ `SUSPENDED` ของ agent หนึ่งตัว

operator สั่ง pause และ quarantine ได้ มีเพียง `registry_approver` ที่ resume, release หรือ rollback ได้ ภายใต้การแบ่งแยกหน้าที่เดียวกับการเปิดใช้ทีละรายการ CLI คือ `eacpctl fleet status|list|operation|pause|quarantine|resume|release|rollback`

## Phase 18: Agent FinOps

การเรียก model ที่ผ่าน LLM gateway (Phase 25b) ถูกวัดและคิดราคาโดย gateway ส่วนการเรียกที่ทำนอก gateway EACP รับข้อมูลค่าใช้จ่ายเข้ามา โดย runtime ของ agent ชี้ OTLP/HTTP trace exporter ไปที่ `<api>/v1/agent/otlp` ด้วย agent key เป็น JSON (`OTEL_EXPORTER_OTLP_PROTOCOL=http/json`) จะ gzip หรือไม่ก็ได้ span ที่มี usage ตามแบบ OpenTelemetry GenAI (`gen_ai.operation.name` พร้อม `gen_ai.usage.*_tokens`) ถูกบันทึกให้ agent ของ key นั้นและคิดราคาใน PostgreSQL span อื่นถูกละเว้น

- **ตารางราคา** admin เพิ่มราคาต่อล้าน token ด้วย `POST /v1/finops/prices` (หรือ `eacpctl finops price add`) ราคามีผลตั้งแต่ตอนนี้ไป ไม่ย้อนหลัง usage ที่ไม่มีราคาจะยังไม่ถูกคิดราคาและทำให้เกิด alert `unpriced_usage` ไม่เคยนับเป็นศูนย์
- **billing** admin นำเข้ารายการ billing ของผู้ให้บริการด้วย `POST /v1/finops/billing` (หรือ `eacpctl finops billing import <file.json>`) ต่อ agent ต่อวัน ใช้ยอดที่มากกว่าระหว่างยอดที่รายงานกับยอดที่เรียกเก็บ
- **chargeback** `GET /v1/finops/chargeback?group_by=agent|team|account&from=…&to=…` รวมค่าใช้จ่ายของ tool จากงบประมาณแบบแข็ง (ที่ commit และที่ถือไว้) กับค่าใช้จ่ายของ LLM
- **soft limit และ alert** admin ตั้ง soft limit รายเดือนให้บัญชีงบประมาณด้วย `PUT /v1/finops/soft-limits/{account}` ตัวประเมิน (ทุก `EACP_FINOPS_INTERVAL` ค่าเริ่มต้น 1m) ออก alert ที่ 80 % และ 100 % ของวงเงิน และออก alert เมื่อการใช้จ่ายรายชั่วโมงผิดปกติ และเมื่อมี usage ที่ไม่มีราคา operator รับทราบ alert ด้วย `POST /v1/finops/alerts/{id}/ack`
- **dashboard** `GET /v1/finops/dashboard` แสดงค่าใช้จ่ายวันนี้และตั้งแต่ต้นเดือน, agent ที่ใช้มากที่สุด, การถูกขวางด้วยงบประมาณแบบแข็งวันนี้ และ alert ที่เปิดอยู่

soft limit และ alert ไม่เคยขวางสิ่งใด มีเพียงงบประมาณแบบแข็งที่ขวาง

## Phase 19: release และการประเมิน

release ย้าย agent จาก version ที่ `ACTIVE` (stable) ไปยัง candidate: `EVALUATING → SHADOW → CANARY → PROMOTED` หรือ `ROLLED_BACK`

- **เปิด** registry editor เปิด release ด้วย `POST /v1/releases` (หรือ `eacpctl release open`) โดยระบุชุดการประเมินที่ต้องผ่าน, เกณฑ์ของ replay และ shadow, ขั้นของ canary เป็น basis point และค่าที่ยอมได้ของ guardrail
- **ประเมิน** role ของ registry บันทึกผลของชุดการประเมินพร้อม digest ของ dataset และการอ้างอิงหลักฐาน (`POST /v1/releases/{id}/evaluations`) EACP ใช้ผลเหล่านี้เป็นด่าน แต่ไม่ได้รันการประเมินเอง
- **replay และ shadow** runtime ของ candidate ส่งสิ่งที่จะทำสำหรับ action อ้างอิงไปที่ `POST /v1/agent/release/observations` ด้วย key ของ candidate PDP ตัดสินข้อเสนอ PostgreSQL จับคู่กับ action อ้างอิงและคำนวณความสอดคล้อง replay ตอบกลับด้วยผลลัพธ์ที่ EACP บันทึกไว้ของ action อ้างอิงเท่านั้น candidate ยังไม่ `ACTIVE` ก่อน canary จึงไม่มีสิ่งใดในขั้นนี้ทำงานได้จริง
- **canary** `registry_approver` คนที่สองเลื่อน release (`POST /v1/releases/{id}/advance` พร้อมสถานะที่ตนทบทวน) candidate กลายเป็น `ACTIVE` คู่กับ version stable และให้บริการเฉพาะ subject ที่ bucket อยู่ในขั้นปัจจุบัน ที่เหลือเป็น `DENIED canary_cohort` runtime ถาม `GET /v1/agent/release/route?subject=` ว่าจะใช้ version ไหน ทุกขั้นและการ promote ต้องมี action ของ candidate มากพอและไม่ละเมิด guardrail เมื่อเทียบกับ version stable (การปฏิเสธ, ความล้มเหลว, ผลลัพธ์ที่ไม่รู้แน่ชัด, latency p95, ค่าใช้จ่ายต่อ action)
- **rollback** operator หรือผู้อนุมัติ rollback ด้วย `POST /v1/releases/{id}/rollback` และทุก `EACP_RELEASE_INTERVAL` (ค่าเริ่มต้น 30s) control plane ยัง rollback canary ที่รายงานละเมิด guardrail ด้วย การ promote ไม่เคยเกิดขึ้นอัตโนมัติ

## Phase 20–21: Governance-as-Code

ทีม platform เก็บ registry ที่อยู่ภายใต้ governance ไว้ใน Git ได้ ([ADR-026](adr/ADR-026-governance-as-code.md)) bundle คือไดเรกทอรีที่มี
`eacp.yml` และ `resources/*.yml` ประกอบด้วย connector, tool และ contract, agent พร้อม version และ allowlist (Phase 20) รวมถึง principal,
การมอบ role, group, policy, งบประมาณ และราคา (Phase 21) target ผูก bundle กับ API และ tenant และ `eacpctl` แทนค่าตัวแปรก่อนส่งเอกสาร JSON
หนึ่งฉบับ bundle ไม่เคยมีค่า secret

- **วางแผน** `eacpctl bundle plan` (หรือ `POST /v1/change-sets` พร้อม `dry_run`) เทียบ bundle กับ registry ใน snapshot เดียว และตอบกลับ
  เป็นขั้นตอนที่เรียงลำดับแล้ว แต่ละขั้นอยู่ใน stage `submit` หรือ `approve` PostgreSQL คำนวณ digest ของสถานะที่ต้องการและของ registry ที่อ่าน
- **deploy และอนุมัติ** `eacpctl bundle deploy` บันทึก change set และ submit โดย stage submit ทำงานในนามผู้ submit ผ่าน transaction ของ registry
  ชุดเดียวกับ API คนที่สองอนุมัติ (`eacpctl bundle approve`) และ stage approve เปิดใช้ข้อเสนอและย้าย version change set ที่ registry เปลี่ยนไป
  แล้วถือว่าล้าสมัยและไม่รันอะไรเลย
- **ไม่มีการลบ** เมื่อใช้ `--prune` สิ่งที่ bundle ไม่ได้ประกาศแล้วจะถูกปลดระวางหรือเพิกถอนผ่าน lifecycle move ที่มีอยู่ version ไม่เคยถูกเปิดใช้
  คู่กับ version อื่นที่ `ACTIVE` (ให้ใช้ release) containment ไม่เคยถูกย้อน และ MCP tool ไม่เคยถูกประกาศ
- **drift** `eacpctl bundle drift` เทียบ registry กับ change set ล่าสุดที่ apply แล้วของ bundle และรายงานแต่ละ address เป็น `in_sync`, `modified`,
  `missing` หรือ `unmanaged_reference` ไม่เคยซ่อมแซมสิ่งใด

[ตัวอย่าง 03](../examples/03-governance-as-code/README.th.md) รันวงจรทั้งหมดนี้

## Phase 22: Agent SOC และ operator console

- **incident** ทุก `EACP_INCIDENT_INTERVAL` (ค่าเริ่มต้น 15s) control plane เปิด incident จากสัญญาณที่มีอยู่ คือ MCP drift, kill, circuit ที่เปิด, ผลลัพธ์ที่ไม่รู้แน่ชัด, canary rollback และการใช้จ่ายเกินใน FinOps แต่ละรายการพร้อม blast radius operator รับทราบ มอบหมาย จดบันทึก เชื่อมโยง และปิด incident ได้ (`/v1/incidents`, `eacpctl incident`) incident ระดับ critical ต้องปิดโดยคนที่สอง `GET /v1/soc/summary` (`eacpctl soc summary`) ให้ตัวนับของ SOC
- **console** เปิด `http://localhost:8080/ui/` แล้ว sign in ด้วย principal API key key อยู่ในหน่วยความจำของแท็บเท่านั้น การ reload หรือไม่มีการใช้งาน 30 นาทีจะ sign out console แสดง overview, incident, security (kill, circuit, tool ที่ถูกกักกัน), fleet, การอนุมัติ, การทำงาน, inventory, dependency และค่าใช้จ่าย และจัดการวงจรชีวิตของ incident และ containment ผ่าน API ชุดเดียวกันหลังกล่องยืนยัน console ไม่มีอำนาจของตัวเอง (ADR-028) ตั้ง `EACP_UI=off` เพื่อปิด

## Phase 23: รันหลาย replica และ Kubernetes

รัน replica ของ `controlplane-api` และ `execution-worker` กี่ตัวก็ได้กับฐานข้อมูลเดียว ไม่มี leader และ PostgreSQL เป็นผู้ตัดสินทุก loop เบื้องหลัง (ADR-029) ตัวประเมินของ incident, FinOps และ release ข้าม tenant ที่ replica อื่นกำลังประเมิน sweeper ยอมรับ action ที่ replica อื่นย้ายไปก่อน และ worker แต่ละตัวต้องมี `EACP_WORKER_ID` ไม่ซ้ำกัน (ค่าเริ่มต้นคือชื่อ host) ตั้ง `EACP_SHUTDOWN_DELAY` (0s–60s ค่าเริ่มต้น 0s) เมื่ออยู่หลัง load balancer เมื่อได้ SIGTERM service จะให้ `/readyz` ล้ม ปิดการเชื่อมต่อแบบ keep-alive หลังตอบแต่ละครั้ง หยุด loop และยังให้บริการต่อตามเวลาที่ตั้งก่อนปิด `internal/worker` `TestReplicasShareTheWorkAndSurviveLosingOne` รันชุด loop ของ API สามชุดและ worker สามตัว แล้วหยุดอย่างละตัวกลางทาง

บน Kubernetes ให้ติดตั้ง Helm chart `deployments/helm/eacp` (Phase 23b, ADR-029 Rev 1.1) ซึ่งมี Deployment ที่ harden และมี replica สำหรับ API, worker และ PDP, ขอบเขตเครือข่ายแบบ compose ในรูป NetworkPolicy, Secret ที่อ้างอิงด้วยชื่อเท่านั้น, migration ในรูป hook, disruption budget และ CPU autoscaling ที่เลือกเปิดได้ PostgreSQL และ NATS อยู่ภายนอก ดู [docs/KUBERNETES.md](KUBERNETES.md) ส่วน `bash scripts/k8s-e2e.sh` พิสูจน์บน minikube cluster สองโหนด

## Phase 24a: credential แบบ just-in-time

credential ของ connector ไม่จำเป็นต้องเป็น secret แบบคงที่อีกต่อไป (ADR-019) รายการในไฟล์ connector-secrets ของ worker อาจระบุ OAuth 2.0 client-credentials provider แทน `value`:

```json
{"tenant_id": "…", "secret_ref": "erp-jit", "host": "erp.internal:8443",
 "oauth2": {"token_url": "https://idp.internal/oauth2/token", "client_id": "eacp-worker",
            "client_secret_file": "/run/secrets/idp", "scope": "erp.purchase"}}
```

worker สร้าง Bearer token (ต้องหมดอายุภายในหนึ่งชั่วโมง) ก่อนการเรียกไม่นาน ใช้ซ้ำเฉพาะเมื่อ token มีอายุเกินการเรียกทั้งหมดบวก 30 วินาที และไม่เคยเก็บไว้ ถ้า token endpoint ล้มจะไม่มีการ dispatch และ worker หยุด claim งานของ binding นั้นช่วง back-off 1–60 วินาที token และ client secret ถูกปิดบังจากทุก log มีเพียง worker ที่ถือสิ่งเหล่านี้ agent, API และ PostgreSQL ไม่เคยได้ถือ `DEMO=J scripts/demo.sh` รัน demo ของ JIT

## Phase 24b: workload identity federation

OAuth provider ยืนยันตัวตนได้โดยไม่ต้องมี client secret (ADR-019 Rev 1.1) ให้ระบุ JWT ที่ platform ออกและหมุนเวียนให้แทน เช่น Kubernetes service-account token ที่ chart project เข้าไปใน worker:

```json
{"tenant_id": "…", "secret_ref": "erp-wif", "host": "erp.internal:8443",
 "oauth2": {"token_url": "https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token",
            "client_id": "<application id>", "client_assertion_file": "/run/secrets/eacp-identity/token",
            "scope": "api://erp/.default"}}
```

worker อ่านไฟล์ทุกครั้งที่สร้าง token และส่งเป็น client assertion ตาม RFC 7523 ซึ่งเป็นคำขอที่ Entra ID กำหนดไว้สำหรับ federated credential assertion ที่อ่านไม่ได้ รูปแบบผิด หรือใกล้หมดอายุ จะทำให้ binding นั้น back off เหมือน token endpoint ล้ม token อาจมีอายุได้ถึงหนึ่งวัน (Entra ID ออกให้ 60–90 นาที) แต่ใช้ได้ไม่เกินหนึ่งชั่วโมง เมื่อเปิด `worker.workloadIdentity.enabled` Helm chart จะ project token เข้าไปใน pod ของ worker เท่านั้น ภายใต้ ServiceAccount ของ worker เอง ดู [docs/KUBERNETES.md](KUBERNETES.md) ส่วน `scripts/k8s-e2e.sh` รัน `TestFederatedJITDemo` ซึ่งซื้อของด้วย token ที่สร้างจาก service-account issuer จริงของ cluster

## Phase 24c: private_key_jwt

ในที่ที่ไม่มี platform ออก identity ให้ worker (VM, compose, host ภายในองค์กร) worker เซ็น client assertion เองด้วย private key ได้ (ADR-019 Rev 1.2) IdP ลงทะเบียนเฉพาะครึ่งที่เป็น public ในรูป certificate (Entra ID) หรือ JWKS (Okta, Keycloak):

```json
{"tenant_id": "…", "secret_ref": "erp-pkjwt", "host": "erp.internal:8443",
 "oauth2": {"token_url": "https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token",
            "client_id": "<application id>", "scope": "api://erp/.default",
            "private_key_jwt": {"alg": "PS256", "key_file": "/run/secrets/idp/key.pem",
                                "certificate_file": "/run/secrets/idp/cert.pem"}}}
```

ทุกคำขอ token มี assertion ใหม่ที่ใช้ได้ห้านาทีพร้อม `jti` ไม่ซ้ำ (RS256, ES256 หรือ PS256 พร้อม `x5t#S256` ของ certificate ตามที่ Entra ID ต้องการ) key และ certificate ถูกอ่านครั้งเดียวตอนเริ่มทำงานและตรวจสอบตอนนั้น ยกเว้นช่วงอายุของ certificate ซึ่ง certificate ที่หมดอายุจะทำให้การสร้าง token ของ binding นั้นล้ม (พร้อม back-off) แทนที่จะทำให้ทั้ง worker ล้ม key ถูกปิดบังจาก log และไม่เคยถูกส่งให้ connector บน Kubernetes ให้ใส่ PEM ไว้ในไฟล์ secret ของ worker โดยตรง (`key`, `certificate`) `eacpctl dev-client-key` เขียน key, certificate และ JWKS สำหรับ development ส่วน `DEMO=J scripts/demo.sh` รัน `TestPrivateKeyJWTDemo` ด้วย

## Phase 24d: credential จาก Vault

credential ใดๆ ในไฟล์ secret ของ worker เก็บไว้ใน HashiCorp Vault KV v2 แทนได้ (ADR-019 Rev 1.3) ทั้ง token แบบคงที่ (`value_vault`), OAuth client secret (`client_secret_vault`) หรือ key และ certificate ของ `private_key_jwt` (`key_vault`, `certificate_vault`) worker login ด้วย identity ของตัวเองและอ่านค่าเมื่อต้องใช้:

```json
{"vault": {"address": "https://vault.internal:8200", "refresh_seconds": 300,
           "auth": {"kubernetes": {"role": "eacp-worker", "jwt_file": "/run/secrets/eacp-vault-identity/token"}}},
 "secrets": [{"tenant_id": "…", "secret_ref": "erp", "host": "erp.internal:8443",
              "value_vault": {"path": "eacp/erp", "key": "token"}}]}
```

บน Kubernetes `worker.vaultIdentity.enabled` project token ที่มี audience `vault` เข้าไปใน pod ของ worker เท่านั้น ที่อื่นให้ใช้ `"approle": {"role_id_file": …, "secret_id_file": …}` worker เริ่มทำงานได้แม้ Vault ล่ม และไม่เคยติดต่อ Vault ตอนโหลด ค่าถูก cache ไว้ `refresh_seconds` (30–3600) และไม่เคยใช้ค่าที่เก่าเกินนั้น การหมุนค่าใน Vault มีผลภายในหนึ่งรอบ หรือทันทีหลังระบบปลายทางปฏิเสธค่าเก่า โดยไม่ต้องรีสตาร์ต ถ้า Vault ล้ม เฉพาะ binding ที่ต้องใช้จะค้างใน `QUEUED` พร้อม back-off ตามปกติ token และค่าจาก Vault ถูกปิดบังจากทุก log และไม่เคยถูกเก็บ `DEMO=J scripts/demo.sh` รัน `TestVaultDemo` กับ Vault แบบ dev-mode ที่ใช้ AppRole ด้วย ส่วน `scripts/k8s-e2e.sh` รันด้วย Kubernetes auth

## Phase 24e: SPIFFE JWT-SVID

เมื่อ SPIRE รับรอง (attest) worker แล้ว binding ไม่ต้องถือ secret ใดเลย (ADR-019 Rev 1.4) worker ขอ JWT-SVID ของตัวเองจาก SPIRE agent สำหรับ audience ที่ binding ระบุ แล้วยื่นให้ระบบปลายทางที่รู้จัก SPIFFE โดยตรงเป็น Bearer (`value_spiffe`) หรือเป็น client assertion ของการสร้าง token แบบ OAuth (`client_assertion_spiffe`):

```json
{"spiffe": {"endpoint": "unix:///spiffe-workload-api/spire-agent.sock",
            "spiffe_id": "spiffe://example.org/ns/eacp/sa/eacp-worker"},
 "secrets": [{"tenant_id": "…", "secret_ref": "erp", "host": "erp.internal:8443",
              "value_spiffe": {"audience": "erp-api"}}]}
```

บน Kubernetes `worker.spiffe.enabled` mount socket ของ Workload API เข้าไปใน pod ของ worker เท่านั้นผ่าน SPIFFE CSI driver worker เริ่มทำงานได้แม้ agent ล่ม และไม่เคยติดต่อ agent ตอนโหลด ปฏิเสธ SVID ของ identity อื่นที่ไม่ใช่ `spiffe_id` ไม่เคยส่ง SVID ที่อาจหมดอายุระหว่างการเรียก และพักเฉพาะงานของ binding นั้นพร้อม back-off ตามปกติเมื่อติดต่อ agent ไม่ได้หรือ agent ไม่มี entry ให้ ให้ตั้ง TTL ของ JWT-SVID ใน registration entry ของ worker อย่างน้อย 2.5 × (call budget ที่ยาวที่สุด + 30 วินาที) เพราะ agent แจก SVID ที่ cache ไว้จนเกือบครึ่งอายุ การลบ entry ไม่ได้เพิกถอนสิ่งที่ออกไปแล้ว worker อาจใช้ SVID ต่อได้จนครบ TTL จึงควร contain ด้วย kill SVID ถูกปิดบังจากทุก log และไม่เคยถูกเก็บ `scripts/k8s-e2e.sh` ติดตั้ง SPIRE สำหรับ development และรัน `TestSPIFFEDemo`

## Phase 24f: token exchange

binding แลก identity ของ worker เองเป็น access token ได้ (ADR-019 Rev 1.5) ด้วย token exchange ตาม RFC 8693 ของ service-account token ที่ project มาหรือ JWT-SVID ของ worker ที่ STS แล้วเลือกต่อด้วยการ impersonate GCP service account ได้ นี่คือ GCP Workload Identity Federation และ token exchange ของ Keycloak หรือ Okta:

```json
{"tenant_id": "…", "secret_ref": "gcs", "host": "storage.googleapis.com:443",
 "oauth2": {"grant": "token_exchange", "token_url": "https://sts.googleapis.com/v1/token",
            "audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/eacp/providers/k8s",
            "scope": "https://www.googleapis.com/auth/cloud-platform",
            "subject_token": {"file": "/run/secrets/eacp-identity/token"},
            "impersonate": {"url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/eacp@p.iam.gserviceaccount.com:generateAccessToken",
                            "scope": ["https://www.googleapis.com/auth/devstorage.read_only"]}}}
```

subject token ถูกอ่านใหม่ (หรือขอจาก SPIRE agent) ทุกครั้งที่สร้าง token และส่งไปที่ STS เท่านั้น การยืนยันตัวตนของ client ที่ STS เป็นทางเลือก การแลกต้องได้ access token กลับมา (`issued_token_type`) เมื่อใช้ `impersonate` federated token จะถูกส่งไปที่ endpoint ของการ impersonate เท่านั้นและไม่เคยถูก cache token ของ service account คือตัวที่ connector ได้รับ token สุดท้ายต้องเป็นไปตามทุกกฎของ Phase 24a และขั้นใดที่ล้มจะพักเฉพาะ binding นั้นพร้อม back-off ตามปกติ subject token และ federated token ถูกปิดบังจากทุก log และไม่เคยถูกเก็บ `scripts/k8s-e2e.sh` รัน `TestTokenExchangeDemo` กับ STS ของ Fake ERP

## Phase 24g: AWS STS และ SigV4

AWS ไม่ออก Bearer token binding แบบ `aws` (ADR-019 Rev 1.6) จึง assume IAM role ด้วย token ที่ project มาหรือ JWT-SVID ของ worker (`AssumeRoleWithWebIdentity`) แล้วเซ็นทุกการเรียกไปยัง connector ด้วย key ชั่วคราว (SigV4) ตามที่ API Gateway แบบ IAM authorisation, Lambda function URL และ AWS API ต้องการ:

```json
{"tenant_id": "…", "secret_ref": "erp-aws", "host": "abc123.execute-api.eu-west-1.amazonaws.com:443",
 "aws": {"role_arn": "arn:aws:iam::123456789012:role/eacp-worker", "region": "eu-west-1", "service": "execute-api",
         "subject_token": {"file": "/run/secrets/eacp-identity/token"}}}
```

ค่าเริ่มต้นของ STS endpoint คือ endpoint ของ region นั้น key ถูกขอให้มีอายุไม่เกินหนึ่งชั่วโมงและใช้ไม่เกินหนึ่งชั่วโมง HTTP connector เซ็นทั้งการ execute และ lookup ด้วย signer ของ `aws-sdk-go-v2` ที่ pin ไว้หลังตั้ง header อื่นครบแล้ว secret key ไม่เคยออกจาก worker และ session token ไปกับการเรียกที่เซ็นแล้วไปยัง host ที่ผูกไว้เท่านั้น ทั้งสองถูกปิดบังจากทุก log และไม่เคยถูกเก็บ ส่วน access key id ถูก log ไว้เพื่อจับคู่ใน CloudTrail ได้ MCP server ไม่ถูกเซ็น `scripts/k8s-e2e.sh` รัน `TestAWSDemo` กับ AWS STS ของ Fake ERP ส่วนนี้ทำให้ Phase 24 ครบ

## Phase 25a: A2A delegation ภายใต้ governance

agent ส่งงานต่อให้ agent ระยะไกลที่พูด [A2A 1.0](https://a2a-protocol.org) ได้โดยไม่ต้องถือ credential หรือเข้าถึง agent นั้นเอง ([ADR-030](adr/ADR-030-a2a-delegation.md)) ลงทะเบียน agent ระยะไกลเป็น connector ที่ endpoint คือ URL ของ JSON-RPC interface:

```bash
eacpctl connector register --name procurement --protocol a2a --endpoint https://agents.example.com/a2a --secret-ref procurement-agent
```

scanner ของ worker อ่าน Agent Card (`/.well-known/agent-card.json`) ด้วย credential ที่ worker ถือ และค้นพบ tool หนึ่งตัวคือ `delegate` ซึ่งรับรองจาก card ทั้งใบ skill ใหม่หรือการเปลี่ยนแปลงอื่นใดนอกจาก URL ของไอคอนและเอกสารจะกักกัน tool นั้น contract ของ `delegate` ไม่มี idempotency และไม่มี lookup การ delegate แต่ละครั้งจึงถูกส่งครั้งเดียว worker ส่ง `SendMessage` (id ของ action เป็น `messageId`) ติดตาม task ด้วย `GetTask` และยกเลิก task ที่เลิกติดตาม (หมดเวลา ถูก kill หรือ agent ระยะไกลขอ input เพิ่ม) ผลลัพธ์แบบนั้นถือว่าไม่รู้แน่ชัดและส่งให้คนตัดสิน พร้อม id ของ task ระยะไกลในหลักฐานของ action เก็บเพียง id, สถานะ และ digest ไม่เคยเก็บผลลัพธ์ของ agent ระยะไกล `DEMO=D scripts/demo.sh` รัน demo ของ A2A

## Phase 25b: LLM gateway

agent เรียก model ผ่าน EACP ได้ด้วย Anthropic หรือ OpenAI SDK ที่ไม่ต้องแก้ โดยใช้ EACP agent key ของตัวเองเป็น API key ([ADR-031](adr/ADR-031-llm-gateway.md)) ชี้ base URL ของ SDK ไปที่ `llm-gateway` (`:8083`) ซึ่งให้บริการ `POST /v1/messages` และ `POST /v1/chat/completions` ทั้งแบบ stream และไม่ stream ประกาศแต่ละ model และชื่อของ key ผู้ให้บริการ แล้วเพิ่มลงใน allowlist ของ agent version (`"models": ["sonnet"]` คู่กับ `"tools"` โดยคนที่สองเป็นผู้เปิดใช้):

```bash
eacpctl llm-model register --name sonnet --provider anthropic --base-url https://api.anthropic.com \
  --upstream claude-sonnet-4-5 --secret-ref anthropic --max-output-tokens 64000
```

ทุกการเรียกถูกตัดสินใน PostgreSQL ก่อนส่ง model ต้องอยู่ใน allowlist, ต้องไม่มี kill scope ที่ตรงกัน (operator kill `model` ได้แล้ว) และการจองงบประมาณแบบแข็งต้องพอตามราคาในตารางราคา gateway เป็นผู้ถือ key ของผู้ให้บริการแต่เพียงผู้เดียว (`EACP_LLM_SECRETS_FILE` ในรูปแบบ manifest เดียวกับของ worker) ตัดการเรียกที่ถูก kill ภายในไม่กี่วินาที และปิดยอดตาม usage ที่ผู้ให้บริการรายงานด้วยราคาของ PostgreSQL `eacpctl llm-calls list` แสดง ledger คือ token, ค่าใช้จ่าย และผลลัพธ์ ไม่เคยแสดงเนื้อหา `DEMO=L scripts/demo.sh` รัน demo ของ LLM gateway

worker ลงทะเบียน HTTP connector ของ Phase 6 และรัน reconciler ของ Phase 7 Fake ERP ต้องใช้ credential สำหรับการเรียกที่มีอภิสิทธิ์ และเก็บ operation log ไว้ใน volume ของ Compose ที่คงทน

## Phase 26b: ช่องทางรับผลลัพธ์

contract เลือกเก็บผลลัพธ์ของการเรียกที่สำเร็จไว้ให้ agent ที่เรียกอ่านได้ นาน 60 วินาทีถึงหนึ่งวัน (`result_retention_seconds`; [ADR-034](adr/ADR-034-result-channel.md)) การเปิดใช้คือการออก contract เวอร์ชันใหม่ จึงต้องให้คนที่สองเป็นผู้เปิดใช้ HTTP ส่ง `result` ในคำตอบของ execute มา MCP ส่ง `CallToolResult` ทั้งก้อน (digest ของมันคือ reference) และ A2A ส่ง artifact ของ task

| ความสามารถ | หลักฐาน |
|---|---|
| เก็บเฉพาะความสำเร็จ โดยผู้ถือ lease บันทึกใน transaction เดียวกับที่ย้าย action ไปเป็น `SUCCEEDED` PostgreSQL คำนวณ digest ขนาด และเวลาหมดอายุ | `migrations/00026_result_channel.sql`, `TestOnlyTheLeaseHolderRecordsASuccess`, `TestASuccessKeepsItsOutputForTheAgent`, `TestAKilledSuccessKeepsNoOutput` |
| มีเพียงเวอร์ชัน `ACTIVE` ของ agent ที่เรียกเท่านั้นที่อ่านเนื้อหาได้ (`GET /v1/actions/{id}/result`) operator เห็นแค่ขนาดและ digest ในหลักฐาน | `TestOnlyTheActionsAgentReadsTheResult`, `TestResultContentIsNotSelectable`, `TestTheCallingAgentReadsItsResult` |
| มีขอบเขต: JSON แบบ RFC 8785 ไม่เกิน 64 KiB ผลลัพธ์ที่มี credential ที่ worker ถืออยู่หรือใหญ่เกินจะไม่ถูกเก็บ แต่ action ยังสำเร็จ | `TestPrepareOutput`, `TestACredentialInTheOutputIsWithheld`, `TestAnOversizedOutputIsWithheldAndTheActionSucceeds`, `TestAWithheldResultSaysWhy` |
| มีเวลาจำกัด: หมดอายุแล้วอ่านไม่ได้ และ sweeper ลบเนื้อหาทิ้งครั้งเดียว | `TestTheSweeperClearsExpiredResultsOnce`, `TestPruneNeedsTheSweeper` |
| ไม่อยู่ใน log, journal หรือข้อความใดๆ | `TestASuccessKeepsItsOutputForTheAgent` |

## Phase 27a-1: กฎของ Agent Studio

พนักงานที่มี role `studio_author` บันทึกนิยามของ agent ได้ ได้แก่ input, ขั้น `tool_call` และขั้น `respond` ปิดท้าย ([ADR-033](adr/ADR-033-agent-studio-and-runtime-credentials.md)) PostgreSQL ตรวจนิยาม คำนวณ digest และ tool ที่ต้องใช้ แล้วสร้าง agent ใน registry ที่พนักงานเป็นเจ้าของ เวอร์ชันสถานะ `REGISTERED` และ allowlist ที่มีเฉพาะ tool เหล่านั้น `registry_approver` ที่ไม่ใช่ผู้เขียนเป็นผู้อนุมัติหรือปฏิเสธคำขอ และการอนุมัติจะแทนที่เวอร์ชันที่ใช้งานอยู่ของ agent runtime ที่จะรัน agent ของ Studio (Phase 27a-2) ถือเพียง `studio_runtime`

| ความสามารถ | หลักฐาน |
|---|---|
| PostgreSQL ตรวจนิยาม: schema version 1, input แบบข้อความไม่เกิน 10 ตัว, 1 ถึง 20 ขั้น, tool ที่มีอยู่จริง, placeholder อ้างได้เฉพาะ input ที่ประกาศและขั้นก่อนหน้า, ไม่มีค่าที่ดูเหมือนความลับ และไม่เกิน 64 KiB | `migrations/00027_studio.sql`, `TestTheDefinitionRules` |
| allowlist คือ capability ที่คำนวณได้พอดี ไม่มีใครขยายหรือเปิดใช้นอกการตัดสิน | `TestTheCapabilityIsTheSortedDistinctTools`, `TestTheRegistryCannotWidenAStudioAgent` |
| ผู้เขียนเขียนแถวใน registry ได้เฉพาะในการบันทึกของ Studio เฉพาะ agent ของตนเอง และเฉพาะในแผนกของตน | `TestAStudioAuthorWritesNoRegistryRowDirectly`, `TestOnlyAStudioAuthorInTheDepartmentSaves`, `TestOnlyTheOwnerAddsAVersion` |
| คนที่สองเป็นผู้ตัดสินเพียงครั้งเดียว การอนุมัติปลดเวอร์ชันก่อนหน้า และตัดสินได้เฉพาะเวอร์ชันที่รออยู่ | `TestTheOwnerAndNonApproversCannotDecide`, `TestApprovalActivatesTheVersionAndReplacesThePreviousOne`, `TestRejectionRetiresTheVersion`, `TestOnlyAWaitingVersionIsApproved` |
| `studio_runtime` ถือได้โดย service principal เพียงลำพัง และเสนอ key ของ agent ได้เฉพาะเวอร์ชันของ Studio ที่อนุมัติแล้ว | `TestStudioRuntimeIsHeldAlone`, `TestStudioRolesKeepTheirPrincipalKind`, `TestTheRuntimeProposesCredentialsOnlyForApprovedStudioVersions` |
| API (`/v1/studio/...`) แสดงขั้นของแต่ละเวอร์ชันและผู้ที่ต้องดำเนินการต่อ คิวของผู้อนุมัติอธิบาย tool เป็นภาษาที่เข้าใจง่าย | `TestStudioAuthorsSaveAndApproversDecide` |

## Phase 27a-2: `agent-runtime` การรัน และ key ที่ derive

สมาชิกในแผนกของ agent สั่งรัน agent ของ Studio ที่อนุมัติแล้วพร้อม input ได้ (`POST /v1/studio/agents/{id}/runs`) service ใหม่ `agent-runtime` รับงานรันผ่าน API ทำงานในนามเวอร์ชันของ agent ด้วย key ที่ derive จาก master secret ซึ่งมีเพียง runtime ที่ถือ และส่งทุกขั้น `tool_call` ผ่านเส้นทาง action ตามปกติ โดยมีพนักงานเป็น subject ([ADR-033](adr/ADR-033-agent-studio-and-runtime-credentials.md) Rev 1.2) policy, การอนุมัติ, budget และ kill scope มีผลเหมือนกับ agent อื่นทุกตัว พนักงานอ่านคำตอบได้หนึ่งชั่วโมง runtime เสนอ key ของแต่ละเวอร์ชันและ key ถัดไปก่อนหมดอายุ 30 วัน โดย `registry_approver` เป็นผู้อนุมัติ และ operator เพิกถอน key ของ Studio ทั้งหมดได้ในครั้งเดียว

| ความสามารถ | หลักฐาน |
|---|---|
| หากไม่มีรายการใน Hub มีเพียงเจ้าของที่เริ่มการรันได้ (Phase 27b) และ input ต้องตรงกับที่ประกาศไว้พอดี | `TestARunIsStartedByItsOwnerWithItsInputs`, `TestStudioRunsThroughTheAPI` |
| runtime ถือการรันได้ทีละหนึ่งตัว และมีเพียงผู้ถือ lease ที่เปลี่ยนสถานะการรันได้ | `TestOnlyTheLeaseHolderMovesARun`, `TestTwoRuntimesNeverShareARun`, `TestRuntimeRoutesAreForTheRuntimeOnly` |
| แต่ละขั้นเป็น action ของการรันนั้นเอง ภายใต้ `studio:<run>:<index>` หลัง crash จะทำ action เดิมต่อ ไม่ส่งซ้ำ | `TestAStepIsTheRunsOwnAction`, `TestACrashBeforeTheStepRecordResubmitsTheSameAction`, `TestARunEndToEnd` |
| การรันล้มเหลวแบบปิดพร้อมเหตุผลที่ระบุชื่อ: key ขาด หมดอายุ หรือถูกเพิกถอน ขั้นถูกปฏิเสธ เวอร์ชันถูกแทนที่ หรือเลยกำหนดเวลา | `TestARunFailsClosedWithoutAKey`, `TestARevokedKeyFailsTheRun`, `TestADeniedStepFailsTheRun`, `TestAReplacedVersionStopsTheRun`, `TestAStepAwaitingApprovalFailsAtTheDeadline`, `TestTheSweeperExpiresStudioRuns` |
| placeholder รับ input ที่ประกาศไว้และผลลัพธ์ก่อนหน้าจากช่องทางรับผลลัพธ์ ค่าที่ขาดจะไม่ถูกส่งออกไป | `TestRenderSubstitutesInputsAndOutputs` |
| มีเพียงผู้สั่งรันที่อ่านคำตอบได้ภายในหนึ่งชั่วโมง และไม่บันทึก input หรือคำตอบลง journal | `TestOnlyTheRequesterReadsTheAnswerBeforeItExpires`, `TestRunsAreJournaledWithoutInputsOrAnswer` |
| key derive จาก master ที่มีเพียง runtime ถือ และไม่มี key หรือ master ปรากฏใน response, log, แถว, journal หรือข้อความ | `TestKeyFromSecretAuthenticatesUnchanged`, `TestOnlyTheRuntimeHoldsTheStudioMaster`, `TestTheRuntimeRefusesAWeakMaster`, `TestNoKeyOrMasterLeaks` |
| runtime เสนอ key และ key ถัดไปแต่ไม่เคยอนุมัติเอง operator เพิกถอน key ของ Studio ได้ทั้งหมด และ key ใกล้หมดอายุจะเปิด incident | `TestRotationProposesASuccessorAndNeverApproves`, `TestKeysAreDueBeforeTheyExpire`, `TestTheBulkRevocationRevokesOnlyStudioKeys`, `TestOperatorsRevokeEveryStudioKey`, `TestAnExpiringStudioKeyOpensAnIncident` |

## Phase 27a-3a: การแพ็กเกจ `agent-runtime` และ demo ของ Studio

`agent-runtime` มาพร้อม image และไบนารีของ release ใน compose มันรันใต้ profile `studio` บนเครือข่าย `agents` เท่านั้น ใน Helm `studio.enabled` เพิ่มมันพร้อม ServiceAccount และ NetworkPolicy ของตัวเอง และ Secret สองตัวที่อ้างอิงด้วยชื่อ ([docs/KUBERNETES.md](KUBERNETES.md)) `DEMO=S scripts/demo.sh` พา agent ยอดวันลาของ HR จาก template ผ่านการอนุมัติไปจนถึงการรัน ([docs/DEMO.th.md](DEMO.th.md#agent-studio-demo))

| ความสามารถ | หลักฐาน |
|---|---|
| runtime เข้าถึงได้เพียง API และไม่มี URL ของฐานข้อมูล secret ของ connector หรือ provider มีเพียงมันที่ mount master ของ Studio | `TestTheRuntimeReachesOnlyTheAPI`, `TestOnlyTheRuntimeHoldsTheStudioMaster` (compose), `TestTheRuntimePodIsHardened`, `TestTheRuntimeReachesOnlyTheAPIInTheCluster` (Helm) |
| chart ปฏิเสธ runtime ที่ไม่มี Secret ของมัน หรือมี secret, URL ของฐานข้อมูล หรือตัวแปรที่ chart ตั้งเองใน `studio.env` | `TestTheRuntimeNeedsItsSecrets`, `TestRuntimeEnvIsValidated`, `TestTheRuntimeIsOffByDefault` |
| Fake MCP ตอบเฉพาะ tool ที่อยู่ในรายการ และ `get_leave_balance` คืน structured content | `TestAToolOutsideTheListIsRefused`, `TestLeaveBalanceAnswersStructuredContent`, `TestLeaveBalanceErrIsAToolError` |
| จาก template ถึงคำตอบ โดยมีผู้อนุมัติคนที่สอง รันได้เฉพาะในแผนก key ล้มเหลวแบบปิด และไม่มี secret หรือคำตอบรั่วไหล | `TestStudioDemo` |

## Phase 27a-3b: หน้า Agent Studio

`/studio/` เป็นหน้าที่สองที่สร้างจากโมดูลและกฎเดียวกับ console ([ADR-028](adr/ADR-028-operator-console.md) Rev 1.2) พนักงานเริ่มจาก template ยอดวันลา บันทึก agent ลงในกลุ่มของตัวเอง เห็นเป็นภาษาธรรมดาว่าตอนนี้อยู่ขั้นไหนและใครต้องทำอะไรต่อ แล้วรันมันได้ ผู้อนุมัติทะเบียนตัดสินคำขอและ key ของ runtime ในหน้าเดียวกัน มีทั้งภาษาอังกฤษและไทย ([คู่มือการใช้งาน](USER_GUIDE.th.md#สำหรับพนักงาน-agent-studio))

| ความสามารถ | หลักฐาน |
|---|---|
| หน้านี้ใช้ไฟล์และกฎเดียวกับ console แต่ละหน้าเสิร์ฟเฉพาะ HTML ของตัวเองและเข้าสู่ระบบแยกกัน | `TestServesTheStudioWithItsHeaders`, `TestEachPageServesOnlyItsOwnHTML`, `TestIndexLoadsOnlyTheConsole`, `TestConsoleUsesNoDangerousSinks`, `TestEveryConsoleCallIsARealRoute` |
| template ตรงกับ fixture ของ demo และฟอร์มสร้างนิยามตรงตามที่เซิร์ฟเวอร์รับ | `the leave-balance template is the demo fixture, value for value`, `the form round-trips the template to the same definition`, `the template form saves the fixture definition into the author’s department` (`jstest`) |
| ทุกสถานะและเหตุผลที่ล้มเหลวอ่านเป็นภาษาธรรมดา ทั้งอังกฤษและไทย ค่าที่ไม่รู้จักจะแสดงตามที่ส่งมา | `every status has a sentence that says who acts next; an unknown one is shown as sent`, `every failure reason the runtime names has its own sentence; an unknown one is shown as sent`, `TestEveryTranslatedTextHasAThaiEntry` |
| ผู้อนุมัติไม่ได้ตัดสิน agent ของตัวเอง การอนุมัติคำขอหรือ key ต้องผ่านการยืนยันพร้อมเหตุผล | `an approver’s own agent is not offered for their decision`, `an approver approves someone else’s agent with a reason, and a runtime key` (`jstest`) |
| `/v1/me` แสดงเฉพาะกลุ่มที่ผู้เรียกเป็นสมาชิกอยู่ | `TestMeListsTheCallersGroups` |

## Phase 27b: Agent Hub

เจ้าของ agent เสนอมันเข้า Hub สำหรับแผนกของตนหรือทั้งองค์กร หัวหน้าแผนก หรือผู้ดูแลระบบหรือผู้อนุมัติทะเบียนสำหรับทั้งองค์กร เป็นผู้เผยแพร่ ([ADR-033](adr/ADR-033-agent-studio-and-runtime-credentials.md) Rev 1.3) ทุกคนที่มันเข้าถึงจะค้นเจอ รันมันในนามของตัวเอง และคัดลอกได้ โดยสำเนาไม่มีสิทธิ์ใดติดไปด้วย PostgreSQL ตัดสินทุกขั้น ([คู่มือการใช้งาน](USER_GUIDE.th.md#แบ่งปันใน-hub)) การทดลองกับแผนกต่างๆ (gate) ไม่ได้เกิดขึ้น และบันทึกไว้ว่ายังไม่ผ่าน

| ความสามารถ | หลักฐาน |
|---|---|
| หัวหน้าแผนกถูกกำหนดได้เฉพาะตอนที่ผู้ดูแลระบบเพิ่มสมาชิก และเฉพาะมนุษย์ และไม่เปลี่ยนภายหลัง | `TestALeadIsSetOnlyWhenAnAdminAddsTheMembership`, `TestALeadIsAddedThroughTheAPIAndShownInMe` |
| มีเพียงเจ้าของที่เสนอได้ เฉพาะเวอร์ชันที่ `ACTIVE` และได้รับอนุมัติ และเสนอได้ครั้งละหนึ่งคำเสนอ | `TestOnlyTheOwnerProposesAnActiveApprovedVersion` |
| การอนุมัติเป็นชั้น: หัวหน้าแผนกของ agent สำหรับแผนก ผู้ดูแลระบบหรือผู้อนุมัติทะเบียนสำหรับทั้งองค์กร ผู้ดูแลระบบสำหรับ template และไม่มีใครตัดสินของตัวเอง | `TestEachScopeHasItsApprover`, `TestTheTemplateTagNeedsAnAdmin` |
| รายการเผยแพร่ได้เฉพาะเวอร์ชันที่ยัง `ACTIVE` การเลิกแนะนำและการถอนทำได้เพียงลดการเข้าถึง | `TestApprovalNeedsTheVersionStillActive`, `TestDeprecateAndWithdrawOnlyNarrow` |
| PostgreSQL แสดงแต่ละรายการเฉพาะกลุ่มเป้าหมายของมัน และให้รันได้เฉพาะผ่านรายการที่มองเห็น | `TestTheHubShowsEachListingToItsAudience`, `TestOthersRunOnlyThroughAVisibleListing` |
| สำเนาเป็น agent ใหม่ที่ยังไม่ได้รับอนุมัติ ไม่มี allowlist, key หรือรายการ | `TestACloneCarriesNoPermission` |
| รายการและคำเสนอเขียนได้เฉพาะผ่านฟังก์ชันของมัน และถูกบันทึกใน journal | `TestTheHubIsWrittenOnlyThroughItsFunctions` |
| API และหน้าเว็บ: ค้นหา รัน คัดลอก เสนอ และตัดสิน | `TestTheHubThroughTheAPI`, `the owner proposes a ready agent to the Hub and sees who decides it`, `a lead decides a Hub proposal, and never loads the approvers’ queue` (`jstest`) |
| หัวหน้า HR เผยแพร่ให้ HR เพื่อนร่วมงานรันมัน และสำเนาของผู้เขียนฝ่ายการเงินเริ่มต้นแบบยังไม่ได้รับอนุมัติ | `TestStudioDemo` |

## Benchmark

`scripts/bench.sh` วัดทั้ง stack ภายใต้โหลดแบบ open loop บน stack ของ compose ที่แยกออกมา (MASTER_PLAN §104) ครอบคลุมเส้นทางของ action ทั้งกับ PDP
ภายใน process และผ่าน AGT sidecar ตั้งแต่ 100 ถึง 10,000 agent ที่ลงทะเบียนไว้ และ LLM gateway กับผู้ให้บริการปลอม บันทึก throughput, percentile ของ
latency แต่ละขั้น, error, รายการซ้ำ และการใช้ทรัพยากร ผลการรันล่าสุดอยู่ใน [BENCHMARKS.md](BENCHMARKS.md) ซึ่งทุกตารางสร้างจากผลดิบที่ commit ไว้
ตัวเลขมาจากเครื่อง development เครื่องเดียว ไม่ใช่การอ้างความจุสำหรับ production
