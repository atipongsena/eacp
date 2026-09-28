[English](RELEASING.md) | [ไทย](RELEASING.th.md)

# การออก release ของ EACP

release หนึ่งตัวคือ Git tag `vX.Y.Z` บน `main` เมื่อ push tag ขึ้นไป [`release.yml`](../.github/workflows/release.yml) จะทำงาน
และจะไม่เผยแพร่อะไรเลยถ้า CI ทั้งชุดไม่ผ่านบน commit ที่ติด tag นั้น

## ก่อน release แรก

ขั้นตอนเหล่านี้เป็นการตั้งค่าบน GitHub ไม่ใช่ไฟล์ใน repository ทำครั้งเดียวหลังจากสร้าง repository และ push `main` แล้ว

1. **ดู CI รอบแรก** workflow ทั้งหมดผ่านการตรวจด้วยการรัน `scripts/ci/*.sh` บนเครื่องและด้วย `actionlint` แล้ว แต่ runner ของ
   GitHub อาจยังต่างออกไป เช่น เวอร์ชันของ Docker Compose หรือพื้นที่ดิสก์ ถ้าเจอปัญหาให้แก้ก่อนติด tag
2. **ป้องกัน `main`** บังคับให้ merge ผ่าน pull request และต้องผ่าน check ของ `ci` (`lint`, `test (slow)`, `test (rest)`, `helm`,
   `sidecar`, `vuln`) และห้าม force push
3. **เปิด private vulnerability reporting** (Settings → Security → Private vulnerability reporting) ซึ่ง
   [SECURITY.th.md](../SECURITY.th.md) แนะนำให้ผู้รายงานใช้ช่องทางนี้
4. **เปิด Dependabot alerts และ security updates** ส่วนการอัปเดตเวอร์ชันถูกตั้งเวลาไว้แล้วใน
   [`.github/dependabot.yml`](../.github/dependabot.yml)
5. **หลัง release แรก** ถ้าต้องการให้ pull image ได้โดยไม่ต้อง login ให้เปลี่ยน GHCR package ทั้งสองตัว (`eacp` และ `eacp-agt-pdp`)
   เป็น public ในหน้าตั้งค่าของ package

## การออก release

1. ย้ายรายการใต้ `## [Unreleased]` ใน [CHANGELOG.md](../CHANGELOG.md) ไปเป็นหัวข้อใหม่ `## [X.Y.Z]` เพิ่มลิงก์เปรียบเทียบของเวอร์ชันนั้น
   ไว้ท้ายไฟล์ แล้ว merge เข้า `main` ผ่าน pull request
2. ติด tag ที่ commit ที่ merge แล้ว และ push tag:

   ```bash
   git tag -a vX.Y.Z -m "EACP vX.Y.Z"
   git push origin vX.Y.Z
   ```

3. ดู workflow `release` จนจบ แล้วตรวจหน้า release ว่ามี release notes, archive สี่ไฟล์ และ `SHA256SUMS`

## workflow ทำอะไรบ้าง

| Job | ทำอะไร |
|---|---|
| `verify` | รัน `ci.yml` ทั้งชุดบน commit ที่ติด tag ทุก job ถัดไปต้องรอให้ job นี้ผ่าน |
| `binaries` | `scripts/ci/release-binaries.sh` build `controlplane-api`, `execution-worker`, `llm-gateway` และ `eacpctl` สำหรับ linux/amd64, linux/arm64, darwin/arm64 และ windows/amd64 โดยฝังเวอร์ชันไว้ในไบนารี และเขียน `SHA256SUMS` |
| `images` | push `ghcr.io/atipongsena/eacp:vX.Y.Z` (ไบนารีทุกตัวของ EACP) และ `ghcr.io/atipongsena/eacp-agt-pdp:vX.Y.Z` (AGT sidecar) เฉพาะ linux/amd64 |
| `publish` | ดึง release notes จากหัวข้อของเวอร์ชันนั้นใน `CHANGELOG.md` แล้วสร้าง GitHub release พร้อม archive ถ้าไม่มีหัวข้อนั้น job จะล้ม |

ไบนารีทุกตัวรายงานเวอร์ชันของตัวเองได้ `eacpctl version` พิมพ์เวอร์ชันออกมา และทุก service บันทึกเวอร์ชันลง log ตอนเริ่มทำงาน

## การตรวจ release

ใครก็ตรวจ archive ที่ดาวน์โหลดมาได้:

```bash
sha256sum -c SHA256SUMS --ignore-missing
```

ถ้า build จาก source เองจะได้เวอร์ชัน `dev` ถ้าต้องการสร้างไบนารีแบบเดียวกับ release ให้ build ด้วย flag ชุดเดียวกัน:

```bash
bash scripts/ci/release-binaries.sh vX.Y.Z dist
```
