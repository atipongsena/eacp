// run.js shows one Studio run: its state, each step's action and the answer
// (only its requester reads it) or why it failed. It asks to be polled while
// the run waits or runs. Without a run id it lists the runs started in this
// tab: the API has no run list, and this page stores nothing.
import {h, section, notice, table, link, badge, kv, relTime, emptyState} from '../dom.js';
import {format, isUUID} from '../router.js';
import {t} from '../i18n.js';
import {failureSentence, runSentence} from './status.js';

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? one(ctx, ctx.route.parts[0]) : list(ctx);
}

function list(ctx) {
  return section(t('Runs you started in this tab'), table([
    [t('Run'), r => link(r.id.slice(0, 8), format('runs', [r.id]))],
    [t('Started'), r => relTime(r.at, Date.now(), {node: true})],
  ], ctx.started, emptyState(t('No runs yet in this tab.'),
    t('Open a ready agent and run it; this list is forgotten when the page reloads.'), 'execution')));
}

async function one(ctx, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not a run id')});
  const res = await ctx.client.call('studio.run', {params: {id}});
  if (!res.ok) return notice(res);
  const r = res.data;
  if (r.state === 'QUEUED' || r.state === 'RUNNING') ctx.pollEvery(2000);
  const own = r.requested_by === ctx.session.me().principalId;
  return h('div', {},
    h('p', {}, link(t('← Agent'), format('agents', [r.agent_id]))),
    h('h1', {}, t('Run {id}', {id: r.id.slice(0, 8)})),
    h('div', {class: 'chips'}, badge(r.state)),
    h('p', {class: 'status-line', role: 'status'}, runSentence(r.state)),
    r.state === 'FAILED' ? h('div', {class: 'notice error', role: 'alert'}, failureSentence(r.failure_reason),
      ' ', h('code', {}, r.failure_reason)) : null,
    r.answer !== undefined ? section(t('Answer'), h('p', {class: 'answer'}, r.answer),
      h('p', {class: 'hint'}, t('Only you can read this answer, until {time}.', {time: clockTime(r.answer_expires_at)})))
      : r.state === 'SUCCEEDED' && !own ? h('p', {class: 'hint'}, t('Only the person who ran it reads the answer.')) : null,
    section(t('Steps'), table([
      [t('Step'), s => `${s.index + 1}. ${s.step_id}`],
      [t('Action state'), s => badge(s.action_state)],
      [t('Action'), s => h('code', {}, s.action_id)],
    ], r.steps, t('No step has run yet.'))),
    section(t('Record'), kv([
      [t('Run id'), h('code', {}, r.id)],
      [t('Version id'), h('code', {}, r.version_id)],
      [t('Started'), relTime(r.created_at, Date.now(), {node: true})],
      [t('Deadline'), relTime(r.deadline, Date.now(), {node: true})],
      r.finished_at ? [t('Finished'), relTime(r.finished_at, Date.now(), {node: true})] : null,
    ])));
}

// clockTime shows an expiry as a clock time in the browser's zone.
const clockTime = value => {
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleTimeString();
};
