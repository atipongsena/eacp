"""The sidecar's HTTP surface and transport rules (ADR-002 §2, §8)."""

import http.client
import json
import threading
import time
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


class SlowEngine:
    """Decides after a pause, so a decision can be in flight while a drain ends."""

    def __init__(self, pause):
        self.pause = pause
        self.started = threading.Event()
        self.finished = threading.Event()

    def evaluate(self, req):
        self.started.set()
        time.sleep(self.pause)
        self.finished.set()
        return {"verdict": "allow", "decision_id": "slow", "policy_version": 1}


class DrainTest(unittest.TestCase):
    """ADR-029 §3 for the PDP: Kubernetes keeps routing new connections to a
    stopping pod until every node has seen its endpoint go, so the sidecar
    keeps serving for the delay, tells kept-alive clients to leave, answers
    every decision it accepted and closes idle connections itself while its
    network still works."""

    def serve(self, pause=1.0):
        self.engine = SlowEngine(pause)
        self.server = make_server(Settings(listen="127.0.0.1:0", instance_id="drain-test"), self.engine)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)

    def conn(self):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        self.addCleanup(c.close)
        return c

    @staticmethod
    def health(conn):
        conn.request("GET", "/healthz")
        resp = conn.getresponse()
        resp.read()
        return resp.status, resp.getheader("Connection")

    def evaluate_in_background(self, conn):
        result = {}

        def post():
            conn.request("POST", "/v1/evaluate", body=b"{}", headers={"Content-Type": "application/json"})
            resp = conn.getresponse()
            resp.read()
            result["answer"] = (resp.status, resp.getheader("Connection"))

        t = threading.Thread(target=post, daemon=True)
        t.start()
        return t, result

    def drain_in_background(self, delay, timeout=10):
        done = threading.Event()

        def run():
            self.server.drain(delay, timeout)
            done.set()

        threading.Thread(target=run, daemon=True).start()
        return done

    def test_it_serves_through_the_delay_then_answers_what_it_accepted(self):
        self.serve(pause=1.0)
        kept, idle = self.conn(), self.conn()
        for c in (kept, idle):
            self.assertEqual(self.health(c), (200, None), "connections are kept alive before the drain")
        began = time.monotonic()
        drained = self.drain_in_background(delay=1.5)
        time.sleep(0.3)
        self.assertEqual(self.health(self.conn()), (200, "close"), "a new connection during the delay is served")
        self.assertEqual(self.health(kept), (200, "close"), "a kept-alive client is answered and told to leave")
        time.sleep(0.9)
        t, result = self.evaluate_in_background(self.conn())  # still deciding when the delay ends
        self.assertTrue(self.engine.started.wait(5))
        self.assertTrue(drained.wait(10))
        self.assertTrue(self.engine.finished.is_set(), "the drain ended before an accepted decision was answered")
        self.assertGreaterEqual(time.monotonic() - began, 1.5)
        t.join(5)
        self.assertEqual(result.get("answer"), (200, "close"))
        self.assertEqual(idle.sock.recv(1), b"", "an idle kept-alive connection must be closed by the drain")
        with self.assertRaises(ConnectionRefusedError):
            self.health(self.conn())

    def test_without_a_delay_it_stops_accepting_at_once_but_answers_in_flight_work(self):
        self.serve(pause=1.0)
        t, result = self.evaluate_in_background(self.conn())
        self.assertTrue(self.engine.started.wait(5))
        drained = self.drain_in_background(delay=0)
        refused, deadline = False, time.monotonic() + 2
        while not refused and time.monotonic() < deadline:
            try:  # a connection the listener took is answered, never reset
                self.assertEqual(self.health(self.conn()), (200, "close"))
            except ConnectionRefusedError:
                refused = True
        self.assertTrue(refused, "without a delay the listener closes at once")
        self.assertFalse(self.engine.finished.is_set(), "the listener closed only after the decision finished")
        self.assertTrue(drained.wait(10))
        self.assertTrue(self.engine.finished.is_set())
        t.join(5)
        self.assertEqual(result.get("answer"), (200, "close"))

    def test_the_timeout_bounds_the_wait_for_in_flight_work(self):
        self.serve(pause=3.0)
        self.evaluate_in_background(self.conn())
        self.assertTrue(self.engine.started.wait(5))
        began = time.monotonic()
        self.assertTrue(self.drain_in_background(delay=0, timeout=0.5).wait(5))
        self.assertLess(time.monotonic() - began, 2.0)
        self.assertFalse(self.engine.finished.is_set())


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

    def test_shutdown_delay_and_timeout_are_whole_seconds(self):
        s = Settings.from_env({})
        self.assertEqual((s.shutdown_delay, s.shutdown_timeout), (0, 15))
        s = Settings.from_env({"AGT_PDP_SHUTDOWN_DELAY": "10s", "AGT_PDP_SHUTDOWN_TIMEOUT": "20s"})
        self.assertEqual((s.shutdown_delay, s.shutdown_timeout), (10, 20))
        self.assertEqual(Settings.from_env({"AGT_PDP_SHUTDOWN_DELAY": "60s"}).shutdown_delay, 60)
        bad = [("AGT_PDP_SHUTDOWN_DELAY", v) for v in ("61s", "-1s", "1.5s", "10", "1m", "", " 10s")]
        bad += [("AGT_PDP_SHUTDOWN_TIMEOUT", v) for v in ("0s", "10", "s", "-5s", "")]
        for key, value in bad:
            with self.subTest(key=key, value=value), self.assertRaises(ConfigError):
                Settings.from_env({key: value})

    def test_listen_must_be_host_port(self):
        for bad in ("8181", "127.0.0.1", "127.0.0.1:http", "127.0.0.1:70000"):
            with self.subTest(bad), self.assertRaises(ConfigError):
                Settings.from_env({"AGT_PDP_LISTEN": bad})


if __name__ == "__main__":
    unittest.main()
