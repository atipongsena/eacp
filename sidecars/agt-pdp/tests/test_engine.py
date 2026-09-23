"""The AGT engine adapter: conformance with EACP's reference set, failure
mapping and the checks on ACS output (ADR-002 §4-§8).

These tests run the real pinned stack (agt-policies, ACS, OPA); the image's
test stage runs them on every build.
"""

import copy
import json
import os
import threading
import types
import unittest
from pathlib import Path

import rfc8785

from agt_pdp import engine as engine_mod
from agt_pdp.engine import Engine, PDPError, decide
from agt_pdp.bundle import parse_bundle


def load_reference():
    path = os.environ.get("EACP_CONFORMANCE_REFERENCE") or (
        Path(__file__).resolve().parents[3] / "test" / "conformance" / "governance_reference.json")
    return json.loads(Path(path).read_text(encoding="utf-8"))


def build_request(ref, case):
    binding = dict(ref["binding"])
    binding.update(case.get("binding", {}))
    policy = ref["policies"][case["policy"]]
    return {
        "protocol": "eacp-agt-pdp/1",
        "binding": binding,
        "risk_class": case.get("risk_class", ref["risk_class"]),
        "side_effect_class": case.get("side_effect_class", ref["side_effect_class"]),
        "policy": {"bundle_id": policy["id"], "version": policy["version"], "content": policy["content"]},
    }


class ConformanceTest(unittest.TestCase):
    """Every reference case gives the local provider's outcome, digests
    included, except the documented divergences, which fail closed."""

    @classmethod
    def setUpClass(cls):
        cls.engine = Engine(instance_id="conformance")
        cls.ref = load_reference()

    def test_reference_set(self):
        self.assertGreater(len(self.ref["cases"]), 10)
        for case in self.ref["cases"]:
            with self.subTest(case["name"]):
                want = case.get("agt_expect") or case["expect"]
                req = build_request(self.ref, case)
                if want.get("error"):
                    with self.assertRaises(PDPError) as ctx:
                        self.engine.evaluate(req)
                    if want["error"] != "unavailable":
                        self.assertEqual(ctx.exception.code, want["error"])
                    self.assertIn(ctx.exception.status, (422, 503))
                    continue
                got = self.engine.evaluate(req)
                self.assertEqual(got["verdict"], want["verdict"])
                self.assertEqual(got["reasons"], want["reasons"])
                self.assertEqual(rfc8785.dumps(got["enforced_payload"]), rfc8785.dumps(want["enforced_payload"]))
                self.assertEqual(got["approval"], want.get("approval"))
                self.assertEqual(got["input_digest"], want["input_digest"])
                self.assertEqual(got["enforced_digest"], want["enforced_digest"])
                self.assertEqual(got["policy_bundle_id"], req["policy"]["bundle_id"])
                self.assertEqual(got["policy_version"], req["policy"]["version"])
                self.assertTrue(got["evidence"]["acs_action_identity"].startswith("sha256:"))
                self.assertEqual(got["evidence"]["adapter_digest"], engine_mod.ADAPTER_DIGEST)

    def test_decision_identity(self):
        case = self.ref["cases"][0]
        got = self.engine.evaluate(build_request(self.ref, case))
        self.assertEqual(got["protocol"], "eacp-agt-pdp/1")
        self.assertEqual(got["provider"], "microsoft-agt")
        self.assertEqual(got["provider_instance_id"], "conformance")
        self.assertEqual(got["versions"], engine_mod.versions())
        self.assertEqual(got["evidence"]["rule_id"], "routine")
        self.assertEqual(got["evidence"]["versions"], engine_mod.versions())
        self.assertTrue(got["evaluated_at"].endswith("Z"))
        other = self.engine.evaluate(build_request(self.ref, case))
        self.assertNotEqual(got["decision_id"], other["decision_id"])


class FailureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ref = load_reference()

    def request(self, **binding):
        req = build_request(self.ref, self.ref["cases"][0])
        req["binding"].update(binding)
        return req

    def test_acs_runtime_error_is_unavailable_not_deny(self):
        deep = 1
        for _ in range(70):
            deep = [deep]
        with self.assertRaises(PDPError) as ctx:
            Engine(instance_id="t").evaluate(self.request(payload={"deep": deep}))
        self.assertEqual((ctx.exception.status, ctx.exception.code), (503, "pdp_runtime_error"))

    def test_missing_opa_is_unavailable(self):
        old = os.environ.get("ACS_OPA_PATH")
        os.environ["ACS_OPA_PATH"] = "/nonexistent/opa"
        try:
            with self.assertRaises(PDPError) as ctx:
                Engine(instance_id="t").evaluate(self.request())
            self.assertEqual((ctx.exception.status, ctx.exception.code), (503, "pdp_runtime_error"))
        finally:
            if old is None:
                del os.environ["ACS_OPA_PATH"]
            else:
                os.environ["ACS_OPA_PATH"] = old

    def test_bad_requests(self):
        e = Engine(instance_id="t")
        good = self.request()
        mutations = {
            "protocol": lambda r: r.update({"protocol": "eacp-agt-pdp/2"}),
            "extra field": lambda r: r.update({"x": 1}),
            "binding extra": lambda r: r["binding"].update({"x": "y"}),
            "binding missing": lambda r: r["binding"].pop("subject"),
            "blank subject": lambda r: r["binding"].update({"subject": ""}),
            "subject not string": lambda r: r["binding"].update({"subject": 1}),
            "tenant not uuid": lambda r: r["binding"].update({"tenant": "tenant-a"}),
            "tenant upper case": lambda r: r["binding"].update({"tenant": "AAAAAAAA-0000-4000-8000-000000000001"}),
            "nil tenant": lambda r: r["binding"].update({"tenant": "00000000-0000-0000-0000-000000000000"}),
            "bundle id": lambda r: r["policy"].update({"bundle_id": "x"}),
            "version 0": lambda r: r["policy"].update({"version": 0}),
            "version bool": lambda r: r["policy"].update({"version": True}),
            "risk class": lambda r: r.update({"risk_class": 3}),
            "payload integer out of range": lambda r: r["binding"].update({"payload": {"a": 2 ** 60}}),
            "not an object": lambda r: r.clear(),
        }
        for name, fn in mutations.items():
            req = copy.deepcopy(good)
            fn(req)
            with self.subTest(name):
                with self.assertRaises(PDPError) as ctx:
                    e.evaluate(req)
                self.assertEqual((ctx.exception.status, ctx.exception.code), (400, "bad_request"))

    def test_invalid_policy_is_422(self):
        req = self.request()
        req["policy"]["content"] = {"format_version": 1, "rules": []}
        with self.assertRaises(PDPError) as ctx:
            Engine(instance_id="t").evaluate(req)
        self.assertEqual((ctx.exception.status, ctx.exception.code), (422, "policy_invalid"))


def fake_result(decision, reason=None, rule_id=None, transform=None, applied=None):
    evidence = None
    if rule_id is not None:
        evidence = types.SimpleNamespace(artefact=None, verification_pointers={"eacp_rule_id": rule_id})
    verdict = types.SimpleNamespace(decision=types.SimpleNamespace(value=decision), reason=reason, message=None,
                                    transform=transform, evidence=evidence)
    return types.SimpleNamespace(verdict=verdict, transformed_policy_target=applied,
                                 transformed_policy_target_applied=applied is not None,
                                 input_identity="sha256:aa", enforced_identity="sha256:aa")


class DecideChecksTest(unittest.TestCase):
    """decide() trusts nothing ACS returns that disagrees with the bundle."""

    bundle = parse_bundle({"format_version": 1, "rules": [
        {"id": "a", "verdict": "allow", "reason": "ok"},
        {"id": "t", "verdict": "transform", "reason": "cap", "set": {"amount": 1}},
        {"id": "e", "verdict": "escalate", "reason": "review",
         "approval": {"quorum": 2, "eligible_roles": ["approver"], "ttl_seconds": 60}},
    ]})
    payload = {"amount": 5}

    def check(self, result):
        return decide(self.bundle, self.payload, result)

    def test_consistent_output(self):
        self.assertEqual(self.check(fake_result("allow", "ok", "a"))[:3], ("allow", "ok", self.payload))
        verdict, reason, enforced, approval, rule = self.check(fake_result(
            "transform", "cap", "t", types.SimpleNamespace(path="$policy_target", value={"amount": 1}), {"amount": 1}))
        self.assertEqual((verdict, enforced, approval, rule), ("transform", {"amount": 1}, None, "t"))
        verdict, _, _, approval, _ = self.check(fake_result("escalate", "review", "e"))
        self.assertEqual(approval, {"quorum": 2, "eligible_roles": ["approver"], "ttl_seconds": 60})
        self.assertEqual(self.check(fake_result("deny", "no_matching_rule"))[:2], ("deny", "no_matching_rule"))

    def test_inconsistent_output_is_malformed(self):
        cases = {
            "runtime error": fake_result("deny", "runtime_error:policy_invocation_failed"),
            "unknown rule": fake_result("allow", "ok", "zzz"),
            "verdict differs from rule": fake_result("allow", "review", "e"),
            "reason differs from rule": fake_result("allow", "other", "a"),
            "allow without rule": fake_result("allow", "ok"),
            "deny without rule, other reason": fake_result("deny", "nope"),
            "transform not applied": fake_result("transform", "cap", "t",
                                                 types.SimpleNamespace(path="$policy_target", value={"amount": 1})),
            "transform of a sub-path": fake_result("transform", "cap", "t",
                                                   types.SimpleNamespace(path="$policy_target.amount", value=1), 1),
            "transform result not object": fake_result("transform", "cap", "t",
                                                       types.SimpleNamespace(path="$policy_target", value=[1]), [1]),
            "unknown decision": fake_result("maybe", "ok", "a"),
        }
        for name, result in cases.items():
            with self.subTest(name), self.assertRaises(PDPError) as ctx:
                self.check(result)
            self.assertEqual(ctx.exception.status, 503, name)


class CacheAndConcurrencyTest(unittest.TestCase):
    def test_runtime_cache_is_keyed_by_content_and_bounded(self):
        ref = load_reference()
        e = Engine(instance_id="t", cache_size=2)
        req = build_request(ref, ref["cases"][0])
        e.evaluate(req)
        e.evaluate(req)
        self.assertEqual(e.runtimes_built, 1)
        # Same id and version with other content must not reuse the runtime.
        changed = copy.deepcopy(req)
        changed["policy"]["content"]["rules"][-2]["reason"] = "changed"
        self.assertEqual(e.evaluate(changed)["reasons"], ["changed"])
        self.assertEqual(e.runtimes_built, 2)
        for version in (2, 3, 4):
            r = copy.deepcopy(req)
            r["policy"]["version"] = version
            e.evaluate(r)
        self.assertEqual(e.cached_runtimes(), 2)
        self.assertLessEqual(len(list(e.work_dir.iterdir())), 2)

    def test_concurrent_evaluations(self):
        ref = load_reference()
        e = Engine(instance_id="t")
        cases = [c for c in ref["cases"] if not (c.get("agt_expect") or c["expect"]).get("error")]
        errors = []

        def run(case):
            try:
                got = e.evaluate(build_request(ref, case))
                if got["enforced_digest"] != case["expect"]["enforced_digest"]:
                    errors.append(case["name"])
            except Exception as exc:  # noqa: BLE001 - collected for the assertion
                errors.append(f"{case['name']}: {exc!r}")

        threads = [threading.Thread(target=run, args=(c,)) for c in cases * 3]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(errors, [])


if __name__ == "__main__":
    unittest.main()
