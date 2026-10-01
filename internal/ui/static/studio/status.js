// status.js says in plain words where a Studio agent is and why a run
// failed (program spec 8.3: the employee is never left guessing). The server
// names each status, the role that acts next and each failure reason
// (ADR-033); this module only words them. A value it does not know is shown
// as the server sent it, never hidden.
import {t} from '../i18n.js';

// STAGES are the steps from saving to running; stageIndex is the first one
// not yet done (4 when all are done, -1 when the version left the path).
export const STAGES = [
  () => t('Saved'),
  () => t('Approved by a second person'),
  () => t('Runtime key approved'),
  () => t('Ready to run'),
];

const INDEX = {waiting_for_approval: 1, waiting_for_credential: 2, ready: 4};

export const stageIndex = v => INDEX[v?.status] ?? -1;

export function statusSentence(v) {
  switch (v?.status) {
  case 'waiting_for_approval':
    return t('Waiting for a registry approver to approve the tools it asks for. Its author cannot approve it.');
  case 'waiting_for_credential':
    return v.waiting_on === 'studio_runtime'
      ? t('Approved. Waiting for the agent runtime to propose its key; this takes about a minute.')
      : t('Approved. Waiting for a registry approver to approve the key the agent runtime proposed.');
  case 'ready':
    return t('Ready: a run starts as soon as you ask.');
  case 'rejected':
    return t('Rejected: {reason}', {reason: v.decision_reason ?? '—'});
  case 'replaced':
    return t('Replaced by a newer version.');
  default:
    return t('This version is {status}.', {status: String(v?.status ?? '—')});
  }
}

// FAILURES word every failure reason agent-runtime names (ADR-033 Rev 1.2).
export const FAILURES = {
  credential_pending: () => t('The agent has no approved key yet: a registry approver must approve the one the runtime proposed.'),
  credential_expired: () => t('The agent’s key has expired: the runtime proposes a new one for a registry approver.'),
  credential_revoked: () => t('The agent’s key was revoked while it ran.'),
  version_replaced: () => t('A newer version replaced this one while it ran.'),
  action_denied: () => t('A step was denied by policy, its allowlist, a budget or a kill switch; nothing was done.'),
  action_failed: () => t('A step failed: the system it called refused or could not answer.'),
  action_cancelled: () => t('A step was cancelled.'),
  action_unknown: () => t('A step’s outcome is unknown: an operator must check the system before anyone retries.'),
  result_unavailable: () => t('A step succeeded, but its result could not be read back.'),
  answer_too_large: () => t('The answer was too long to keep.'),
  deadline_exceeded: () => t('The run took longer than its time limit.'),
  killed: () => t('An operator stopped this run with the kill switch; it sends no more steps.'),
};

export const failureSentence = reason => (Object.hasOwn(FAILURES, reason)
  ? FAILURES[reason]() : t('The run failed: {reason}.', {reason: String(reason ?? '—')}));

export function runSentence(state) {
  switch (state) {
  case 'QUEUED': return t('Waiting for the agent runtime to pick it up.');
  case 'RUNNING': return t('Running: each step goes through the governed action path.');
  case 'SUCCEEDED': return t('Finished.');
  case 'FAILED': return t('Failed.');
  default: return t('The run is {state}.', {state: String(state ?? '—')});
  }
}
