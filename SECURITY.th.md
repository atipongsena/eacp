[English](SECURITY.md) | [ไทย](SECURITY.th.md)

# นโยบายด้านความปลอดภัย

EACP ทำหน้าที่เป็นขอบเขตความปลอดภัย เพราะเป็นตัวตัดสินว่า action ของ AI agent จะแตะระบบขององค์กรได้หรือไม่ ช่องโหว่ที่ทำให้
action เลี่ยง governance ได้ ถูกทำซ้ำสองครั้ง ทำงานได้โดยไม่มีการอนุมัติ หรือทำให้ credential รั่วไหล คือสิ่งที่เราอยากทราบที่สุด

## การรายงานช่องโหว่

โปรดรายงานแบบส่วนตัวผ่าน **private vulnerability reporting** ของ GitHub โดยเปิดแท็บ **Security** ของ repository แล้วเลือก
**Report a vulnerability** ห้ามเปิด issue, pull request หรือ discussion สาธารณะสำหรับช่องโหว่ที่สงสัย

รายงานที่มีประโยชน์ควรบอกว่า:

- ผู้โจมตีทำอะไรได้ และทำจากตำแหน่งไหน (agent ที่มี key ของตัวเอง, operator หรือเครื่องที่อยู่ในเครือข่ายเดียวกัน)
- ขั้นตอนหรือ test ที่ทำให้เกิดซ้ำได้ โดยรันกับ `docker compose up` หรือกับ examples
- commit หรือ release ที่คุณทดสอบ

คุณจะได้รับการตอบรับภายในหนึ่งสัปดาห์ เราจะตกลงวันเปิดเผยข้อมูลร่วมกับคุณ ให้เครดิตใน release notes เว้นแต่คุณไม่ต้องการ
และจะเผยแพร่ GitHub security advisory พร้อมการแก้ไข

## เวอร์ชันที่รองรับ

| เวอร์ชัน | รองรับ |
|---|---|
| `main` | รองรับ |
| release ล่าสุด | รองรับ |
| release ที่เก่ากว่า | ไม่รองรับ ให้อัปเกรดเป็น release ล่าสุด |

## ขอบเขตที่ครอบคลุม

- control plane, execution worker, LLM gateway, `eacpctl` และ AGT sidecar PDP
- schema ของฐานข้อมูล trigger และนโยบาย Row-Level Security (`migrations/`)
- Helm chart และ network policy (`deployments/helm/`)
- สิ่งที่รับประกันไว้ใน threat model (`docs/security/THREAT_MODEL.th.md`) และ [ADR-001](docs/adr/ADR-001-product-boundary-and-enforcement-point.md)
  สำหรับ conforming deployment ตามนิยามใน ADR-001 §3a

## สิ่งที่ไม่นับเป็นช่องโหว่

- **secret สำหรับ development ที่อยู่ใน repository** ไฟล์ใน `deployments/docker/secrets/` และ credential manifest ใน `deployments/k8s/`
  เป็นค่าของ fake service ที่รันบนเครื่อง (Fake ERP, Fake LLM, Fake A2A และ Vault สำหรับ dev) ใช้ที่อื่นไม่ได้ และห้ามนำไปใช้ซ้ำ
- **fake service** (`cmd/fakeerp`, `cmd/fakellm`, `cmd/fakea2a`, `cmd/fakemcp`) มีไว้สำหรับ test และ demo เท่านั้น และไม่รวมอยู่ใน release
- **deployment ที่ไม่ใช่ conforming** เช่น agent ถือ credential ของ connector เอง หรือมีเส้นทางเครือข่ายตรงไปยังระบบปลายทาง ในกรณีนี้
  EACP บังคับขอบเขตของตัวเองไม่ได้ ADR-001 §3a อธิบายเหตุผลไว้
