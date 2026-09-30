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
