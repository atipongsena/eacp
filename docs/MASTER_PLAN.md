# Enterprise Agent Control Plane

## Master Development Plan

**Working name:** EACP
**Full name:** Enterprise Agent Control Plane
**Primary execution language:** Go
**Governance foundation:** Microsoft Agent Governance Toolkit / Agent Control Specification
**Architecture style:** Control Plane + Governance Plane + Execution Plane
**Target:** Enterprise AI Agent Infrastructure
**Status:** Master Architecture Plan
**Revision date:** 2026-09-23

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
│          Microsoft Agent Governance Toolkit / ACS            │
│                                                              │
│ Identity       Policy       Trust       Approval             │
│ Delegation     Context      Audit       Runtime Controls     │
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
internal/integrations/agt/
```

Core domain ห้าม depend กับ Microsoft-specific types โดยตรง

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

tools:
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
QUARANTINED
REVOKED
```

ทุก transition ต้องมีเหตุผลและ audit event

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

Interface concept:

```go
type GovernanceProvider interface {
    Evaluate(
        context.Context,
        GovernanceRequest,
    ) (GovernanceDecision, error)

    Revalidate(
        context.Context,
        RevalidationRequest,
    ) (GovernanceDecision, error)
}
```

Providers:

```text
MicrosoftAGT
OPA
Cedar
OpenFGA
Custom HTTP PDP
```

Microsoft AGT เป็น primary integration

---

# 13. Governance Context

Control Plane เก็บ references:

```text
agent_id
agent_version
subject_id
tenant_id

governance_provider
policy_decision_id
approval_request_id
approval_resolution_id

action_digest

trace_id
```

ไม่ duplicate governance engine state โดยไม่จำเป็น

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

---

# 15. Atomic Execution Boundary

นี่เป็นหัวใจสำคัญที่สุดของ Execution Plane

หลัง governance อนุญาตแล้ว Action ยังไม่ถือว่าเริ่ม execution

ต้องผ่าน atomic boundary

```text
BEGIN TRANSACTION

validate execution claim

consume one-time approval

claim idempotency key

reserve budget

create action

create execution journal

create outbox event

COMMIT
```

หลัง COMMIT เท่านั้น:

```text
Action is executable
```

ก่อน COMMIT:

```text
No worker should execute the action
```

---

# 16. Why Atomic Execution Boundary Matters

ป้องกัน:

```text
Approval ถูกใช้สองครั้ง

Worker race

Action ถูก queue แต่ DB ไม่มี record

Budget ถูกใช้เกินจาก race

Duplicate request สร้าง external effect ซ้ำ
```

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

```text
CREATED
   ↓
AUTHORIZED
   ↓
ADMITTED
   ↓
QUEUED
   ↓
LEASED
   ↓
PREPARING
   ↓
EXECUTING
   ↓
SUCCEEDED
```

Alternative:

```text
DENIED
CANCELLED
EXPIRED

RETRY_WAIT

UNKNOWN_OUTCOME
     ↓
RECONCILING

FAILED

DEAD_LETTER
```

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
→ retry may be allowed

STILL_UNKNOWN
→ retry reconciliation later

CONFLICT
→ human/operator intervention
```

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
AND lease_generation = $2;
```

generation 10 จะ update ไม่สำเร็จ

---

# 24. Fair Scheduler

ต้องรองรับหลาย tenant/team

```text
Team A     10,000 jobs
Team B        100 jobs
Team C        100 jobs
```

A ห้าม starvation B/C

MVP:

```text
Weighted Deficit Round Robin
+
Priority
+
Aging
```

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

side_effect: irreversible

idempotency:
  mode: native

reconciliation:
  supported: true

concurrency:
  group: sap-write
  max_inflight: 20

data:
  sensitivity: confidential
```

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

---

# 43. Outcome Attestation Bridge

AEF สร้าง verified outcome:

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
```

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

PolicyReference
GovernanceDecisionReference

ApprovalReference

BudgetAccount
BudgetReservation
UsageRecord

Lease

ExecutionJournal

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
Budgets
Outbox
Dependencies
Deployments
Kill state
Incidents
```

---

# 60. Messaging

Use:

```text
NATS JetStream
```

for:

```text
action dispatch
execution events
kill propagation
registry updates
health events
reconciliation jobs
```

Assume:

```text
messages may duplicate
```

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

INSERT action;

INSERT outbox_event;

COMMIT;
```

Outbox publisher:

```text
Postgres
 ↓
NATS
```

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

POST /v1/actions

GET /v1/actions/{id}

POST /v1/actions/{id}/cancel

POST /v1/actions/{id}/reconcile

GET /v1/dependencies/blast-radius

POST /v1/killswitch

GET /v1/incidents

GET /v1/budgets

GET /v1/fleet/health
```

---

# 66. CLI

```text
eacp agent list

eacp agent inspect procurement-agent

eacp action inspect act_123

eacp action reconcile act_123

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
```

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
```

---

# 70. Credential Architecture

อย่าส่ง static secret เข้า Agent โดยตรงถ้าเลี่ยงได้

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
│   ├── scheduler/
│   ├── event-worker/
│   └── eacpctl/
│
├── internal/
│   ├── registry/
│   ├── lifecycle/
│   ├── governance/
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

เริ่ม:

```text
Control Plane API
Worker
Scheduler
Postgres
NATS
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
Microsoft Agent Governance Toolkit

Agent Control Specification

Temporal

River

NATS JetStream

OpenTelemetry Collector

Kubernetes scheduler concepts

SPIFFE/SPIRE
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

---

# 74. Required ADRs

```text
ADR-001 Product Boundary

ADR-002 AGT Integration Strategy

ADR-003 Agent Registry Model

ADR-004 Action State Machine

ADR-005 Atomic Execution Boundary

ADR-006 Delivery Semantics

ADR-007 Lease and Fencing

ADR-008 Idempotency

ADR-009 Unknown Outcome

ADR-010 Reconciliation

ADR-011 Scheduler Fairness

ADR-012 Budget Reservation

ADR-013 Connector Contract

ADR-014 PostgreSQL + NATS

ADR-015 Dependency Graph

ADR-016 Distributed Kill Switch

ADR-017 Outcome Attestation

ADR-018 Release & Evaluation
```

---

# 75. Phase 1 — Platform Foundation

Build:

```text
Go services
PostgreSQL
NATS
configuration
logging
OpenTelemetry
migration system
health endpoints
graceful shutdown
```

---

# 76. Phase 2 — Agent Registry

Build:

```text
Agent
AgentVersion
Owner
Environment
Risk class
Dependencies
Lifecycle
```

CLI:

```text
agent register
agent list
agent inspect
```

---

# 77. Phase 3 — AGT Integration

Implement:

```text
MicrosoftAGTProvider
```

support:

```text
governance decision
action digest
approval reference
revalidation
trace/evidence references
```

Compatibility code isolated

---

# 78. Phase 4 — Action & Atomic Boundary

Implement:

```text
Action
State machine
Idempotency
Approval consume integration
Budget reservation stub
Outbox
```

---

# 79. Phase 5 — Durable Queue

Implement:

```text
Transactional outbox
NATS
Inbox dedup
Worker delivery
```

---

# 80. Phase 6 — Lease & Fencing

Implement:

```text
claim
heartbeat
expire
reclaim
generation fencing
```

Concurrency tests mandatory

---

# 81. Phase 7 — Fair Scheduler

Implement:

```text
tenant scheduling
team scheduling
priority
aging
connector capacity
```

Benchmark fairness

---

# 82. Phase 8 — Connector Framework

Build:

```text
HTTP
Fake ERP
```

Fake ERP supports:

```text
success
fail before execute
execute then timeout
slow response
429
5xx
outage
```

---

# 83. Phase 9 — UNKNOWN_OUTCOME

Implement ambiguous state

flagship test:

```text
Fake ERP executes
then response disappears
```

Expected:

```text
UNKNOWN_OUTCOME
```

---

# 84. Phase 10 — Reconciliation

```text
UNKNOWN
 ↓
reconcile
 ↓
external record found
 ↓
SUCCEEDED
```

---

# 85. Phase 11 — Budget Reservation

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

---

# 86. Phase 12 — Backpressure & Bulkheads

```text
global limits
tenant limits
connector limits
queue limits
worker limits
```

---

# 87. Phase 13 — Circuit Breakers & Retry Budgets

Implement:

```text
breaker
backoff
jitter
retry budget
```

---

# 88. Phase 14 — MCP Registry

```text
discovery
fingerprint
schema tracking
risk metadata
quarantine
```

---

# 89. Phase 15 — Dependency Graph

Store:

```text
Agent → Model

Agent → MCP

MCP → Tool

Tool → System

Agent → Agent
```

Implement blast radius queries

---

# 90. Phase 16 — Distributed Kill Switch

Integrate:

```text
AGT kill semantics
+
Control Plane distributed propagation
```

---

# 91. Phase 17 — Fleet Operations

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

# 92. Phase 18 — Agent FinOps

Build:

```text
cost telemetry
hierarchical budgets
chargeback
spend dashboard
anomaly alerts
```

---

# 93. Phase 19 — Release & Evaluation

Build:

```text
AgentRelease
Evaluation
Replay
Shadow
Canary
Rollback
```

---

# 94. Phase 20 — Agent SOC

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

# 95. Phase 21 — Kubernetes / HA

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

# 96. Phase 22 — JIT Credentials

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

---

# 97. Phase 23 — A2A

Support governed remote Agent execution

```text
Agent A
 ↓
A2A
 ↓
Agent B
```

authority stays governed

execution remains observable

---

# 98. Flagship Demo — Procurement

```text
Employee
 ↓
Procurement Agent
 ↓
AGT / ACS
 ↓
Requires Manager + Finance Approval
 ↓
Atomic Execution Boundary
 ↓
Budget reserve
 ↓
Scheduler
 ↓
Worker
 ↓
SAP MCP
 ↓
Create PO 2.4M THB
```

---

# 99. Failure Scenario

SAP:

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
SAP query
 ↓
PO-9822 exists
 ↓
CONFIRMED_SUCCESS
 ↓
Outcome attestation
```

---

# 100. Security Incident Demo

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

# 101. Release Demo

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
domain state machines
scheduler
budget
retry
digest
dependency graph
```

Integration:

```text
PostgreSQL
NATS
AGT adapter
Fake ERP
```

Concurrency:

```text
lease race
approval consumption
idempotency
budget race
```

Security:

```text
tenant isolation
approval replay
parameter substitution
MCP drift
credential leak
```

Chaos:

```text
worker crash
DB outage
NATS disconnect
connector outage
duplicate message
timeout after external commit
```

---

# 103. Critical Invariants

```text
1. A stale worker cannot commit.

2. One-time approval cannot release two execution claims.

3. Hard budgets cannot oversubscribe.

4. Duplicate queue messages must not imply duplicate external effects.

5. Timeout after dispatch does not automatically mean failure.

6. Irreversible non-idempotent action is never blindly retried.

7. Sensitive action governance is revalidated before execution.

8. Tenant isolation cannot be bypassed.

9. Connector failure cannot starve unrelated connector pools.

10. Audit/evidence references remain reconstructible end-to-end.
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
```

---

# 108. Upstream Contribution Strategy

AEF / Control Plane เป็น independent project ก่อน

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
PostgreSQL ApprovalStore

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

MVP:

```text
Agent Registry

AGT integration

Action API

Atomic Execution Boundary

PostgreSQL

NATS

Worker

Lease

Fencing

Fair scheduler

Idempotency

Fake ERP

UNKNOWN_OUTCOME

Reconciliation

Budget reservation

OpenTelemetry

Basic dependency graph

Basic kill switch propagation
```

---

# 111. Portfolio-Ready Definition

ต้อง demo ได้:

```text
Register Agent

Govern action

Approve high-risk action

Execute through Go Fabric

Kill worker

Recover lease

Duplicate message

Prevent duplicate execution

Execute then timeout

Reconcile outcome

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

AEF:

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
╚═════════════════════════════╤═════════════════════════════════╝
                              │
                              ▼
┌───────────────────────────────────────────────────────────────┐
│                  GOVERNANCE FOUNDATION                        │
│                                                               │
│                Microsoft AGT / ACS                            │
│                                                               │
│ Policy │ Identity │ Trust │ Approval │ Context │ Audit       │
└─────────────────────────────┬─────────────────────────────────┘
                              │
                       Governance Decision
                              │
                              ▼
╔═══════════════════════════════════════════════════════════════╗
║             DISTRIBUTED EXECUTION FABRIC — GO                ║
║                                                               ║
║ Atomic Execution Boundary                                    ║
║ Fair Scheduler                                               ║
║ Durable Queue                                                ║
║ Lease + Fencing                                              ║
║ Budget Reservation                                           ║
║ Idempotency                                                  ║
║ Retry Safety                                                 ║
║ UNKNOWN_OUTCOME                                              ║
║ Reconciliation                                               ║
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
