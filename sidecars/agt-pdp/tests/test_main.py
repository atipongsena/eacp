"""The sidecar process on SIGTERM (ADR-029 §3): it keeps serving for
AGT_PDP_SHUTDOWN_DELAY, then stops and exits cleanly."""

import http.client
import os
import signal
import socket
import subprocess
import sys
import time
import unittest

import agt_pdp


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class SigtermTest(unittest.TestCase):
    def start(self, delay):
        port = free_port()
        env = dict(os.environ, AGT_PDP_LISTEN=f"127.0.0.1:{port}", AGT_PDP_INSTANCE_ID="sigterm-test",
                   AGT_PDP_SHUTDOWN_DELAY=delay, AGT_PDP_SHUTDOWN_TIMEOUT="5s")
        proc = subprocess.Popen([sys.executable, "-m", "agt_pdp"], env=env, text=True,
                                cwd=os.path.dirname(os.path.dirname(os.path.abspath(agt_pdp.__file__))),
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        self.addCleanup(lambda: proc.poll() is None and proc.kill())
        self.assertIn('"started"', proc.stdout.readline())
        return proc, port

    @staticmethod
    def health(port):
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        try:
            conn.request("GET", "/healthz")
            resp = conn.getresponse()
            resp.read()
            return resp.status, resp.getheader("Connection")
        finally:
            conn.close()

    def test_it_keeps_serving_for_the_delay_after_sigterm(self):
        proc, port = self.start("2s")
        began = time.monotonic()
        proc.send_signal(signal.SIGTERM)
        time.sleep(0.5)
        self.assertEqual(self.health(port), (200, "close"), "a PDP that got SIGTERM must still accept connections")
        out, _ = proc.communicate(timeout=15)
        self.assertEqual(proc.returncode, 0, out)
        self.assertGreaterEqual(time.monotonic() - began, 2.0)
        self.assertIn('"draining"', out)
        self.assertIn('"stopped"', out)

    def test_without_a_delay_it_stops_promptly(self):
        proc, _ = self.start("0s")
        began = time.monotonic()
        proc.send_signal(signal.SIGTERM)
        out, _ = proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 0, out)
        self.assertLess(time.monotonic() - began, 3.0)
        self.assertIn('"stopped"', out)


if __name__ == "__main__":
    unittest.main()
