"""Example 02: an agent calls an LLM through the EACP gateway with the
official Anthropic SDK. The only changes an agent needs are the base URL
and its own EACP key; the provider key stays in the gateway."""

import json
import os
import sys
import urllib.request

import anthropic

api = os.environ["EACP_API"]
gateway = os.environ["EACP_GATEWAY"]


def step(title):
    print(f"\n== {title}")


# The agent's own EACP key, never a provider key.
client = anthropic.Anthropic(api_key=os.environ["AGENT_KEY"], base_url=gateway)

step("1. The agent asks an allowlisted model (sonnet) through the gateway")
message = client.messages.create(
    model="sonnet",
    max_tokens=256,
    messages=[{"role": "user", "content": "Summarise purchase order po-1042 in one line."}],
)
print(f"answer: {message.content[0].text!r}")
print(f"usage: {message.usage.input_tokens} input and {message.usage.output_tokens} output tokens")

step("2. The agent asks a model outside its allowlist (opus)")
try:
    client.messages.create(
        model="opus", max_tokens=64, messages=[{"role": "user", "content": "Hello"}]
    )
    sys.exit("the gateway let a model outside the allowlist through")
except anthropic.PermissionDeniedError as err:
    print(f"refused before anything reached the provider: HTTP {err.status_code}, {err.body['error']['type']}")

step("3. An operator reads the gateway's ledger")
request = urllib.request.Request(
    f"{api}/v1/llm-calls?agent={os.environ['AGENT_ID']}&limit=2",
    headers={"Authorization": f"Bearer {os.environ['OPERATOR_KEY']}"},
)
with urllib.request.urlopen(request, timeout=10) as response:
    calls = json.load(response)["calls"]
for call in calls:
    cost = f"{call['cost_amount']} {call['cost_unit']}" if call.get("cost_amount") else "no cost"
    print(f"{call['model']}: {call['state']} {call.get('denial') or call.get('outcome', '')}, {cost}")
states = {call["model"]: call["state"] for call in calls}
if states.get("sonnet") != "SETTLED" or states.get("opus") != "DENIED":
    sys.exit(f"unexpected ledger states: {states}")

print("\nExample 02 passed.")
