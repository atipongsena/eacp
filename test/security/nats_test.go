package security

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// natsSession speaks the NATS client protocol from a throwaway container on
// the internal bus network: it connects as user with password, publishes
// an empty message to subject, and returns the server's replies.
func natsSession(user, password, subject string) (string, error) {
	script := fmt.Sprintf(`{ printf 'CONNECT {"user":"%s","pass":"%s","verbose":true,"pedantic":false}\r\nPUB %s 0\r\n\r\nPING\r\n'; sleep 2; } | nc -w 3 nats 4222`,
		user, password, subject)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_bus", "busybox:1.37", "sh", "-c", script).CombinedOutput()
	return string(out), err
}

func natsSubscribeSession(user, password, subject string) (string, error) {
	script := fmt.Sprintf(`{ printf 'CONNECT {"user":"%s","pass":"%s","verbose":true,"pedantic":false}\r\nSUB %s 1\r\nPING\r\n'; sleep 2; } | nc -w 3 nats 4222`,
		user, password, subject)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_bus", "busybox:1.37", "sh", "-c", script).CombinedOutput()
	return string(out), err
}

// Positive control: the relay credential may publish work hints, so the
// negative test below fails for the permission and not for a broken probe.
// ($JS.API.INFO is a harmless subject the relay may publish on.)
func TestRelayCredentialMayPublish(t *testing.T) {
	requireCompose(t)
	out, err := natsSession("relay", "relay_dev", "$JS.API.INFO")
	if err != nil || !strings.Contains(out, "PONG") || strings.Contains(out, "-ERR") {
		t.Fatalf("relay session (err=%v): %s", err, out)
	}
}

// ADR-014 §6: the worker's NATS credential reads its own consumer only; it
// cannot forge a work hint or a dashboard event.
func TestWorkerCredentialCannotPublishHintsOrEvents(t *testing.T) {
	requireCompose(t)
	for _, subject := range []string{"eacp.work.probe", "eacp.events.probe", "$JS.API.STREAM.DELETE.EACP_WORK"} {
		out, err := natsSession("worker", "worker_dev", subject)
		if err != nil || !strings.Contains(out, "Permissions Violation for Publish") {
			t.Errorf("worker publishing on %s (err=%v): %s", subject, err, out)
		}
	}
}

func TestWorkerMaySubscribeOnlyToKillEvents(t *testing.T) {
	requireCompose(t)
	allowed, err := natsSubscribeSession("worker", "worker_dev", "eacp.events.*.kill.changed")
	if err != nil || !strings.Contains(allowed, "PONG") || strings.Contains(allowed, "Permissions Violation") {
		t.Fatalf("worker kill subscription (err=%v): %s", err, allowed)
	}
	denied, err := natsSubscribeSession("worker", "worker_dev", "eacp.events.>")
	if err != nil || !strings.Contains(denied, "Permissions Violation for Subscription") {
		t.Fatalf("worker broad events subscription (err=%v): %s", err, denied)
	}
}

// ADR-014 §6: NATS is on an internal network the agent runtime cannot reach.
func TestAgentCannotReachNATS(t *testing.T) {
	requireCompose(t)
	if out, err := fromAgent("nc", "-z", "-w", "3", "nats", "4222"); err == nil {
		t.Fatalf("agent reached NATS: %q", out)
	}
}

// Both services are wired to NATS with their own credential, and the relay
// and the hint consumer came up.
func TestTheRelayAndTheWorkerUseNATS(t *testing.T) {
	requireCompose(t)
	for service, want := range map[string]string{
		"controlplane-api": "EACP_NATS_URL=nats://relay:",
		"execution-worker": "EACP_NATS_URL=nats://worker:",
	} {
		if env, _ := inspect(t, service); !strings.Contains(env, want) {
			t.Errorf("%s is not configured for NATS: %s", service, env)
		}
	}
	for service, want := range map[string]string{
		"controlplane-api": "outbox relay started",
		"execution-worker": "work hints on",
	} {
		logs, err := exec.Command("docker", "compose", "logs", "--no-color", service).CombinedOutput()
		if err != nil || !strings.Contains(string(logs), want) {
			t.Errorf("%s did not log %q (err=%v)", service, want, err)
		}
		if strings.Contains(string(logs), "relay_dev") || strings.Contains(string(logs), "worker_dev") {
			t.Errorf("%s logged a NATS password", service)
		}
	}
}
