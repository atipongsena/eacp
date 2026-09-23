"""EACP bundle validation and its mapping to Rego data (ADR-002 §3, §8).

The sidecar must accept exactly what the local provider accepts, and refuse
what ACS cannot express (escalate with set) instead of deciding differently.
"""

import copy
import unittest

from agt_pdp.bundle import PolicyError, parse_bundle
from agt_pdp.strictjson import StrictJSONError, loads

BASE = {"format_version": 1, "rules": [
    {"id": "a", "match": {"operation": "x", "subject": ""}, "verdict": "allow", "reason": "ok"},
    {"id": "t", "verdict": "transform", "reason": "cap", "set": {"amount": 1, "nested": {"n": 2.5}}},
    {"id": "e", "match": {"tool": "t"}, "verdict": "escalate", "reason": "review",
     "approval": {"quorum": 2, "eligible_roles": ["approver"], "ttl_seconds": 600}},
]}


def mutated(fn):
    b = copy.deepcopy(BASE)
    fn(b)
    return b


class ParseBundleTest(unittest.TestCase):
    def test_valid_bundle_maps_to_rego_data(self):
        bundle = parse_bundle(BASE)
        self.assertEqual(bundle.rego_data(), {"rules": [
            {"id": "a", "match": {"operation": "x"}, "verdict": "allow", "reason": "ok"},
            {"id": "t", "match": {}, "verdict": "transform", "reason": "cap", "set": {"amount": 1, "nested": {"n": 2.5}}},
            {"id": "e", "match": {"tool": "t"}, "verdict": "escalate", "reason": "review"},
        ]})
        self.assertEqual(bundle.rules["e"].approval, {"quorum": 2, "eligible_roles": ["approver"], "ttl_seconds": 600})
        self.assertIsNone(bundle.rules["a"].approval)

    def test_null_optional_fields_are_absent(self):
        b = mutated(lambda b: b["rules"][0].update({"match": None, "set": None, "approval": None}))
        self.assertEqual(parse_bundle(b).rego_data()["rules"][0]["match"], {})

    def test_integral_numbers_are_integers(self):
        b = mutated(lambda b: (b.update({"format_version": 1.0}), b["rules"][2]["approval"].update({"quorum": 2.0})))
        self.assertEqual(parse_bundle(b).rules["e"].approval["quorum"], 2)

    def test_escalate_with_set_is_unsupported_not_invalid(self):
        b = mutated(lambda b: b["rules"][2].update({"set": {"amount": 1}}))
        with self.assertRaises(PolicyError) as ctx:
            parse_bundle(b)
        self.assertEqual(ctx.exception.code, "policy_unsupported")
        # An empty set changes nothing, as in the local provider.
        parse_bundle(mutated(lambda b: b["rules"][2].update({"set": {}})))

    def test_invalid_bundles(self):
        cases = {
            "not an object": lambda b: b.clear() or b.update({"rules": "x"}),
            "extra top-level key": lambda b: b.update({"x": 1}),
            "format_version 2": lambda b: b.update({"format_version": 2}),
            "format_version bool": lambda b: b.update({"format_version": True}),
            "format_version 1.5": lambda b: b.update({"format_version": 1.5}),
            "no rules": lambda b: b.update({"rules": []}),
            "rule not object": lambda b: b["rules"].append("x"),
            "unknown rule key": lambda b: b["rules"][0].update({"priority": 1}),
            "blank id": lambda b: b["rules"][0].update({"id": " "}),
            "duplicate id": lambda b: b["rules"][1].update({"id": "a"}),
            "blank reason": lambda b: b["rules"][0].update({"reason": "  "}),
            "reason not string": lambda b: b["rules"][0].update({"reason": 1}),
            "unknown verdict": lambda b: b["rules"][0].update({"verdict": "maybe"}),
            "unknown match key": lambda b: b["rules"][0]["match"].update({"agent": "x"}),
            "match value not string": lambda b: b["rules"][0]["match"].update({"tool": 1}),
            "match not object": lambda b: b["rules"][0].update({"match": ["x"]}),
            "allow with set": lambda b: b["rules"][0].update({"set": {"a": 1}}),
            "allow with approval": lambda b: b["rules"][0].update({"approval": BASE["rules"][2]["approval"]}),
            "transform without set": lambda b: b["rules"][1].update({"set": {}}),
            "transform with approval": lambda b: b["rules"][1].update({"approval": BASE["rules"][2]["approval"]}),
            "blank set key": lambda b: b["rules"][1]["set"].update({"": 1}),
            "set number too large": lambda b: b["rules"][1]["set"].update({"n": 2 ** 53 + 1}),
            "nested set number too large": lambda b: b["rules"][1]["set"].update({"n": [{"x": 1e300}]}),
            "escalate without approval": lambda b: b["rules"][2].update({"approval": None}),
            "quorum 0": lambda b: b["rules"][2]["approval"].update({"quorum": 0}),
            "quorum 6": lambda b: b["rules"][2]["approval"].update({"quorum": 6}),
            "quorum bool": lambda b: b["rules"][2]["approval"].update({"quorum": True}),
            "ttl 0": lambda b: b["rules"][2]["approval"].update({"ttl_seconds": 0}),
            "ttl too long": lambda b: b["rules"][2]["approval"].update({"ttl_seconds": 86401}),
            "other role": lambda b: b["rules"][2]["approval"].update({"eligible_roles": ["admin"]}),
            "two roles": lambda b: b["rules"][2]["approval"].update({"eligible_roles": ["approver", "approver"]}),
            "unknown approval key": lambda b: b["rules"][2]["approval"].update({"when": 1}),
            "missing approval key": lambda b: b["rules"][2]["approval"].pop("ttl_seconds"),
        }
        for name, fn in cases.items():
            with self.subTest(name):
                with self.assertRaises(PolicyError) as ctx:
                    parse_bundle(mutated(fn))
                self.assertEqual(ctx.exception.code, "policy_invalid")
        with self.assertRaises(PolicyError):
            parse_bundle(["not", "an", "object"])


class StrictJSONTest(unittest.TestCase):
    def test_rejects_ambiguous_json(self):
        for raw in [b'{"a":1,"a":2}', b'{"a":NaN}', b'{"a":Infinity}', b'[1] 2', b'{"a":"\\ud800"}', b"\xff",
                    b'{"a":' + b"[" * 200 + b"]" * 200 + b"}"]:
            with self.subTest(raw[:20]), self.assertRaises(StrictJSONError):
                loads(raw)

    def test_accepts_plain_json(self):
        self.assertEqual(loads(b'{"a":[1,2.5,"\\u00e9",null,true]}'), {"a": [1, 2.5, "é", None, True]})


if __name__ == "__main__":
    unittest.main()
