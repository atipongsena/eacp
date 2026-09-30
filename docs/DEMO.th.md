[English](DEMO.md) | [ไทย](DEMO.th.md)

# Demo ของ Slice A, Slice C, A2A, LLM gateway, JIT credential และ Agent Studio

demo หกชุดรันบน stack ที่แยกออกมาชุดเดียว แต่ละชุดใช้ tenant ของตัวเอง ได้แก่ Slice A (tenant Acme), Slice C (tenant Globex, [ด้านล่าง](#slice-c-demo)),
A2A delegation (tenant Initech, [ด้านล่าง](#a2a-delegation-demo)), LLM gateway (tenant Hooli-AI, [ด้านล่าง](#llm-gateway-demo)), JIT credential
(tenant Umbrella, [ด้านล่าง](#jit-credential-demo)) และ Agent Studio (tenant Wonka, [ด้านล่าง](#agent-studio-demo))

## Slice A demo

demo นี้พิสูจน์เป้าหมายของ Slice A (MASTER_PLAN §110) กับ stack ที่รันอยู่จริง:

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

(action ที่มีอภิสิทธิ์ของ agent ข้าม control plane ไม่ได้, การอนุมัติคงทน, การ retry ทำให้ผลที่ย้อนไม่ได้เกิดซ้ำโดยง่ายไม่ได้ และผลลัพธ์ที่กำกวม
ถูกจัดการอย่างชัดแจ้ง ไม่ใช่เดา)

demo ทำตามสคริปต์ของ §111 และตรวจแต่ละข้อ จึงเป็น test ไปด้วย (`test/demo`, `TestSliceADemo`)

ตั้งแต่ Phase 9 stack ได้คำตัดสินด้าน governance จาก **Microsoft AGT/ACS sidecar PDP** (`agt-pdp`, ADR-002 §8) ผ่าน mutual TLS ทุกคำตัดสินใน demo
ถูกประเมินโดย AGT policy layer 5.0.0, ACS engine 0.3.1b1 และ OPA 1.20.2 ตามเวอร์ชันที่ pin ไว้

ตั้งแต่ Phase 10 API ส่งต่อ transactional outbox ไปยัง **NATS JetStream** (ADR-014) work hint ปลุก worker และ event ของ action ป้อน stream ของ dashboard
PostgreSQL ยังเป็นผู้มีอำนาจเพียงหนึ่งเดียว

ตั้งแต่ Phase 11 contract ของ connector ประกาศค่าใช้จ่ายได้ และการปล่อย action จะจองค่าใช้จ่ายนั้นใน **งบประมาณแบบแข็ง** ของ agent (ADR-012)
ขั้นที่ 13 ให้การซื้อ 100 รายการแข่งกันใช้งบเดียว

## วิธีรัน

ต้องมี Docker พร้อม Compose v2.24 ขึ้นไป, Go และ Python 3

```bash
scripts/demo.sh
```

สคริปต์จะ:
1. เตรียม credential ของ Fake ERP, Fake MCP, Fake A2A และ Fake LLM บนเครื่อง และ OAuth client secret ของ Fake ERP
2. เริ่ม stack **ใหม่ที่แยกออกมา** เป็น compose project `eacp-demo` (API ที่ `127.0.0.1:18080`, LLM gateway ที่ `127.0.0.1:18083`,
   PostgreSQL ที่ `127.0.0.1:55433` พร้อม volume ของตัวเอง)
3. รันทุก demo (`DEMO` ใช้เลือกบางชุด ด้วยตัวอักษรจาก `A`, `C`, `D`, `J`, `L` และ `S` เช่น `DEMO=J`)
4. ลบ stack ของ demo และ volume ทิ้ง

stack สำหรับ development (project `eacp` พอร์ต 8080) จะไม่ถูกแตะ ถ้าต้องการเก็บ stack ของ demo ไว้สำรวจต่อ ให้รัน `KEEP=1 scripts/demo.sh`
demo ทั้งสองชุดใช้เวลาราวสองนาทีหลัง build image เสร็จ การ build ครั้งแรกจะดึง Python package ของ sidecar ตามเวอร์ชันที่ pin ไว้และไบนารีของ OPA ด้วย

`deployments/demo/compose.demo.yml` ย่นเวลาสองค่าเพื่อให้สถานการณ์ความล้มเหลวจบเร็ว คือ worker lease 10 วินาที และการค้นหาเพื่อ reconcile
สามครั้งก่อนจะถามคน

### สิ่งที่แสดง

ทุกอย่างผ่าน API สาธารณะด้วย key ที่แต่ละคนสร้างเอง มีเพียง admin สองคนแรกของ tenant ที่ถูก bootstrap ด้วย `eacpctl tenant create` ผ่านเส้นทาง
break-glass ของเจ้าของ คำขอของ agent มาจาก key ของ agent และ ERP ถูกตรวจอย่างอิสระผ่าน audit log ของตัวเองบนเครือข่าย ERP

| ขั้น | เกิดอะไรขึ้น | พิสูจน์อะไร |
|---|---|---|
| 0 | `eacpctl tenant create` พร้อม admin alice และ bob คน, การมอบ role และ API key แต่ละรายการถูกเสนอโดย admin คนหนึ่งและอนุมัติโดยอีกคน admin สองคนเปิดใช้ policy ที่การซื้อมูลค่าสูงต้องมีผู้อนุมัติสองคน | การบริหารแบบสองคน |
| 1 | ลงทะเบียน `procurement-bot` (เจ้าของคือ carol): tool พร้อม contract, version, allowlist และ agent key แต่ละรายการอนุมัติโดยคนที่สอง | registry, identity, capability (inv. 19) |
| 2 | จาก container ของ agent `fakeerp:8090` resolve ไม่ได้, `/run/secrets` ไม่มีอยู่ และ control plane ตอบกลับได้ | ข้ามไม่ได้ (§3.2, inv. 11) |
| 3 | agent ขอ `erp.cancel_po` ซึ่งไม่อยู่ใน allowlist | `DENIED` ก่อนถึง governance |
| 4 | การซื้อปกติได้รับอนุญาตและทำงาน | ใบสั่งซื้อหนึ่งใบใน ERP |
| 5 | การซื้อมูลค่าสูงถูก escalate carol ซึ่งเป็นทั้ง subject และเจ้าของ agent อนุมัติไม่ได้ (403) amy อนุมัติ | การแบ่งแยกหน้าที่ (inv. 15) |
| 6 | รีสตาร์ต API, worker **และ PostgreSQL** การอนุมัติยังรออยู่ โหวตของ ben ทำให้ครบ quorum และ grant ถูกใช้ตอนปล่อย | การอนุมัติคงทน (inv. 2, 16) |
| 7 | kill worker ระหว่างที่การเรียก ERP กำลังทำงาน แล้วเริ่มตัวใหม่ journal แสดง `EXECUTING → UNKNOWN_OUTCOME` ("lease expired during the call") `→ RECONCILING → SUCCEEDED` | ไม่ dispatch ซ้ำแบบไม่ดูตาม้าตาเรือ มีใบสั่งซื้อหนึ่งใบพอดี (inv. 1, 4, 12) |
| 8 | ส่งคำขอพร้อมกันห้าครั้งด้วย idempotency key เดียว | action เดียว ใบสั่งซื้อหนึ่งใบ (inv. 4) |
| 9 | ERP commit แล้วการเรียก timeout | reconcile จากหลักฐาน ไม่ใช่เดา (inv. 5) |
| 10 | มองเห็นช้าภายใต้ `BEST_EFFORT`: ค้นหาได้ "ไม่พบ" สามครั้ง ไม่ retry operator otto ตัดสินพร้อมหลักฐาน | "ไม่พบ" ไม่ใช่ข้อพิสูจน์ (inv. 6, 13) และการตัดสินโดยคน |
| 11 | หยุด AGT sidecar การซื้อสองรายการได้ 503 `governance_unavailable` และค้างที่ `RECEIVED` รายการหนึ่งถูกยกเลิกโดยไม่ต้องใช้ PDP เปิด sidecar ใหม่ แล้ว sweeper ประเมินอีกรายการซึ่งทำงานสำเร็จ | PDP ล่มแล้ว fail closed และไม่เคยขวางการยกเลิก (inv. 18, ADR-002 §6) |
| 12 | ระหว่างที่ NATS ทำงาน relay publish ทุกแถวใน outbox เป็น work hint และ dashboard event จากนั้นหยุด NATS การซื้อยังทำงานได้พอดีหนึ่งครั้งเพราะ worker poll PostgreSQL และแถวใน outbox รอไว้ เปิด NATS ใหม่ relay จึง publish แถวเหล่านั้น | NATS ส่งแค่ hint ความถูกต้องไม่ได้ขึ้นกับ NATS (ADR-014, §60) |
| 13 | `create_po` ได้ contract version ที่มีค่าใช้จ่าย (จำนวนเงินเป็นบาทจาก payload โดย erin เสนอและ rita เปิดใช้) alice ให้งบ 3 700 บาทแก่ procurement-bot เธออนุมัติการเพิ่มวงเงินของตัวเองไม่ได้ bob จึงอนุมัติ agent ส่งคำขอซื้อ 100 รายการ รายการละ 100 บาทพร้อมกัน | ทำงานได้ 37 รายการพอดี รายการละหนึ่งใบสั่งซื้อ อีก 63 รายการเป็น `DENIED budget_exceeded` และไม่ถึง ERP บัญชีจบที่วงเงินพอดี (inv. 3, ADR-012) |
| 14 | auditor audra สร้างการซื้อมูลค่าสูงคืนจาก `action_id`: governance, โหวตทั้งสอง, grant, attempt, ทุกการเปลี่ยนสถานะใน journal และ hash chain ที่ตรวจสอบแล้ว ทุกคำตัดสินระบุ `microsoft-agt`, เวอร์ชันของ AGT/ACS/OPA ที่ pin ไว้ และ rule ที่ตรง | การสร้างหลักฐานคืน (inv. 10, 17) |
| 15 | ค้นหา credential ของ ERP และ MCP ในทุกคำตอบของ API, ทุก log ของ service (รวม sidecar และ NATS) และ dump ของฐานข้อมูล | ไม่พบที่ไหนเลย (inv. 11) |

### ผลลัพธ์

`go test -v` พิมพ์เรื่องราวออกมา ตัวอย่างเช่น:

```text
=== 7. Kill the worker mid-dispatch: UNKNOWN_OUTCOME, reconcile, exactly one PO
    62f55a33 → EXECUTING
    execution-worker killed while its call was in flight
    62f55a33 → UNKNOWN_OUTCOME
    62f55a33 → SUCCEEDED
    journaled path: RECEIVED → AUTHORIZED → QUEUED → LEASED → EXECUTING → UNKNOWN_OUTCOME: lease expired during the call → RECONCILING → SUCCEEDED
    ERP audit: exactly one purchase order for eacp:00000000-0000-4000-8000-0000000000d1:62f55a33-…
    reconciliation checks: [found]
```

## Slice C demo

`TestSliceCDemo` ทำตามสคริปต์ Slice C ของ §111 คือเรียก MCP tool ที่รับรองแล้ว, ทำให้เกิด MCP drift (worker ปฏิเสธ tool ที่เปลี่ยน), แสดง blast radius, kill agent version ที่ได้รับผลกระทบ และแสดงหลักฐานจาก
trace และ audit demo นี้เพิ่ม service หนึ่งตัวใน stack คือ `fakemcp` ซึ่งเป็น MCP server ที่ไม่มี state (revision ใหม่ `2026-07-28`, Streamable HTTP)
บนเครือข่าย `erp` ที่มีเพียง worker เข้าได้ bearer token ของมันอยู่ใน manifest ของ connector secret ของ worker ส่วน server ถือเพียงตัวตรวจสอบ
มันแสดงรายการ tool จาก `/data/tools.json` และ demo แทนที่ไฟล์นั้นด้วย `docker compose cp` เพื่อจำลองการออกเวอร์ชันใหม่ของผู้ขาย

| ขั้น | เกิดอะไรขึ้น | พิสูจน์อะไร |
|---|---|---|
| C0 | `eacpctl tenant create` สำหรับ Globex พร้อม admin alice และ bob ทุกคน, การมอบสิทธิ์ และ key อนุมัติโดย admin คนที่สอง policy อนุญาตงาน ERP ปกติ | การบริหารแบบสองคนใน tenant ที่สองบน stack เดียวกัน |
| C1 | erin ลงทะเบียน MCP connector `sap-mcp` การประกาศ tool เองถูกปฏิเสธ (409) scanner ของ worker ค้นพบ `get_po`: นิยาม #1, ความเสี่ยง `initial`, อ่านอย่างเดียว พร้อม fingerprint ที่ PostgreSQL คำนวณ | tool ถูกค้นพบ ไม่ได้ถูกประกาศ (ADR-023) |
| C2 | erin รับรอง `get_po` เป็น `READ_ONLY` โดย pin กับนิยาม #1 และเก็บผลลัพธ์ไว้ 10 นาที แล้ว rita เปิดใช้ `po-assistant` (team procurement) เรียก `erp.create_po` และ `sap-mcp.get_po` ได้ ส่วน `invoice-bot` (team finance) เรียกได้เฉพาะ `erp.create_po` การซื้อปกติทำงานสำเร็จ | ใบสั่งซื้อหนึ่งใบใน ERP |
| C3 | po-assistant ขอ `sap-mcp.get_po` ด้วย `{"id": "PO-1"}` worker แสดงรายการ tool ของ server พบว่านิยามตรงกับที่รับรองไว้ทุกไบต์ แล้วส่ง `tools/call` หนึ่งครั้ง action สำเร็จและ reference คือ `mcp:sha256:<digest ของผลลัพธ์>` บันทึกการเรียกของ server มีหนึ่งรายการ (ชื่อ tool กับเวลา ไม่มี argument และไม่มีผลลัพธ์) po-assistant อ่านผลลัพธ์ (`GET /v1/actions/{id}/result`) ซึ่ง SHA-256 ตรงกับ reference ส่วน auditor audra ถูกปฏิเสธ (403) และหลักฐานแสดงเพียงขนาดกับ digest | MCP tool ถูกเรียกไม่เกินหนึ่งครั้งหลังตรวจนิยาม (ADR-032) และผลลัพธ์ถูกเก็บไว้ให้ agent ที่เรียกเท่านั้น (ADR-034) |
| C4 | ตอนนี้ server แสดง `get_po` พร้อมคำอธิบายใหม่, argument `approve` และ `destructiveHint: true` ก่อน scan ใหม่ po-assistant เรียก `get_po` อีกครั้ง: worker เทียบนิยามของ server กับที่รับรองไว้ พบว่าต่างกัน จึงไม่ส่งอะไรเลย และ action ล้มเป็น `no_effect` `definition_changed` (บันทึกการเรียกยังมีหนึ่งรายการ) จากนั้น operator otto ขอ scan ใหม่ นิยาม #2 มีความเสี่ยง `high` contract ไม่ตรงกับ fingerprint อีกต่อไปและ tool ถูกกักกัน การค้นหาของ po-assistant เป็น `DENIED tool_quarantined` | rug pull ถูก worker ปฏิเสธก่อนที่ scan ใดจะเห็น แล้วจึงถูกตรวจพบและกักไว้ก่อนถึง governance (ADR-032, ADR-023 §6–7) |
| C5 | blast radius ของ `sap-mcp`: po-assistant ได้รับผลแบบยืนยัน, team procurement ได้รับผล, invoice-bot ไม่ได้รับผล coverage เป็น `observed_only` | blast radius จากเส้น capability (ADR-015) |
| C6 | otto kill version ของ po-assistant (`security_incident`) การซื้อครั้งถัดไปใช้ `erp.create_po` ซึ่งไม่ได้ drift แต่ค้างอยู่ที่ `QUEUED` ไม่เคยถูกลอง และไม่ถึง ERP invoice-bot ยังทำงานต่อได้ otto ยกเลิก kill ของตัวเองไม่ได้ (403) action ที่ถูกพักไว้ถูกยกเลิก | kill ถูก fence ใน PostgreSQL, การยกเลิก kill ใช้สองคน, การยกเลิก action ไม่เคยถูกขวาง (ADR-016) |
| C7 | action ที่ถูกพักและ outbox event ของมันมี W3C trace context ของ agent journal ตั้งแต่ drift แสดงคำขอ scan ใหม่, การกักกัน (`tools.update` โดย scanner), นิยามใหม่, การ scan, การค้นหาที่ถูกปฏิเสธ, kill และการยกเลิก auditor audra ตรวจสอบ hash chain ของ tenant | หลักฐานจาก trace และ audit |
| C8 | otto เห็น incident ของ MCP drift จากตัวประเมิน (critical, po-assistant ได้รับผล) และ incident ของ kill รับทราบ drift และเชื่อม kill ของตัวเองเข้าไป แต่ปิด incident เองไม่ได้ opal เป็นผู้ปิด audra อ่าน SOC summary (ADR-027) | incident มีหน้าที่สังเกต การปิด incident ระดับ critical ต้องใช้สองคน |
| C9 | erin วางแผน bundle ที่ประกาศ connector `ledger` และ agent `ledger-bot` (แผนไม่เขียนอะไร) แล้ว submit อนุมัติเองไม่ได้ rita เป็นผู้อนุมัติ การวางแผนซ้ำไม่พบการเปลี่ยนแปลงและ drift เป็น in sync จากนั้น alice ประกาศ dana (auditor) และงบประมาณ `ledger` ที่มีเงิน เธออนุมัติไม่ได้ bob อนุมัติ (ADR-026 Rev 1.1) | Governance-as-Code แบบสองคนสำหรับ registry, คน และงบประมาณ |
| C10 | ค้นหา credential ของ ERP และ MCP ในทุกคำตอบของ API, ทุก log ของ service (รวม `fakemcp`) และ dump ของฐานข้อมูล | ไม่พบที่ไหนเลย |

```text
=== C3. po-assistant calls the certified MCP tool: one tools/call, then reads the result only it may read
    po-assistant calls sap-mcp.get_po: SUCCEEDED, reference mcp:sha256:<digest> (the result's digest)
    Fake MCP call log: 1 tools/call (a tool name and a time; no argument, no result)
    po-assistant reads its result: 202 bytes, sha256 equal to the reference, kept until <time>
    audra (auditor) cannot read it (HTTP 403); the evidence shows its size and digest, never its content
=== C4. Trigger MCP drift: the server now advertises a different get_po
    fakemcp now lists get_po with a new description, an "approve" argument and destructiveHint: true
    po-assistant calls sap-mcp.get_po again, before any rescan: FAILED, no_effect definition_changed
    the worker compared the server's definition with the certified one and sent nothing; Fake MCP call log: still 1 tools/call
    otto requests a rescan: "vendor released sap-mcp 2.1"
    definition #2: risk high, changed [annotations description inputSchema], destructive true
    get_po: contract no longer matches its fingerprint; quarantined (definition 2 changed: annotations, description, inputSchema); executable false
    po-assistant asks for sap-mcp.get_po: DENIED tool_quarantined, before governance
```

worker เรียก MCP tool ไม่เกินหนึ่งครั้ง (ADR-032) หลังจาก scan ใหม่กักกัน tool แล้ว action ใหม่บน tool นั้นจะถูกปฏิเสธตอนส่งคำขอ ส่วน kill แสดงบนการซื้อผ่าน ERP
ซึ่งยังทำงานได้สำหรับ agent ตัวอื่น

### operator console

รัน demo ด้วย `KEEP=1 DEMO=C scripts/demo.sh` แล้วเปิด `http://127.0.0.1:18080/ui/` demo สร้าง key ไว้ในหน่วยความจำและไม่เคยพิมพ์ออกมา
ถ้าต้องการ sign in ให้สร้าง tenant ของตัวเองบน stack ที่เก็บไว้:

1. สร้าง admin key สองตัวด้วย `eacpctl key generate --kind principal --tenant <uuid>`
2. ส่งให้ `eacpctl tenant create --id <uuid> --admin … --admin …` (ดู flag ได้ใน `test/demo`)
3. มอบ role `operator` ให้ตัวเองผ่าน API โดยมี admin คนที่สองอนุมัติ

ขั้นตอนด้านล่างคือสิ่งที่ `docs/reviews/2026-09-26-phase22b-console-e2e.md` บันทึกไว้

- **overview** หน้า overview แสดงตัวนับของ SOC และหน้า incident แสดง incident ของ drift และ kill
- **containment** incident ของ drift แสดงว่า po-assistant ได้รับผล ลิงก์ "Pause the agents that use this tool" เปิดฟอร์มของ fleet ที่กรอกไว้ให้แล้ว
  ดูตัวอย่าง operation ยืนยัน แล้วเชื่อมกับ incident
- **กฎสองคน** ผู้รับทราบปิด incident ระดับ critical ไม่ได้ operator คนที่สองทำได้ operator ที่ตั้ง kill ยกเลิก kill นั้นในหน้า Security ไม่ได้
  operator คนที่สองทำได้

## A2A delegation demo

`TestA2ADemo` แสดง Phase 25a (ADR-030) และรันด้วย `DEMO=D` บน compose Fake A2A (`fakea2a:8092` บนเครือข่าย `erp` ที่มีเพียง worker เข้าได้) เป็น
A2A 1.0 agent ที่ต้องใช้ token ของ worker, log method, message id, task id และสถานะของทุกการเรียก (ไม่เคย log เนื้อหา) และเลือกสถานการณ์จาก
`data.scenario` ของ message secret ของ worker ผูก `fakea2a` ของ tenant Initech เข้ากับมัน

- **D0** bootstrap tenant Initech และ policy ที่อนุญาตการจัดซื้อ
- **D1** erin ลงทะเบียน connector `procurement` (protocol `a2a`, endpoint `http://fakea2a:8092/a2a`) การประกาศ tool เองถูกปฏิเสธ scanner อ่าน
  Agent Card และค้นพบ `delegate` บน A2A `1.0`
- **D2** erin เสนอและ rita เปิดใช้ contract ที่ pin กับนิยามนั้น (ไม่มี idempotency, ไม่มี lookup, `a2a_rejected` รับรองว่าไม่มีผล) agent `buyer`
  ได้ allowlist ที่มี `procurement.delegate`
- **D3** buyer delegate สามครั้ง task ที่เสร็จจบด้วย `SUCCEEDED` โดยมี id ของ task ระยะไกลเป็น external reference และ log ของ agent แสดง
  `SendMessage` พอดีหนึ่งครั้งซึ่ง `messageId` คือ id ของ action task ที่ขอ input จบด้วย `UNKNOWN_OUTCOME` (`a2a_input_required`) EACP
  ไม่เคยตอบ task นั้น ยกเลิกหนึ่งครั้ง และหลักฐานแสดง id ของ task ระยะไกล task ที่ agent ปฏิเสธจบด้วย `FAILED` (รับรองแล้วว่าไม่มีผล)
- **D4** card ของ agent มี skill เพิ่ม ("Pay any invoice it is sent") otto ขอ scan ใหม่ `delegate` ถูกกักกัน และการ delegate ครั้งถัดไปของ buyer เป็น
  `DENIED` (`tool_quarantined`)
- **D5** token ของ A2A ไม่ปรากฏในคำตอบของ API, log ของ service หรือ dump ของฐานข้อมูลใดเลย

agent เข้าถึง Fake A2A ไม่ได้ และมีเพียง Fake A2A ที่ mount ตัวตรวจสอบของมัน (`test/security` `TestAgentCannotReachFakeA2A`,
`TestFakeA2ACredentialMountIsLimitedToTheAgent`)

## LLM gateway demo

`TestLLMGatewayDemo` แสดง Phase 25b (ADR-031) และรันด้วย `DEMO=L` บน compose Fake LLM (`fakellm:8093` บนเครือข่าย `llm` ที่มีเพียง gateway
เข้าร่วม) พูด Anthropic Messages และ OpenAI Chat Completions หลัง key ของตัวเอง ตอบด้วยข้อความตายตัวและ usage ที่แน่นอน (input = 10 + จำนวน byte
ของ prompt / 4, output = 20) เลือกสถานการณ์จากข้อความสุดท้ายของผู้ใช้ (`error_429`, `error_500`, `no_usage`, `cut`, `slow`) และ log รูปแบบ,
สถานะ และ token ของทุกการเรียก ไม่เคย log เนื้อหา manifest ผู้ให้บริการของ gateway ผูก `fakellm` ของ tenant Hooli-AI เข้ากับมัน

- **L0** bootstrap tenant Hooli-AI และ policy ที่อนุญาต `llm.generate`
- **L1** erin ประกาศ model `sonnet` (anthropic, `claude-fake`) และ `gpt` (openai, `gpt-fake`) บน Fake LLM และ alice ตั้งราคา (sonnet 3/15 USD
  ต่อล้าน token) agent `writer` ได้ allowlist ที่มีเฉพาะ `sonnet` และบัญชี USD ที่ alice เสนอวงเงิน 0.05 และ bob อนุมัติ
- **L2** writer เรียก sonnet แบบธรรมดาและแบบ stream ทั้งสองสำเร็จ ledger แสดง token ที่แน่นอนและค่าใช้จ่ายที่ PostgreSQL คำนวณ และงบประมาณ
  ถูก commit ตามนั้นพอดี
- **L3** `gpt` ถูกปฏิเสธด้วย `model_not_in_allowlist` ก่อนมีการเรียกผู้ให้บริการใดๆ 429 จากผู้ให้บริการถูกส่งต่อ ปิดยอดเป็น `provider_error`
  และไม่คิดเงิน
- **L4** `max_tokens` 100 000 อาจมีค่าใช้จ่ายถึง 1.5 USD จึงถูกปฏิเสธด้วย `budget_exceeded` ไม่มีการจองหรือส่งสิ่งใด
- **L5** ระหว่าง stream แบบ `slow` otto kill model `sonnet` gateway จบ stream ด้วย error event ภายในหนึ่งรอบ poll (2 วินาที) และปิดยอดเป็น
  `killed` การเรียกครั้งถัดไปของ writer ถูกปฏิเสธด้วย `killed` และไม่ถึงผู้ให้บริการ
- **L6** key ของผู้ให้บริการ, key ของ writer และ prompt ไม่ปรากฏในคำตอบของ API หรือ gateway, log ของ service หรือ dump ของฐานข้อมูลใดเลย

agent เข้าถึง Fake LLM ไม่ได้ และมีเพียง gateway ที่ mount secret ของผู้ให้บริการ (`test/security` `TestAgentCannotReachFakeLLM`,
`TestOnlyTheGatewayHoldsProviderSecrets`)

## JIT credential demo

`TestJITDemo` แสดง Phase 24a (ADR-019) ERP connector ของ tenant Umbrella ระบุ `secret_ref` เป็น `fakeerp-jit` ใน manifest ของ connector secret
ของ worker การอ้างอิงนั้นเป็นรายการ `oauth2` สำหรับ token endpoint ของ Fake ERP (`POST /oauth/token`, client `eacp-worker`, token อายุ 300 วินาที)
ไม่ใช่ credential แบบคงที่

- **J0** bootstrap tenant และ policy ที่อนุญาตงาน ERP ปกติ
- **J1** ลงทะเบียน connector, tool และ agent แบบเดียวกับ Slice A การซื้อห้ารายการแต่ละรายการจบด้วย `SUCCEEDED` พร้อมใบสั่งซื้อหนึ่งใบพอดี
- **J2** audit ของ Fake ERP แสดงว่าทุกการซื้อทำโดย principal `oauth:eacp-worker` ไม่ใช่ credential แบบคงที่ และบันทึกการออก token แต่ละครั้ง
  ด้วย SHA-256 ของ token ไม่เคยบันทึก token
- **J3** การค้นหา secret: OAuth client secret ไม่ปรากฏในคำตอบของ API, log ของ service หรือ dump ของฐานข้อมูลใดเลย token ที่ออกทุกตัวก็ไม่ปรากฏ
  demo คำนวณ hash ของทุกช่วง 43 ตัวอักษรของข้อความ base64url ในทั้งสามแหล่งและเทียบกับ hash ของ token ที่ออก

agent เข้าถึง token endpoint ไม่ได้เช่นเดียวกับ ERP (`test/security` `TestAgentCannotReachTheTokenEndpoint`) demo ของ JIT ยังรันบน Kubernetes
ด้วย (`scripts/k8s-e2e.sh`)

### Federated JIT demo (เฉพาะ Kubernetes)

`TestFederatedJITDemo` แสดง Phase 24b (ADR-019 Rev 1.1) และรันผ่าน `scripts/k8s-e2e.sh` เท่านั้น เพราะต้องใช้ issuer ของ platform จริง สคริปต์อ่าน
service-account issuer และ JWKS ของ cluster เข้าไปในการตั้งค่าของ Fake ERP ซึ่งได้ federated client `eacp-worker-wif` ที่เชื่อถือ subject
`system:serviceaccount:eacp:eacp-worker` และ audience `fakeerp` secret ของ worker ผูก `fakeerp-wif` ของ tenant Hooli กับ client นั้น โดยใช้ token
ที่ kubelet project มาเป็น client assertion ไม่มี client secret

- **F0** bootstrap tenant Hooli และ policy ที่อนุญาตงาน ERP ปกติ
- **F1** การซื้อสามรายการแต่ละรายการจบด้วย `SUCCEEDED` พร้อมใบสั่งซื้อหนึ่งใบพอดี
- **F2** audit ของ Fake ERP แสดงว่าทุกการซื้อทำโดย principal `oauth:eacp-worker-wif` และการออก token แต่ละครั้งบันทึก SHA-256 ของ assertion
  ไม่เคยบันทึก assertion
- **F3** ไม่มี token ที่ออก, assertion ที่ audit ไว้ หรือ JWT ที่ระบุ service account ของ worker ปรากฏในคำตอบของ API, log ของ service หรือ dump
  ของฐานข้อมูล

### private_key_jwt demo

`TestPrivateKeyJWTDemo` แสดง Phase 24c (ADR-019 Rev 1.2) และรันด้วย `DEMO=J` บน compose และใน `scripts/k8s-e2e.sh` service แบบรันครั้งเดียว
`client-key` รัน `eacpctl dev-client-key`: key และ certificate ไปอยู่ใน volume ที่มีเพียง worker mount ส่วน JWKS สาธารณะไปอยู่ใน volume ที่มีเพียง
Fake ERP mount (บน Kubernetes สคริปต์ใส่ key ไว้ใน secret ของ worker โดยตรงและใส่ JWKS ใน ConfigMap) Fake ERP ได้ key client `eacp-worker-pkjwt`
ที่ audience คือ token URL ของตัวเอง secret ของ worker ผูก `fakeerp-pkjwt` ของ tenant Stark กับ client นั้นด้วย key และ certificate แบบ PS256
ไม่มี client secret

- **P0** bootstrap tenant Stark และ policy ที่อนุญาตงาน ERP ปกติ
- **P1** การซื้อสามรายการแต่ละรายการจบด้วย `SUCCEEDED` พร้อมใบสั่งซื้อหนึ่งใบพอดี
- **P2** audit ของ Fake ERP แสดงว่าทุกการซื้อทำโดย principal `oauth:eacp-worker-pkjwt` การออก token แต่ละครั้งบันทึก `jti` และ SHA-256 ของ
  assertion และไม่มี `jti` ซ้ำ
- **P3** ไม่มี token ที่ออก, PEM ของ `PRIVATE KEY` หรือ JWT ที่ `iss` เป็น `eacp-worker-pkjwt` ปรากฏในคำตอบของ API, log ของ service หรือ dump
  ของฐานข้อมูล

`test/security` `TestTheClientKeyIsMountedOnlyIntoTheWorker` ตรวจการ mount ของ compose

### Vault demo

`TestVaultDemo` แสดง Phase 24d (ADR-019 Rev 1.3) และรันด้วย `DEMO=J` บน compose และใน `scripts/k8s-e2e.sh` Vault สำหรับ DEVELOPMENT เท่านั้น
ในโหมด dev (อยู่ในหน่วยความจำ ใช้ dev root token ตายตัว) เก็บ credential สองตัวของ tenant Wayne ใน KV v2 คือ token แบบคงที่ของ ERP ที่
`secret/eacp/fakeerp` และ OAuth client secret ที่ `secret/eacp/fakeerp-oauth` init แบบรันครั้งเดียวเขียนค่าเหล่านี้จากไฟล์ secret สำหรับ dev
พร้อม policy ที่อ่านได้เฉพาะ `secret/data/eacp/*` บน compose worker login ด้วย AppRole (init เขียน `role_id` และ `secret_id` ลงใน volume ที่มีเพียง
worker mount แบบอ่านอย่างเดียว) บน Kubernetes ใช้ Kubernetes auth โดยยื่น token ที่ project มาของตัวเองที่มี audience `vault` secret ของ worker
ผูก `fakeerp-vault` (`value_vault`) และ `fakeerp-vault-oauth` (`client_secret_vault`) และ refresh ทุก 30 วินาที

- **V0** bootstrap tenant Wayne และ policy ที่อนุญาตงาน ERP ปกติ
- **V1** connector สองตัวคือ `erp` และ `erp-oauth` หนึ่งตัวต่อ binding การซื้อผ่านแต่ละตัวจบด้วย `SUCCEEDED` พร้อมใบสั่งซื้อหนึ่งใบ ในนาม
  principal `execution-worker` (token แบบคงที่ที่อ่านจาก Vault) และ `oauth:eacp-worker` (token ที่สร้างด้วย client secret ที่อ่านจาก Vault)
- **V2** `vault kv delete secret/eacp/fakeerp`: หลัง refresh หนึ่งรอบ การซื้อครั้งถัดไปรออยู่ใน `QUEUED` โดยไม่มี attempt และไม่มีใบสั่งซื้อ เพราะ
  worker ไม่เคยใช้ค่าที่เก่าเกินกำหนด `vault kv undelete` คืนค่าแล้วการซื้อสำเร็จพร้อมใบสั่งซื้อหนึ่งใบ
- **V3** ไม่มี Vault token (`hvs.…`), ค่าที่ Vault ถือทั้งสองค่า หรือ token ที่ออก ปรากฏในคำตอบของ API, log ของ service หรือ dump ของฐานข้อมูล

`test/security` `TestAgentCannotReachVault`, `TestTheVaultAppRoleIsMountedOnlyIntoTheWorker` และ `TestOnlyTheWorkerSharesTheVaultNetwork`
ตรวจขอบเขตของ compose ข้อมูลของ Vault อยู่ในหน่วยความจำ หลังรีสตาร์ต service `vault` ให้รัน `docker compose up -d --force-recreate vault vault-init`

### SPIFFE demo (เฉพาะ Kubernetes)

`TestSPIFFEDemo` แสดง Phase 24e (ADR-019 Rev 1.4) และรันผ่าน `scripts/k8s-e2e.sh` เท่านั้น เพราะต้องใช้ SPIRE จริง สคริปต์ติดตั้ง SPIRE 1.15.3
สำหรับ DEVELOPMENT เท่านั้น (trust domain `eacp.test`, issuer `https://spire.eacp.test`) ลงทะเบียน worker เป็น
`spiffe://eacp.test/ns/eacp/sa/eacp-worker` พร้อม JWT-SVID อายุหนึ่งชั่วโมง และให้ trust bundle แก่ Fake ERP Fake ERP ยอมรับ JWT-SVID ของ worker
สำหรับ audience `fakeerp-api` เป็น Bearer และมี OAuth client `eacp-worker-spiffe` ที่ยืนยันตัวตนด้วย JWT-SVID สำหรับ audience `fakeerp-token`
secret ของ worker ผูก `fakeerp-spiffe` (`value_spiffe`) และ `fakeerp-spiffe-oauth` (`client_assertion_spiffe`) ของ tenant Cyberdyne และ SPIFFE CSI
driver mount socket ของ agent เข้าไปใน pod ของ worker เพียงตัวเดียว ไม่มี secret สำหรับ binding ใดเลย

- **S0** bootstrap tenant Cyberdyne และ policy ที่อนุญาตงาน ERP ปกติ
- **S1** connector สองตัวคือ `erp` และ `erp-oauth` หนึ่งตัวต่อ binding การซื้อผ่านแต่ละตัวจบด้วย `SUCCEEDED` พร้อมใบสั่งซื้อหนึ่งใบ ในนาม principal
  `spiffe:spiffe://eacp.test/ns/eacp/sa/eacp-worker` (audit ของ ERP บันทึก SHA-256 ของ SVID ไม่เคยบันทึก SVID) และ `oauth:eacp-worker-spiffe`
  (token ที่สร้างโดยใช้ SVID เป็น client assertion)
- **S2** ไม่มี token ที่ออก, SVID ที่ audit ไว้ หรือ JWT ที่ระบุ SPIFFE ID ของ worker ปรากฏในคำตอบของ API, log ของ service หรือ dump ของฐานข้อมูล

ไม่ได้แสดงการซื้อที่ถูกพักไว้ เพราะ worker ใช้ SVID ที่ถืออยู่ต่อได้จนครบ TTL (ที่นี่หนึ่งชั่วโมง) การลบ registration entry จึงหยุดการซื้อครั้งถัดไป
ภายในเวลาของ demo ไม่ได้ `internal/worker` ครอบคลุมทุกเส้นทางความล้มเหลวกับ Workload API ปลอม

### Token-exchange demo (เฉพาะ Kubernetes)

`TestTokenExchangeDemo` แสดง Phase 24f (ADR-019 Rev 1.5) และรันผ่าน `scripts/k8s-e2e.sh` เท่านั้น เพราะ subject token คือ token ที่ project มาของ
cluster และ JWT-SVID ของ SPIRE สำหรับ development Fake ERP รัน STS (audience `fakeerp-sts`) ที่เชื่อถือ issuer ทั้งสอง และให้บริการ
`generateAccessToken` สำหรับ `eacp-erp@eacp-demo.iam.gserviceaccount.com` secret ของ worker ผูก `fakeerp-sts` ของ tenant Tyrell (token ที่ project
มา แลกโดยไม่ยืนยันตัวตนของ client) และ `fakeerp-sts-sa` (แลก JWT-SVID แล้วใช้ federated token impersonate service account) ไม่มี secret สำหรับ
binding ใดเลย

- **X0** bootstrap tenant Tyrell และ policy ที่อนุญาตงาน ERP ปกติ
- **X1** connector สองตัวคือ `erp` และ `erp-oauth` หนึ่งตัวต่อ binding การซื้อผ่านแต่ละตัวจบด้วย `SUCCEEDED` พร้อมใบสั่งซื้อหนึ่งใบ ในนาม principal
  `sts:system:serviceaccount:eacp:eacp-worker` และ `sa:eacp-erp@eacp-demo.iam.gserviceaccount.com` audit ของ ERP แสดงว่า subject ทั้งสองถูกแลก
  แต่ละตัวพร้อม SHA-256 ของ subject token (ไม่เคยบันทึก token) และการ impersonate
- **X2** ไม่มี token ที่ออก (ที่แลก, federated หรือ impersonate) และไม่มี subject token หรือ JWT ที่ระบุ subject ใดปรากฏในคำตอบของ API, log ของ
  service หรือ dump ของฐานข้อมูล

### AWS demo (เฉพาะ Kubernetes)

`TestAWSDemo` แสดง Phase 24g (ADR-019 Rev 1.6) และรันผ่าน `scripts/k8s-e2e.sh` เท่านั้นด้วยเหตุผลเดียวกัน Fake ERP รัน AWS STS สำหรับ role
`arn:aws:iam::000000000000:role/eacp-erp` ที่เชื่อถือ issuer ทั้งสอง และตรวจ SigV4 (`us-east-1`, `execute-api`) บน API ของตัวเอง secret ของ worker
ผูก `fakeerp-aws` ของ tenant Soylent (token ที่ project มา, session `eacp-worker-k8s`) และ `fakeerp-aws-spiffe` (JWT-SVID สำหรับ
`sts.amazonaws.com`, session `eacp-worker-spiffe`) ไม่มี secret สำหรับ binding ใดเลย

- **Y0** bootstrap tenant Soylent และ policy ที่อนุญาตงาน ERP ปกติ
- **Y1** connector สองตัวคือ `erp` และ `erp-oauth` หนึ่งตัวต่อ binding การซื้อผ่านแต่ละตัวจบด้วย `SUCCEEDED` พร้อมใบสั่งซื้อหนึ่งใบ ในนาม principal
  `aws:arn:aws:sts::000000000000:assumed-role/eacp-erp/eacp-worker-k8s` และ `…/eacp-worker-spiffe` การ execute แต่ละครั้งเซ็นด้วย key ที่ STS
  ออกให้ session นั้น audit ของ ERP แสดง key id, SHA-256 ของ session token และ SHA-256 ของ subject token ไม่เคยบันทึก token หรือ key
- **Y2** ไม่มี web identity token, secret key (ซึ่งคำนวณคืนจาก key id แต่ละตัวที่ audit ไว้) หรือ session token ปรากฏในคำตอบของ API, log ของ
  service หรือ dump ของฐานข้อมูล

## Agent Studio demo

`TestStudioDemo` แสดง Phase 27a และ 27b (ADR-033) และรันด้วย `DEMO=S` บน compose เท่านั้น Fake MCP ตัวที่สอง `fakemcp-hr` (`fakemcp-hr:8091` บนเครือข่าย
`erp` ที่มีเพียง worker เข้าได้ และมี token ของตัวเอง) มี tool เดียวคือ `get_leave_balance` ซึ่งตอบ `{"days": N}` ตาม id ของพนักงาน (E-1 มี 12 วัน)
และตอบ tool error สำหรับ `ERR` `agent-runtime` รันใต้ compose profile `studio` บนเครือข่าย `agents` เท่านั้น ไม่มี URL ของฐานข้อมูล และไม่มี secret
ของ connector หรือ provider demo เขียน master และ key ของมันลงใน volume `studio_runtime` ผ่าน stdin แล้วจึงเริ่มมัน

- **S0** bootstrap tenant Wonka stella (`studio_author`) และ hana อยู่ในกลุ่ม HR ที่มี lena เป็นหัวหน้า finn (`studio_author`) อยู่ในฝ่ายการเงิน ส่วน carol ไม่อยู่ทั้งสองกลุ่ม service principal `studio-runtime` ถือ `studio_runtime`
  เพียงบทบาทเดียว policy อนุญาตการค้นข้อมูล HR แบบอ่านอย่างเดียว
- **S1** erin ลงทะเบียน connector `hr-mcp` scanner ค้นพบ `get_leave_balance` erin รับรองให้เป็น `READ_ONLY` ลองครั้งเดียว และเก็บผลลัพธ์ 10 นาที
  rita เปิดใช้ contract
- **S2** agent-runtime เริ่มด้วย master แบบสุ่มขนาด 32 ไบต์ที่สร้างใหม่ และ key ของตัวเอง
- **S3** stella บันทึก template ยอดวันลา PostgreSQL คำนวณ capability ของมัน stella อนุมัติ agent ของตัวเองไม่ได้ rita อนุมัติให้ run ที่เริ่มก่อน key
  ได้รับอนุมัติจะล้มเหลวแบบปิด (`credential_pending`) และไม่ส่ง action ใดเลย runtime เสนอ key ของ version นั้น rita อนุมัติจากคิวของ Studio
- **S4** run ของ stella สำหรับ E-1 ตอบว่า "You have 12 days of leave left." step ของมันเป็น action ปกติของ agent version นั้น ในนามของ stella
  ผ่าน `hr-mcp.get_leave_balance` carol ซึ่งอยู่นอก HR รัน agent ไม่ได้ (HTTP 403) rita เห็น run และ step ของมันแต่ไม่เห็นคำตอบ fakemcp-hr
  บันทึกการเรียกหนึ่งครั้ง
- **S5** run สำหรับ `ERR` ได้ tool error ซึ่งไม่ได้รับรองว่าไม่มีผล แต่การเรียกแบบ `READ_ONLY` ไม่มีผลที่ต้องสงสัย เมื่อใช้ความพยายามครั้งเดียวไปแล้ว
  action จึงจบด้วย `FAILED` และ run จบด้วย `FAILED` (`action_failed`) โดยไม่มีคำตอบ
- **S6** Hub: hana ซึ่งอยู่ใน HR รัน agent ไม่ได้ก่อนที่มันจะถูกเผยแพร่ (HTTP 403) stella เสนอมันให้ HR แต่เผยแพร่เองไม่ได้ (HTTP 409)
  rita ก็ไม่ได้ (HTTP 403) lena หัวหน้า HR เผยแพร่มัน hana พบมันใน Hub และรันในนามของตัวเอง: เวอร์ชันเดียวกัน subject และคำตอบของเธอเอง
  finn ไม่เห็นอะไรจนกว่า rita จะเผยแพร่ให้ทั้งองค์กร (lena ทำไม่ได้ HTTP 403) สำเนาของ finn ในฝ่ายการเงินอยู่ในสถานะ `waiting_for_approval`
  และรันไม่ได้ (HTTP 409)
- **S7** otto เพิกถอน key ของ Studio ทั้งหมด run ถัดไปล้มเหลวแบบปิด (`credential_pending`)
- **S8** master, key ที่ derive ทุกตัว และ key ของ runtime ไม่ปรากฏในคำตอบของ API, log ของ service (รวมของ runtime) หรือ dump ของฐานข้อมูลใดเลย
  คำตอบไม่อยู่ใน log บรรทัดใดและไม่อยู่ใน journal และ audit chain ตรวจสอบผ่าน

runtime เข้าถึงได้เพียง API และมีเพียงมันที่ถือ master ของ Studio (`test/security` `TestTheRuntimeReachesOnlyTheAPI`,
`TestOnlyTheRuntimeHoldsTheStudioMaster`)

## ขอบเขต

credential ของ demo, id ของ tenant และ token ของ Fake ERP, Fake MCP และ Fake A2A เป็นค่าสำหรับ development บนเครื่อง (ดู
`deployments/docker/secrets`) ข้อยืนยันทั้งหมดใช้ได้กับ conforming deployment เท่านั้น (ADR-001 §3a) ระบบปลายทางออก credential ที่มีอภิสิทธิ์ให้
EACP worker เท่านั้น และ agent ไม่มีเส้นทางเครือข่ายไปถึง EACP ไม่ได้อ้างว่าทำงานแบบ exactly-once ผลลัพธ์เป็น idempotent เมื่อระบบปลายทางรองรับ,
effectively-once เมื่อ reconcile ได้ และ at-most-once เมื่อการ retry ไม่ปลอดภัย (MASTER_PLAN §21)
