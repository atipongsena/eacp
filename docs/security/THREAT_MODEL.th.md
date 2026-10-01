[English](THREAT_MODEL.md) | [ไทย](THREAT_MODEL.th.md)

# Threat model

เอกสารนี้คือ threat model ที่ [MASTER_PLAN](../MASTER_PLAN.md) §68 กำหนดไว้ ระบุว่า EACP ปกป้องอะไร ขอบเขตความเชื่อถืออยู่ตรงไหน
ภัยคุกคามที่แต่ละขอบเขตพร้อมการควบคุมที่ตอบแต่ละข้อ และสิ่งที่ยังอยู่นอกเหนือการควบคุม การควบคุมทุกข้อในหน้านี้มีอยู่จริงในโค้ด
ทุกแถวระบุ ADR ที่ตัดสินเรื่องนั้นและ test ที่บังคับใช้ ส่วน [INVARIANTS.md](../INVARIANTS.md) รวบรวม test ทั้งหมดเบื้องหลังการรับประกันแต่ละข้อ

## สมมติฐานตั้งต้น

EACP ไม่เชื่อถือ input ใดเลย (MASTER_PLAN §67) agent ไม่น่าเชื่อถือ เช่นเดียวกับผลลัพธ์ของ model, metadata ของ tool, MCP server,
A2A agent ระยะไกล, message ในคิว และคำตอบจาก API ภายนอก การควบคุมที่ได้ผลเฉพาะเมื่อ agent ทำตัวดีไม่นับเป็นการควบคุม

## ทรัพย์สิน

| ทรัพย์สิน | อยู่ที่ไหน | ทำไมจึงสำคัญ |
|---|---|---|
| credential ของ connector | หน่วยความจำของ execution worker โหลดจากไฟล์ secret, Vault หรือ JIT provider ([ADR-019](../adr/ADR-019-credential-custody.md)) | ให้สิทธิ์เข้าถึงระบบขององค์กรในระดับที่มีอภิสิทธิ์ |
| key ของผู้ให้บริการ model | หน่วยความจำของ LLM gateway ([ADR-031](../adr/ADR-031-llm-gateway.md)) | ใช้จ่ายเงินและเข้าถึงผู้ให้บริการได้ |
| สถานะของระบบองค์กร | ระบบปลายทาง | ผลของ action เช่น ใบสั่งซื้อ การจ่ายเงิน และข้อมูล |
| การอนุมัติและ grant | PostgreSQL ([ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md)) | การตัดสินใจของมนุษย์ที่อนุญาต action ที่ระบุตรงตัวหนึ่งรายการ |
| audit journal | PostgreSQL เป็น hash chain ราย tenant ([ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md)) | หลักฐานว่าเกิดอะไรขึ้น ใครทำ และทำไม |
| ข้อมูลของ tenant | PostgreSQL ภายใต้ Row-Level Security | registry, action, payload, งบประมาณ และค่าใช้จ่ายของแต่ละ tenant |
| ผลลัพธ์ของ tool ที่เก็บไว้ | PostgreSQL นานสูงสุดหนึ่งวันต่อ contract ที่เลือกเปิด ([ADR-034](../adr/ADR-034-result-channel.md)) | เนื้อหาจากระบบระยะไกลที่เชื่อถือไม่ได้และอาจเป็นข้อมูลส่วนบุคคล มีเพียง agent ที่เรียกเท่านั้นที่อ่านได้ |
| API key | เก็บเฉพาะ hash ใน PostgreSQL ([`internal/identity`](../../internal/identity)) | ใช้ยืนยันตัวตนของ agent และคน |

## ขอบเขตความเชื่อถือ

| # | ขอบเขต | สิ่งที่ข้าม |
|---|---|---|
| B1 | agent → API และ LLM gateway | ทุกคำขอที่ agent ส่งด้วย EACP key ของตัวเอง |
| B2 | API → policy decision point | คำถามด้าน governance และคำตัดสิน |
| B3 | execution worker → ระบบปลายทาง | การเรียก connector, การ scan MCP, A2A delegation และการค้นหาเพื่อ reconcile |
| B4 | service → PostgreSQL | ทุกการเปลี่ยนสถานะและการอ่าน |
| B5 | service → NATS | work hint และ kill signal |
| B6 | คน → API และ console | operator, ผู้อนุมัติ, registry editor และ registry approver, admin |
| B7 | LLM gateway → ผู้ให้บริการ model | การเรียก model |
| B8 | source และ build → release | dependency, CI และไฟล์ release |

stack ของ compose ([`docker-compose.yml`](../../docker-compose.yml)) และ Helm chart ทำให้ B1, B3 และ B7 เป็นขอบเขตทางเครือข่าย
agent เข้าถึงระบบปลายทาง, PostgreSQL, PDP, NATS และผู้ให้บริการ model ไม่ได้ ([ARCHITECTURE.th.md](../ARCHITECTURE.th.md))

## ภัยคุกคามและการควบคุม

แต่ละตารางใช้ STRIDE คือ **S**poofing (ปลอมตัว), **T**ampering (แก้ไขดัดแปลง), **R**epudiation (ปฏิเสธการกระทำ), **I**nformation
disclosure (ข้อมูลรั่วไหล), **D**enial of service (ทำให้ใช้งานไม่ได้) และ **E**levation of privilege (ยกระดับสิทธิ์) test ระบุเป็น package
และชื่อ function

### B1: agent ไปยัง API และ gateway

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| S | ปลอมตัวเป็น agent | agent แต่ละตัวยืนยันตัวตนด้วย key ของตัวเอง ซึ่ง EACP เก็บไว้เพียง hash | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md) | `internal/identity` `TestAuthenticateAgent`, `TestAuthenticationFailures` |
| T | สลับพารามิเตอร์: payload ที่ทำงานจริงต่างจากที่ได้รับอนุมัติ | payload ที่บังคับใช้ถูกผูกด้วย digest กับคำตัดสินและ grant ถ้าไม่ตรงจะปฏิเสธและแจ้งเตือน | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/action` `TestDigestMismatchDeniesAndAlerts`; `internal/worker` `TestTamperedEnforcedPayloadIsDeniedWithAnAlert` |
| T | การทำงานซ้ำจากการส่งคำขอซ้ำ | idempotency key หนึ่งตัวได้ action เดียว body ที่ต่างไปภายใต้ key เดิมถูกปฏิเสธ | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/action` `TestConcurrentSubmissionsWithOneKeyCreateOneAction`; `internal/worker` `TestDuplicateSubmissionsProduceOneEffect` |
| R | agent ปฏิเสธว่าไม่ได้ขอ action | ทุก action ผูกกับ agent และ version ที่ยืนยันตัวตนแล้ว และทุก transition ถูกบันทึกลง journal | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/action` `TestActionInsertRequiresMatchingAgentAndDerivesIdentity`, `TestActionTransitionsAreJournaledWithActorKinds` |
| I | เข้าถึงข้าม tenant | Row-Level Security บนทุกตารางของ tenant และ unique key ผูกกับ tenant | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), MASTER_PLAN §69 | `internal/storage` `TestTenantSeesOnlyItsOwnRows`, `TestMissingTenantContextSeesNothing` |
| D | ถล่มคิว | admission limit ตอบ 429 โดยไม่สร้าง action และคิว pending กับคิวของ connector มีขอบเขต | [ADR-022](../adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) | `internal/api` `TestAdmissionLimitIs429`; `internal/action` `TestAdmissionLimitRejectsWithoutCreatingAnAction` |
| D | ผลาญงบ (denial of wallet) | งบประมาณแบบแข็งถูกจองใน PostgreSQL ตอนปล่อย action และการเรียก LLM จองตามที่ PostgreSQL ประเมินก่อนจะส่งสิ่งใดออกไป | [ADR-012](../adr/ADR-012-budget-reservation.md), [ADR-031](../adr/ADR-031-llm-gateway.md) | `internal/action` `TestConcurrentReleasesNeverOversubscribeAHardBudget`; `internal/llm` `TestAdmitReservesTheEstimate` |
| E | ยกระดับสิทธิ์หรือ confused deputy: agent ใช้ tool ที่ไม่ได้รับมอบ | tool ต้องอยู่ใน allowlist ของ version ที่ `ACTIVE` และมี contract ที่รับรองแล้ว ตรวจตอนส่งคำขอ ตอนปล่อย และอีกครั้งตอน dispatch | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/registry` `TestCheckCapabilityDeniesSuspendedRevokedAndDrifted`; `internal/worker` `TestDispatchIntentRefusesDriftAndRevocation` |
| E | ข้าม control plane: agent เรียกระบบปลายทางโดยตรง | agent ไม่มี credential ขององค์กรและไม่มีเส้นทางเครือข่ายไปยังระบบปลายทาง | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) | `test/security` `TestAgentCannotReachFakeERP`, `TestOnlyTheWorkerHoldsConnectorSecrets`, `TestFakeERPRejectsUnauthenticatedPrivilegedCall` |
| E | prompt injection ทำให้ agent ขอ action ที่เป็นอันตราย | EACP ไม่อ่าน prompt แต่จำกัดสิ่งที่คำขอใดๆ ทำได้ allowlist, policy, การอนุมัติ, งบประมาณ และ kill switch มีผลกับทุก action ไม่ว่าอะไรทำให้ agent ขอ | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) | test ของการควบคุมข้างต้น และดูความเสี่ยงที่เหลืออยู่ |

### B2: API ไปยัง policy decision point

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| S | คำตัดสินด้าน governance ที่ปลอมขึ้น | เข้าถึง sidecar ผ่าน mutual TLS ด้วย CA เฉพาะ มีเพียง API และ sidecar ที่ถือ key และ agent ไม่มีเส้นทางไปถึง | [ADR-002](../adr/ADR-002-agt-integration-sidecar-pdp.md) | `test/security` `TestPDPRefusesAClientWithoutCertificate`, `TestAgentCannotReachThePDP`, `TestOnlyTheAPIAndTheSidecarHoldThePDPPKI` |
| T | คำตัดสินที่ถูกนำมาใช้ซ้ำหรือไม่ครบ | หลักฐานของคำตัดสินถูกเก็บพร้อม digest ของ input และของ payload ที่บังคับใช้ หลักฐานที่ไม่ครบจะ fail closed | [ADR-002](../adr/ADR-002-agt-integration-sidecar-pdp.md) | `internal/governance` `TestRecordDecisionPersistsBoundedProviderEvidence`, `TestEvaluateCheckedFailsClosedOnProviderErrorAndIncompleteEvidence` |
| T | การอนุมัติที่ล้าสมัยหลัง policy เปลี่ยน | ตอนปล่อยจะตรวจซ้ำภายใต้ policy เวอร์ชันปัจจุบัน และ grant ใช้ไม่ได้หลัง policy เปลี่ยน | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/action` `TestPolicyChangeAfterGrant`; `internal/approval` `TestGrantCannotBeConsumedTwiceOrAfterPolicyChange` |
| D | PDP ล่ม | fail closed: action ค้างที่ `RECEIVED` และทำงานไม่ได้ ส่วนการยกเลิก, การอ่านเพื่อ reconcile และการกักกันไม่ต้องใช้ PDP | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) T2a | `internal/action` `TestGovernanceOutageKeepsActionReceivedUntilResubmission`, `TestCancelBeforeDispatchNeverConsultsGovernance` |

### B3: execution worker ไปยังระบบปลายทาง

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| S | ผู้เรียกที่ไม่ใช่ EACP ทำการเรียกที่มีอภิสิทธิ์ | ใน conforming deployment ระบบปลายทางรับการเรียกที่มีอภิสิทธิ์จาก credential ของ worker เท่านั้น ซึ่ง Fake ERP บังคับใช้เอง | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) §3a | `test/security` `TestFakeERPRejectsUnauthenticatedPrivilegedCall` |
| T | tool poisoning หรือ MCP rug pull: นิยามของ tool เปลี่ยนหลังได้รับการรับรอง | tool ถูกค้นพบ ไม่ได้ถูกประกาศ PostgreSQL คำนวณ fingerprint ของทุกนิยาม การเปลี่ยนที่เสี่ยงสูงหรือ tool ที่รับรองแล้วหายไปจะถูกกักกัน และต้องใช้ผู้อนุมัติอีกคนจึงปล่อยได้ | [ADR-023](../adr/ADR-023-mcp-registry-and-tool-fingerprint.md) | `internal/worker` `TestScannerDiscoversToolsAndQuarantinesDrift`; `internal/fleet` `TestQuarantineAndReleaseByAnotherApprover`; `internal/registry` `TestMissingCertifiedToolIsQuarantined` |
| T | A2A agent ระยะไกลเปลี่ยน card หลังได้รับการรับรอง | card เป็นส่วนหนึ่งของนิยามของ delegate การเปลี่ยนแปลงทำให้ถูกกักกัน | [ADR-030](../adr/ADR-030-a2a-delegation.md) | `internal/registry` `TestA2ADefinitionChangeQuarantinesACertifiedDelegate` |
| T | worker ที่ล้าสมัยทำ dispatch ซ้ำ | lease ถูก fence ด้วย generation ในฐานข้อมูล worker ที่เสีย lease เขียนหรือ dispatch ไม่ได้ | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/worker` `TestLeaseRaceHasExactlyOneWinner`, `TestStaleWorkerBeforeIntentNeverDispatches` |
| T | ผลลัพธ์ที่ไม่รู้แน่ชัดถูกตีความผิดว่าล้มเหลวหรือสำเร็จ | ผลที่กำกวมกลายเป็น `UNKNOWN_OUTCOME` และถูก reconcile ไม่เคย retry แบบไม่ดูตาม้าตาเรือ | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/worker` `TestLostResponseIsReconciledToSuccessWithOneRecord`, `TestDelayedVisibilityIsNeverRetried` |
| T | reconcile ได้ผลลบลวง: "ไม่พบ" นำไปสู่การ retry และรายการซ้ำ | หลักฐานเชิงลบนับเฉพาะเมื่อ contract เป็น `AUTHORITATIVE` และทุกการเรียกได้ยุติแล้ว นอกนั้นให้คนตัดสิน | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) Rev 2.5 | `internal/worker` `TestNegativeEvidenceNeedsAnAuthoritativeContract`, `TestDecideAppliesTheProofStandard` |
| R | ไม่มีบันทึกว่าระบบปลายทางทำอะไร | attempt, external reference และการตรวจเพื่อ reconcile เป็นส่วนหนึ่งของหลักฐาน | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/worker` `TestEvidenceReconstructsTheWholeActionFromItsID` |
| I | credential ถูกขโมย หรือ connector ส่ง secret กลับมา | credential อยู่เฉพาะใน worker ไม่เคยถูกเก็บหรือบันทึกลง journal ถูกปิดบังจาก log และ field ที่ connector ส่งกลับมาซึ่งมี secret อยู่จะถูกทิ้ง | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md), [ADR-019](../adr/ADR-019-credential-custody.md) | `internal/worker` `TestSecretCanaryNeverLeaks`; `internal/logging` `TestRegisteredSecretValuesAreRedactedInMessageAndAttrs` |
| I | ผลลัพธ์ของ tool ไปถึง agent อื่น คน log หรือ journal หรือมี credential ติดอยู่ | เก็บผลลัพธ์เฉพาะความสำเร็จของ contract ที่เลือกเปิด อ่านได้เฉพาะเวอร์ชัน `ACTIVE` ของ agent ที่เรียกผ่าน PostgreSQL ไม่เก็บถ้ามี credential ของ worker อยู่ มีขนาดจำกัด หมดอายุและถูกลบ ไม่เคยลง log, journal หรือข้อความ | [ADR-034](../adr/ADR-034-result-channel.md) | `internal/worker` `TestOnlyTheActionsAgentReadsTheResult`, `TestResultContentIsNotSelectable`, `TestACredentialInTheOutputIsWithheld`, `TestASuccessKeepsItsOutputForTheAgent` |
| D | retry storm หรือ connector ที่ล้มทำให้ connector อื่นขาดทรัพยากร | retry budget จำกัดจำนวนครั้ง ค่าใช้จ่าย และเวลา circuit breaker และ bulkhead แยก connector ออกจากกัน | [ADR-022](../adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) | `internal/action` `TestRetryBudgetBoundsRetryCost`; `internal/worker` `TestConnectorFailureDoesNotStarveUnrelatedConnectors` |
| E | agent ที่ถูก kill ยังทำงานต่อ | สถานะ kill ถูกตรวจใน PostgreSQL ตอน claim และ dispatch และ poll ระหว่างการเรียก kill ระหว่างการเรียกทำให้ผลลัพธ์เป็นไม่รู้แน่ชัด ไม่เคย retry | [ADR-016](../adr/ADR-016-distributed-kill-switch.md) | `internal/worker` `TestRawDispatchIntentIsDatabaseFencedByKill`, `TestKillDuringCallForcesUnknownOutcome` |

### B4: service ไปยัง PostgreSQL

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| S | service รันด้วย role ที่ bypass Row-Level Security ได้ | service ไม่ยอมเริ่มทำงานถ้าไม่ได้เชื่อมต่อด้วย role ที่ไม่มี `BYPASSRLS` | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md) | `internal/service` `TestStartRefusesRoleThatCanBypassRLS` |
| T | ดัดแปลง audit | journal เพิ่มได้อย่างเดียว เป็น hash chain ราย tenant และต่อท้ายใน transaction เดียวกับการเปลี่ยนแปลง client ตั้งค่า field ของ chain เองไม่ได้ และการตรวจสอบจับการดัดแปลงได้ | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md) §8 | `internal/audit` `TestVerifyDetectsTampering`, `TestClientCannotForgeChainFields`; `internal/registry` `TestRawSQLChangesAreAuditedByTheDatabase` |
| T | ใช้การอนุมัติซ้ำ: grant หนึ่งตัวปล่อย action สองรายการ | grant ถูกใช้ได้ไม่เกินหนึ่งครั้งภายใต้ row lock | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/approval` `TestConcurrentConsumeSucceedsOnce`; `internal/action` `TestApprovalThenParallelReleasesConsumeTheGrantOnce` |
| T | แย่งงบประมาณ (budget race) | การจองทำภายใต้ lock ของบัญชีระดับล่างสุด และวงเงินของบัญชีลูกถูกกันไว้จากบัญชีแม่ | [ADR-012](../adr/ADR-012-budget-reservation.md) | `internal/action` `TestConcurrentReleasesNeverOversubscribeAHardBudget`; `internal/budget` `TestEscrowBoundsChildrenByTheirParent` |
| I | ตารางใหม่ที่ไม่มีการแยก tenant | ทุกตาราง, policy และ function แบบ `SECURITY DEFINER` ถูกทบทวนใน catalog test | MASTER_PLAN §69 | `internal/storage` `TestEveryTableFollowsTheRLSConventionAndCrossTenantPathsAreReviewed` |

### B5: service ไปยัง NATS

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| S | message ที่ปลอมหรือถูกส่งซ้ำก่อให้เกิด action | message มีเพียงสัญญาณ ไม่มีสิ่งใดถูก claim ทำงาน ยกเลิก หรือตัดสินจาก message และแต่ละบทบาทมีผู้ใช้และสิทธิ์ใน NATS ของตัวเอง | [ADR-014](../adr/ADR-014-postgresql-authority-nats-signals.md) | `test/security` `TestWorkerCredentialCannotPublishHintsOrEvents`, `TestAgentCannotReachNATS` |
| I | ข้อมูลอ่อนไหวอยู่ใน message | message มีเพียง id, สถานะ และ epoch ไม่เคยมีเหตุผล payload หรือ secret | [ADR-014](../adr/ADR-014-postgresql-authority-nats-signals.md) | `internal/messaging` `TestTransitionsWriteDashboardEvents`, `TestOutboxRowsComeOnlyFromTheActionTriggers` |
| D | NATS หายไป | การ poll ยังเปิดอยู่ และ outbox เก็บแถวไว้จนกว่า NATS จะกลับมา | [ADR-014](../adr/ADR-014-postgresql-authority-nats-signals.md) | `internal/messaging` `TestWithoutNATSTheWorkerStillExecutes`, `TestNATSOutageLeavesRowsUnpublishedUntilItRecovers` |

### B6: คนไปยัง API และ console

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| E | อนุมัติตัวเอง หรือผู้อนุมัติจาก tenant อื่น | การแบ่งแยกหน้าที่ใน PostgreSQL: subject, เจ้าของ, กลุ่มของเจ้าของ และผู้ที่เปิดใช้ อนุมัติไม่ได้ และผู้อนุมัติจำกัดอยู่ใน tenant | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/approval` `TestSelfAndCrossTenantApprovalRejected`, `TestOwnerAndEnablingActorsCannotApprove` |
| E | operator หรือ admin ที่ประสงค์ร้าย | กฎสองคน: การเปิดใช้ใน registry โดยคนที่สอง, การเพิ่มวงเงินงบประมาณ, การยกเลิก kill, การ retry หลังคนตัดสิน และการอนุมัติ change set ทุกการกระทำที่ต้องใช้สิทธิ์ถูกบันทึกลง journal พร้อมผู้กระทำและเหตุผล | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-012](../adr/ADR-012-budget-reservation.md), [ADR-016](../adr/ADR-016-distributed-kill-switch.md), [ADR-026](../adr/ADR-026-governance-as-code.md) | `internal/api` `TestKillAPIRequiresOperatorAndSecondOperatorToResume`; `internal/worker` `TestHumanResolutionIsSeparatedJournaledAndTwoPersonForRetry` |
| I | console ทำ key รั่วหรือรันเนื้อหาที่ถูกแทรก | key อยู่ในหน่วยความจำของแท็บเท่านั้น, CSP เข้มงวด, ไม่มี HTML sink, browser storage, cookie หรือ origin อื่น | [ADR-028](../adr/ADR-028-operator-console.md) | `internal/ui` `TestConsoleUsesNoDangerousSinks` |
| R | operator ปฏิเสธว่าไม่ได้ตัดสินหรือ kill | kill และการตัดสินถูกบันทึกลง journal พร้อมผู้กระทำและเหตุผล | [ADR-016](../adr/ADR-016-distributed-kill-switch.md) | `internal/worker` `TestKillAndResumeAreJournaledWithActorAndReason` |

### B7: gateway ไปยังผู้ให้บริการ model

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| E | agent ใช้ model ที่ไม่ได้รับมอบ | admission ใน PostgreSQL ตรวจ allowlist ของ model ก่อนจะส่งสิ่งใดออกไป | [ADR-031](../adr/ADR-031-llm-gateway.md) | `internal/llm` `TestStoreDenialIsReturnedNotAnError` |
| I | prompt, คำตอบ หรือ key ไปอยู่ใน storage หรือ log | gateway เก็บเพียง metadata และไม่เคย log prompt, คำตอบ, header หรือ key | [ADR-031](../adr/ADR-031-llm-gateway.md) | `internal/llmgateway` `TestNothingSecretIsPersistedByTheGateway`, `TestNothingSecretIsLogged` |
| D | ผลาญงบผ่านการเรียก model | จองงบประมาณตอน admission ถ้าไม่รู้ usage จะคิดเต็มจำนวนที่จองไว้ และ kill ตัด stream ที่กำลังทำงานได้ | [ADR-031](../adr/ADR-031-llm-gateway.md), [ADR-016](../adr/ADR-016-distributed-kill-switch.md) | `internal/llmgateway` `TestTheGatewayMetersAndLimitsSpend`, `TestKillCutsAStream` |

### B8: supply chain

| STRIDE | ภัยคุกคาม | การควบคุม | ตัดสินโดย | บังคับใช้โดย |
|---|---|---|---|---|
| T | GitHub Action ที่ถูกเจาะ | ทุก action ถูก pin ด้วย commit SHA เต็ม | [RELEASING.th.md](../RELEASING.th.md) | `test/opensource` `TestWorkflowActionsArePinned` |
| T | Go dependency ที่มีช่องโหว่ | `govulncheck` ใน CI และ Dependabot อัปเดตให้ | [RELEASING.th.md](../RELEASING.th.md) | [`scripts/ci/vuln.sh`](../../scripts/ci/vuln.sh) |
| T | การเปลี่ยน governance engine ที่ไม่ผ่านการทบทวน | เวอร์ชันของ AGT, ACS และ OPA ถูก pin การขยับเวอร์ชันต้องมีบันทึก spike และ conformance run ที่ผ่าน | [ADR-002](../adr/ADR-002-agt-integration-sidecar-pdp.md) | `integrations/governance/microsoftagt` `TestHealthChecksTheVersionPins`; ชุด conformance ของ sidecar |
| T | ไฟล์ release ที่ถูกดัดแปลง | ทุก release เผยแพร่ `SHA256SUMS` | [RELEASING.th.md](../RELEASING.th.md) | [`scripts/ci/release-binaries.sh`](../../scripts/ci/release-binaries.sh) |

## สมมติฐาน

- **conforming deployment** ([ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) §3a): ระบบปลายทางรับการเรียกที่มีอภิสิทธิ์
  จาก identity ของ worker ของ EACP เท่านั้น, agent ไม่มีเส้นทางเครือข่ายไปยังระบบปลายทาง และ EACP key ของ agent ไม่ให้สิทธิ์ใดๆ ที่ระบบปลายทาง
  EACP พิสูจน์เงื่อนไขแรกให้ระบบใดๆ ก็ได้ไม่ได้
- **ผู้ดูแลฐานข้อมูลที่เชื่อถือได้** PostgreSQL คือผู้มีอำนาจ superuser หรือใครก็ตามที่แก้ไฟล์ข้อมูลได้ จะเปลี่ยนแปลงอะไรก็ได้ รวมถึง journal
  (การตรวจสอบจะเห็นว่า chain ขาด แต่เป็นการรู้หลังเกิดเหตุ)
- **host ที่เชื่อถือได้สำหรับ worker และ gateway** ใครที่ควบคุมหน่วยความจำของ process ทั้งสองได้ย่อมถือ credential
- **เวลาที่ถูกต้อง** lease, การหมดอายุ และ grant ใช้นาฬิกาของฐานข้อมูล

## ความเสี่ยงที่เหลืออยู่

- **prompt injection ถูกจำกัดขอบเขต แต่ไม่ได้ถูกป้องกัน** agent ที่ถูกชักจูงยังขอ action ใดก็ได้ที่ allowlist, policy, การอนุมัติ และงบประมาณ
  อนุญาต allowlist ที่แคบและการอนุมัติสำหรับ operation ที่อ่อนไหวคือสิ่งที่จำกัดความเสียหาย
- **worker ที่ถูกเจาะถือ credential ที่ใช้งานได้จริง** ฐานข้อมูลยัง fence สิ่งที่ worker บันทึกได้ แต่ worker อาจใช้ credential นอก EACP จนกว่าจะถูก
  เพิกถอน credential แบบ JIT ที่มีอายุสั้น ([ADR-019](../adr/ADR-019-credential-custody.md)) ช่วยทำให้ช่วงเวลานี้แคบลง
- **ตรวจไม่พบการ bypass** ใน deployment ที่ไม่ conforming EACP มองไม่เห็นการเรียกที่อ้อมผ่าน การอ่าน audit log ของระบบปลายทางเพื่อหา principal
  ที่ไม่ใช่ EACP เป็นงานในอนาคต (ADR-001 §3a)
- **ข้อมูลส่วนบุคคลใน payload** payload ของ action ถูกเก็บตามที่ส่งมาภายใต้ Row-Level Security EACP ไม่ได้จำแนกหรือปิดบังข้อมูลส่วนบุคคลในนั้น
- **ผลลัพธ์ของ tool ที่เก็บไว้เป็นข้อมูลธรรมดาในฐานข้อมูล** contract ที่เลือกเปิดช่องทางรับผลลัพธ์จะเก็บผลลัพธ์ของความสำเร็จไว้นานสูงสุดหนึ่งวัน
  ได้รับการปกป้องด้วย Row-Level Security สิทธิ์ระดับคอลัมน์ และการป้องกันข้อมูลที่เก็บของ PostgreSQL เอง ไม่ได้เข้ารหัสในระดับแอปพลิเคชัน
  ([ADR-034](../adr/ADR-034-result-channel.md))
- **สองคนที่สมรู้ร่วมคิดกัน** กฎสองคนหยุดคนหนึ่งคนได้ แต่หยุดสองคนที่ตกลงกันไม่ได้
- **ยังไม่มี kill scope แบบ global** ต้องรอ platform authority
  ([ADR-016](../adr/ADR-016-distributed-kill-switch.md)) ส่วน kill ทั้ง tenant ใช้งานได้แล้ว และตั้งแต่ Rev 1.1 kill run เดียวของ
  Studio ได้ด้วย โดย PostgreSQL ผูก action ของ run นั้นไว้กับมัน

## ความครอบคลุมของ MASTER_PLAN §68

| ภัยคุกคามใน §68 | หัวข้อ |
|---|---|
| Prompt injection | B1 |
| Tool poisoning, MCP rug pull | B3 |
| Agent impersonation | B1 |
| Confused deputy, privilege escalation | B1 |
| Approval replay | B4 |
| Parameter substitution | B1 |
| Duplicate execution | B1, B3 |
| Credential theft | B3 |
| Cross-tenant access | B1, B4 |
| Budget race | B4 |
| Denial of wallet | B1, B7 |
| Retry storm | B3 |
| Queue flooding | B1 |
| Compromised worker | ความเสี่ยงที่เหลืออยู่ |
| Compromised connector | B3 |
| Audit manipulation | B4 |
| Supply-chain compromise | B8 |
| Unknown-outcome mishandling | B3 |
| Control-plane bypass | B1, สมมติฐาน |
| Stale-worker duplicate dispatch | B3 |
| False-negative reconciliation | B3 |
| Self-approval, cross-tenant approver | B6 |
| A stale approval after a policy change | B2 |
| Forged or replayed governance decision | B2 |
| Malicious operator or admin | B6 |
| Sensitive data in traces and audit | B3, B5, B7, ความเสี่ยงที่เหลืออยู่ |

## ขอบเขต model และ preview ของ Studio

คำตอบ model เป็นข้อมูลที่ไม่เชื่อถือ ไม่ใช่คำสั่งให้ส่งงาน schema แบบปิด การเปรียบเทียบ scalar อย่างแม่นยำ
ปลายทางตายตัว และชุด capability ทั้งหมดจาก PostgreSQL จำกัด graph prompt injection เพิ่ม tool/model/ปลายทาง
ไม่ได้ แต่ model ยังให้ค่าที่ผ่าน schema แล้วทำให้เข้าใจผิดได้ จึงยังต้องมีการอนุมัติและตรวจโดยมนุษย์ Studio ต้องมี
leaf hard budget ก่อน admit model intent ที่มี fence หนึ่งตัวผูก call เดียว takeover ไม่ส่ง call ที่ admit แล้วซ้ำ
ปฏิเสธ fence เก่า และ run ที่ถูก kill ยังหยุดแม้ operator คนที่สองจะยกเลิก containment

runtime ถือ key ของ agent ที่ derive แต่ไม่มี key provider หรือ URL database credential provider อยู่เฉพาะ gateway
JSON แบบมีชนิดเป็นส่วนตัว มีขอบเขต บันทึกพร้อม settlement อ่านได้เฉพาะ lease ที่ยังถืออยู่และล้างเมื่อจบหรือเลย deadline
ไม่เก็บ prompt หรือ envelope provider operator เห็น metadata เท่านั้น หาก runtime ถูกยึด ยังเสี่ยงต่อ capability
ของ agent ที่อนุมัติแล้วและ output ส่วนตัวที่ยังอยู่จนกว่าจะหยุด preview มี mode เปลี่ยนไม่ได้ ใช้ตัวอย่าง tool เป็นส่วนตัว
และ insert action ใน PostgreSQL ไม่ได้ draft ทดสอบโดยไม่เรียกอะไร ดู [ADR-033 Rev 1.4](../adr/ADR-033-agent-studio-and-runtime-credentials.md)
