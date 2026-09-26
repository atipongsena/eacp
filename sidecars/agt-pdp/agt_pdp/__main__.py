"""Run the sidecar: python -m agt_pdp. Startup fails closed: an invalid
transport configuration or an engine stack whose versions cannot be read
(OPA missing, ACS not built) exits non-zero before listening."""

import json
import logging
import os
import signal
import ssl
import sys
import threading

from .engine import Engine, versions
from .server import ConfigError, Settings, make_server


def main():
    logging.basicConfig(level=logging.INFO, stream=sys.stdout, format="%(message)s")
    log = logging.getLogger("agt_pdp")
    try:
        settings = Settings.from_env()
        stack = versions()
    except ConfigError as exc:
        print(f"agt-pdp: config: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 - fail closed on an unusable engine stack
        print(f"agt-pdp: engine stack unavailable: {exc!r}", file=sys.stderr)
        return 1
    engine = Engine(instance_id=settings.instance_id, work_dir=os.environ.get("AGT_PDP_WORK_DIR") or None)
    try:
        server = make_server(settings, engine)
    except (OSError, ssl.SSLError) as exc:
        print(f"agt-pdp: listen: {exc}", file=sys.stderr)
        return 1
    stop = threading.Event()
    drained = threading.Event()

    def drain():
        log.info(json.dumps({"event": "draining", "delay_seconds": settings.shutdown_delay,
                             "timeout_seconds": settings.shutdown_timeout}))
        left = server.drain(settings.shutdown_delay, settings.shutdown_timeout)
        if left:
            log.warning(json.dumps({"event": "drain_timeout", "unanswered": left}))
        drained.set()

    def shutdown(*_):
        if not stop.is_set():
            stop.set()
            threading.Thread(target=drain, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    log.info(json.dumps({"event": "started", "listen": settings.listen, "mtls": settings.tls,
                         "instance": settings.instance_id, "versions": stack}))
    server.serve_forever()
    drained.wait()
    log.info(json.dumps({"event": "stopped"}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
