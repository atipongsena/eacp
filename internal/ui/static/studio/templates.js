// templates.js holds the Studio page's templates, static in the page
// (Phase 27a-3b). A template only fills the form: its definition is saved,
// validated and approved like any other. The leave-balance definition is the
// Studio demo's fixture (test/demo/testdata/leave-balance.json); a node test
// keeps the two equal.
import {t} from '../i18n.js';

export const TEMPLATES = [{
  id: 'leave-balance',
  title: () => t('Leave balance (HR)'),
  summary: () => t('Looks up how many days of leave an employee has left, through the HR system’s read-only tool.'),
  name: 'leave-bot',
  displayName: 'Leave balance',
  description: 'Tells an employee how many days of leave they have left.',
  definition: {
    schema_version: 1,
    kind: 'agent',
    inputs: {employee_id: {type: 'string', max_length: 64}},
    steps: [
      {id: 'lookup', kind: 'tool_call', tool: 'hr-mcp.get_leave_balance', tool_schema_version: '1',
        operation: 'lookup', target: 'hr', resource: 'leave_balance',
        payload: {employee_id: '{{inputs.employee_id}}'}},
      {id: 'answer', kind: 'respond',
        text: 'You have {{steps.lookup.output.structuredContent.days}} days of leave left.'},
    ],
    limits: {timeout_seconds: 120},
  },
}];

// template returns the template with id, or null.
export const template = id => TEMPLATES.find(x => x.id === id) ?? null;

TEMPLATES.push({id: "leave-triage", title: () => t('Leave triage (HR)'), summary: () => t('Looks up the balance, classifies it with a governed model and follows a fixed choice.'), name: "leave-triage", displayName: "Leave triage (HR)", description: "Looks up the balance, classifies it with a governed model and follows a fixed choice.", definition: {
  "schema_version": 2,
  "kind": "agent",
  "inputs": {
    "employee_id": {
      "type": "string",
      "max_length": 64
    }
  },
  "steps": [
    {
      "id": "lookup",
      "kind": "tool_call",
      "tool": "hr-mcp.get_leave_balance",
      "tool_schema_version": "1",
      "operation": "lookup",
      "target": "hr",
      "resource": "leave_balance",
      "payload": {
        "employee_id": "{{inputs.employee_id}}"
      },
      "next": "classify"
    },
    {
      "id": "classify",
      "kind": "llm",
      "model": "triage",
      "instruction": "Return one JSON object with eligible (boolean). Classify this request; do not choose a destination.",
      "input": {
        "days": "{{steps.lookup.output.structuredContent.days}}"
      },
      "max_output_tokens": 100,
      "output_schema": {
        "type": "object",
        "properties": {
          "eligible": {
            "type": "boolean"
          }
        },
        "required": [
          "eligible"
        ],
        "additionalProperties": false
      },
      "next": "choose"
    },
    {
      "id": "choose",
      "kind": "branch",
      "condition": {
        "left": "{{steps.classify.output.eligible}}",
        "operator": "eq",
        "right": true
      },
      "then": "yes",
      "else": "no"
    },
    {
      "id": "yes",
      "kind": "respond",
      "text": "The request is eligible. Check HR policy before applying."
    },
    {
      "id": "no",
      "kind": "respond",
      "text": "Ask HR to check this request."
    }
  ],
  "limits": {
    "timeout_seconds": 120,
    "max_output_tokens": 100
  }
}});

TEMPLATES.push({id: "procurement-triage", title: () => t('Procurement triage'), summary: () => t('Classifies a request with a governed model; both paths answer without placing an order.'), name: "procurement-triage", displayName: "Procurement triage", description: "Classifies a request with a governed model; both paths answer without placing an order.", definition: {
  "schema_version": 2,
  "kind": "agent",
  "inputs": {
    "request": {
      "type": "string",
      "max_length": 1000
    }
  },
  "steps": [
    {
      "id": "classify",
      "kind": "llm",
      "model": "triage",
      "instruction": "Return one JSON object with eligible (boolean). Classify this request; do not choose a destination.",
      "input": {
        "request": "{{inputs.request}}"
      },
      "max_output_tokens": 100,
      "output_schema": {
        "type": "object",
        "properties": {
          "eligible": {
            "type": "boolean"
          }
        },
        "required": [
          "eligible"
        ],
        "additionalProperties": false
      },
      "next": "choose"
    },
    {
      "id": "choose",
      "kind": "branch",
      "condition": {
        "left": "{{steps.classify.output.eligible}}",
        "operator": "eq",
        "right": true
      },
      "then": "yes",
      "else": "no"
    },
    {
      "id": "yes",
      "kind": "respond",
      "text": "Send this request to procurement for review."
    },
    {
      "id": "no",
      "kind": "respond",
      "text": "Add the missing information before procurement review."
    }
  ],
  "limits": {
    "timeout_seconds": 120,
    "max_output_tokens": 100
  }
}});
