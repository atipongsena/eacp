[English](README.md) | [ไทย](README.th.md)

# 03: Governance-as-Code

ทีม platform เก็บ connector, tool, contract และ agent ไว้ใน Git รีวิวแบบเดียวกับโค้ด และ deploy แบบเดียวกับโค้ด EACP รับการเปลี่ยนแปลง
แบบนี้ได้ แต่ไม่ถือว่าเป็นอำนาจใหม่ bundle เป็นเพียงแผน ทุกขั้นยังผ่านกฎของ registry ชุดเดียวกับ API และไม่มีอะไรมีผลจนกว่า
คนที่สองจะอนุมัติ

[`bundle/eacp.yml`](bundle/eacp.yml) ประกาศ connector ชื่อ `ledger` (Fake ERP) ที่มี tool แบบอ่านอย่างเดียวหนึ่งตัวคือ
`get_balance` พร้อม contract ของมัน และ agent ชื่อ `ledger-bot` ที่ได้รับอนุญาตให้ใช้ tool นั้น agent มี sam เป็นเจ้าของ
และควรอยู่ในสถานะ `ACTIVE`

## วิธีรัน

ต้องมี Go และ `jq`, stack ที่รันอยู่ และต้องรัน `examples/setup.sh` มาแล้วหนึ่งครั้ง ([examples/README.th.md](../README.th.md))
สคริปต์รัน `eacpctl` จาก source ด้วย `go run`

```bash
bash examples/03-governance-as-code/run.sh
```

## ผลลัพธ์ที่ควรเห็น

บน tenant ใหม่ (id ของ change set เปลี่ยนทุกครั้งที่รัน):

```text
== 1. Validate the bundle (offline: eacpctl resolves the YAML, the target and its variables)
bundle ledger, target examples: valid

== 2. Plan: what would change (a dry run records nothing)
  submit   create connector.ledger
  submit   create tool.ledger.get_balance
  submit   propose contract.ledger.get_balance
  submit   create agent.ledger-bot
  submit   create version.ledger-bot
  submit   propose allowlist.ledger-bot
  approve  activate contract.ledger.get_balance
  approve  activate allowlist.ledger-bot
  approve  transition version.ledger-bot

== 3. Erin (registry editor) deploys: the submit stage runs, nothing is active yet
change set bdba9ca4-f58e-4177-a867-26172f2bef83 is SUBMITTED

== 4. Rita (registry approver, a second person) approves: the change set is applied
change set bdba9ca4-f58e-4177-a867-26172f2bef83 is APPLIED

== 5. Drift: the registry matches the bundle's last applied change set
  in_sync agent.ledger-bot
  in_sync connector.ledger
  in_sync tool.ledger.get_balance
  in_sync version.ledger-bot

Example 03 passed.
```

ถ้ารันซ้ำ bundle จะตรงกับ registry อยู่แล้ว แผนจะบอกว่า `nothing to change`, `deploy` ไม่บันทึก change set,
ขั้นที่ 4 ถูกข้าม และ drift ยังเป็น `in_sync`

## เกิดอะไรขึ้นบ้าง

1. **ตรวจความถูกต้อง** `eacpctl bundle validate` อ่าน YAML, resolve target (`examples` ซึ่งผูกกับ tenant ของตัวอย่าง)
   และตัวแปร แล้วพิมพ์ bundle เป็น JSON ซึ่งเป็นรูปแบบเดียวที่ API รับ ขั้นนี้ไม่ต้องใช้ server
2. **วางแผน** `eacpctl bundle plan --dry-run` ส่ง bundle ไปที่ API ซึ่งเทียบกับ registry ของ tenant ใน snapshot เดียว
   และตอบกลับเป็นขั้นตอนที่จะทำ แต่ละขั้นมี stage ขั้นใน stage `submit` สร้าง object และยื่นข้อเสนอ (proposal)
   ขั้นใน stage `approve` เปิดใช้ข้อเสนอเหล่านั้นและย้าย agent version ไปเป็น `ACTIVE` การ dry run ไม่บันทึกอะไร
3. **deploy** erin ซึ่งเป็น `registry_editor` รัน `eacpctl bundle deploy` API วางแผนอีกครั้ง บันทึก change set พร้อม
   digest ของสถานะที่ต้องการและของ registry ณ ตอนวางแผน แล้ว erin ส่ง (submit) change set นั้น การ submit ตรวจก่อนว่า
   registry ยังตรงกับ digest ของแผน แล้วจึงรัน stage `submit` ในนามของ erin ผ่าน transaction ของ registry เอง
   connector, tool และ agent ใหม่ถูกสร้างขึ้น แต่ contract และ allowlist ยังเป็นเพียงข้อเสนอ และ agent version ยังไม่ active
   การ submit ปิดผนึก change set ด้วย digest ของสิ่งที่สร้างขึ้น
4. **อนุมัติ** rita ซึ่งเป็น `registry_approver` และเป็นคนละคนกัน รัน `eacpctl bundle approve` EACP ตรวจ digest
   ที่ปิดผนึกไว้ (ถ้ามีอะไรเปลี่ยนหลัง submit ถือว่า change set ล้าสมัยและจะไม่รันอะไรเลย) แล้วจึงรัน stage `approve`
   ในนามของ rita ทุกขั้นยังต้องผ่าน trigger ของตัวเอง เช่น ผู้เขียน allowlist เปิดใช้ allowlist ของตัวเองไม่ได้ change set
   จึงเป็น `APPLIED`
5. **drift** `eacpctl bundle drift` เทียบ registry กับ change set ล่าสุดที่ apply แล้วของ bundle ทุก address เป็น
   `in_sync` ถ้ามีการเปลี่ยนแปลงนอก bundle ไม่ว่าผ่าน API หรือ console จะปรากฏที่นี่เป็น `modified` หรือ `missing`

## สิ่งที่ตัวอย่างนี้แสดง

- การเปลี่ยน registry รีวิวแบบโค้ดได้และยังต้องใช้สองคน ผู้ submit ไม่มีทางเป็นผู้อนุมัติ (ADR-026)
- bundle ไม่เพิ่มอำนาจใดๆ trigger ของ registry ใน PostgreSQL ตัดสินทุกขั้นแบบเดียวกับที่ทำกับ API
- แผนผูกกับ registry ณ ตอนที่วางแผน change set ที่ registry เปลี่ยนไปแล้วจะไม่ถูกรัน
- drift อ่านอย่างเดียว มีหน้าที่รายงาน ไม่เคยซ่อมแซม
