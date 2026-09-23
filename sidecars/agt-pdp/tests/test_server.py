"""The sidecar's HTTP surface and transport rules (ADR-002 §2, §8)."""

import http.client
import json
import threading
import unittest

from agt_pdp.engine import Engine
from agt_pdp.server import ConfigError, Settings, make_server
from tests.test_engine import build_request, load_reference


class ServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ref = load_reference()
        cls.server = make_server(Settings(listen="127.0.0.1:0", instance_id="server-test"), Engine(instance_id="server-test"))
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def call(self, method, path, body=None, headers=None):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=30)
        try:
            conn.request(method, path, body=body, headers=headers or {})
            resp = conn.getresponse()
            raw = resp.read()
            return resp.status, resp.getheader("Content-Type"), json.loads(raw) if raw else None
        finally:
            conn.close()

    def evaluate(self, payload, **headers):
        body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        return self.call("POST", "/v1/evaluate", body, {"Content-Type": "application/json", **headers})

    def test_health_reports_protocol_and_versions(self):
        status, ctype, body = self.call("GET", "/healthz")
        self.assertEqual((status, ctype), (200, "application/json"))
        self.assertEqual(body["status"], "ok")
        self.assertEqual(body["protocol"], "eacp-agt-pdp/1")
        self.assertEqual(set(body["versions"]), {"agt", "acs", "opa"})
        self.assertEqual(body["provider_instance_id"], "server-test")

    def test_evaluate(self):
        case = self.ref["cases"][0]
        status, ctype, body = self.evaluate(build_request(self.ref, case))
        self.assertEqual((status, ctype), (200, "application/json"))
        self.assertEqual(body["verdict"], "allow")
        self.assertEqual(body["input_digest"], case["expect"]["input_digest"])

    def test_error_statuses(self):
        unsupported = next(c for c in self.ref["cases"] if c.get("agt_expect"))
        self.assertEqual(self.evaluate(build_request(self.ref, unsupported))[0], 422)
        status, _, body = self.evaluate(b'{"protocol":')
        self.assertEqual((status, body["error"]), (400, "bad_request"))
        self.assertEqual(self.evaluate(b'{"a":1,"a":2}')[0], 400)
        status, _, _ = self.call("POST", "/v1/evaluate", b"{}", {"Content-Type": "text/plain"})
        self.assertEqual(status, 415)
        self.assertEqual(self.call("GET", "/v1/evaluate")[0], 405)
        self.assertEqual(self.call("GET", "/nope")[0], 404)
        self.assertEqual(self.call("POST", "/healthz", b"{}", {"Content-Type": "application/json"})[0], 405)

    def test_oversized_body_is_refused_before_reading(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=30)
        try:
            conn.putrequest("POST", "/v1/evaluate")
            conn.putheader("Content-Type", "application/json")
            conn.putheader("Content-Length", str(64 << 20))
            conn.endheaders()
            resp = conn.getresponse()
            self.assertEqual(resp.status, 413)
        finally:
            conn.close()

    def test_missing_length_is_refused(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=30)
        try:
            conn.putrequest("POST", "/v1/evaluate")
            conn.putheader("Content-Type", "application/json")
            conn.putheader("Transfer-Encoding", "chunked")
            conn.endheaders()
            conn.send(b"0\r\n\r\n")
            self.assertEqual(conn.getresponse().status, 411)
        finally:
            conn.close()


class SettingsTest(unittest.TestCase):
    def test_plaintext_only_on_loopback(self):
        for ok in ("127.0.0.1:8181", "[::1]:8181", "localhost:8181"):
            Settings.from_env({"AGT_PDP_LISTEN": ok})
        for bad in ("0.0.0.0:8181", "10.1.2.3:8181", "agt-pdp:8181", "[::]:8181", ":8181"):
            with self.subTest(bad), self.assertRaises(ConfigError):
                Settings.from_env({"AGT_PDP_LISTEN": bad})

    def test_tls_needs_every_file(self):
        with self.assertRaises(ConfigError):
            Settings.from_env({"AGT_PDP_LISTEN": "0.0.0.0:8443", "AGT_PDP_TLS_CERT_FILE": "/c", "AGT_PDP_TLS_KEY_FILE": "/k"})
        s = Settings.from_env({"AGT_PDP_LISTEN": "0.0.0.0:8443", "AGT_PDP_TLS_CERT_FILE": "/c",
                               "AGT_PDP_TLS_KEY_FILE": "/k", "AGT_PDP_TLS_CLIENT_CA_FILE": "/ca"})
        self.assertTrue(s.tls)

    def test_listen_must_be_host_port(self):
        for bad in ("8181", "127.0.0.1", "127.0.0.1:http", "127.0.0.1:70000"):
            with self.subTest(bad), self.assertRaises(ConfigError):
                Settings.from_env({"AGT_PDP_LISTEN": bad})


if __name__ == "__main__":
    unittest.main()
