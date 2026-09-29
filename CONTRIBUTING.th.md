[English](CONTRIBUTING.md) | [ไทย](CONTRIBUTING.th.md)

# การมีส่วนร่วมกับ EACP

ขอบคุณที่สนใจร่วมพัฒนา EACP เป็นตัวตัดสินว่า AI agent จะลงมือกับระบบขององค์กรได้หรือไม่ กฎจึงเข้มงวด การเปลี่ยนแปลงจะได้รับการยอมรับ
เมื่อรักษาการรับประกันทุกข้อไว้และพิสูจน์ด้วย test หน้านี้อธิบายวิธีเตรียมเครื่อง กฎที่ต้องทำตาม และวิธีรัน test แต่ละระดับ

## ก่อนเริ่ม

- ถ้าเป็น bug ให้เปิด issue พร้อมขั้นตอนที่ทำให้เกิดซ้ำได้ ถ้าสงสัยว่าเป็นช่องโหว่ อย่าเปิด issue ให้ทำตาม [SECURITY.th.md](SECURITY.th.md)
- ถ้าเป็นความสามารถใหม่หรือการเปลี่ยนการรับประกัน ให้เปิด issue ก่อน การเปลี่ยนแปลงเชิงบรรทัดฐานต้องมี ADR ใน
  [`docs/adr/`](docs/adr/README.md) ก่อนโค้ด และตกลงเรื่อง ADR กันก่อนมีคนลงมือเขียนจะง่ายกว่า
- อ่าน [AGENTS.md](AGENTS.md) ซึ่งเขียนไว้สำหรับทั้ง AI coding assistant และคน และรวบรวมกฎที่ทุกการเปลี่ยนแปลงต้องรักษา
- ถ้าอยากรู้ว่าคนใช้ EACP กันอย่างไรก่อนลงมือแก้ อ่าน[คู่มือการใช้งาน](docs/USER_GUIDE.th.md)

## การเตรียมเครื่อง

ต้องมี Go (เวอร์ชันตามที่ระบุใน `go.mod`), Docker พร้อม Compose v2, Python 3 และ Bash (บน Windows ใช้ Git Bash) ต้องมี Node.js สำหรับ
test JavaScript ของ console และ Helm v4.3.0 สำหรับ test ของ chart
(`scripts/ci/helm.sh` ดาวน์โหลดไว้ใน `.tools/` ให้บน Linux)

```bash
python3 deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d postgres
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
go vet ./... && go test -race ./...
```

ถ้าไม่ได้ตั้ง `EACP_TEST_ADMIN_DSN` test ที่ใช้ PostgreSQL จะถูกข้าม ไม่ใช่ผ่าน ให้รันก่อนเปิด pull request

## กฎ

กฎเหล่านี้มาจาก [MASTER_PLAN](docs/MASTER_PLAN.md) §106–§107 และ [AGENTS.md](AGENTS.md):

- **ADR มาก่อนการเปลี่ยนแปลงเชิงบรรทัดฐาน** ADR คือแหล่งความจริง และ ADR มีน้ำหนักเหนือ master plan การเปลี่ยน state transition, การรับประกัน,
  ขอบเขตความเชื่อถือ หรืออำนาจ ต้องมี ADR ใหม่หรือฉบับแก้ไข
- **เขียน test ที่ล้มก่อน** เขียน test ดูให้เห็นว่าล้มด้วยเหตุผลที่ถูกต้อง แล้วจึงทำให้ผ่าน
- **รัน test ด้วย `-race`** การรับประกันของระบบนี้อยู่ที่การทำงานพร้อมกัน
- **ห้ามลดความเข้มของ test หรือพฤติกรรม fail closed เพื่อให้ CI ผ่าน** ถ้า test ผิด ให้แก้ test และอธิบายเหตุผลใน pull request
- **ห้ามแต่ง API ของ upstream ขึ้นมาเอง** ตรวจกับ source ของ module หรือเอกสารก่อน และบันทึกสิ่งที่ตรวจไว้ใน
  [`research/REFERENCES.md`](research/REFERENCES.md) เมื่อสำคัญ
- **ทำตามธรรมเนียม Row-Level Security** ใน `migrations/00001_foundation.sql` สำหรับทุกตารางของ tenant และเพิ่มตาราง, policy หรือ function แบบ
  `SECURITY DEFINER` ใหม่ทุกตัวลงใน `internal/storage/rls_catalog_test.go`
- **ให้ PostgreSQL เป็นผู้มีอำนาจ** กฎของ registry อยู่ใน trigger และการเขียนที่ต้องใช้สิทธิ์ทุกครั้งผูก actor และต่อท้าย audit event ใน transaction เดียวกัน
- **ห้าม log, เก็บ หรือบันทึกค่า secret ลง journal**
- **ห้ามอ้างว่าเป็น exactly-once** ใช้คำตาม MASTER_PLAN §21
- **รักษาทั้งสองภาษา** เอกสารสำหรับผู้ใช้และผู้ร่วมพัฒนามีทั้งภาษาอังกฤษ (`X.md`) และภาษาไทย (`X.th.md`) ให้แก้ทั้งสองไฟล์ โดยคงหัวข้อ,
  code block และรูปภาพให้เหมือนกันและเรียงลำดับเดียวกัน `test/opensource` เป็นผู้ตรวจ
- **เขียน commit message แบบ conventional** เช่น `feat: …`, `fix: …`, `docs: …`, `ci: …`, `refactor: …`

## test

| ระดับ | คำสั่ง | ต้องมี |
|---|---|---|
| unit และ integration กับ PostgreSQL | `go vet ./... && go test -race ./...` | PostgreSQL จาก compose และ `EACP_TEST_ADMIN_DSN` |
| lint | `bash scripts/ci/lint.sh` | Go |
| JavaScript ของ console | `(cd internal/ui && node --test jstest/*.test.mjs)` | Node.js |
| Helm chart | `bash scripts/ci/helm.sh` | Helm v4.3.0 ใน `.tools/` |
| AGT sidecar และ conformance | `docker build -f sidecars/agt-pdp/Dockerfile --target test .` | Docker |
| การแยกเครือข่าย | `bash scripts/ci/compose-security.sh` | stack ของ compose ทั้งชุด |
| ตัวอย่าง | `bash scripts/ci/examples.sh` | stack ของ compose ทั้งชุด, `jq`, Python 3.10 ขึ้นไป |
| demo | `scripts/demo.sh` | Docker |
| Kubernetes | `bash scripts/k8s-e2e.sh` | minikube |

pull request รัน test ระดับแรกๆ ใน CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) ส่วนการรันกับ compose, ตัวอย่าง, demo และ benchmark
รันทุกคืน ([`.github/workflows/nightly.yml`](.github/workflows/nightly.yml))

## pull request

pull request หนึ่งรายการควรมีจุดประสงค์เดียว อธิบายว่าเปลี่ยนอะไรและเพราะอะไร ระบุ ADR ที่ทำตาม และบอก test ที่พิสูจน์การเปลี่ยนแปลง
template ของ pull request มี checklist ให้แล้ว

## สัญญาอนุญาต

EACP ใช้ [Apache License 2.0](LICENSE) การส่งผลงานเข้ามาถือว่าคุณยอมให้ผลงานนั้นอยู่ภายใต้สัญญาอนุญาตเดียวกัน (inbound = outbound ตามข้อ 5
ของสัญญาอนุญาต) ไม่มี contributor license agreement
