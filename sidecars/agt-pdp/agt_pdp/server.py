"""HTTP surface of the AGT sidecar PDP (wire protocol eacp-agt-pdp/1).

    GET  /healthz       protocol, engine versions, instance id
    POST /v1/evaluate   one decision (see engine.Engine.evaluate)

Transport (ADR-002 §2, §8): with a server certificate, key and client CA the
sidecar speaks TLS 1.3 only and requires a client certificate from that CA;
without them it refuses to listen anywhere but loopback. Bodies are bounded,
concurrency is bounded (503 busy beyond it) and every socket has a timeout.

Shutdown (ADR-029 §3): Server.drain keeps accepting and answering for
AGT_PDP_SHUTDOWN_DELAY while every response closes its connection, then
stops accepting, closes idle kept-alive connections and waits up to
AGT_PDP_SHUTDOWN_TIMEOUT for the decisions it accepted. Kubernetes keeps
routing new connections to a stopping pod until every node has seen its
endpoint go, so a sidecar that stopped at once refused them.
"""

import ipaddress
import json
import logging
import os
import re
import select
import socket
import ssl
import sys
import threading
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .engine import PROTOCOL, PDPError, versions
from .strictjson import StrictJSONError, loads

log = logging.getLogger("agt_pdp.server")

MAX_BODY = 4 << 20  # payload (1 MiB) + policy bundle + binding, with headroom
MAX_CONCURRENT = 16
SOCKET_TIMEOUT = 30
MAX_SHUTDOWN_DELAY = 60  # seconds, as EACP_SHUTDOWN_DELAY


class ConfigError(ValueError):
    pass


def _seconds(env, key, default):
    raw = env.get(key, default)
    if not re.fullmatch(r"[0-9]+s", raw):
        raise ConfigError(f"{key} must be whole seconds like 10s, not {raw!r}")
    return int(raw[:-1])


@dataclass(frozen=True)
class Settings:
    listen: str = "127.0.0.1:8181"
    instance_id: str = "agt-pdp"
    cert_file: str = ""
    key_file: str = ""
    client_ca_file: str = ""
    max_body: int = MAX_BODY
    max_concurrent: int = MAX_CONCURRENT
    shutdown_delay: int = 0  # seconds of serving after SIGTERM before the listener closes
    shutdown_timeout: int = 15  # seconds to wait for accepted decisions after that

    @property
    def tls(self):
        return bool(self.cert_file)

    @property
    def address(self):
        host, _, port = self.listen.rpartition(":")
        return host.strip("[]"), int(port)

    @classmethod
    def from_env(cls, env=None):
        env = os.environ if env is None else env
        s = cls(listen=env.get("AGT_PDP_LISTEN", cls.listen),
                instance_id=env.get("AGT_PDP_INSTANCE_ID") or f"agt-pdp/{socket.gethostname()}",
                cert_file=env.get("AGT_PDP_TLS_CERT_FILE", ""), key_file=env.get("AGT_PDP_TLS_KEY_FILE", ""),
                client_ca_file=env.get("AGT_PDP_TLS_CLIENT_CA_FILE", ""),
                shutdown_delay=_seconds(env, "AGT_PDP_SHUTDOWN_DELAY", "0s"),
                shutdown_timeout=_seconds(env, "AGT_PDP_SHUTDOWN_TIMEOUT", "15s"))
        s.validate()
        return s

    def validate(self):
        if not 0 <= self.shutdown_delay <= MAX_SHUTDOWN_DELAY:
            raise ConfigError(f"AGT_PDP_SHUTDOWN_DELAY must be from 0s to {MAX_SHUTDOWN_DELAY}s")
        if self.shutdown_timeout < 1:
            raise ConfigError("AGT_PDP_SHUTDOWN_TIMEOUT must be at least 1s")
        host, sep, port = self.listen.rpartition(":")
        if not sep or not host or not port.isdigit() or not 0 <= int(port) <= 65535:
            raise ConfigError(f"AGT_PDP_LISTEN must be host:port, not {self.listen!r}")
        files = [self.cert_file, self.key_file, self.client_ca_file]
        if any(files) and not all(files):
            raise ConfigError("mutual TLS needs AGT_PDP_TLS_CERT_FILE, AGT_PDP_TLS_KEY_FILE and AGT_PDP_TLS_CLIENT_CA_FILE")
        if not self.tls and not _loopback(host.strip("[]")):
            raise ConfigError(f"without mutual TLS the sidecar listens on loopback only, not {host!r}")


def _loopback(host):
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def server_tls_context(s: Settings):
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    ctx.load_cert_chain(s.cert_file, s.key_file)
    ctx.load_verify_locations(cafile=s.client_ca_file)
    ctx.verify_mode = ssl.CERT_REQUIRED
    return ctx


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "agt-pdp"
    sys_version = ""
    timeout = SOCKET_TIMEOUT

    def setup(self):
        # The TLS handshake runs here, in the request thread, so a slow or
        # hostile client cannot stall the accept loop.
        if isinstance(self.request, ssl.SSLSocket):
            self.request.settimeout(self.timeout)
            self.request.do_handshake()
        super().setup()
        self.answered = False
        self.server.track(self)  # busy: a new connection carries a request

    def finish(self):
        try:
            super().finish()
        finally:
            self.server.untrack(self)

    def handle_one_request(self):
        # After an answer the connection is idle, and a drain may close it.
        if self.answered and not self.server.mark(self, busy=False):
            self.close_connection = True
            return
        super().handle_one_request()
        self.answered = True

    def parse_request(self):
        # A request line arrived: the drain now waits for the answer.
        self.server.mark(self, busy=True)
        return super().parse_request()

    def log_message(self, fmt, *args):
        pass

    def _send(self, status, body):
        if self.server.draining.is_set():
            self.close_connection = True  # the client moves to another replica
        raw = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        if self.close_connection:
            self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)

    def _error(self, status, code, detail="", close=False):
        if close:
            self.close_connection = True
        self._send(status, {"error": code, "detail": detail[:512]})

    def do_GET(self):
        if self.path == "/healthz":
            try:
                self._send(200, {"status": "ok", "protocol": PROTOCOL, "versions": versions(),
                                 "provider_instance_id": self.server.settings.instance_id})
            except Exception as exc:  # noqa: BLE001 - health reports, never raises
                self._error(503, "unhealthy", type(exc).__name__)
        elif self.path == "/v1/evaluate":
            self._error(405, "method_not_allowed")
        else:
            self._error(404, "not_found")

    def do_POST(self):
        started = time.monotonic()
        if self.path != "/v1/evaluate":
            self._error(405 if self.path == "/healthz" else 404,
                        "method_not_allowed" if self.path == "/healthz" else "not_found", close=True)
            return
        if (self.headers.get("Content-Type") or "").split(";")[0].strip().lower() != "application/json":
            self._error(415, "unsupported_media_type", close=True)
            return
        length = self.headers.get("Content-Length")
        if self.headers.get("Transfer-Encoding") or length is None or not length.isdigit():
            self._error(411, "length_required", close=True)
            return
        if int(length) > self.server.settings.max_body:
            self._error(413, "too_large", close=True)
            return
        body = self.rfile.read(int(length))
        try:
            req = loads(body)
        except StrictJSONError as exc:
            self._error(400, "bad_request", str(exc))
            return
        if not self.server.slots.acquire(timeout=1):
            self._error(503, "busy")
            return
        try:
            decision = self.server.engine.evaluate(req)
        except PDPError as exc:
            log.warning(json.dumps({"event": "evaluate", "status": exc.status, "error": exc.code, "detail": exc.detail[:256]}))
            self._error(exc.status, exc.code, exc.detail)
            return
        except Exception as exc:  # noqa: BLE001 - never leak a traceback to the client
            log.exception("evaluate failed")
            self._error(500, "internal", type(exc).__name__)
            return
        finally:
            self.server.slots.release()
        log.info(json.dumps({"event": "evaluate", "status": 200, "verdict": decision["verdict"],
                             "decision_id": decision["decision_id"], "policy_version": decision["policy_version"],
                             "ms": round((time.monotonic() - started) * 1000, 1)}))
        self._send(200, decision)


class Server(ThreadingHTTPServer):
    daemon_threads = True
    block_on_close = False

    def __init__(self, settings, engine):
        host, port = settings.address
        self.address_family = socket.AF_INET6 if ":" in host else socket.AF_INET
        super().__init__((host, port), Handler)
        self.settings = settings
        self.engine = engine
        self.slots = threading.BoundedSemaphore(settings.max_concurrent)
        self.draining = threading.Event()
        self._conns = {}  # handler -> busy; guarded by _changed
        self._changed = threading.Condition()
        self._closing = False
        self._closed = set()  # idle connections the drain closed
        if settings.tls:
            self.socket = server_tls_context(settings).wrap_socket(
                self.socket, server_side=True, do_handshake_on_connect=False)

    def track(self, handler):
        with self._changed:
            self._conns[handler] = True

    def untrack(self, handler):
        with self._changed:
            self._conns.pop(handler, None)
            self._changed.notify_all()

    def mark(self, handler, busy):
        """Record whether a connection is answering a request. Returns False
        once the drain has closed idle connections: an idle one stops."""
        with self._changed:
            if not busy and self._closing:
                return False
            self._conns[handler] = busy
            self._changed.notify_all()
            return True

    def drain(self, delay, timeout):
        """Stop serving without refusing a request Kubernetes still routes
        here (ADR-029 §3). For delay seconds the listener stays open and every
        response closes its connection; then the listener closes after
        serving the connections already in its backlog, idle kept-alive
        connections are closed from this side while the network still works,
        and the drain waits up to timeout seconds for requests being answered.
        serve_forever must be running in another thread. Returns how many
        connections were still unanswered."""
        self.draining.set()
        if delay > 0:
            time.sleep(delay)
        self.shutdown()
        # Connections the kernel completed while the loop stopped would be
        # reset by the close; serve them instead.
        while select.select([self.socket], [], [], 0)[0]:
            try:
                request, client_address = self.get_request()
            except OSError:
                break  # a failing accept ends the backlog instead of spinning
            try:
                self.process_request(request, client_address)
            except Exception:  # noqa: BLE001 - as socketserver does
                self.handle_error(request, client_address)
                self.shutdown_request(request)
        self.server_close()
        deadline = time.monotonic() + timeout
        with self._changed:
            self._closing = True
            for handler, busy in self._conns.items():
                if not busy:
                    self._closed.add(handler.connection)
                    try:
                        # The plain socket's shutdown: a FIN, even under TLS.
                        socket.socket.shutdown(handler.connection, socket.SHUT_RDWR)
                    except OSError:
                        pass
            while True:
                left = sum(self._conns.values())
                remaining = deadline - time.monotonic()
                if not left or remaining <= 0:
                    return left
                self._changed.wait(remaining)

    def handle_error(self, request, client_address):
        if request in self._closed:
            return  # an idle connection the drain closed: its read ends without close_notify
        exc = sys.exc_info()[1]
        log.warning(json.dumps({"event": "connection_error", "error": type(exc).__name__, "detail": str(exc)[:200]}))


def make_server(settings: Settings, engine) -> Server:
    settings.validate()
    return Server(settings, engine)
