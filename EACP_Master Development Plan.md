\# Enterprise Agent Control Plane



\## Master Development Plan



\*\*Working name:\*\* EACP

\*\*Full name:\*\* Enterprise Agent Control Plane

\*\*Primary execution language:\*\* Go

\*\*Governance foundation:\*\* Microsoft Agent Governance Toolkit / Agent Control Specification

\*\*Architecture style:\*\* Control Plane + Governance Plane + Execution Plane

\*\*Target:\*\* Enterprise AI Agent Infrastructure

\*\*Status:\*\* Master Architecture Plan

\*\*Revision date:\*\* 2026-09-23



\---



\# 1. Vision



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



\---



\# 2. Problem



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



\---



\# 3. Core Principle



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



&#x20;       │

&#x20;       ▼



GOVERNANCE PLANE



Who is this?

Should this action be allowed?

What policy applies?

Does it require approval?

What restrictions apply?



&#x20;       │

&#x20;       ▼



EXECUTION PLANE



How do we execute it correctly?

Which worker runs it?

How do we avoid duplicate effects?

What happens on failure?

How do we recover?

```



\---



\# 4. Relationship With Microsoft AGT



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

&#x20;                    Enterprise Agent Control Plane



┌──────────────────────────────────────────────────────────────┐

│                       CONTROL PLANE                          │

│                                                              │

│ Agent Registry      Dependency Graph      Fleet Management   │

│ Release Manager     Evaluation            Agent SOC          │

│ FinOps              Operator UI           API / CLI          │

└──────────────────────────────┬───────────────────────────────┘

&#x20;                              │

&#x20;                              ▼

┌──────────────────────────────────────────────────────────────┐

│                     GOVERNANCE PLANE                         │

│                                                              │

│          Microsoft Agent Governance Toolkit / ACS            │

│                                                              │

│ Identity       Policy       Trust       Approval             │

│ Delegation     Context      Audit       Runtime Controls     │

└──────────────────────────────┬───────────────────────────────┘

&#x20;                              │

&#x20;                      Governance Decision

&#x20;                              │

&#x20;                              ▼

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

&#x20;                              │

&#x20;                              ▼

&#x20;                MCP / A2A / HTTP / gRPC

&#x20;                              │

&#x20;             ┌────────────────┼────────────────┐

&#x20;             ▼                ▼                ▼

&#x20;            SAP              DB              GitHub

```



\---



\# 5. Extension-First Strategy



ไม่ fork Microsoft AGT ทั้ง repo เป็นค่าเริ่มต้น



ใช้:



```text

Microsoft AGT

&#x20;     │

&#x20;     │ public contracts

&#x20;     ▼

Compatibility / Integration Layer

&#x20;     │

&#x20;     ▼

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



\---



\# 6. Product Modules



ระบบใหญ่แบ่งเป็น 10 modules



```text

1\. Agent Registry

2\. Governance Integration

3\. Distributed Execution Fabric

4\. Tool / Connector Registry

5\. Dependency \& Blast Radius Graph

6\. Agent Fleet Operations

7\. Agent SRE / Observability

8\. Agent FinOps

9\. Release / Evaluation Platform

10\. Agent Security Operations Center

```



\---



\# 7. Module 1 — Agent Registry



Agent Registry เป็น CMDB สำหรับ AI Agents



ทุก production agent ต้องมี Passport



ตัวอย่าง:



```yaml

agent\_id: procurement-agent

version: 14



owner:

&#x20; team: procurement-ai

&#x20; email: procurement-ai@example.com



environment: production



runtime:

&#x20; framework: langgraph

&#x20; language: python



models:

&#x20; - claude

&#x20; - gpt



tools:

&#x20; - supplier\_search

&#x20; - sap.create\_po

&#x20; - email.send



data\_classes:

&#x20; - internal

&#x20; - confidential



risk\_class: high



governance:

&#x20; provider: microsoft-agt

&#x20; policy\_set: procurement-production



lifecycle:

&#x20; status: active

&#x20; created\_at: ...

&#x20; expires\_at: ...

```



\---



\# 8. Agent Lifecycle



Agent state:



```text

DRAFT

&#x20; ↓

REGISTERED

&#x20; ↓

CERTIFIED

&#x20; ↓

STAGING

&#x20; ↓

CANARY

&#x20; ↓

ACTIVE

&#x20; ↓

DEPRECATED

&#x20; ↓

RETIRED

```



Alternative:



```text

QUARANTINED

REVOKED

```



ทุก transition ต้องมีเหตุผลและ audit event



\---



\# 9. Ownership Requirement



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



\---



\# 10. Agent Versioning



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



\---



\# 11. Agent Bill of Materials



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

&#x20;   ├── supplier-db

&#x20;   └── finance-api

```



เป้าหมาย:



```text

Reproducibility

Security

Audit

Blast radius

Release comparison

```



\---



\# 12. Module 2 — Governance Integration



Microsoft AGT / ACS เป็น default governance provider



แต่ Control Plane ต้องไม่ lock-in



Interface concept:



```go

type GovernanceProvider interface {

&#x20;   Evaluate(

&#x20;       context.Context,

&#x20;       GovernanceRequest,

&#x20;   ) (GovernanceDecision, error)



&#x20;   Revalidate(

&#x20;       context.Context,

&#x20;       RevalidationRequest,

&#x20;   ) (GovernanceDecision, error)

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



\---



\# 13. Governance Context



Control Plane เก็บ references:



```text

agent\_id

agent\_version

subject\_id

tenant\_id



governance\_provider

policy\_decision\_id

approval\_request\_id

approval\_resolution\_id



action\_digest



trace\_id

```



ไม่ duplicate governance engine state โดยไม่จำเป็น



\---



\# 14. Action-Bound Approval



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

&#x20;    ↓

RFC 8785 JCS

&#x20;    ↓

SHA-256

&#x20;    ↓

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



\---



\# 15. Atomic Execution Boundary



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



\---



\# 16. Why Atomic Execution Boundary Matters



ป้องกัน:



```text

Approval ถูกใช้สองครั้ง



Worker race



Action ถูก queue แต่ DB ไม่มี record



Budget ถูกใช้เกินจาก race



Duplicate request สร้าง external effect ซ้ำ

```



\---



\# 17. Module 3 — Distributed Execution Fabric



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



\---



\# 18. Action State Machine



```text

CREATED

&#x20;  ↓

AUTHORIZED

&#x20;  ↓

ADMITTED

&#x20;  ↓

QUEUED

&#x20;  ↓

LEASED

&#x20;  ↓

PREPARING

&#x20;  ↓

EXECUTING

&#x20;  ↓

SUCCEEDED

```



Alternative:



```text

DENIED

CANCELLED

EXPIRED



RETRY\_WAIT



UNKNOWN\_OUTCOME

&#x20;    ↓

RECONCILING



FAILED



DEAD\_LETTER

```



\---



\# 19. First-Class UNKNOWN\_OUTCOME



ห้ามถือว่า:



```text

timeout = failure

```



ตัวอย่าง:



```text

Agent Control Plane

&#x20;     │

&#x20;     │ create PO

&#x20;     ▼

&#x20;    SAP



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

&#x20;   ↓

timeout after dispatch

&#x20;   ↓

UNKNOWN\_OUTCOME

```



ไม่ใช่:



```text

FAILED

```



\---



\# 20. Reconciliation Engine



```text

UNKNOWN\_OUTCOME

&#x20;      ↓

Reconciliation Worker

&#x20;      ↓

Check external world state

&#x20;      ↓

```



ผล:



```text

CONFIRMED\_SUCCESS

CONFIRMED\_NOT\_EXECUTED

STILL\_UNKNOWN

CONFLICT

```



จากนั้น:



```text

CONFIRMED\_SUCCESS

→ SUCCEEDED



CONFIRMED\_NOT\_EXECUTED

→ retry may be allowed



STILL\_UNKNOWN

→ retry reconciliation later



CONFLICT

→ human/operator intervention

```



\---



\# 21. No Fake Exactly-Once



ห้ามเขียน marketing ว่า:



> Exactly-once execution across every external system



ใช้ terminology:



```text

Idempotent where supported



Effectively-once where reconcilable



At-most-once when retry is unsafe

```



\---



\# 22. Execution Lease



Worker ต้อง acquire lease



```text

action\_id

worker\_id

lease\_generation

leased\_until

heartbeat\_at

```



Worker crash:



```text

heartbeat stops

&#x20;↓

lease expires

&#x20;↓

another worker may claim

```



\---



\# 23. Fencing Token



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

AND lease\_generation = $2;

```



generation 10 จะ update ไม่สำเร็จ



\---



\# 24. Fair Scheduler



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

\+

Priority

\+

Aging

```



\---



\# 25. Scheduling Dimensions



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



\---



\# 26. Backpressure



เมื่อ capacity เต็ม:



```text

Agent

&#x20;↓

Admission

&#x20;↓



ACCEPT



QUEUE



THROTTLE



REJECT

```



ห้าม queue แบบ unbounded



\---



\# 27. Bulkhead Isolation



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



\---



\# 28. Circuit Breaker



```text

CLOSED

&#x20;↓ failures

OPEN

&#x20;↓ cooldown

HALF\_OPEN

&#x20;↓ probe success

CLOSED

```



scope:



```text

provider

connector

endpoint

tool

```



\---



\# 29. Retry Safety



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



\---



\# 30. Retry Budget



มี:



```text

max\_attempts

max\_elapsed\_time

max\_retry\_cost

```



เพื่อหยุด retry storm



\---



\# 31. Module 4 — Tool / Connector Registry



ทุก external capability ต้อง register



ตัวอย่าง:



```yaml

connector: sap-production

tool: create\_po



protocol: mcp



side\_effect: irreversible



idempotency:

&#x20; mode: native



reconciliation:

&#x20; supported: true



concurrency:

&#x20; group: sap-write

&#x20; max\_inflight: 20



data:

&#x20; sensitivity: confidential

```



\---



\# 32. Side-Effect Classification



```text

READ\_ONLY



REVERSIBLE\_WRITE



IRREVERSIBLE\_WRITE



EXTERNAL\_COMMUNICATION



FINANCIAL



ADMINISTRATIVE

```



หนึ่ง action อาจมีหลาย tag



\---



\# 33. Tool Fingerprint



Tool definition ต้อง fingerprint



```text

Tool Schema

&#x20;+

Metadata

&#x20;+

Security attributes

&#x20;   ↓

Canonical form

&#x20;   ↓

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



\---



\# 34. Tool Certification



Lifecycle:



```text

DISCOVERED

&#x20;  ↓

SCANNED

&#x20;  ↓

VALIDATED

&#x20;  ↓

CERTIFIED

&#x20;  ↓

ACTIVE

```



Alternative:



```text

QUARANTINED

REVOKED

```



\---



\# 35. Module 5 — Dependency Graph



สร้าง graph:



```text

Agent

&#x20;↓

Model

&#x20;↓

MCP Server

&#x20;↓

Tool

&#x20;↓

Application

&#x20;↓

Dataset

```



อีกแบบ:



```text

Agent A

&#x20;↓ delegates

Agent B

&#x20;↓ uses

MCP X

&#x20;↓ accesses

Database Y

```



\---



\# 36. Blast Radius



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



\---



\# 37. Initial Graph Storage



MVP:



```text

PostgreSQL

\+

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



\---



\# 38. Module 6 — Fleet Operations



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



\---



\# 39. Distributed Kill Switch



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



\---



\# 40. Kill Propagation



```text

Operator

&#x20;↓

Control Plane

&#x20;↓

Postgres authoritative kill state

&#x20;↓

NATS event

&#x20;↓

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



\---



\# 41. Module 7 — Agent SRE \& Observability



ใช้ AGT SRE + OpenTelemetry



Control Plane เพิ่ม fleet-level view



Trace:



```text

User Request

&#x20; ↓

Agent Run

&#x20; ↓

Governance Evaluation

&#x20; ↓

Approval

&#x20; ↓

Admission

&#x20; ↓

Queue Wait

&#x20; ↓

Worker Lease

&#x20; ↓

Tool Execution

&#x20; ↓

Reconciliation

&#x20; ↓

Outcome

```



\---



\# 42. Execution Evidence vs Governance Evidence



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

action\_id

decision\_id

trace\_id

```



\---



\# 43. Outcome Attestation Bridge



AEF สร้าง verified outcome:



```text

CONFIRMED\_SUCCESS

CONFIRMED\_FAILURE

UNKNOWN

CONFLICT

```



แล้ว bridge กลับไป Governance/Audit plane



```text

Execution Evidence

&#x20;      ↓

Outcome Attestation

&#x20;      ↓

AGT / Audit / SIEM

```



\---



\# 44. Key Metrics



```text

actions\_received\_total



actions\_authorized\_total



actions\_denied\_total



actions\_executed\_total



actions\_unknown\_total



actions\_reconciled\_total



queue\_depth



queue\_wait\_seconds



active\_workers



lease\_expiration\_total



stale\_commit\_rejected\_total



retry\_total



connector\_error\_rate



budget\_reserved



budget\_committed



governance\_latency



execution\_overhead

```



\---



\# 45. Module 8 — Agent FinOps



รวม:



```text

Cost observation

Budget

Reservation

Quota

Forecast

Chargeback

```



\---



\# 46. Hierarchical Budget



```text

Organization

&#x20;     ↓

Business Unit

&#x20;     ↓

Department

&#x20;     ↓

Team

&#x20;     ↓

Agent

&#x20;     ↓

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



\---



\# 47. Strict Budget Reservation



```text

RESERVE estimate

&#x20;      ↓

EXECUTE

&#x20;      ↓

OBSERVE actual

&#x20;      ↓

COMMIT actual

&#x20;      ↓

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



\---



\# 48. Chargeback



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



\---



\# 49. Module 9 — Release \& Evaluation Platform



Agent production deployment ต้องผ่าน pipeline



```text

Code

&#x20;↓

Unit tests

&#x20;↓

Policy tests

&#x20;↓

Security tests

&#x20;↓

Agent evaluations

&#x20;↓

Replay

&#x20;↓

Shadow

&#x20;↓

Canary

&#x20;↓

Production

```



\---



\# 50. Agent Certification Pipeline



```text

REGISTER

&#x20;↓

BUILD

&#x20;↓

STATIC CHECK

&#x20;↓

POLICY TEST

&#x20;↓

SECURITY SCAN

&#x20;↓

EVALUATION

&#x20;↓

HUMAN REVIEW

&#x20;↓

CERTIFIED

```



\---



\# 51. Replay



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



\---



\# 52. Shadow Deployment



```text

Production request

&#x20;      │

&#x20;      ├─────────────► v14 real execution

&#x20;      │

&#x20;      └─────────────► v15 shadow

```



v15:



```text

cannot perform destructive effects

```



เก็บเฉพาะ decision/result comparison



\---



\# 53. Canary



Deployment progression:



```text

1%

&#x20;↓

5%

&#x20;↓

25%

&#x20;↓

50%

&#x20;↓

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



\---



\# 54. Module 10 — Agent Security Operations Center



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



\---



\# 55. Agent SOC Dashboard



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



\---



\# 56. Incident View



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



\---



\# 57. Human-In-The-Loop



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



\---



\# 58. Core Domain Entities



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



\---



\# 59. Storage Architecture



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



\---



\# 60. Messaging



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



\---



\# 61. Cache



Redis optional



Use for:



```text

hot registry cache

policy reference cache

rate limits where appropriate

dashboard acceleration

```



Redis ไม่ใช่ source of truth สำหรับ correctness-critical execution state



\---



\# 62. Transactional Outbox



```sql

BEGIN;



INSERT action;



INSERT outbox\_event;



COMMIT;

```



Outbox publisher:



```text

Postgres

&#x20;↓

NATS

```



ป้องกัน:



```text

DB committed

but process crashed before publish

```



\---



\# 63. Inbox / Dedup



Consumer เก็บ:



```text

message\_id

consumer

processed\_at

```



duplicate:



```text

ACK

do not execute again

```



\---



\# 64. API Layer



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



\---



\# 65. Example APIs



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



\---



\# 66. CLI



```text

eacp agent list



eacp agent inspect procurement-agent



eacp action inspect act\_123



eacp action reconcile act\_123



eacp dependency blast-radius sap-mcp



eacp kill agent procurement-agent:v14



eacp connector disable sap-production



eacp budget inspect procurement



eacp fleet status

```



\---



\# 67. Security Architecture



Zero trust assumption:



```text

Agent is untrusted



LLM output is untrusted



Tool metadata is untrusted



MCP server is untrusted



Queue input is untrusted



External API output is untrusted

```



\---



\# 68. Threat Model



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



\---



\# 69. Tenant Isolation



ทุก record critical มี:



```text

tenant\_id

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



\---



\# 70. Credential Architecture



อย่าส่ง static secret เข้า Agent โดยตรงถ้าเลี่ยงได้



```text

Agent

&#x20;↓

Action

&#x20;↓

Execution Worker

&#x20;↓

Credential Provider

&#x20;↓

short-lived credential

&#x20;↓

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



\---



\# 71. Repository Structure



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

│   └── MASTER\_PLAN.md

│

├── AGENTS.md

├── CLAUDE.md

├── THIRD\_PARTY\_NOTICES.md

├── docker-compose.yml

└── README.md

```



\---



\# 72. Service Boundary



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



\---



\# 73. Phase 0 — Research \& Architecture



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

docs/security/THREAT\_MODEL.md

```



\---



\# 74. Required ADRs



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



ADR-018 Release \& Evaluation

```



\---



\# 75. Phase 1 — Platform Foundation



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



\---



\# 76. Phase 2 — Agent Registry



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



\---



\# 77. Phase 3 — AGT Integration



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



\---



\# 78. Phase 4 — Action \& Atomic Boundary



Implement:



```text

Action

State machine

Idempotency

Approval consume integration

Budget reservation stub

Outbox

```



\---



\# 79. Phase 5 — Durable Queue



Implement:



```text

Transactional outbox

NATS

Inbox dedup

Worker delivery

```



\---



\# 80. Phase 6 — Lease \& Fencing



Implement:



```text

claim

heartbeat

expire

reclaim

generation fencing

```



Concurrency tests mandatory



\---



\# 81. Phase 7 — Fair Scheduler



Implement:



```text

tenant scheduling

team scheduling

priority

aging

connector capacity

```



Benchmark fairness



\---



\# 82. Phase 8 — Connector Framework



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



\---



\# 83. Phase 9 — UNKNOWN\_OUTCOME



Implement ambiguous state



flagship test:



```text

Fake ERP executes

then response disappears

```



Expected:



```text

UNKNOWN\_OUTCOME

```



\---



\# 84. Phase 10 — Reconciliation



```text

UNKNOWN

&#x20;↓

reconcile

&#x20;↓

external record found

&#x20;↓

SUCCEEDED

```



\---



\# 85. Phase 11 — Budget Reservation



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



\---



\# 86. Phase 12 — Backpressure \& Bulkheads



```text

global limits

tenant limits

connector limits

queue limits

worker limits

```



\---



\# 87. Phase 13 — Circuit Breakers \& Retry Budgets



Implement:



```text

breaker

backoff

jitter

retry budget

```



\---



\# 88. Phase 14 — MCP Registry



```text

discovery

fingerprint

schema tracking

risk metadata

quarantine

```



\---



\# 89. Phase 15 — Dependency Graph



Store:



```text

Agent → Model



Agent → MCP



MCP → Tool



Tool → System



Agent → Agent

```



Implement blast radius queries



\---



\# 90. Phase 16 — Distributed Kill Switch



Integrate:



```text

AGT kill semantics

\+

Control Plane distributed propagation

```



\---



\# 91. Phase 17 — Fleet Operations



Build:



```text

fleet list

health

pause

resume

quarantine

rollback

```



\---



\# 92. Phase 18 — Agent FinOps



Build:



```text

cost telemetry

hierarchical budgets

chargeback

spend dashboard

anomaly alerts

```



\---



\# 93. Phase 19 — Release \& Evaluation



Build:



```text

AgentRelease

Evaluation

Replay

Shadow

Canary

Rollback

```



\---



\# 94. Phase 20 — Agent SOC



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



\---



\# 95. Phase 21 — Kubernetes / HA



After architecture stable:



```text

Helm



multiple API nodes



multiple scheduler nodes



worker autoscaling



Pod disruption



leader election where required

```



\---



\# 96. Phase 22 — JIT Credentials



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



\---



\# 97. Phase 23 — A2A



Support governed remote Agent execution



```text

Agent A

&#x20;↓

A2A

&#x20;↓

Agent B

```



authority stays governed



execution remains observable



\---



\# 98. Flagship Demo — Procurement



```text

Employee

&#x20;↓

Procurement Agent

&#x20;↓

AGT / ACS

&#x20;↓

Requires Manager + Finance Approval

&#x20;↓

Atomic Execution Boundary

&#x20;↓

Budget reserve

&#x20;↓

Scheduler

&#x20;↓

Worker

&#x20;↓

SAP MCP

&#x20;↓

Create PO 2.4M THB

```



\---



\# 99. Failure Scenario



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

UNKNOWN\_OUTCOME

&#x20;↓

Reconcile

&#x20;↓

SAP query

&#x20;↓

PO-9822 exists

&#x20;↓

CONFIRMED\_SUCCESS

&#x20;↓

Outcome attestation

```



\---



\# 100. Security Incident Demo



MCP fingerprint changes



```text

sap-mcp



fingerprint:

ABC → XYZ

```



Control Plane:



```text

Detect drift



&#x20;↓



Dependency Graph



&#x20;↓



41 Agents affected



&#x20;↓



Distributed kill / quarantine



&#x20;↓



Block new execution



&#x20;↓



Incident created

```



\---



\# 101. Release Demo



Agent:



```text

v14 production

v15 candidate

```



Pipeline:



```text

Replay



&#x20;↓



Shadow



&#x20;↓



Canary 5%



&#x20;↓



Monitor SLO + cost + policy



&#x20;↓



Promote

```



หรือ:



```text

Rollback

```



\---



\# 102. Testing Strategy



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



\---



\# 103. Critical Invariants



```text

1\. A stale worker cannot commit.



2\. One-time approval cannot release two execution claims.



3\. Hard budgets cannot oversubscribe.



4\. Duplicate queue messages must not imply duplicate external effects.



5\. Timeout after dispatch does not automatically mean failure.



6\. Irreversible non-idempotent action is never blindly retried.



7\. Sensitive action governance is revalidated before execution.



8\. Tenant isolation cannot be bypassed.



9\. Connector failure cannot starve unrelated connector pools.



10\. Audit/evidence references remain reconstructible end-to-end.

```



\---



\# 104. Load Benchmark



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



\---



\# 105. Never Fake Benchmarks



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



\---



\# 106. Development Rules for Codex / Claude Code



ทุก phase:



```text

READ MASTER PLAN



&#x20;↓



READ relevant ADR



&#x20;↓



INSPECT existing code



&#x20;↓



STUDY relevant upstream reference



&#x20;↓



STATE invariants



&#x20;↓



WRITE tests



&#x20;↓



IMPLEMENT



&#x20;↓



RUN tests



&#x20;↓



RUN race detector



&#x20;↓



UPDATE docs

```



\---



\# 107. AI Development Rule



Codex/Claude ห้าม:



```text

invent API without verifying upstream



copy external project without attribution



disable test to make CI green



silently weaken fail-closed behavior



claim exactly-once without proof



implement next phase automatically

```



\---



\# 108. Upstream Contribution Strategy



AEF / Control Plane เป็น independent project ก่อน



จากนั้น integration กับ AGT



เส้นทาง:



```text

Standalone Control Plane



&#x20;↓



Working AGT integration



&#x20;↓



Example



&#x20;↓



Discussion with Microsoft maintainers



&#x20;↓



Small upstream contribution



&#x20;↓



Reusable integration



&#x20;↓



Possible provider / protocol proposal

```



\---



\# 109. Potential Microsoft Contributions



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



\---



\# 110. MVP Definition



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



UNKNOWN\_OUTCOME



Reconciliation



Budget reservation



OpenTelemetry



Basic dependency graph



Basic kill switch propagation

```



\---



\# 111. Portfolio-Ready Definition



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



\---



\# 112. Open-Source Ready Definition



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



\---



\# 113. Enterprise-Ready Direction



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



\---



\# 114. Main Differentiator



อย่าแข่งขันว่า:



```text

เรามี policy มากกว่า AGT



เรามี Agent framework ดีกว่า LangGraph



เรามี workflow engine ดีกว่า Temporal

```



สิ่งที่ Control Plane ของเราควรเด่นคือ:



> \*\*Unified enterprise operations for governed autonomous agents.\*\*



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



\---



\# 115. Product Story



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

\+

security

\+

operations

\+

FinOps

\+

release management

\+

dependency intelligence

```



รวมกันเป็น:



```text

Enterprise Autonomous Agent Infrastructure

```



\---



\# 116. Final Architecture



```text

┌───────────────────────────────────────────────────────────────┐

│                    AGENT APPLICATIONS                         │

│                                                               │

│ OpenAI │ Claude │ LangGraph │ MAF │ AutoGen │ Custom         │

└─────────────────────────────┬─────────────────────────────────┘

&#x20;                             │

&#x20;                             ▼

╔═══════════════════════════════════════════════════════════════╗

║                ENTERPRISE AGENT CONTROL PLANE                ║

║                                                               ║

║  Registry                Dependency Graph                     ║

║  Fleet Management        Security Operations                 ║

║  FinOps                  Release Management                  ║

║  Evaluations             Incident Management                 ║

║  Operator UI             API / CLI                           ║

╚═════════════════════════════╤═════════════════════════════════╝

&#x20;                             │

&#x20;                             ▼

┌───────────────────────────────────────────────────────────────┐

│                  GOVERNANCE FOUNDATION                        │

│                                                               │

│                Microsoft AGT / ACS                            │

│                                                               │

│ Policy │ Identity │ Trust │ Approval │ Context │ Audit       │

└─────────────────────────────┬─────────────────────────────────┘

&#x20;                             │

&#x20;                      Governance Decision

&#x20;                             │

&#x20;                             ▼

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

║ UNKNOWN\_OUTCOME                                              ║

║ Reconciliation                                               ║

║ Backpressure / Bulkhead / Circuit Breaker                    ║

╚═════════════════════════════╤═════════════════════════════════╝

&#x20;                             │

&#x20;                             ▼

&#x20;                  MCP / A2A / HTTP / gRPC

&#x20;                             │

&#x20;            ┌────────────────┼────────────────┐

&#x20;            ▼                ▼                ▼

&#x20;           SAP            Databases         GitHub

```



\---



\# 117. One-Sentence Positioning



> \*\*Enterprise Agent Control Plane is an open infrastructure platform for governing, operating, securing, scheduling, and reliably executing autonomous AI agents across enterprise systems.\*\*



\---



\# 118. Technical Positioning



> \*\*Microsoft AGT governs agent actions; the Go execution fabric executes them safely; the Enterprise Agent Control Plane manages the entire agent fleet.\*\*



\---



\# 119. Final Goal



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



> \*\*สร้าง infrastructure ที่ทำให้องค์กรสามารถปล่อย Agent จำนวนมากเข้าสู่ production ได้โดยยังควบคุมความเสี่ยง การทำงาน ค่าใช้จ่าย และผลกระทบได้\*\*



นั่นคือ Enterprise Agent Control Plane



