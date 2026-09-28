[English](README.md) | [ไทย](README.th.md)

# 02: เรียก LLM ผ่าน gateway ของ EACP

agent ไม่ได้เรียกแค่ระบบขององค์กร แต่เรียก language model ด้วย LLM gateway ของ EACP ทำให้การเรียกเหล่านั้นอยู่ใต้การควบคุม
ชุดเดียวกับ action อื่นๆ คือ model ต้องอยู่ใน allowlist ของ agent, policy ต้องอนุญาต, งบประมาณต้องพอ, kill switch
หยุดการเรียกได้ และทุกการเรียกถูกบันทึกลง ledger พร้อมค่าใช้จ่าย

agent ในตัวอย่างนี้เป็นโค้ด Python ธรรมดาที่ใช้
[Anthropic SDK](https://github.com/anthropics/anthropic-sdk-python) ตัวทางการ มีแค่สองอย่างที่เปลี่ยน คือ base URL
ชี้ไปที่ gateway และ API key เป็น EACP key ของ agent เอง key ของผู้ให้บริการอยู่ใน gateway เท่านั้น ใน stack นี้ผู้ให้บริการคือ
Fake LLM ซึ่งตอบตาม Anthropic Messages API ด้วยข้อความตายตัวและจำนวน token ที่แน่นอน

## วิธีรัน

ต้องมี Python 3.10 ขึ้นไป (ขั้นต่ำของ SDK), stack ที่รันอยู่ และต้องรัน `examples/setup.sh` มาแล้วหนึ่งครั้ง
([examples/README.th.md](../README.th.md)) สคริปต์จะสร้าง virtual environment ที่ `examples/02-llm-gateway/.venv`
ในการรันครั้งแรก และติดตั้ง SDK ตามเวอร์ชันที่ pin ไว้ใน `requirements.txt`

```bash
bash examples/02-llm-gateway/run.sh
```

## ผลลัพธ์ที่ควรเห็น

```text
== 1. The agent asks an allowlisted model (sonnet) through the gateway
answer: 'Hello from fakellm.'
usage: 21 input and 20 output tokens

== 2. The agent asks a model outside its allowlist (opus)
refused before anything reached the provider: HTTP 403, permission_error

== 3. An operator reads the gateway's ledger
opus: DENIED model_not_in_allowlist, no cost
sonnet: SETTLED succeeded, 0.000363 USD

Example 02 passed.
```

## เกิดอะไรขึ้นบ้าง

1. **การเรียกที่ได้รับอนุญาต** `client.messages.create(model="sonnet", ...)` ถูกส่งไปที่ gateway ซึ่งตรวจ key ของ agent
   แล้วถาม policy decision point โดยส่งเฉพาะ metadata ของการเรียก คือ operation `llm.generate`, ชื่อ model,
   เป็น stream หรือไม่, เพดาน output และขนาดคำขอ PDP ไม่เคยเห็น prompt จากนั้น PostgreSQL รับการเรียกเข้าระบบ
   (admission) โดยตรวจว่า model อยู่ใน allowlist ของ agent, ไม่มี kill switch ครอบคลุม และงบประมาณของ agent
   จองค่าใช้จ่ายตามที่ PostgreSQL ประเมินได้ เมื่อผ่านทั้งหมดแล้ว gateway จึงส่งคำขอต่อไปครั้งเดียวด้วย key
   ของผู้ให้บริการที่ agent ไม่เคยเห็น คำตอบถูกส่งกลับโดยไม่แก้ไข และการเรียกถูกปิดยอด (settle) ด้วยจำนวน token
   จริงจากผู้ให้บริการ
2. **การเรียกที่ถูกปฏิเสธ** `opus` มีอยู่ใน tenant แต่ไม่อยู่ใน allowlist ของ agent นี้ admission จึงปฏิเสธและไม่มีอะไรถูกส่งไป
   ถึงผู้ให้บริการ SDK โยน `PermissionDeniedError` ในรูปแบบ error ของ Anthropic agent ไม่ต้องจัดการเป็นพิเศษ เพราะเป็น
   error แบบเดียวกับที่ผู้ให้บริการส่งกลับมา
3. **ledger** operator เรียก `GET /v1/llm-calls?agent=...` แต่ละการเรียกมีสถานะ คือ `SETTLED` พร้อมค่าใช้จ่ายที่
   PostgreSQL คำนวณจากตารางราคาของ tenant หรือ `DENIED` พร้อมเหตุผลและไม่มีค่าใช้จ่าย ledger ไม่เก็บ prompt, คำตอบ
   หรือ key ใดๆ

## สิ่งที่ตัวอย่างนี้แสดง

- agent ใช้ SDK มาตรฐานได้โดยไม่ต้องแก้ นอกจาก base URL และ key ของตัวเอง (ADR-031)
- credential ของผู้ให้บริการอยู่ใน gateway agent ถือเพียง EACP key ของตัวเอง
- allowlist, งบประมาณ และ kill switch มีผลกับการเรียก model ก่อนที่จะมีอะไรออกจาก gateway
- ค่าใช้จ่ายถูกคำนวณใน PostgreSQL จากตารางราคา และไม่มีการเก็บเนื้อหา
