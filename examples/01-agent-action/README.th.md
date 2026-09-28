[English](README.md) | [ไทย](README.th.md)

# 01: ใบสั่งซื้อของ agent ตั้งแต่ต้นจนจบ

AI agent ชื่อ `procurement-bot` ต้องการออกใบสั่งซื้อมูลค่า 250,000 บาทในระบบ ERP แต่ agent ไม่มี credential ของ ERP
และเชื่อมต่อไปยัง ERP ไม่ได้เลย จึงต้องขอผ่าน EACP ซึ่งจะตัดสินใจ รอคนที่ต้องเห็นชอบ สั่งทำรายการครั้งเดียว
และเก็บหลักฐานของทุกขั้นตอนไว้

ตัวอย่างนี้ใช้ `curl` เล่นทุกบทบาท ทั้ง agent, ผู้อนุมัติสองคน (amy และ ben) และ operator (otto) ที่อ่านหลักฐานภายหลัง

## วิธีรัน

ต้องมี `curl` และ `jq`, stack ที่รันอยู่ และต้องรัน `examples/setup.sh` มาแล้วหนึ่งครั้ง ([examples/README.th.md](../README.th.md))

```bash
bash examples/01-agent-action/run.sh
```

## ผลลัพธ์ที่ควรเห็น

id ของ action และเลขอ้างอิงจาก ERP จะเปลี่ยนทุกครั้งที่รัน ส่วนที่เหลือจะเหมือนเดิม:

```text
== 1. The agent submits a 250,000 THB purchase order
action e29da5e6-a871-4896-9446-3ea06e045711 is PENDING_APPROVAL: the policy escalates high-value purchases to two approvers

== 2. The agent retries the same request: EACP answers with the same action
same idempotency key → action e29da5e6-a871-4896-9446-3ea06e045711 again, nothing new is created

== 3. Two approvers vote
amy:  request PENDING
ben:  request GRANTED

== 4. EACP releases the action and the worker executes it against the Fake ERP
state: AUTHORIZED
state: SUCCEEDED

== 5. The evidence, as an operator sees it
governance: escalate under policy v1 (a high-value purchase needs two approvers)
governance: escalate under policy v1 (a high-value purchase needs two approvers)
approval: GRANTED, quorum 2, 2 votes, grant consumed by this action: true
attempt 1: succeeded, external reference PO-e29da5e6-a871-4896-9446-3ea06e045711
journal: RECEIVED → PENDING_APPROVAL → AUTHORIZED → QUEUED → LEASED → EXECUTING → SUCCEEDED
audit chain: 100 entries, verified: true

Example 01 passed.
```

## เกิดอะไรขึ้นบ้าง

1. **ส่งคำขอ** agent ส่ง `POST /v1/actions` ด้วย EACP key ของตัวเองพร้อม header `Idempotency-Key` คำขอระบุ operation
   (`purchase_high_value`), target (`erp`), tool (`erp.create_po`) และ payload EACP ตรวจ agent กับ registry
   (agent version ต้อง active และ tool ต้องอยู่ใน allowlist) แล้วถาม policy decision point กฎ `high-value` ของ policy
   ตอบว่า `escalate` action จึงรออยู่ในสถานะ `PENDING_APPROVAL` พร้อมคำขออนุมัติที่ต้องใช้ผู้อนุมัติสองคน
2. **retry** agent retry เป็นเรื่องปกติ เพราะเครือข่ายล่มและ process รีสตาร์ตได้ idempotency key เดิมกับ body เดิม
   จะได้ action เดิมกลับมา การ retry จึงไม่มีทางสร้างใบสั่งซื้อใบที่สอง ส่วน key เดิมกับ body ที่ต่างไปจะถูกปฏิเสธด้วย
   `409 idempotency_conflict`
3. **อนุมัติ** amy และ ben โหวตผ่าน `POST /v1/approvals/{id}/votes` โหวตแรกทำให้คำขอยังเป็น `PENDING` โหวตที่สอง
   ครบ quorum จึงเป็น `GRANTED` grant ผูกกับ payload ที่ policy เห็นแบบตรงตัว (ผ่าน digest) และใช้ได้ครั้งเดียว
   การแบ่งแยกหน้าที่ถูกบังคับใน PostgreSQL คือเฉพาะมนุษย์ที่โหวตได้ subject ของ action (sam) และเจ้าของ agent
   อนุมัติไม่ได้ และไม่มีใครโหวตซ้ำได้
4. **ปล่อยและทำรายการ** ที่ release boundary EACP ถาม policy อีกครั้ง คำตอบยังคงเป็น `escalate` และ grant ตอบโจทย์นั้น
   action จึงเป็น `AUTHORIZED` จากนั้น execution worker ซึ่งเป็น process เดียวที่ถือ credential ของ ERP จะ claim action
   บันทึก dispatch intent ก่อนเรียกสิ่งใด เรียก Fake ERP หนึ่งครั้ง แล้วบันทึกผลเป็น `SUCCEEDED` พร้อมเลขอ้างอิงใบสั่งซื้อ
   จาก ERP
5. **หลักฐาน** operator อ่าน `GET /v1/actions/{id}/evidence` ซึ่งมีคำตัดสินของ policy ทั้งสองครั้ง (ตอนส่งคำขอและตอนปล่อย),
   การอนุมัติพร้อมโหวตและ grant ที่ถูกใช้แล้ว, ทุก attempt และรายการ journal ที่ติดตามการเปลี่ยนสถานะแต่ละครั้ง journal
   เป็น hash chain และ EACP ตรวจสอบ chain ขณะอ่านหลักฐาน

## สิ่งที่ตัวอย่างนี้แสดง

- agent ไม่เคยแตะ credential ของ ERP เพราะ execution worker เป็นผู้ถือ (ADR-001)
- การตัดสินใจของมนุษย์ผูกกับ payload แบบตรงตัวและใช้ได้ครั้งเดียว (ADR-005)
- การ retry ปลอดภัย: idempotency key หนึ่งตัวได้ action หนึ่งรายการ (ADR-004)
- ทุกขั้นทิ้งหลักฐานที่ตรวจสอบย้อนหลังได้ ไม่ใช่แค่บรรทัดใน log
