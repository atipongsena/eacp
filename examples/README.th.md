[English](README.md) | [ไทย](README.th.md)

# ตัวอย่าง

โปรแกรมเล็กๆ สามตัวที่ใช้ EACP ในแบบเดียวกับที่ agent, ผู้อนุมัติ, operator และทีม platform ใช้งานจริง ทุกตัวรันกับ
stack `docker compose` บนเครื่อง พิมพ์สิ่งที่เกิดขึ้นทีละขั้น และจบด้วย exit code ที่ไม่ใช่ศูนย์ถ้า EACP ไม่ทำงานตามที่อธิบายไว้
ตัวอย่างทั้งหมดรันใน CI ทุกคืน จึงถูกต้องตรงกับโค้ดอยู่เสมอ

| ตัวอย่าง | แสดงอะไร | ต้องมี |
|---|---|---|
| [01-agent-action](01-agent-action/README.th.md) | ใบสั่งซื้อมูลค่าสูงของ agent: governance, การอนุมัติสองคน, การทำงานกับ Fake ERP, การ retry แบบ idempotent และหลักฐานที่ตรวจสอบแล้ว | `curl`, `jq` |
| [02-llm-gateway](02-llm-gateway/README.th.md) | การเรียก LLM ผ่าน gateway ของ EACP ด้วย Anthropic SDK ตัวทางการ: model ที่อยู่ใน allowlist, model ที่ถูกปฏิเสธ และ ledger ของ gateway | Python 3.10 ขึ้นไป |
| [03-governance-as-code](03-governance-as-code/README.th.md) | bundle YAML ที่ประกาศ connector และ agent ถูก plan เป็น change set และจะมีผลก็ต่อเมื่อคนที่สองอนุมัติแล้ว | Go, `jq` |

## ก่อนเริ่ม

ต้องมี Docker พร้อม Compose v2, Go (เวอร์ชันตามที่ระบุใน `go.mod`), Python 3 และ Bash ถ้าใช้ Windows ให้ใช้ Git Bash
ที่ root ของ repository ให้สร้าง secret สำหรับ fake service บนเครื่องก่อน (เป็นไฟล์ที่ git ไม่ติดตาม) แล้วจึงเริ่ม stack และรอจนทุก
service พร้อม:

```bash
python3 deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --build --wait
```

จากนั้นเตรียม tenant สำหรับตัวอย่าง ครั้งเดียว:

```bash
bash examples/setup.sh
```

`setup.sh` สร้าง tenant ชื่อ `examples` และเตรียมทุกอย่างที่ตัวอย่างทั้งสามต้องใช้ โดยคุยกับ API เหมือน client ทั่วไป
ยกเว้นขั้นแรก คือการสร้าง tenant และ admin สองคน ซึ่งเป็นคำสั่ง bootstrap (`eacpctl tenant create`) ที่รันใน container
`migrate` ของ stack

| ใคร | บทบาท | ใช้ใน |
|---|---|---|
| alice, bob | `admin` (tenant ต้องมีสองคนเสมอ) | setup |
| erin | `registry_editor` | 03: ส่ง change set |
| rita, ravi | `registry_approver` | 03: rita อนุมัติ change set |
| otto, olga | `operator` | 01: อ่านหลักฐาน, 02: อ่าน ledger ของ LLM |
| amy, ben | `approver` | 01: ผู้อนุมัติสองคน |
| sam | ไม่มี | เจ้าของ agent และเป็น subject ของทุก action |
| procurement-bot | agent | 01 และ 02 |

นอกจากนี้ยังเพิ่ม policy (การซื้อมูลค่าสูงต้องมีผู้อนุมัติสองคน), connector ของ Fake ERP พร้อม tool สองตัว (`create_po` สำหรับตัวอย่าง 01 และ `create_po_eventual`
ซึ่ง ERP จะแสดงใบสั่งซื้อให้เห็นภายหลัง ใช้ในสถานการณ์สำหรับภาพหน้าจอ),
model LLM ปลอมสองตัว (`sonnet` ที่ agent ใช้ได้ และ `opus` ที่ใช้ไม่ได้), ราคาของ model และงบประมาณของ agent

key ทุกตัวถูกเขียนลง `examples/.env` ซึ่ง git ไม่ติดตาม และ `setup.sh` ไม่พิมพ์ key ออกมาเลย จึงควรรักษาไว้แบบนั้น
อย่านำไฟล์นี้ไปวางที่ไหน การรัน `setup.sh` ซ้ำนั้นปลอดภัย ถ้า `.env` ยังใช้ได้ มันจะบอกแล้วไม่ทำอะไรเพิ่ม

## วิธีรัน

```bash
bash examples/01-agent-action/run.sh
bash examples/02-llm-gateway/run.sh
bash examples/03-governance-as-code/run.sh
```

ตัวอย่างแต่ละตัวรันซ้ำได้กี่ครั้งก็ได้ ถ้า jq ไม่อยู่ใน `PATH` ให้ระบุตำแหน่งผ่าน `JQ`
(`JQ=/path/to/jq bash examples/01-agent-action/run.sh`)

## การล้างข้อมูล

```bash
docker compose down -v
rm examples/.env
```

`down -v` ลบ volume ของฐานข้อมูล ซึ่งรวมถึง tenant ของตัวอย่างด้วย ให้ลบ `examples/.env` ด้วย เพราะ key ในไฟล์ใช้ไม่ได้แล้ว
เมื่อ tenant หายไป และการรัน `setup.sh` ครั้งถัดไปจะเขียนไฟล์ใหม่ให้
