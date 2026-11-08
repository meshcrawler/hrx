// Bundled HRX plugin. It never executes tools, reads forum content, or ACKs work.
import {readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';

export async function pendingAction(binding, directory, sessionID) {
  if (binding.project !== directory || binding.session !== sessionID) return false;
  for (const filename of binding.states) {
    let state;
    try { state = JSON.parse(await readFile(filename, 'utf8')); }
    catch (error) { if (error.code === 'ENOENT') continue; throw new Error('HRX action guard cannot read bridge state'); }
    const m = state.mapping;
    if (m?.agent_id !== binding.identity || m?.thread_id !== sessionID || m?.project_directory !== directory) continue;
    if (state.pending && !state.pending.ack_requested && ['enqueueing', 'notified', 'delivery_uncertain'].includes(state.pending.delivery)) return true;
  }
  return false;
}

export function createGuard(binding, directory) {
  let initial = binding.initial_action;
  let turn = '', required = 0;
  return async event => {
    if (event.kind !== 'primary' || event.sessionID !== binding.session || directory !== binding.project || event.request.method !== 'POST') return;
    if (!new URL(event.request.url).pathname.endsWith('/chat/completions')) return;
    const body = await event.request.clone().json();
    if (body.model !== binding.model || !body.tools?.length || body.tool_choice === 'none' || typeof body.tool_choice === 'object') return;
    const user = body.messages?.findLast(m => m.role === 'user');
    if (!user) return;
    const key = createHash('sha256').update(JSON.stringify(body.messages.slice(0, body.messages.lastIndexOf(user) + 1))).digest('hex');
    if (key !== turn) { turn = key; required = 0; }
    const first = initial && body.messages.at(-1)?.role === 'user';
    const pending = await pendingAction(binding, directory, event.sessionID);
    if (!first && !pending) return;
    // Bound enforcement even if a tool fails or the worker cannot handle an event.
    // Pending work is retained; relaxing tool_choice does not claim completion.
    if (required >= 12) return;
    required++;
    initial = false;
    const headers = new Headers(event.request.headers);
    headers.delete('content-length');
    event.request = new Request(event.request, {headers, body: JSON.stringify({...body, tool_choice: 'required'})});
  };
}

export default {
  id: 'hrx-action-guard-v1',
  async setup(ctx) {
    const raw = process.env.HRX_OPENCODE_ACTION_GUARD;
    if (!raw) return; // Installed on every HRX OpenCode launch; ordinary chat stays unchanged.
    let binding;
    try { binding = JSON.parse(raw); }
    catch { throw new Error('Invalid HRX action guard binding'); }
    if (binding.version !== 1 || !Array.isArray(binding.states) || !binding.session || !binding.identity || !binding.project || !binding.model) throw new Error('Invalid HRX action guard binding');
    await ctx.session.hook('http.request', createGuard(binding, ctx.location.directory));
  },
};
