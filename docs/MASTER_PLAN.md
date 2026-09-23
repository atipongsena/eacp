# Enterprise Agent Control Plane

## Master Development Plan

- **Working name:** EACP
- **Full name:** Enterprise Agent Control Plane
- **Primary execution language:** Go
- **Governance foundation:** Microsoft Agent Governance Toolkit / Agent Control Specification (via sidecar PDP — see ADR-002)
- **Architecture style:** Control Plane + Governance Plane + Execution Plane
- **Target:** Enterprise AI Agent Infrastructure
- **Status:** Master Architecture Plan — **Revision 2** (post-review; ADR-001/002/004/005 are the normative detail)
- **Revision date:** 2026-09-23

---

# 0. Revision 2 — What Changed

Rev 2 นำผลจาก `docs/reviews/2026-09-23-master-plan-review.md` (Claude + Codex cross-review) มาใช้

ส่วนที่แก้จะมีป้าย **Rev 2** เลข section เดิมไม่เปลี่ยน เพื่อให้ review และ ADR อ้างอิงได้ตรงกัน

การตัดสินใจหลัก:

| # | Decision | ADR | Sections |
|---|---|---|---|
| 1 | Slice แรกของ EACP เป็น **Agent Tool / Action Control Plane** ยังไม่ทำ LLM gateway แต่ออกแบบ interface เผื่อไว้ | ADR-001 | §3.1, §45, §65 |
| 2 | **Enforcement point อยู่ใน Slice A**: agent ไม่มี credential ของ privileged system และ side effect เกิดได้เฉพาะผ่าน EACP หรือ execution proxy ที่ EACP ควบคุม | ADR-001 | §1, §3.2, §70 |
| 3 | AGT/ACS เชื่อมผ่าน **sidecar PDP** หลัง `GovernanceProvider` interface ส่วน Go เป็น core | ADR-002 | §4, §5, §12, §83 |
| 4 | **AGT decides, EACP stores**: approval state อยู่ใน Postgres ของ EACP และ bind กับ `enforced_digest` + `policy_version` | ADR-005 | §13, §14, §15 |
| 5 | Action state machine ฉบับเต็ม มี `PENDING_APPROVAL`, `NEEDS_HUMAN_RESOLUTION` และ crash recovery ครบทุก state | ADR-004 | §18 |
| 6 | Fencing ครอบคลุม **execution semantics** ไม่ใช่แค่ DB write: dispatch intent และ reclaim → reconcile | ADR-004, ADR-005 | §19, §22, §23 |
| 7 | Operation identity และ reconciliation proof standard ต่อ connector ถ้าได้แค่ "not found" ห้าม auto-retry destructive action | ADR-004 | §20, §31 |
| 8 | Postgres เป็น execution authority เพียงแหล่งเดียว NATS เข้ามาใน Slice B เพื่อส่ง hint/event เท่านั้น | ADR-014 | §60 |
| 9 | Tenant isolation ใช้ Postgres RLS ตั้งแต่ schema แรก | ADR-021 | §69 |
| 10 | MVP แบ่งเป็น **Slice A / B / C** และ demo แรกใช้เฉพาะ Slice A | — | §73–§97, §110, §111 |

**Slice A goal statement:**

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

ถ้า assumption ใดยังไม่ resolve ให้เลือกทางที่ **conservative ที่สุดด้าน correctness / safety** ก่อน

คำว่า "AEF" ในเอกสารเดิมหมายถึง **Execution Fabric** (Execution Plane)

---

# 1. Vision

Enterprise Agent Control Plane คือระบบกลางสำหรับองค์กรที่มี AI Agent จำนวนมาก

เป้าหมายไม่ใช่การสร้าง Agent Framework ใหม่

แต่คือการทำให้บริษัทสามารถ:

```text
Discover
Govern
Authorize
Schedule
Execute
Observe
Audit
Control
Contain
Evaluate
Deploy
```

AI Agents จากหลายทีม หลาย framework และหลาย environment ผ่านระบบกลางเดียว

แนวคิดหลัก:

> Teams may build agents however they want, but enterprise resources are accessed through the Control Plane.

หรือ:

> คุณสร้าง Agent ด้วยอะไรก็ได้ แต่ถ้าจะใช้ทรัพยากรขององค์กร ต้องผ่าน Control Plane

**Rev 2 — หลักการนี้ต้องถูก enforce จริง ไม่ใช่แค่ convention:**

> Agent ต้องไม่มี credential สำหรับเรียก privileged external system โดยตรง
> Privileged side effect ทุกอย่างต้อง execute ผ่าน EACP หรือ execution proxy ที่ EACP ควบคุม

รายละเอียดอยู่ใน §3.2 และ §70

---

# 2. Problem

องค์กรหนึ่งอาจมี:

```text
Finance          120 Agents
Procurement       80 Agents
Engineering      450 Agents
Sales            170 Agents
HR                60 Agents
Security          90 Agents
Customer Service 300 Agents

Total          1,270 Agents
```

Agent เหล่านี้อาจสร้างด้วย:

```text
OpenAI Agents
Claude
LangGraph
Microsoft Agent Framework
AutoGen
CrewAI
Custom Python
Custom Go
Internal platforms
```

และเชื่อมต่อ:

```text
SAP
Salesforce
Databases
GitHub
Email
Slack / Teams
Cloud infrastructure
MCP Servers
A2A Agents
LLM Providers
Internal APIs
```

ถ้าแต่ละทีมจัดการทุกอย่างเอง จะเกิดการสร้างซ้ำ:

```text
Authentication
Authorization
Policy
Approval
Credentials
MCP integration
Retry
Rate limits
Budget control
Audit
Tracing
Kill switch
Deployment
Evaluation
```

และองค์กรจะไม่มีคำตอบง่าย ๆ สำหรับคำถามเช่น:

```text
เรามี production agents กี่ตัว?

ใครเป็นเจ้าของ Agent นี้?

Agent ไหนเข้าถึงข้อมูลลูกค้า?

Agent ไหนใช้ MCP server นี้?

Agent ไหนสามารถสร้าง Purchase Order?

ถ้า MCP server ถูก compromise จะกระทบ Agent ไหนบ้าง?

วันนี้ Agent ทั้งองค์กรใช้เงิน LLM ไปเท่าไร?

Agent ไหนกำลังสร้าง error ซ้ำ ๆ?

จะหยุด Agent version หนึ่งทั้งองค์กรได้อย่างไร?

Agent action นี้ได้รับอนุมัติจากใคร?

Action นี้เกิดขึ้นจริงหรือไม่?

ถ้า API timeout เรารู้ได้อย่างไรว่าการทำงานสำเร็จหรือไม่?
```

Enterprise Agent Control Plane ถูกสร้างมาเพื่อตอบปัญหาเหล่านี้

---

# 3. Core Principle

ระบบแบ่ง responsibility ออกเป็นสาม plane

```text
CONTROL PLANE

What exists?
Who owns it?
Where is it?
How is it configured?
What depends on what?
How is it deployed?
What is its health?

        │
        ▼

GOVERNANCE PLANE

Who is this?
Should this action be allowed?
What policy applies?
Does it require approval?
What restrictions apply?

        │
        ▼

EXECUTION PLANE

How do we execute it correctly?
Which worker runs it?
How do we avoid duplicate effects?
What happens on failure?
How do we recover?
```

## 3.1 Product Boundary — Rev 2

ช่วงแรก EACP คือ **Agent Tool / Action Control Plane**

EACP อยู่ใน path ของ **tool / action calls** ที่แตะทรัพยากรองค์กร เช่น SAP, DB, GitHub, Email หรือ MCP tools

EACP **ไม่เป็น LLM gateway** ใน MVP

```text
Agent ──LLM call──► LLM Provider             (ไม่ผ่าน EACP)
  │                      │
  │                      └─ usage/cost ─► EACP FinOps (ingest: OTel GenAI spans / provider billing)
  │
  └──tool/action──► EACP ──► Governance ──► Execution Fabric ──► Enterprise System
```

เหตุผล:

```text
LLM gateway เป็น product แยกที่มีคู่แข่งเยอะ
latency-sensitive
ไม่ใช่ differentiator ของ EACP (§114)
```

LLM gateway จะเป็น optional module ภายหลัง (§97, ADR-001)

**Architecture ต้องเผื่อ LLM gateway ไว้ตั้งแต่ตอนนี้:**

```text
Ingress (per call type)            Shared core (reused by every ingress)
────────────────────────           ─────────────────────────────────────
Tool / Action API   (Slice A)  ─┐  Principal authentication
LLM Gateway         (later)    ─┼─► Capability check
A2A ingress         (later)    ─┘  GovernanceProvider
                                   Approval store
                                   Audit / evidence journal
                                   Budget (Slice B)
                                   Telemetry
```

Shared core ต้องไม่ผูกกับรูปแบบ "tool call" เช่น governance request ใช้ `operation` + `target` + `payload` ทั่วไป

ingress ใหม่จึงต่อเข้ามาได้โดยไม่ต้องแก้ core

## 3.2 Enforcement Point — Rev 2 (Slice A)

หลัก "resources are accessed through the Control Plane" จะจริงได้ก็ต่อเมื่อ agent **ไม่มี credential** ของ privileged system และ **ไม่มี network path** ไปหา system นั้นโดยตรง

```text
Connector credentials  → อยู่ใน Execution Worker / execution proxy ที่ EACP ควบคุมเท่านั้น
                         ไม่เคยถูกส่งกลับผ่าน API ไม่อยู่ใน action payload และไม่อยู่ใน log
Agent                  → มีแค่ agent identity (credential สำหรับเรียก EACP API)
Capability             → agent เรียกได้เฉพาะ tool ที่อยู่ใน allowlist ของ AgentVersion ที่ ACTIVE
                         ตรวจแบบ deterministic ก่อนถึง governance และ fail closed
Network                → agent runtime ไม่มี egress ไป privileged system
                         demo: docker network segmentation
                         production: network policy / egress firewall
```

ใน Slice A ใช้ static secret ที่ worker ถือเท่านั้น

JIT / short-lived credentials อยู่ Phase 22 (§96)

**ขอบเขตของ claim (Rev 2.1):** claim "cannot bypass" ใช้ได้กับ **conforming deployment** (ADR-001 §3a)

คือ target system ออก privileged credential ให้เฉพาะ EACP worker/proxy และ agent ไม่มี network route ไปถึง target

**หลักฐานที่ Slice A ต้องแสดงได้:**

```text
agent เรียก Fake ERP ตรง ๆ          → ล้มเหลว (ไม่มี credential และไม่มี network path)
agent เรียก tool นอก allowlist       → DENIED ก่อนถึง governance
ค้น secret ใน API response / log / DB → ไม่พบ
```

---

# 4. Relationship With Microsoft AGT

Microsoft Agent Governance Toolkit เป็น Governance Foundation

ใช้ AGT/ACS สำหรับสิ่งเช่น:

```text
Policy
Identity
Trust
Delegation
Approval
Runtime governance
Context governance
Protocol governance
Audit evidence
SRE primitives
```

Enterprise Agent Control Plane ไม่ควรสร้างของเหล่านี้ซ้ำโดยไม่มีเหตุผล

**Rev 2 — แบ่งความรับผิดชอบให้ชัด:**

```text
AGT / ACS (sidecar PDP)          EACP (Go core)
──────────────────────           ──────────────────────────────────────
Policy evaluation / verdict      Approval STATE: request, vote, grant, consume
  (allow/warn/deny/escalate/     Approver eligibility / SoD enforcement
   transform)                    Decision evidence persistence
Action identity semantics        Execution, idempotency, lease/fencing
Audit primitives                 Durable audit journal
```

ACS เป็น stateless จึง **ไม่ใช่ state store** ของ approval (§5.1)

Architecture:

```text
                     Enterprise Agent Control Plane

┌──────────────────────────────────────────────────────────────┐
│                       CONTROL PLANE                          │
│                                                              │
│ Agent Registry      Dependency Graph      Fleet Management   │
│ Release Manager     Evaluation            Agent SOC          │
│ FinOps              Operator UI           API / CLI          │
└──────────────────────────────┬───────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────┐
│                     GOVERNANCE PLANE                         │
│                                                              │
│  GovernanceProvider (Go interface)                           │
│    ├── local provider        (Slice A)                       │
│    └── Microsoft AGT / ACS   (sidecar PDP, Slice B)          │
│                                                              │
│ Policy verdicts    Identity    Trust    Delegation           │
│ EACP-owned: Approval Store · Decision Evidence · SoD         │
└──────────────────────────────┬───────────────────────────────┘
                               │
                       Governance Decision
                               │
                               ▼
╔══════════════════════════════════════════════════════════════╗
║                      EXECUTION PLANE                         ║
║                                                              ║
║               Go Distributed Execution Fabric               ║
║                                                              ║
║ Admission           Fair Scheduler       Durable Queue       ║
║ Budget Reservation  Lease / Fencing      Idempotency         ║
║ Retry Safety        Backpressure          Reconciliation      ║
║ Connector Runtime   Worker Fleet         Execution Journal   ║
╚══════════════════════════════╤═══════════════════════════════╝
                               │
                               ▼
                 MCP / A2A / HTTP / gRPC
                               │
              ┌────────────────┼────────────────┐
              ▼                ▼                ▼
             SAP              DB              GitHub
```

---

# 5. Extension-First Strategy

ไม่ fork Microsoft AGT ทั้ง repo เป็นค่าเริ่มต้น

ใช้:

```text
Microsoft AGT
      │
      │ public contracts
      ▼
Compatibility / Integration Layer
      │
      ▼
Enterprise Agent Control Plane
```

เหตุผล:

```text
AGT ยัง evolve เร็ว
API ยังเป็น Public Preview
ACS ยังพัฒนา
package layout เปลี่ยนได้
```

ดังนั้น AGT-specific code ต้องถูก isolate

ตัวอย่าง:

```text
integrations/governance/microsoftagt/   (Go client ของ sidecar)
sidecars/agt-pdp/                        (AGT Python/Rust sidecar)
```

Core domain ห้าม depend กับ Microsoft-specific types โดยตรง

## 5.1 Upstream Reality (verified 2026-09-23) — Rev 2

```text
AGT                     Public Preview v4.1.0, MIT, may break before GA
ACS                     in-process, stateless library (Rust core; SDK: Python/Node/.NET/Rust)
ACS Go binding          ไม่มี
AGT Go SDK              core only (policy/identity/trust/audit)
                        ไม่มี approval chains (issue #3083)
ACS verdicts            allow / warn / deny / escalate / transform
Action binding          JCS (RFC 8785) + SHA-256; approval binds to enforced identity
```

ผลที่ตามมา:

```text
Go เรียก ACS ผ่าน sidecar PDP (ADR-002)
EACP เป็นเจ้าของ approval state (ADR-005)
Pin AGT version + conformance tests ใน CI
```

---

# 6. Product Modules

ระบบใหญ่แบ่งเป็น 10 modules

```text
1. Agent Registry
2. Governance Integration
3. Distributed Execution Fabric
4. Tool / Connector Registry
5. Dependency & Blast Radius Graph
6. Agent Fleet Operations
7. Agent SRE / Observability
8. Agent FinOps
9. Release / Evaluation Platform
10. Agent Security Operations Center
```

---

# 7. Module 1 — Agent Registry

Agent Registry เป็น CMDB สำหรับ AI Agents

ทุก production agent ต้องมี Passport

ตัวอย่าง:

```yaml
agent_id: procurement-agent
version: 14

owner:
  team: procurement-ai
  email: procurement-ai@example.com

environment: production

runtime:
  framework: langgraph
  language: python

models:
  - claude
  - gpt

tools:                     # Rev 2: นี่คือ capability allowlist ที่ถูก enforce
  - supplier_search
  - sap.create_po
  - email.send

data_classes:
  - internal
  - confidential

risk_class: high

governance:
  provider: microsoft-agt
  policy_set: procurement-production

lifecycle:
  status: active
  created_at: ...
  expires_at: ...
```

---

# 8. Agent Lifecycle

Agent state:

```text
DRAFT
  ↓
REGISTERED
  ↓
CERTIFIED
  ↓
STAGING
  ↓
CANARY
  ↓
ACTIVE
  ↓
DEPRECATED
  ↓
RETIRED
```

Alternative:

```text
SUSPENDED      (Rev 2: หยุดชั่วคราวและ resume ได้)
QUARANTINED
REVOKED
```

ทุก transition ต้องมีเหตุผลและ audit event

**Rev 2:** เฉพาะ AgentVersion ที่ `ACTIVE` (และ `CANARY` ตาม rollout) เท่านั้นที่ submit action ได้

สถานะนี้ถูกตรวจซ้ำใน release boundary (§15) และตอน dispatch-intent commit (§23.1)

ถ้าถูก SUSPENDED หรือ QUARANTINED ระหว่างทาง action ที่ยังไม่ dispatch จะไม่ถูก execute

---

# 9. Ownership Requirement

Production Agent ที่ไม่มี owner ถือเป็น risk

Policy:

```text
if environment == production
and owner == null

→ deployment denied
```

Owner สามารถเป็น:

```text
Human
Team
Service owner
Business unit
```

---

# 10. Agent Versioning

แยก:

```text
Agent
```

กับ:

```text
AgentVersion
```

ตัวอย่าง:

```text
procurement-agent

v12
v13
v14
```

แต่ละ version อาจต่างกันใน:

```text
Prompt
Model
Tools
Policies
MCP dependency
Code hash
Evaluation score
Risk class
```

---

# 11. Agent Bill of Materials

สร้าง:

> Agent BOM / Agent SBOM

ตัวอย่าง:

```text
Procurement Agent v14

├── Code
│   └── git sha 92ca...
│
├── Model
│   └── claude-...
│
├── MCP
│   ├── sap-mcp v3
│   └── supplier-mcp v7
│
├── Skills
│   ├── supplier-analysis
│   └── purchase-order
│
├── Policies
│   └── procurement-prod@17
│
└── Data
    ├── supplier-db
    └── finance-api
```

เป้าหมาย:

```text
Reproducibility
Security
Audit
Blast radius
Release comparison
```

---

# 12. Module 2 — Governance Integration

Microsoft AGT / ACS เป็น default governance provider

แต่ Control Plane ต้องไม่ lock-in

Interface concept (Rev 2 — ADR-002):

```go
type GovernanceProvider interface {
    // Evaluate is pure: no approval state lives in the provider.
    Evaluate(ctx context.Context, req GovernanceRequest) (GovernanceDecision, error)
}

type GovernanceDecision struct {
    Verdict         Verdict // Allow | Warn | Deny | Escalate | Transform
    EnforcedPayload []byte  // payload after transform (== input when no transform)
    InputDigest     Digest  // SHA-256(JCS(input binding))
    EnforcedDigest  Digest  // SHA-256(JCS(enforced binding))
    PolicyBundleID  string
    PolicyVersion   string
    ProviderID      string  // which PDP instance answered
    DecisionID      string
    Reasons         []string
    Approval        *ApprovalRequirement // set when Verdict == Escalate
    EvaluatedAt     time.Time
}
```

**Revalidation** ไม่ใช่ primitive ของ ACS

EACP นิยาม revalidation เองว่า = เรียก `Evaluate` ซ้ำด้วย snapshot ปัจจุบันก่อนเข้า atomic boundary (§15)

ถ้า error, timeout หรือ response ไม่ครบ → **fail closed** สำหรับ action ใหม่ที่มี side effect

Rev 2.1: fail closed ในที่นี้หมายถึง action จะยังไม่ execute โดยยังค้างเป็น RECEIVED และ API ตอบ 503 ให้ retry ได้ ไม่ได้หมายถึงการ DENIED แบบถาวร (ADR-002 §6)

แต่ห้ามบล็อก cancel, reconciliation read และ containment

Providers:

```text
local            (Slice A — deterministic rules, versioned policy bundles)
MicrosoftAGT     (Slice B — sidecar PDP over HTTP; primary production integration)
OPA
Cedar
OpenFGA
Custom HTTP PDP
```

Microsoft AGT เป็น primary production integration

แต่ Slice A ใช้ `local` provider เพื่อให้ execution correctness ไม่ต้องรอ AGT

---

# 13. Governance Context

Control Plane เก็บ:

```text
agent_id
agent_version
subject_id
tenant_id

governance_provider
provider_instance_id
policy_decision_id
policy_bundle_id
policy_version
verdict
reasons

input_digest
enforced_digest
enforced_payload

approval_request_id
approval_grant_id

trace_id
```

**Rev 2 — reference governance *decisions*, own approval *state*:**

EACP ไม่ reimplement policy engine

แต่ EACP **เป็นเจ้าของ approval state** และ **decision evidence** เพราะ:

```text
ACS stateless → ไม่มี state ให้ "duplicate"
approval ต้องถูก consume ใน transaction เดียวกับ action (§15)
audit ต้อง reconstruct ได้ว่า policy version ไหนตัดสิน (invariant 10)
```

ถ้าไม่มี decision evidence → action ห้ามเข้า atomic boundary (fail closed)

---

# 14. Action-Bound Approval

ใช้แนวคิดของ AGT approval protocol

Action ต้อง bind กับ:

```text
Agent
Subject
Operation
Target
Tool schema version
Resource
Parameters
```

จากนั้นสร้าง canonical digest

```text
ActionBinding
     ↓
RFC 8785 JCS
     ↓
SHA-256
     ↓
Action Digest
```

เปลี่ยน parameter:

```text
2,400,000 THB
```

เป็น:

```text
24,000,000 THB
```

approval เดิมใช้ไม่ได้

## 14.1 Enforced Digest — Rev 2

ถ้า policy ตอบ `transform` (เช่น redact field หรือ cap amount) payload ที่จะ execute จริงคือ **enforced payload**

```text
input binding     → JCS → SHA-256 → input_digest      (request identity: idempotency conflict check, audit)
enforced binding  → JCS → SHA-256 → enforced_digest   (approval, revalidation, execution ใช้ตัวนี้)
```

Idempotency key ซ้ำ:

```text
input_digest เดิม     → คืน action เดิม
input_digest ต่างกัน  → 409 Conflict
```

Approver เห็นและอนุมัติ **enforced payload**

Worker execute **เฉพาะ** enforced payload ที่ persist ไว้ใน atomic boundary และห้ามใช้ payload จาก agent โดยตรง

## 14.2 Approval Model — Rev 2 (Slice A, ADR-005)

Approval state อยู่ใน Postgres ของ EACP:

```text
approval_requests   (tenant_id, action_id, enforced_digest, policy_version,
                     required_quorum, eligible_roles, expires_at, state)
approval_votes      (request_id, approver_principal_id, decision, reason,
                     authorization_basis, voted_at)
approval_grants     (request_id, action_id, enforced_digest, policy_version,
                     expires_at, consumed_at, consumed_by_action_id)
```

กฎ (conservative):

```text
Grant bind กับ tenant + action + enforced_digest + policy_version
Grant ใช้ได้ครั้งเดียว: consume ใน atomic boundary ด้วย
    UPDATE ... WHERE consumed_at IS NULL AND expires_at > now()
Grant หมดอายุได้ และ PENDING_APPROVAL ก็หมดอายุได้ (→ EXPIRED)
Separation of duties:
    approver ≠ subject ที่ขอ
    approver ≠ owner ของ agent
    approver อยู่ tenant เดียวกัน และมี role ที่ policy กำหนด
Quorum: ต้องครบจำนวน vote ที่ policy กำหนด
    deny vote เดียว → DENIED (short-circuit)
Policy version เปลี่ยนก่อน consume → grant เดิมใช้ไม่ได้
    → revalidate ใหม่
    → allow: ไปต่อได้ (void request/grant เดิม) / escalate: ขอ approval ใหม่ / deny: DENIED
Policy version เป็น immutable และมี tenant policy pointer ที่ถูก lock ร่วมกัน
    ระหว่าง activation กับ release/dispatch (ADR-005 §5)
Policy / allowlist / contract activation เป็น two-person operation
    และคนที่ author หรือ activate ห้าม approve action ที่พึ่งพาสิ่งนั้น
Team owner: ตรวจ membership จาก group_memberships ถ้า resolve ไม่ได้ → ปฏิเสธ vote
ทุก vote และ grant มี authorization basis และ audit event
```

Approval **durable**: survive restart เพราะอยู่ใน Postgres ไม่อยู่ใน memory ของ process ใด

---

# 15. Atomic Execution Boundary

นี่เป็นหัวใจสำคัญที่สุดของ Execution Plane

หลัง governance อนุญาตแล้ว Action ยังไม่ถือว่าเริ่ม execution

ต้องผ่าน atomic boundary

**Rev 2 — ลำดับเต็ม (ADR-005):**

```text
(0) Submission — transaction แยก
    authenticate agent → capability check → claim idempotency key
    → persist action (RECEIVED, input payload, input_digest)

(1) Governance — นอก DB transaction เพราะห้ามถือ DB lock ระหว่างรอ PDP
    Evaluate → persist decision evidence
    → AUTHORIZED | PENDING_APPROVAL | DENIED

(2) Revalidation — นอก transaction, ก่อน release
    Evaluate อีกครั้งด้วย snapshot ปัจจุบัน
    → ได้ policy_version + enforced_digest ล่าสุด

(3) Release boundary — ONE TRANSACTION
    BEGIN
      lock action row (state ∈ {AUTHORIZED}), check not_after
      verify revalidation: policy_version ตรงกับ current policy version
                           enforced_digest ตรงกับที่ persist ไว้
      verify agent version ยัง ACTIVE และ tool ยังอยู่ใน allowlist
      consume one-time approval grant (ถ้ามี)
      reserve budget                       (hook ใน Slice A; hard budget ใน Slice B)
      transition AUTHORIZED → QUEUED
      append execution journal + audit event
      insert outbox event
    COMMIT
```

ถ้าเงื่อนไขใดไม่ผ่าน → ROLLBACK ทั้งหมด

ผลคือ approval ไม่ถูกเผาทิ้ง และ action ไม่ถูก queue

หลัง COMMIT เท่านั้น:

```text
Action is executable
```

ก่อน COMMIT:

```text
No worker should execute the action
```

การ execute จริงยังต้องผ่าน **fenced dispatch** อีกชั้น (§23)

---

# 16. Why Atomic Execution Boundary Matters

ป้องกัน:

```text
Approval ถูกใช้สองครั้ง

Worker race

Action ถูก queue แต่ DB ไม่มี record

Budget ถูกใช้เกินจาก race

Duplicate request สร้าง external effect ซ้ำ

Approval ถูกเผาทิ้งโดยไม่มี action รองรับ   (Rev 2)

Policy เปลี่ยนแล้วแต่ยังใช้ approval เก่า    (Rev 2)
```

Atomic boundary **ไม่ได้** ป้องกัน duplicate external effect จาก worker ที่ค้าง (stale)

กรณีนั้นแก้ด้วย fenced dispatch (§23) และ reconciliation protocol (§20)

---

# 17. Module 3 — Distributed Execution Fabric

Go เป็นภาษาหลัก

หน้าที่:

```text
Admission
Scheduling
Queueing
Worker Coordination
Leasing
Fencing
Idempotency
Retry
Backpressure
Reconciliation
Execution Evidence
```

---

# 18. Action State Machine

**Rev 2 — ตาราง transition ฉบับเต็มอยู่ใน ADR-004 ซึ่งเป็น source of truth**

Happy path:

```text
RECEIVED
   ↓  governance: allow / warn / transform
AUTHORIZED
   ↓  release boundary (§15)
QUEUED
   ↓  worker claim: lease_generation++
LEASED
   ↓  fenced dispatch-intent commit (§23)
EXECUTING          ← หมายถึง "อาจ dispatch ไปแล้ว" ไม่ใช่ "กำลังรอ"
   ↓  definitive success + evidence
SUCCEEDED
```

Approval path:

```text
RECEIVED ──escalate──► PENDING_APPROVAL ──grant (quorum)──► AUTHORIZED
                             │
                             ├── deny vote ──► DENIED
                             ├── timeout   ──► EXPIRED
                             └── cancel    ──► CANCELLED
```

Ambiguity path:

```text
EXECUTING ──timeout/5xx after send / worker crash / lease lost──► UNKNOWN_OUTCOME
UNKNOWN_OUTCOME ──reconciler lease──► RECONCILING
RECONCILING ──positive evidence──────────────────► SUCCEEDED
RECONCILING ──authoritative negative evidence────► RETRY_WAIT or FAILED
RECONCILING ──"not found" only / still unknown──► UNKNOWN_OUTCOME (backoff)
RECONCILING ──conflict / attempts exhausted─────► NEEDS_HUMAN_RESOLUTION
NEEDS_HUMAN_RESOLUTION ──operator resolution────► SUCCEEDED | FAILED | RETRY_WAIT (retry, same operation key)
```

Terminal:

```text
SUCCEEDED  FAILED  DENIED  CANCELLED  EXPIRED
```

Crash recovery (สรุป):

| State ตอน crash | Recovery |
|---|---|
| RECEIVED | Governance sweeper ประเมินใหม่ (Evaluate เป็น pure function) หรือ EXPIRED เมื่อเลย `not_after` ถ้า PDP ล่มจะยังเป็น RECEIVED และ API ตอบ 503 (ไม่ถูก DENIED) |
| PENDING_APPROVAL / AUTHORIZED / QUEUED / RETRY_WAIT | อยู่ใน Postgres (durable) แล้วทำงานต่อได้เลย |
| LEASED (ยังไม่มี dispatch intent) | Lease หมด → กลับ QUEUED ได้อย่างปลอดภัย เพราะยังไม่มี side effect |
| EXECUTING | Lease หมด → **UNKNOWN_OUTCOME** ห้ามกลับ QUEUED ข้อยกเว้นเดียว: connector ที่ certified READ_ONLY หรือ native-idempotent ให้ re-dispatch ได้ด้วย **operation key เดิม** |
| RECONCILING | Reconciler lease หมด → กลับ UNKNOWN_OUTCOME |

การเปลี่ยนจาก Rev 1:

```text
CREATED    → เปลี่ยนชื่อเป็น RECEIVED (persist ก่อน governance เพื่อ audit request ที่ถูก deny)
ADMITTED   → รวมเข้ากับ admission ตอน submission (§26) ถ้าถูก reject ได้ 429 และไม่สร้าง action
PREPARING  → รวมเข้ากับ LEASED (worker-local)
DEAD_LETTER → เป็น concept ของ message/inbox ไม่ใช่ action state
              action ที่ retry หมดแล้วจะเป็น FAILED(reason=retry_exhausted)
```

ทุก transition ต้องมี actor, reason และ audit event ตาม §8

---

# 19. First-Class UNKNOWN_OUTCOME

ห้ามถือว่า:

```text
timeout = failure
```

ตัวอย่าง:

```text
Agent Control Plane
      │
      │ create PO
      ▼
     SAP

SAP creates PO

response lost
```

Control Plane เห็น:

```text
timeout
```

แต่ความจริงอาจเป็น:

```text
PO created
```

ดังนั้น:

```text
EXECUTING
    ↓
timeout after dispatch
    ↓
UNKNOWN_OUTCOME
```

ไม่ใช่:

```text
FAILED
```

**Rev 2 — UNKNOWN_OUTCOME เกิดได้หลายทาง ไม่ใช่แค่ timeout:**

```text
timeout หลังส่ง request
connection reset หลังส่ง request
5xx ที่ connector ไม่ได้ certify ว่าเป็น "no effect"
worker crash / lease หมด ระหว่าง EXECUTING
kill / cancel ระหว่าง EXECUTING
```

ผลที่ถือว่า **definitive** มีแค่สองแบบ:

```text
definitive success   = response สำเร็จที่มี external reference
definitive no-effect = error ที่ connector contract certify ว่าไม่มี side effect
                       เช่น validation 4xx ที่ระบุไว้ หรือ connection refused ก่อนส่ง
```

นอกนั้นทั้งหมดเป็น UNKNOWN_OUTCOME (conservative default)

---

# 20. Reconciliation Engine

```text
UNKNOWN_OUTCOME
       ↓
Reconciliation Worker
       ↓
Check external world state
       ↓
```

ผล:

```text
CONFIRMED_SUCCESS
CONFIRMED_NOT_EXECUTED
STILL_UNKNOWN
CONFLICT
```

จากนั้น:

```text
CONFIRMED_SUCCESS
→ SUCCEEDED

CONFIRMED_NOT_EXECUTED
→ retry may be allowed (เฉพาะเมื่อ evidence ผ่าน proof standard ด้านล่าง)

STILL_UNKNOWN
→ retry reconciliation later → หมดจำนวนครั้ง/เวลา → NEEDS_HUMAN_RESOLUTION

CONFLICT
→ NEEDS_HUMAN_RESOLUTION
```

## 20.1 Operation Identity — Rev 2

ทุก action มี **operation key** ที่คงที่ตลอดอายุ action และไม่เปลี่ยนตาม attempt

```text
operation_key = "eacp:{tenant_id}:{action_id}"
```

Connector ต้องส่ง operation key ไปยัง external system:

```text
native idempotency     → เป็น Idempotency-Key ของ external API
correlation only       → ฝังใน field อ้างอิงของ record (เช่น PO external reference)
                         เพื่อให้ reconciliation ค้นหาได้
none                   → execute ได้แบบ at-most-once เท่านั้น
                         ambiguity ใด ๆ → NEEDS_HUMAN_RESOLUTION
```

## 20.2 Reconciliation Proof Standard — Rev 2

**"Not found" อย่างเดียวไม่ใช่หลักฐานว่าไม่ได้ execute**

เช่น SAP อาจ index ช้า, lookup API อาจ eventual-consistent หรือ query อาจผิด

Connector contract (§31) ต้องประกาศ proof standard:

| Evidence | Positive (success) | Negative (not executed) |
|---|---|---|
| `AUTHORITATIVE` | ค้นเจอ record ด้วย operation key | lookup ด้วย operation key บน read path ที่ **strongly consistent** และ connector certify ว่า "ไม่เจอ = ไม่เคยเกิด" |
| `BEST_EFFORT` | ค้นเจอ record ด้วย operation key | **ใช้ไม่ได้** → STILL_UNKNOWN |
| `NONE` | ใช้ไม่ได้ | ใช้ไม่ได้ → NEEDS_HUMAN_RESOLUTION |

กฎ auto-retry หลัง UNKNOWN_OUTCOME (conservative):

```text
READ_ONLY                                   → retry ได้
native idempotent (same operation key)      → retry ได้ด้วย key เดิม
irreversible / non-idempotent:
    AUTHORITATIVE negative evidence         → retry ได้ด้วย operation key เดิม
                                              ถ้า retry policy อนุญาต
    อื่น ๆ ทั้งหมด                           → NEEDS_HUMAN_RESOLUTION
```

## 20.3 Human Resolution — Rev 2

`NEEDS_HUMAN_RESOLUTION` เป็น state ที่ต้องมีคน resolve อย่างชัดเจน

Operator ที่ authenticated แล้วทำได้:

```text
mark SUCCEEDED  (แนบ external reference / evidence)
mark FAILED     (แนบ evidence ว่าไม่ได้ execute)
authorize retry (ใช้ operation key เดิม; ถ้าเป็น high-risk action ต้องมี SoD)
```

ทุก resolution เป็น privileged action ที่มี reason, actor และ audit event (§57)

---

# 21. No Fake Exactly-Once

ห้ามเขียน marketing ว่า:

> Exactly-once execution across every external system

ใช้ terminology:

```text
Idempotent where supported

Effectively-once where reconcilable

At-most-once when retry is unsafe
```

---

# 22. Execution Lease

Worker ต้อง acquire lease

```text
action_id
worker_id
lease_generation
leased_until
heartbeat_at
```

Worker crash:

```text
heartbeat stops
 ↓
lease expires
 ↓
another worker may claim
```

**Rev 2 — การ claim อ่านจาก Postgres เท่านั้น:**

```sql
SELECT ... FROM actions
WHERE state = 'QUEUED' AND tenant_id = ...
FOR UPDATE SKIP LOCKED
LIMIT n;
```

Worker **ห้าม** execute จาก message ใน queue (NATS) โดยตรง ต้อง claim และ fence ผ่าน Postgres ทุกครั้ง (§60)

Reclaim action ที่ lease หมด:

```text
LEASED    (ไม่มี dispatch intent) → QUEUED           ปลอดภัย: ยังไม่มี side effect
EXECUTING (มี dispatch intent)    → UNKNOWN_OUTCOME  ห้าม re-dispatch
                                    ยกเว้น READ_ONLY / native-idempotent ที่ใช้ operation key เดิม
```

Worker ต้องไม่เริ่ม external call ถ้า lease ที่เหลือน้อยกว่า `connector_timeout + safety_margin`

และ call deadline ต้องสั้นกว่า lease ที่เหลือเสมอ

---

# 23. Fencing Token

Example:

```text
Worker A
generation = 10

A freezes

lease expires

Worker B
generation = 11

A wakes up
```

A ห้าม commit

DB update:

```sql
UPDATE actions
SET state = 'SUCCEEDED'
WHERE id = $1
AND lease_generation = $2
AND state = 'EXECUTING';
```

generation 10 จะ update ไม่สำเร็จ

## 23.1 Fencing ต้องครอบคลุม Execution Semantics — Rev 2

Fencing ที่ DB **อย่างเดียวไม่พอ**

ตัวอย่างความล้มเหลวใน Rev 1:

```text
A (gen 10) ส่ง create_po ไป SAP → freeze → lease หมด
B (gen 11) reclaim → เห็น state EXECUTING → ส่ง create_po อีกครั้ง
→ PO สองใบ
(commit ของ A ถูก reject อย่างถูกต้อง แต่ side effect เกิดไปแล้ว)
```

Rev 2 ใช้ **fenced dispatch protocol**:

```text
1. Dispatch intent (fenced, commit ของตัวเอง, ก่อน external call ทุกครั้ง)
     UPDATE actions SET state='EXECUTING', dispatch_intent_at=now()
     WHERE id=$1 AND lease_generation=$2 AND state='LEASED'
       AND leased_until > now() + $call_budget
     + ตรวจใน transaction เดียวกัน (อ่าน registry แบบ FOR SHARE):
       AgentVersion ACTIVE, tool ยังอยู่ใน allowlist,
       pinned connector contract ยัง active และไม่ถูก revoke,
       tenant policy pointer == pinned policy_version (ถ้าไม่ตรง → กลับไป AUTHORIZED)
     + INSERT action_attempts(attempt_no, lease_generation, worker_id,
                              operation_key, dispatched_at)
     ถ้า update 0 rows → ห้ามเรียก external system

2. External call ใช้ operation key (§20.1) และ deadline < lease ที่เหลือ
   ส่ง fencing generation ไปด้วยสำหรับ target ที่รองรับ conditional write

3. Result commit (fenced)
     ... WHERE lease_generation=$2 AND state='EXECUTING'

4. ถ้า stale worker commit ไม่ผ่าน
     → append "late result evidence" ลง journal (ไม่เปลี่ยน state)
     → reconciler นำไปใช้เป็นหลักฐาน

5. Reclaim action ที่มี dispatch intent → UNKNOWN_OUTCOME (§22) ห้าม re-dispatch
```

ผลลัพธ์:

```text
DB state        ถูก fence ด้วย generation
External effect ถูกจำกัดด้วย dispatch-intent rule + operation key
                (idempotent/correlated ที่ external) + reconciliation
```

---

# 24. Fair Scheduler

ต้องรองรับหลาย tenant/team

```text
Team A     10,000 jobs
Team B        100 jobs
Team C        100 jobs
```

A ห้าม starvation B/C

Slice B (Phase 12):

```text
Weighted Deficit Round Robin
+
Priority
+
Aging
```

**Rev 2:** Slice A ใช้ FIFO claim จาก Postgres บวก per-tenant cap แบบ static (§26)

ยอมรับความเสี่ยง starvation ได้ใน Slice A เพราะเป้าหมายคือ correctness ไม่ใช่ fairness

Fair scheduler ต้องเลือก *ลำดับการ claim* ใน Postgres

ไม่ได้ขึ้นกับลำดับที่ message มาถึงใน NATS (§60)

---

# 25. Scheduling Dimensions

```text
Tenant
Department
Team
Agent
Priority
Deadline
Connector
Cost
Concurrency
Side-effect risk
```

---

# 26. Backpressure

เมื่อ capacity เต็ม:

```text
Agent
 ↓
Admission
 ↓

ACCEPT

QUEUE

THROTTLE

REJECT
```

ห้าม queue แบบ unbounded

**Rev 2:** Slice A มี admission แบบ static ตอน submission

```text
global max QUEUED
per-tenant max QUEUED
```

ถ้าเกิน → 429 และไม่สร้าง action

แต่ละ limit แบบละเอียด (tenant/connector/worker) อยู่ Slice B (Phase 13)

---

# 27. Bulkhead Isolation

แยก resource pool:

```text
SAP writes

GitHub writes

Email

LLM

Database
```

ถ้า SAP ล่ม:

```text
SAP capacity exhausted
```

Email/GitHub ต้องยังทำงาน

---

# 28. Circuit Breaker

```text
CLOSED
 ↓ failures
OPEN
 ↓ cooldown
HALF_OPEN
 ↓ probe success
CLOSED
```

scope:

```text
provider
connector
endpoint
tool
```

---

# 29. Retry Safety

Retry decision ต้องอิง execution semantics

```text
READ
→ retry

IDEMPOTENT WRITE
→ retry same key

REVERSIBLE WRITE
→ conditional retry

IRREVERSIBLE + NON-IDEMPOTENT
→ no blind retry
```

**Rev 2:**

Retry หลัง definitive no-effect ทำได้ตาม retry policy

Retry หลัง UNKNOWN_OUTCOME ต้องผ่านกฎ §20.2 เสมอ

ทุก retry ใช้ **operation key เดิม** (§20.1)

ถ้าไม่รู้ side-effect class → ถือเป็น `IRREVERSIBLE_WRITE` + non-idempotent (at-most-once)

---

# 30. Retry Budget

มี:

```text
max_attempts
max_elapsed_time
max_retry_cost
```

เพื่อหยุด retry storm

---

# 31. Module 4 — Tool / Connector Registry

ทุก external capability ต้อง register

ตัวอย่าง:

```yaml
connector: sap-production
tool: create_po

protocol: mcp

side_effect: [irreversible_write, financial]

idempotency:
  mode: native            # native | correlation_only | none
  key_field: Idempotency-Key

operation_identity:
  correlation_field: external_reference   # operation key ถูกฝังไว้ตรงนี้

reconciliation:
  lookup: by_operation_key
  proof_standard: authoritative  # authoritative | best_effort | none
  consistency: strong

no_effect_errors:                # error ที่ certify ว่าไม่มี side effect
  - http_400_validation
  - connection_refused_before_send

credentials:
  custody: worker         # agent ไม่เคยได้รับ credential

concurrency:
  group: sap-write
  max_inflight: 20

data:
  sensitivity: confidential
```

**Rev 2 — Connector contract เป็น operator-declared และ certified:**

MCP/tool metadata เป็น untrusted (§67)

ดังนั้น `side_effect`, `idempotency`, `reconciliation` และ `no_effect_errors` ต้องประกาศโดย operator ใน registry

ค่าที่ server ประกาศเอง (self-description) เป็นแค่ข้อมูลประกอบ

Contract ผูกกับ tool fingerprint (§33) ถ้า fingerprint เปลี่ยน contract ถือว่า invalid จนกว่าจะ recertify

ถ้าไม่มี contract → tool นั้น execute ไม่ได้ (fail closed)

---

# 32. Side-Effect Classification

```text
READ_ONLY

REVERSIBLE_WRITE

IRREVERSIBLE_WRITE

EXTERNAL_COMMUNICATION

FINANCIAL

ADMINISTRATIVE
```

หนึ่ง action อาจมีหลาย tag

---

# 33. Tool Fingerprint

Tool definition ต้อง fingerprint

```text
Tool Schema
 +
Metadata
 +
Security attributes
    ↓
Canonical form
    ↓
SHA-256
```

เมื่อ MCP reconnect:

```text
old fingerprint
vs
new fingerprint
```

ถ้าเปลี่ยน:

```text
Low risk
→ audit

High risk
→ quarantine / require recertification
```

---

# 34. Tool Certification

Lifecycle:

```text
DISCOVERED
   ↓
SCANNED
   ↓
VALIDATED
   ↓
CERTIFIED
   ↓
ACTIVE
```

Alternative:

```text
QUARANTINED
REVOKED
```

**Rev 2:** การ certify tool ต้องรวม **connector contract** ด้วย (§31):

```text
side-effect class
idempotency mode
operation identity
reconciliation proof standard
no-effect errors
credential custody
```

Contract ผูกกับ fingerprint ถ้า fingerprint เปลี่ยน → contract invalid → tool execute ไม่ได้จนกว่าจะ recertify

---

# 35. Module 5 — Dependency Graph

สร้าง graph:

```text
Agent
 ↓
Model
 ↓
MCP Server
 ↓
Tool
 ↓
Application
 ↓
Dataset
```

อีกแบบ:

```text
Agent A
 ↓ delegates
Agent B
 ↓ uses
MCP X
 ↓ accesses
Database Y
```

---

# 36. Blast Radius

ถ้า:

```text
sap-mcp v3 compromised
```

Control Plane ตอบได้ทันที:

```text
Affected Agents: 41

Affected Teams:
Procurement
Finance
Operations

Affected Workflows: 17

Data classes:
Internal
Confidential

Production runs in last 24h:
1,284
```

---

# 37. Initial Graph Storage

MVP:

```text
PostgreSQL
+
recursive CTE
```

ไม่ใช้ graph database จนมีเหตุผลจริง

ภายหลังอาจเปลี่ยนเป็น:

```text
Neo4j
Memgraph
etc.
```

เมื่อ graph complexity justify

---

# 38. Module 6 — Fleet Operations

มอง Agent เป็น fleet

Operator ต้อง:

```text
list
filter
inspect
pause
resume
kill
quarantine
roll back
upgrade
```

Agent fleet dashboard:

```text
Total Agents      1,270
Production          421
Canary               18
Quarantined           3
Unknown Owner         7
Policy Drift          5
```

---

# 39. Distributed Kill Switch

ใช้ AGT kill semantics

แต่ Control Plane ทำ distributed propagation

scope:

```text
Global
Tenant
Team
Agent
Agent Version
Run
Action
Connector
Tool
Model
```

---

# 40. Kill Propagation

```text
Operator
 ↓
Control Plane
 ↓
Postgres authoritative kill state
 ↓
NATS event
 ↓
Workers / gateways
```

Worker:

```text
check before lease

check before execute

listen for live kill event

cancel context if possible
```

แต่:

```text
cancel != external rollback
```

ดังนั้น dispatched action อาจต้อง reconcile

**Rev 2 (Slice C, Phase 16) — ปิด TOCTOU:**

```text
kill_epoch        Postgres เก็บ kill_epoch แบบ monotonic ต่อ scope
                  เป็น authoritative state
Fenced check      ตรวจ kill state ใน transaction เดียวกับ dispatch-intent commit (§23.1)
                  ถ้า kill แล้ว commit ไม่ผ่าน และไม่มี external call
Backstop          worker poll kill_epoch เป็นระยะ เพราะ NATS event อาจหายหรือช้า
EXECUTING         kill ระหว่าง EXECUTING → cancel context → UNKNOWN_OUTCOME → reconcile
                  ไม่ถือว่า FAILED
Containment       kill / reconcile / cancel ต้องทำงานได้แม้ governance PDP ล่ม
```

Slice A มีแค่ agent `SUSPENDED` lifecycle ที่ถูกตรวจใน release boundary และ dispatch-intent commit

Distributed kill switch เต็มรูปแบบอยู่ Slice C

---

# 41. Module 7 — Agent SRE & Observability

ใช้ AGT SRE + OpenTelemetry

Control Plane เพิ่ม fleet-level view

Trace:

```text
User Request
  ↓
Agent Run
  ↓
Governance Evaluation
  ↓
Approval
  ↓
Admission
  ↓
Queue Wait
  ↓
Worker Lease
  ↓
Tool Execution
  ↓
Reconciliation
  ↓
Outcome
```

---

# 42. Execution Evidence vs Governance Evidence

แยกชัด:

```text
Governance Evidence

Who?
What policy?
What approval?
Why allow?
```

กับ:

```text
Execution Evidence

Which worker?
When dispatched?
Which idempotency key?
Which external transaction?
What happened?
Was reconciliation required?
```

เชื่อมกันด้วย:

```text
action_id
decision_id
trace_id
```

**Rev 2 — Evidence journal (Slice A):**

```text
append-only       ห้าม UPDATE / DELETE (บังคับที่ DB role)
hash-chained      แต่ละ event มี prev_hash ต่อ tenant (tamper-evident)
ครอบคลุม           ทุก state transition, decision evidence, approval vote/grant/consume,
                  dispatch intent, late result evidence, reconciliation evidence,
                  human resolution
```

Governance evidence ต้องเก็บ:

```text
provider
provider_instance_id
policy_bundle_id
policy_version
verdict
reasons
input_digest
enforced_digest
evaluated_at
```

Execution evidence ต้องเก็บ:

```text
worker_id
lease_generation
operation_key
dispatched_at
external_reference
outcome
```

OTel trace context (W3C `traceparent`) ต้องส่งต่อผ่าน action row, outbox และ message headers

เพื่อให้ trace ไม่ขาดตอนที่ queue

---

# 43. Outcome Attestation Bridge

Execution Fabric สร้าง verified outcome:

```text
CONFIRMED_SUCCESS
CONFIRMED_FAILURE
UNKNOWN
CONFLICT
```

แล้ว bridge กลับไป Governance/Audit plane

```text
Execution Evidence
       ↓
Outcome Attestation
       ↓
AGT / Audit / SIEM
```

---

# 44. Key Metrics

```text
actions_received_total

actions_authorized_total

actions_denied_total

actions_executed_total

actions_unknown_total

actions_reconciled_total

queue_depth

queue_wait_seconds

active_workers

lease_expiration_total

stale_commit_rejected_total

retry_total

connector_error_rate

budget_reserved

budget_committed

governance_latency

execution_overhead

approvals_pending                (Rev 2)

approval_grants_consumed_total   (Rev 2)

dispatch_intent_total            (Rev 2)

needs_human_resolution           (Rev 2)

late_result_evidence_total       (Rev 2)
```

**Rev 2 — Label cardinality:** metric labels มีได้เฉพาะมิติที่จำกัดจำนวน เช่น tenant, connector, state, verdict

ห้ามใช้ `agent_id` หรือ `action_id` เป็น label

ถ้าต้องดูราย agent ให้ใช้ traces, logs หรือ query จาก Postgres

---

# 45. Module 8 — Agent FinOps

รวม:

```text
Cost observation
Budget
Reservation
Quota
Forecast
Chargeback
```

**Rev 2 — Scope:**

Hard budget reservation สำหรับ tool actions อยู่ Slice B (Phase 11)

FinOps เต็มรูปแบบอยู่หลัง Slice C (Phase 18)

EACP ไม่อยู่ใน LLM path (§3.1) ดังนั้น LLM cost มาจากการ **ingest** ไม่ใช่การ proxy:

```text
OTel GenAI spans จาก agent runtime
provider billing / usage export
```

ถ้าเพิ่ม LLM gateway module ภายหลัง จะ reserve budget ต่อ LLM call ได้

---

# 46. Hierarchical Budget

```text
Organization
      ↓
Business Unit
      ↓
Department
      ↓
Team
      ↓
Agent
      ↓
Run
```

resources:

```text
Money
Tokens
Model calls
Tool calls
GPU seconds
API units
Concurrent workers
```

---

# 47. Strict Budget Reservation

```text
RESERVE estimate
       ↓
EXECUTE
       ↓
OBSERVE actual
       ↓
COMMIT actual
       ↓
RELEASE remainder
```

ตัวอย่าง:

```text
Remaining = $10

Agent A asks $7
Agent B asks $7
```

atomic reservation:

```text
A gets $7

remaining = $3

B denied/waits
```

ป้องกัน concurrency overspend

**Rev 2 — Contention และการรั่ว:**

```text
Lock order        lock budget account จาก root → leaf เสมอ (กัน deadlock)
Escrow            parent จัดสรร quota ให้ child ล่วงหน้า
                  hot path lock แค่ leaf row ไม่ใช่ Organization row ทุกครั้ง
Reservation TTL   ทุก reservation มี expires_at
                  sweeper release reservation ที่ค้างจาก flow ที่ crash
Hard vs soft      เฉพาะ hard budget ที่อยู่ใน atomic boundary
                  soft budget คำนวณแบบ async
UNKNOWN_OUTCOME   reservation ของ action ที่ outcome ไม่ชัดจะไม่ถูก release จนกว่าจะ reconcile
                  (conservative)
```

---

# 48. Chargeback

แสดง:

```text
Engineering Team

LLM            $8,241
Search APIs      $781
MCP services     $210

Total          $9,232
```

รวม cost ต่อ:

```text
Agent
Team
Business unit
Workflow
User
Customer
```

---

# 49. Module 9 — Release & Evaluation Platform

Agent production deployment ต้องผ่าน pipeline

```text
Code
 ↓
Unit tests
 ↓
Policy tests
 ↓
Security tests
 ↓
Agent evaluations
 ↓
Replay
 ↓
Shadow
 ↓
Canary
 ↓
Production
```

---

# 50. Agent Certification Pipeline

```text
REGISTER
 ↓
BUILD
 ↓
STATIC CHECK
 ↓
POLICY TEST
 ↓
SECURITY SCAN
 ↓
EVALUATION
 ↓
HUMAN REVIEW
 ↓
CERTIFIED
```

---

# 51. Replay

นำ production trace เก่า:

```text
Agent v14
```

มา run กับ:

```text
Agent v15
```

เปรียบเทียบ:

```text
Policy decisions

Tool selection

Cost

Latency

Result quality

Risk

Errors
```

**Rev 2 — Research-grade:**

LLM agent ไม่ deterministic

ดังนั้น replay ต้องมี record/replay connector ที่ตอบด้วย tool response ที่บันทึกไว้

การเปรียบเทียบต้องดูที่ distribution ไม่ใช่ผลที่ตรงกันทุกตัว

---

# 52. Shadow Deployment

```text
Production request
       │
       ├─────────────► v14 real execution
       │
       └─────────────► v15 shadow
```

v15:

```text
cannot perform destructive effects
```

เก็บเฉพาะ decision/result comparison

**Rev 2:** "ห้ามมี destructive effect" ต้องจริงในเชิง **โครงสร้าง**

Shadow ใช้ connector ที่ไม่มี credential สำหรับเขียน และไม่มี network path ไป system จริง (§3.2)

การพึ่งแค่ flag ถือว่าไม่พอ

---

# 53. Canary

Deployment progression:

```text
1%
 ↓
5%
 ↓
25%
 ↓
50%
 ↓
100%
```

Auto rollback ถ้า:

```text
SLO drops
policy violations increase
unknown outcomes rise
cost spikes
latency spikes
```

---

# 54. Module 10 — Agent Security Operations Center

Operator UI รวม:

```text
Inventory
Alerts
Policy violations
Approvals
Active incidents
Agent health
Tool risk
MCP drift
Kill switches
Cost anomalies
Unknown outcomes
```

---

# 55. Agent SOC Dashboard

ตัวอย่าง:

```text
Enterprise Agent Security

Agents
────────────────────────
Registered            1,270
Production              421
High Risk                82
Quarantined               3

Security
────────────────────────
Blocked Actions        4,812
Approval Pending          17
MCP Drift                  5
Policy Drift               3

Execution
────────────────────────
Running                   812
Queued                  3,428
Unknown Outcome             2

FinOps
────────────────────────
Spend Today          $12,421
Budget Warnings            11
Hard Blocks                 2
```

---

# 56. Incident View

หนึ่ง incident:

```text
MCP schema changed unexpectedly
```

Control Plane แสดง:

```text
MCP:
sap-production

Old fingerprint:
abc...

New fingerprint:
xyz...

Affected agents:
41

Active runs:
13

Data sensitivity:
Confidential

Recommended containment:
Disable MCP
Pause affected agents
Re-run certification
```

Operator เป็นคนตัดสินใจ

---

# 57. Human-In-The-Loop

Human intervention ใช้สำหรับ:

```text
High-risk approval
Unknown outcome
Security incident
Policy exception
Conflict resolution
Production promotion
Break-glass
```

AI อาจช่วย:

```text
summarize
classify
recommend
explain
```

แต่ authoritative control decisions ควร deterministic หรือ authenticated human decision ตาม policy

**Rev 2 — Privileged control-plane actions ก็ต้องถูก govern:**

การกระทำของ operator/admin ที่เปลี่ยน safety state:

```text
resolve NEEDS_HUMAN_RESOLUTION
approve / deny
แก้ connector contract
แก้ allowlist ของ AgentVersion
เปลี่ยน policy bundle
ยกเลิก kill
แก้ budget
```

ทุกอย่างในรายการนี้ต้อง:

```text
authenticated principal + role
reason (บังคับ)
audit event ใน hash-chained journal
separation of duties สำหรับ action ที่ high-risk
break-glass: ทำได้ แต่ต้องมี post-incident review
```

---

# 58. Core Domain Entities

```text
Tenant

Organization
Department
Team

HumanPrincipal
ServicePrincipal

Agent
AgentVersion
AgentInstance

Run
Action
ActionAttempt

Tool
ToolVersion
Connector

Model
ModelProvider

PolicyBundle            (Rev 2: versioned, สำหรับ local provider)
GovernanceDecision      (Rev 2: evidence ถูก persist ไม่ใช่แค่ reference)

ApprovalRequest         (Rev 2: EACP-owned)
ApprovalVote
ApprovalGrant

AgentCredential         (Rev 2: agent → EACP identity เท่านั้น)
ConnectorContract       (Rev 2: operator-declared, ผูกกับ fingerprint)
ConnectorSecretRef      (Rev 2: อ้างอิง secret ที่ worker ถือ; ไม่เก็บค่า secret)

BudgetAccount
BudgetReservation
UsageRecord

Lease

ExecutionJournal
AuditEvent              (Rev 2: append-only, hash-chained)
HumanResolution         (Rev 2)

DependencyEdge

Release
Evaluation
Deployment

Incident
KillSwitch

AuditReference
```

---

# 59. Storage Architecture

Primary:

```text
PostgreSQL
```

Use for:

```text
Registry
Actions
Leases
Idempotency
Approvals          (Rev 2)
Decision evidence  (Rev 2)
Audit journal      (Rev 2)
Budgets
Outbox
Dependencies
Deployments
Kill state
Incidents
```

**Rev 2:** Postgres เป็น **source of truth เพียงแหล่งเดียว** สำหรับ execution state (ADR-014)

ทุก table ที่ critical มี `tenant_id` และอยู่ภายใต้ Row-Level Security (§69)

---

# 60. Messaging

Use:

```text
NATS JetStream
```

for:

```text
work-available hints   (Rev 2: เดิมคือ "action dispatch")
execution events
kill propagation
registry updates
health events
reconciliation wake-ups
```

Assume:

```text
messages may duplicate
messages may be lost or delayed   (Rev 2)
```

**Rev 2 — NATS ไม่ใช่ execution authority:**

```text
NATS message = hint ที่มีแค่ action_id
Worker ต้อง claim + fence ผ่าน Postgres เสมอ (§22)
ถ้าไม่มี NATS → ระบบยังถูกต้อง แค่ช้าลง (worker poll Postgres)
```

Slice A ไม่ใช้ NATS: worker poll หรือใช้ Postgres `LISTEN/NOTIFY`

NATS JetStream เข้ามาใน Slice B (Phase 10)

---

# 61. Cache

Redis optional

Use for:

```text
hot registry cache
policy reference cache
rate limits where appropriate
dashboard acceleration
```

Redis ไม่ใช่ source of truth สำหรับ correctness-critical execution state

---

# 62. Transactional Outbox

```sql
BEGIN;

UPDATE action;        -- state transition (Rev 2: e.g. AUTHORIZED → QUEUED)

INSERT outbox_event;

COMMIT;
```

Outbox publisher:

```text
Postgres
 ↓
NATS            (Slice B; Slice A: outbox → log sink / LISTEN-NOTIFY)
```

Outbox event ต้องมี trace context (`traceparent`) ติดไปด้วย

ป้องกัน:

```text
DB committed
but process crashed before publish
```

---

# 63. Inbox / Dedup

Consumer เก็บ:

```text
message_id
consumer
processed_at
```

duplicate:

```text
ACK
do not execute again
```

---

# 64. API Layer

External:

```text
REST / JSON
```

High throughput internal:

```text
gRPC
```

Events:

```text
NATS
```

---

# 65. Example APIs

```text
POST /v1/agents

GET /v1/agents/{id}

POST /v1/actions                        (Rev 2: ต้องมี Idempotency-Key; รองรับ ?wait=<duration>)

GET /v1/actions/{id}

GET /v1/actions/{id}/events             (Rev 2: SSE สำหรับ action ที่ใช้เวลานาน)

POST /v1/actions/{id}/cancel

POST /v1/actions/{id}/reconcile

POST /v1/actions/{id}/resolve           (Rev 2: human resolution; operator เท่านั้น)

GET /v1/approvals?state=pending         (Rev 2)

POST /v1/approvals/{id}/votes           (Rev 2: approver เท่านั้น; ตรวจ SoD)

GET /v1/dependencies/blast-radius

POST /v1/killswitch

GET /v1/incidents

GET /v1/budgets

GET /v1/fleet/health
```

**Rev 2 — Synchronous result path:**

Agent framework ส่วนใหญ่ต้องการผลของ tool call ทันที

```text
POST /v1/actions?wait=30s
  → 200 + result           ถ้าจบ (terminal) ภายในเวลาที่รอ
  → 202 + action_id + state ถ้ายังไม่จบ เช่น PENDING_APPROVAL, QUEUED, UNKNOWN_OUTCOME
```

Client ติดตามต่อด้วย `GET /v1/actions/{id}` หรือ SSE

Agent SDK/adapter ต้องแปลง `202` เป็นผลที่ agent เข้าใจ เช่น "pending approval" โดยไม่ retry เอง

Control-plane overhead SLO จะกำหนดหลังวัดจริง (§105)

---

# 66. CLI

```text
eacp agent list

eacp agent inspect procurement-agent

eacp action inspect act_123

eacp action reconcile act_123

eacp action resolve act_123 --outcome succeeded --evidence PO-9822 --reason "..."   (Rev 2)

eacp approval list --pending                                                     (Rev 2)

eacp approval vote apr_456 --approve --reason "..."                              (Rev 2)

eacp dependency blast-radius sap-mcp

eacp kill agent procurement-agent:v14

eacp connector disable sap-production

eacp budget inspect procurement

eacp fleet status
```

---

# 67. Security Architecture

Zero trust assumption:

```text
Agent is untrusted

LLM output is untrusted

Tool metadata is untrusted

MCP server is untrusted

Queue input is untrusted

External API output is untrusted
```

---

# 68. Threat Model

อย่างน้อยต้องครอบคลุม:

```text
Prompt injection

Tool poisoning

MCP rug pull

Agent impersonation

Confused deputy

Privilege escalation

Approval replay

Parameter substitution

Duplicate execution

Credential theft

Cross-tenant access

Budget race

Denial of wallet

Retry storm

Queue flooding

Compromised worker

Compromised connector

Audit manipulation

Supply-chain compromise

Unknown-outcome mishandling

Control-plane bypass (agent ถือ credential / มี network path ตรง)   (Rev 2)

Stale-worker duplicate dispatch                                    (Rev 2)

False-negative reconciliation ("not found" → retry → duplicate)     (Rev 2)

Approval self-approval / cross-tenant approver                     (Rev 2)

Stale approval หลัง policy เปลี่ยน                                   (Rev 2)

Forged / replayed governance decision (PDP)                        (Rev 2)

Malicious operator / admin                                         (Rev 2)

Sensitive data ใน trace / audit (PII, secrets)                      (Rev 2)
```

รายละเอียดและ mitigation อยู่ที่ `docs/security/THREAT_MODEL.md` (Phase 0)

---

# 69. Tenant Isolation

ทุก record critical มี:

```text
tenant_id
```

ทดสอบ:

```text
Tenant A cannot:

read Tenant B agent

consume Tenant B budget

reuse Tenant B idempotency key

kill Tenant B agent

read Tenant B trace

approve Tenant B action          (Rev 2)

consume Tenant B approval grant  (Rev 2)
```

**Rev 2 — Mechanism (Slice A, ADR-021):**

```text
Postgres Row-Level Security บนทุก table ที่ critical
แต่ละ transaction ตั้ง SET LOCAL app.tenant_id
application role ไม่มี BYPASSRLS
unique key ทุกตัวมี tenant_id อยู่ด้วย:
    (tenant_id, agent_id, idempotency_key)
    (tenant_id, operation_key)
    ...
test tenant isolation ต้องรันกับ RLS จริง ไม่ใช่แค่ application check
```

Data handling:

```text
trace / log / audit ห้ามมี secret
payload ที่เป็น sensitive ถูก redact ตาม data class ก่อนออกนอก Postgres
```

Data residency และ legal hold อยู่ §113

---

# 70. Credential Architecture

**Rev 2 — Credential custody เป็น requirement ของ Slice A ไม่ใช่ optimization:**

> Agent ต้องไม่มี credential สำหรับเรียก privileged external system โดยตรง

(เดิมเขียนว่า "อย่าส่ง static secret เข้า Agent โดยตรงถ้าเลี่ยงได้" ซึ่งอ่อนเกินไป)

Slice A:

```text
Worker ถือ static connector secret (env / mounted file)
ConnectorSecretRef ใน DB เก็บแค่ reference ไม่เก็บค่า
ไม่มี API ใดคืน secret
secret ไม่อยู่ใน action payload, journal, trace หรือ log (มี redaction test)
Agent ได้แค่ agent credential สำหรับเรียก EACP API
```

Phase 22 (JIT credentials):

```text
Agent
 ↓
Action
 ↓
Execution Worker
 ↓
Credential Provider
 ↓
short-lived credential
 ↓
External Resource
```

Providers:

```text
AGT Credential System
SPIFFE/SPIRE
Azure Workload Identity
AWS IAM
Vault
Custom enterprise secret broker
```

---

# 71. Repository Structure

```text
enterprise-agent-control-plane/
│
├── cmd/
│   ├── controlplane-api/
│   ├── execution-worker/
│   ├── scheduler/          (Slice B)
│   ├── event-worker/       (Slice B)
│   ├── fakeerp/            (Slice A demo target)
│   └── eacpctl/
│
├── sidecars/
│   └── agt-pdp/            (Slice B: AGT/ACS sidecar, Python)
│
├── internal/
│   ├── registry/
│   ├── lifecycle/
│   ├── identity/           (Rev 2: agent / operator principals)
│   ├── capability/         (Rev 2: tool allowlist enforcement)
│   ├── governance/
│   ├── approval/           (Rev 2: EACP-owned approval store)
│   ├── digest/             (Rev 2: JCS + SHA-256)
│   ├── audit/              (Rev 2: hash-chained journal)
│   ├── action/
│   ├── admission/
│   ├── scheduler/
│   ├── lease/
│   ├── idempotency/
│   ├── reconciliation/
│   ├── connector/
│   ├── dependency/
│   ├── fleet/
│   ├── killswitch/
│   ├── budget/
│   ├── release/
│   ├── evaluation/
│   ├── incident/
│   ├── telemetry/
│   ├── outbox/
│   ├── inbox/
│   └── storage/
│
├── integrations/
│   ├── governance/
│   │   ├── microsoftagt/
│   │   ├── opa/
│   │   └── local/
│   │
│   ├── connectors/
│   │   ├── http/
│   │   ├── mcp/
│   │   ├── a2a/
│   │   └── fakeerp/
│   │
│   └── credentials/
│
├── api/
│   ├── proto/
│   └── openapi/
│
├── web/
│
├── migrations/
│
├── deployments/
│   ├── docker/
│   ├── helm/
│   └── kubernetes/
│
├── examples/
│   ├── procurement/
│   ├── finance/
│   └── coding-agent/
│
├── test/
│   ├── integration/
│   ├── concurrency/
│   ├── security/
│   ├── chaos/
│   └── load/
│
├── research/
│   └── REFERENCES.md
│
├── docs/
│   ├── architecture/
│   ├── adr/
│   ├── security/
│   ├── benchmarks/
│   └── MASTER_PLAN.md
│
├── AGENTS.md
├── CLAUDE.md
├── THIRD_PARTY_NOTICES.md
├── docker-compose.yml
└── README.md
```

---

# 72. Service Boundary

อย่าแตก microservice เร็วเกินไป

เริ่ม (Rev 2 — Slice A):

```text
Control Plane API
Worker
Postgres
Fake ERP        (demo target, คนละ network กับ agent)
```

Slice B เพิ่ม:

```text
Scheduler
NATS
AGT sidecar PDP
```

เมื่อ scale justify ค่อยแยก:

```text
Registry Service

FinOps Service

Dependency Service

Incident Service

Release Service
```

---

# 73. Phase 0 — Research & Architecture

ก่อนเขียน production code:

ศึกษา:

```text
Microsoft Agent Governance Toolkit   (pin version; ยืนยัน Python SDK / ACS API ที่ sidecar ใช้)
Agent Control Specification
Temporal
River
NATS JetStream
OpenTelemetry Collector
Kubernetes scheduler concepts
SPIFFE/SPIRE
RFC 8785 (JCS) Go implementations
PostgreSQL Row-Level Security
```

สร้าง:

```text
research/REFERENCES.md

docs/architecture/system-boundary.md
docs/architecture/control-plane.md
docs/architecture/governance-plane.md
docs/architecture/execution-plane.md
docs/architecture/failure-model.md
docs/security/THREAT_MODEL.md
```

**Rev 2 — Phase 0 gate:**

ห้ามเริ่ม Phase 1 จนกว่า ADR-001, ADR-002, ADR-004 และ ADR-005 จะ **Accepted**

ADR-004 (state machine) เป็น hard gate เพราะเป็น correctness boundary ของ approval, retry, reconciliation และ audit

---

# 74. Required ADRs

```text
ADR-001 Product Boundary & Enforcement Point         (Phase 0 gate)

ADR-002 AGT Integration Strategy (Sidecar PDP)        (Phase 0 gate)

ADR-003 Agent Registry Model

ADR-004 Action State Machine & Execution Semantics   (Phase 0 gate)

ADR-005 Approval Ownership & Atomic Execution Boundary (Phase 0 gate)

ADR-006 Delivery Semantics

ADR-007 Lease and Fencing

ADR-008 Idempotency

ADR-009 Unknown Outcome

ADR-010 Reconciliation

ADR-011 Scheduler Fairness

ADR-012 Budget Reservation

ADR-013 Connector Contract

ADR-014 PostgreSQL as Execution Authority; NATS for Signals

ADR-015 Dependency Graph

ADR-016 Distributed Kill Switch

ADR-017 Outcome Attestation

ADR-018 Release & Evaluation

ADR-019 Credential Custody                            (Rev 2)

ADR-020 Build vs Adopt Execution Engine (Temporal / River)  (Rev 2)

ADR-021 Tenant Isolation & Data Handling               (Rev 2)
```

ADR-007 ถึง ADR-010 ต้องสอดคล้องกับ ADR-004 ซึ่งเป็นตัวกำหนดหลัก

---

# 75. Phase 1 — Platform Foundation (Slice A)

**Rev 2 — Delivery slices:**

```text
Slice A — Correct, non-bypassable execution     Phase 1–8
Slice B — Enterprise governance & capacity       Phase 9–13
Slice C — Fleet safety                           Phase 14–17
Later   — Operations & ecosystem                 Phase 18–23
```

แต่ละ slice จบด้วย demo ที่ใช้ **เฉพาะ capability ที่มีอยู่ใน slice นั้นหรือก่อนหน้า**

Build:

```text
Go module + services (controlplane-api, execution-worker)
PostgreSQL + migration system
RLS scaffolding (tenant_id + policies ตั้งแต่ migration แรก)
configuration
structured logging (slog) + secret redaction
OpenTelemetry
health endpoints
graceful shutdown
docker-compose: postgres, api, worker, fakeerp
    network แยก: agent network ไม่มี route ไป fakeerp
```

ยังไม่ใช้ NATS ใน Slice A (§60)

---

# 76. Phase 2 — Registry, Identity & Capability (Slice A)

Build:

```text
Agent
AgentVersion
Owner
Environment
Risk class
Lifecycle (อย่างน้อย REGISTERED / ACTIVE / SUSPENDED / RETIRED)
Tool allowlist ต่อ AgentVersion

Agent identity / credential สำหรับเรียก EACP (Slice A: per-agent API key, hashed at rest)
Operator / approver principals + roles

Connector registry + operator-declared ConnectorContract (§31)
ConnectorSecretRef (secret อยู่ที่ worker เท่านั้น)
```

Enforce:

```text
production agent ที่ไม่มี owner → register/activate ไม่ได้ (§9)
capability check: tool ต้องอยู่ใน allowlist ของ ACTIVE AgentVersion
    → ถ้าไม่ผ่าน DENIED ก่อนถึง governance
tool ที่ไม่มี ConnectorContract → execute ไม่ได้
```

CLI:

```text
agent register
agent list
agent inspect
connector register
```

---

# 77. Phase 3 — Governance Interface, Local Provider & Approval Store (Slice A)

Implement:

```text
GovernanceProvider interface (ADR-002)
local provider: deterministic rules จาก versioned policy bundle
    verdicts: allow / warn / deny / escalate / transform
digest: JCS (RFC 8785) + SHA-256, input_digest + enforced_digest
decision evidence persistence
approval store (ADR-005): request, vote, grant, consume
approver eligibility + separation of duties + quorum + expiry
policy-version binding ของ grant
```

Tests:

```text
approval replay (consume สองครั้ง) → ครั้งที่สองล้มเหลว
parameter substitution → digest ไม่ตรง → ใช้ไม่ได้
transform-then-approve → bind กับ enforced payload
self-approval / cross-tenant approval → ถูกปฏิเสธ
policy เปลี่ยนหลัง grant → grant ใช้ไม่ได้
PDP error → fail closed
```

AGT sidecar **ยังไม่ทำ** ใน Phase นี้ ไปทำที่ Phase 9

---

# 78. Phase 4 — Action API & Atomic Boundary (Slice A)

Implement:

```text
POST /v1/actions (Idempotency-Key, ?wait=)
Action state machine ตาม ADR-004
Idempotency (tenant, agent, key) + 409 เมื่อ input_digest ต่างกัน
Release boundary (§15): revalidate → consume grant → QUEUED → journal → outbox
Budget reservation hook (no-op ใน Slice A)
Static admission limits (§26)
Hash-chained audit journal
Outbox table
```

---

# 79. Phase 5 — Worker, Lease, Fencing & Dispatch Intent (Slice A)

Implement:

```text
claim จาก Postgres (FOR UPDATE SKIP LOCKED, FIFO)
heartbeat
lease expire / reclaim ตามกฎ §22
generation fencing บน DB write ทุกตัว
fenced dispatch-intent commit ก่อน external call (§23.1)
late result evidence
credential custody: worker โหลด connector secret; agent ไม่มี
```

Concurrency tests บังคับ:

```text
lease race
stale worker commit ถูก reject
stale worker ไม่ทำให้เกิด dispatch ครั้งที่สอง (non-idempotent)
```

---

# 80. Phase 6 — Connector Framework & Fake ERP (Slice A)

Build:

```text
Connector interface: Execute(operation_key, enforced_payload) / Lookup(operation_key)
HTTP connector
Fake ERP (ต้องใช้ credential; รู้จัก operation key; มี lookup API)
```

Fake ERP supports:

```text
success
fail before execute
execute then timeout
execute then connection reset
slow response
429
5xx before effect
5xx after effect
outage
delayed visibility (record สร้างแล้วแต่ lookup ยังไม่เจอ)
```

---

# 81. Phase 7 — UNKNOWN_OUTCOME, Reconciliation & Human Resolution (Slice A)

Implement:

```text
UNKNOWN_OUTCOME จากทุกสาเหตุใน §19
reconciler lease
proof standard ต่อ connector (§20.2)
"not found" แบบ BEST_EFFORT → STILL_UNKNOWN (ไม่ retry)
NEEDS_HUMAN_RESOLUTION + operator resolve API/CLI (§20.3)
```

Flagship tests:

```text
Fake ERP executes → response หาย → UNKNOWN_OUTCOME → lookup เจอ → SUCCEEDED
Fake ERP executes → delayed visibility → lookup ไม่เจอ → ไม่ retry
    → NEEDS_HUMAN_RESOLUTION หรือเจอภายหลัง → SUCCEEDED
worker ถูก kill ระหว่าง EXECUTING → UNKNOWN_OUTCOME (ไม่ re-dispatch)
```

---

# 82. Phase 8 — Slice A Hardening & Demo

Build:

```text
bypass tests (§3.2): direct call ไป Fake ERP ล้มเหลว; ไม่มี secret รั่ว
tenant isolation tests กับ RLS
chaos: worker crash, DB restart, duplicate submission
race detector บน test suite ทั้งหมด
evidence reconstruction: จาก action_id ต้องได้ governance + approval + execution + outcome
Slice A demo script (§110)
```

Slice A exit criteria = ทุก invariant ใน §103 ที่ติดป้าย [A] มี automated test ที่ผ่าน

---

# 83. Phase 9 — AGT Sidecar PDP (Slice B)

Implement:

```text
sidecars/agt-pdp: Python service ห่อ AGT/ACS (pinned version)
    HTTP API: Evaluate → verdict + enforced payload + policy version + evidence
integrations/governance/microsoftagt: Go client ที่ implement GovernanceProvider
loopback / mTLS, timeout, fail closed
conformance tests: local provider vs AGT provider ต้องได้ verdict เดียวกันบน policy set อ้างอิง
```

ไม่มีการเปลี่ยน approval store หรือ action core (ADR-002)

---

# 84. Phase 10 — NATS JetStream (Slice B)

Implement:

```text
outbox → NATS relay
inbox dedup
work-available hints (action_id only)
event stream สำหรับ dashboards
```

Correctness ต้องไม่ขึ้นกับ NATS (§60)

---

# 85. Phase 11 — Budget Reservation (Slice B)

Atomic:

```text
reserve
commit
release
```

Concurrency test:

```text
100 concurrent reservations
```

must never oversubscribe hard budget

รวมถึง lock ordering, escrow, reservation TTL (§47) และวัด p99 ภายใต้ contention

---

# 86. Phase 12 — Fair Scheduler (Slice B)

Implement:

```text
tenant scheduling
team scheduling
priority
aging
connector capacity
```

Scheduler เลือกลำดับ claim ใน Postgres (§24)

Benchmark fairness

---

# 87. Phase 13 — Backpressure, Bulkheads, Circuit Breakers & Retry Budgets (Slice B)

```text
global / tenant / connector / queue / worker limits
bulkhead per connector group
circuit breaker (per-worker ก่อน + shared "connector disabled" flag)
backoff + jitter
retry budget
```

---

# 88. Phase 14 — MCP Registry & Fingerprint (Slice C)

```text
discovery
fingerprint
schema tracking
risk metadata
contract invalidation เมื่อ fingerprint เปลี่ยน
quarantine
```

---

# 89. Phase 15 — Dependency Graph & Blast Radius (Slice C)

Store:

```text
Agent → Model

Agent → MCP

MCP → Tool

Tool → System

Agent → Agent
```

Implement blast radius queries

Rev 2: edge มี source, freshness และ confidence

ถ้า edge unknown หรือ stale → ถือว่า blast radius **กว้างขึ้น** ไม่ใช่แคบลง

---

# 90. Phase 16 — Distributed Kill Switch (Slice C)

Integrate:

```text
AGT kill semantics
+
Control Plane distributed propagation
+
kill_epoch fenced check (§40)
```

---

# 91. Phase 17 — Fleet Operations (Slice C)

Build:

```text
fleet list
health
pause
resume
quarantine
rollback
```

---

# 92. Phase 18 — Agent FinOps (Later)

Build:

```text
cost telemetry (ingest; §45)
hierarchical budgets
chargeback
spend dashboard
anomaly alerts
```

---

# 93. Phase 19 — Release & Evaluation (Later)

Build:

```text
AgentRelease
Evaluation
Replay (research-grade)
Shadow (structurally non-destructive)
Canary
Rollback
```

---

# 94. Phase 20 — Agent SOC (Later)

Operator UI:

```text
Inventory

Fleet

Security

Approvals

Execution

Dependencies

Cost

Incidents
```

---

# 95. Phase 21 — Kubernetes / HA (Later)

After architecture stable:

```text
Helm

multiple API nodes

multiple scheduler nodes

worker autoscaling

Pod disruption

leader election where required
```

---

# 96. Phase 22 — JIT Credentials (Later)

Implement:

```text
CredentialProvider
```

then:

```text
SPIFFE
Cloud identities
Vault
```

Credential custody (agent ไม่มี credential) บังคับตั้งแต่ Slice A แล้ว

Phase นี้แค่เปลี่ยนจาก static secret เป็น short-lived credential

---

# 97. Phase 23 — A2A & LLM Gateway (Later, optional modules)

A2A: support governed remote Agent execution

```text
Agent A
 ↓
A2A
 ↓
Agent B
```

authority stays governed

execution remains observable

LLM Gateway (optional): ingress ใหม่ที่ใช้ shared core เดิม (§3.1)

```text
identity + capability + GovernanceProvider + audit + budget
```

---

# 98. Flagship Demo — Procurement

**Rev 2 — มีสองระดับ ระดับแรกต้องใช้เฉพาะ Slice A**

Slice A demo (Phase 8):

```text
Employee
 ↓
Procurement Agent            (ไม่มี credential ของ ERP และไม่มี network path ไป ERP)
 ↓
EACP Action API              (agent identity + capability allowlist)
 ↓
local GovernanceProvider     (policy bundle v1: PO > 1M THB → escalate)
 ↓
PENDING_APPROVAL             (Manager + Finance, SoD enforced, durable)
 ↓
Release boundary             (revalidate → consume grant → QUEUED)
 ↓
Worker                       (lease + fenced dispatch intent + operation key)
 ↓
Fake ERP                     (credential อยู่ที่ worker เท่านั้น)
 ↓
Create PO 2.4M THB
```

Full demo (หลัง Slice B/C):

```text
... → AGT / ACS sidecar → ... → Budget reserve → Fair scheduler → Worker → SAP MCP → ...
```

---

# 99. Failure Scenario

SAP (Slice A: Fake ERP):

```text
PO successfully created
```

but:

```text
network timeout
```

Control Plane:

```text
UNKNOWN_OUTCOME
 ↓
Reconcile
 ↓
ERP lookup by operation key (AUTHORITATIVE)
 ↓
PO-9822 exists (external_reference = operation key)
 ↓
CONFIRMED_SUCCESS
 ↓
SUCCEEDED + outcome evidence
```

**Rev 2 — Variant ที่ต้อง demo ด้วย:**

```text
Delayed visibility: PO สร้างแล้วแต่ lookup ยังไม่เจอ
 ↓
"not found" แต่ BEST_EFFORT → STILL_UNKNOWN → ไม่ retry
 ↓
lookup ครั้งต่อมาเจอ → SUCCEEDED
หรือ attempts หมด → NEEDS_HUMAN_RESOLUTION → operator resolve พร้อม evidence
```

```text
Kill worker mid-dispatch
 ↓
lease หมด ขณะ EXECUTING
 ↓
UNKNOWN_OUTCOME (ไม่ re-dispatch)
 ↓
reconcile → SUCCEEDED
 ↓
Fake ERP มี PO แค่ใบเดียว
```

---

# 100. Security Incident Demo (Slice C)

MCP fingerprint changes

```text
sap-mcp

fingerprint:
ABC → XYZ
```

Control Plane:

```text
Detect drift
 ↓
Connector contract invalidated
 ↓
Dependency Graph
 ↓
41 Agents affected
 ↓
Distributed kill / quarantine
 ↓
Block new execution
 ↓
Incident created
```

---

# 101. Release Demo (Later)

Agent:

```text
v14 production
v15 candidate
```

Pipeline:

```text
Replay
 ↓
Shadow
 ↓
Canary 5%
 ↓
Monitor SLO + cost + policy
 ↓
Promote
```

หรือ:

```text
Rollback
```

---

# 102. Testing Strategy

Unit:

```text
domain state machines (property-based: ไม่มี path ที่ผิด ADR-004)
scheduler
budget
retry
digest (JCS test vectors)
dependency graph
approval eligibility / SoD
```

Integration:

```text
PostgreSQL (รวม RLS)
NATS (Slice B)
AGT adapter (Slice B, conformance vs local provider)
Fake ERP
```

Concurrency:

```text
lease race
approval consumption
idempotency
budget race
stale worker dispatch
reclaim during EXECUTING
```

Security:

```text
tenant isolation
approval replay
parameter substitution
transform-then-approve
self-approval / cross-tenant approval
stale approval after policy change
control-plane bypass (direct ERP call)
MCP drift
credential leak (API, payload, journal, trace, log)
```

Chaos:

```text
worker crash
worker crash mid-dispatch
DB outage
NATS disconnect (Slice B)
PDP outage (fail closed; containment still works)
connector outage
duplicate message
duplicate submission
timeout after external commit
delayed visibility at external system
```

---

# 103. Critical Invariants

ป้าย [A]/[B]/[C] คือ slice ที่ invariant นั้นต้องมี automated test ที่ผ่าน

Invariant เดิม (Rev 1) คงไว้ทั้งหมด และเพิ่มคำอธิบายให้ชัดขึ้น:

```text
1. [A] A stale worker cannot commit.
       Rev 2: and cannot cause a second dispatch of a non-idempotent action (invariant 12).

2. [A] One-time approval cannot release two execution claims.

3. [B] Hard budgets cannot oversubscribe.

4. [A] Duplicate queue messages must not imply duplicate external effects.
       Rev 2: nor may duplicate submissions or worker reclaims.

5. [A] Timeout after dispatch does not automatically mean failure.

6. [A] Irreversible non-idempotent action is never blindly retried.

7. [A] Sensitive action governance is revalidated before execution.

8. [A] Tenant isolation cannot be bypassed.

9. [B] Connector failure cannot starve unrelated connector pools.

10. [A] Audit/evidence references remain reconstructible end-to-end.
```

เพิ่มใน Rev 2:

```text
11. [A] Agents never hold credentials for privileged external systems;
        privileged side effects are performed only by EACP-controlled workers or execution proxies.

12. [A] Once dispatch intent is recorded, an action is never re-dispatched unless
        (a) the connector is READ_ONLY or natively idempotent,
        (b) reconciliation produced connector-certified authoritative negative evidence, or
        (c) an authenticated human resolution authorised it —
        and every re-dispatch reuses the same operation key (ADR-004).

13. [A] "Not found" alone never authorizes retry of an irreversible or non-idempotent action;
        retry requires connector-certified authoritative negative evidence
        or an authenticated human resolution.

14. [A] An action executes only its enforced payload, whose digest matches
        the governance decision and approval grant it was released under.

15. [A] An approval grant is bound to tenant + action + enforced_digest + policy_version,
        expires, is consumed at most once,
        and cannot be granted by the requesting subject or the agent's owner.

16. [A] Approval and execution state are durable: a restart of any EACP process
        loses no pending approval, grant, or in-flight action state.

17. [A] Every state transition and privileged operator action is recorded
        in an append-only, hash-chained journal with actor and reason.

18. [A] Governance failure fails closed for new side-effecting actions,
        but never blocks cancellation, reconciliation reads, or containment.

19. [A] Every executed action is attributable to an authenticated agent identity
        and an ACTIVE AgentVersion whose capability allowlist includes the tool.
```

---

# 104. Load Benchmark

Workloads:

```text
100 agents

1,000 agents

5,000 agents

10,000 agents
```

Measure:

```text
Admission throughput

Scheduling throughput

Queue wait

Lease latency

Budget reservation

Idempotency latency

Execution overhead

CPU

RAM

DB utilization

NATS utilization
```

percentiles:

```text
P50
P95
P99
```

---

# 105. Never Fake Benchmarks

README ห้ามเขียน:

```text
100k actions/sec
```

จนกว่าจะวัดจริง

แยก:

```text
Control Plane overhead

Governance latency

External API latency

LLM latency
```

---

# 106. Development Rules for Codex / Claude Code

ทุก phase:

```text
READ MASTER PLAN

 ↓

READ relevant ADR

 ↓

INSPECT existing code

 ↓

STUDY relevant upstream reference

 ↓

STATE invariants

 ↓

WRITE tests

 ↓

IMPLEMENT

 ↓

RUN tests

 ↓

RUN race detector

 ↓

UPDATE docs
```

---

# 107. AI Development Rule

Codex/Claude ห้าม:

```text
invent API without verifying upstream

copy external project without attribution

disable test to make CI green

silently weaken fail-closed behavior

claim exactly-once without proof

implement next phase automatically

resolve an ambiguous safety/correctness assumption optimistically   (Rev 2: เลือก conservative ก่อนเสมอ)
```

---

# 108. Upstream Contribution Strategy

Execution Fabric / Control Plane เป็น independent project ก่อน

จากนั้น integration กับ AGT

เส้นทาง:

```text
Standalone Control Plane

 ↓

Working AGT integration

 ↓

Example

 ↓

Discussion with Microsoft maintainers

 ↓

Small upstream contribution

 ↓

Reusable integration

 ↓

Possible provider / protocol proposal
```

---

# 109. Potential Microsoft Contributions

ถ้า maintainers สนใจ:

```text
PostgreSQL ApprovalStore (Rev 2: สำคัญขึ้น เพราะ ACS stateless และ Go SDK ยังไม่มี approval chain — issue #3083)

Distributed execution integration example

Outcome attestation bridge

Execution provider abstraction

Conformance tests

Go parity fixes
```

อย่าเสนอ massive PR ก่อน discussion

---

# 110. MVP Definition

MVP ไม่ใช่ทั้งระบบ

**Rev 2 — MVP คือ Slice A ต่อด้วย Slice B และ C:**

## Slice A — Correct, non-bypassable execution (Phase 1–8)

Goal statement ที่ต้องพิสูจน์ได้ด้วย automated tests และ demo:

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

```text
Agent Registry (minimal) + agent identity + capability allowlist
Credential custody: secrets at worker only; network-isolated Fake ERP
GovernanceProvider interface + local provider (versioned policy bundles)
EACP-owned approval store (SoD, quorum, expiry, policy-version binding)
Action API (idempotency, ?wait=) + ADR-004 state machine
Atomic release boundary
PostgreSQL (RLS) as sole execution authority; outbox table
Worker: lease, heartbeat, generation fencing, fenced dispatch intent
Connector framework + Fake ERP (operation key, lookup, failure modes)
UNKNOWN_OUTCOME + reconciliation with proof standard
NEEDS_HUMAN_RESOLUTION + operator resolution
Hash-chained audit / evidence journal
OpenTelemetry (basic)
```

ไม่อยู่ใน Slice A: AGT sidecar, NATS, budget, fair scheduler, dependency graph, distributed kill switch, FinOps

## Slice B — Enterprise governance & capacity (Phase 9–13)

```text
AGT / ACS sidecar PDP
NATS JetStream (hints / events)
Hard budget reservation
Fair scheduler
Backpressure, bulkheads, circuit breakers, retry budgets
```

## Slice C — Fleet safety (Phase 14–17)

```text
MCP registry + fingerprint + contract invalidation
Dependency graph + blast radius
Distributed kill switch
Fleet operations
```

แต่ละ slice ต้องผ่าน invariant ที่ติดป้าย slice นั้นใน §103 ก่อนจึงเริ่ม slice ถัดไปได้

---

# 111. Portfolio-Ready Definition

**Rev 2 — แบ่งตาม slice โดย demo ของแต่ละ slice ใช้เฉพาะ capability ที่มีแล้วจริง**

Slice A demo:

```text
Register Agent (owner + tool allowlist)

Agent tries to call Fake ERP directly → fails (no credential, no route)

Agent requests tool outside allowlist → DENIED

Govern action (local provider)

Approve high-risk action (SoD enforced; self-approval rejected)

Restart API + worker while approval pending → approval still there

Execute through Go Fabric

Kill worker mid-dispatch → UNKNOWN_OUTCOME → reconcile → exactly one PO

Duplicate submission / reclaim → no duplicate external effect

Execute then timeout → reconcile outcome

Delayed visibility → no retry → human resolution with evidence

Show audit / evidence chain for one action_id (governance → approval → execution → outcome)
```

Slice B demo:

```text
Same flow through AGT sidecar
Budget race: 100 concurrent reservations, no oversubscription
Fairness under a flooding tenant
```

Slice C demo:

```text
Trigger MCP drift
Show blast radius
Kill affected Agent version
Show trace + audit evidence
```

---

# 112. Open-Source Ready Definition

```text
README

Architecture

ADRs

Threat model

Docker Compose

Examples

CI

Benchmarks

Contributor guide

License

Third-party attribution

Issue templates

Release process
```

---

# 113. Enterprise-Ready Direction

ภายหลัง:

```text
Kubernetes HA

SPIFFE

SSO

RBAC

SIEM integration

Multi-region

Data residency

Managed secrets

Disaster recovery

Enterprise database HA

Long-term audit retention

Compliance exports

LLM Gateway module (optional, §3.1)
```

---

# 114. Main Differentiator

อย่าแข่งขันว่า:

```text
เรามี policy มากกว่า AGT

เรามี Agent framework ดีกว่า LangGraph

เรามี workflow engine ดีกว่า Temporal
```

สิ่งที่ Control Plane ของเราควรเด่นคือ:

> **Unified enterprise operations for governed autonomous agents.**

รวม:

```text
Who owns the agent?

What can it do?

What does it depend on?

Should this action happen?

How should it execute?

Did it actually happen?

How much did it cost?

What else will break if we disable it?

How do we stop it immediately?
```

---

# 115. Product Story

Microsoft AGT:

```text
Governance primitives
```

Execution Fabric:

```text
Distributed execution correctness
```

Control Plane:

```text
Enterprise fleet management
+
security
+
operations
+
FinOps
+
release management
+
dependency intelligence
```

รวมกันเป็น:

```text
Enterprise Autonomous Agent Infrastructure
```

---

# 116. Final Architecture

```text
┌───────────────────────────────────────────────────────────────┐
│                    AGENT APPLICATIONS                         │
│                                                               │
│ OpenAI │ Claude │ LangGraph │ MAF │ AutoGen │ Custom         │
└─────────────────────────────┬─────────────────────────────────┘
                              │
                              ▼
╔═══════════════════════════════════════════════════════════════╗
║                ENTERPRISE AGENT CONTROL PLANE                ║
║                                                               ║
║  Registry                Dependency Graph                     ║
║  Fleet Management        Security Operations                 ║
║  FinOps                  Release Management                  ║
║  Evaluations             Incident Management                 ║
║  Operator UI             API / CLI                           ║
║  Agent Identity · Capability Allowlist · Approval Store      ║
║  Hash-chained Audit Journal                                  ║
╚═════════════════════════════╤═════════════════════════════════╝
                              │
                              ▼
┌───────────────────────────────────────────────────────────────┐
│                  GOVERNANCE FOUNDATION                        │
│                                                               │
│   GovernanceProvider ── Microsoft AGT / ACS (sidecar PDP)     │
│                                                               │
│ Policy │ Identity │ Trust │ Verdicts │ Context │ Audit        │
└─────────────────────────────┬─────────────────────────────────┘
                              │
                       Governance Decision
                              │
                              ▼
╔═══════════════════════════════════════════════════════════════╗
║             DISTRIBUTED EXECUTION FABRIC — GO                ║
║                                                               ║
║ Atomic Execution Boundary                                    ║
║ Fenced Dispatch Intent + Operation Key                       ║
║ Credential Custody (agents hold no enterprise credentials)   ║
║ Fair Scheduler                                               ║
║ Durable Queue                                                ║
║ Lease + Fencing                                              ║
║ Budget Reservation                                           ║
║ Idempotency                                                  ║
║ Retry Safety                                                 ║
║ UNKNOWN_OUTCOME                                              ║
║ Reconciliation (proof standard) + Human Resolution           ║
║ Backpressure / Bulkhead / Circuit Breaker                    ║
╚═════════════════════════════╤═════════════════════════════════╝
                              │
                              ▼
                   MCP / A2A / HTTP / gRPC
                              │
             ┌────────────────┼────────────────┐
             ▼                ▼                ▼
            SAP            Databases         GitHub
```

---

# 117. One-Sentence Positioning

> **Enterprise Agent Control Plane is an open infrastructure platform for governing, operating, securing, scheduling, and reliably executing autonomous AI agents across enterprise systems.**

---

# 118. Technical Positioning

> **Microsoft AGT governs agent actions; the Go execution fabric executes them safely; the Enterprise Agent Control Plane manages the entire agent fleet.**

---

# 119. Final Goal

ระยะเริ่มต้น:

```text
Strong Go portfolio project
```

ระยะกลาง:

```text
Real open-source project
```

ระยะยาว:

```text
Enterprise Agent Infrastructure Platform
```

หัวใจของระบบไม่ใช่การสร้าง Agent เพิ่มอีกตัว

แต่คือ:

> **สร้าง infrastructure ที่ทำให้องค์กรสามารถปล่อย Agent จำนวนมากเข้าสู่ production ได้โดยยังควบคุมความเสี่ยง การทำงาน ค่าใช้จ่าย และผลกระทบได้**

นั่นคือ Enterprise Agent Control Plane
