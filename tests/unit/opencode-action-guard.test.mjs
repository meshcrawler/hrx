import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,writeFile,rm} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {createGuard,pendingAction} from '../../internal/app/assets/opencode-action-guard.mjs';

test('guard scope, lifecycle, request preservation and bounded failure', async () => {
 const directory=await mkdtemp(path.join(os.tmpdir(),'hrx-guard-test-'));
 try {
 const file=path.join(directory,'state.json');
 const binding={project:directory,session:'ses_test',identity:'host/test/agent',model:'custom/route',states:[file],initial_action:true};
 const state={mapping:{project_directory:directory,thread_id:binding.session,agent_id:binding.identity},pending:null};
 const save=()=>writeFile(file,JSON.stringify(state)); await save();
 const guard=createGuard(binding,directory);
 const base={model:binding.model,messages:[{role:'user',content:'task'}],tools:[{type:'function',function:{name:'shell'}}],tool_choice:'auto'};
 const call=async (body=base,extra={})=>{
  const event={sessionID:binding.session,kind:'primary',request:new Request('http://localhost/v1/chat/completions',{method:'POST',headers:{authorization:'Bearer test', 'content-type':'application/json'},body:JSON.stringify(body)}),...extra};
  await guard(event); assert.equal(event.request.headers.get('authorization'),'Bearer test');
  return event.request.json();
 };
 for(const extra of [{kind:'title'},{kind:'compaction'},{sessionID:'ses_other'}]) assert.deepEqual(await call(base,extra),base);
 for(const changes of [{model:'other'},{tools:[]},{tool_choice:'none'},{tool_choice:{type:'function',function:{name:'shell'}}}]) assert.deepEqual(await call({...base,...changes}),{...base,...changes});
 const first=await call();assert.equal(first.tool_choice,'required');assert.deepEqual(first.messages,base.messages);assert.deepEqual(first.tools,base.tools);
 assert.equal((await call()).tool_choice,'auto','initial action enforced once');
 state.pending={delivery:'notified',ack_requested:false};await save();
 const continuation={...base,messages:[...base.messages,{role:'assistant',content:'working'},{role:'tool',content:'read result'}]};
 assert.equal((await call(continuation)).tool_choice,'required');
 state.pending.ack_requested=true;await save();assert.equal((await call(continuation)).tool_choice,'auto');
 state.pending.ack_requested=false;state.pending.delivery='received';await save();assert.equal((await call(continuation)).tool_choice,'auto','not delivered yet');
 state.pending.delivery='notified';state.mapping.thread_id='ses_other';await save();assert.equal(await pendingAction(binding,directory,binding.session),false);
 state.mapping.thread_id=binding.session;await save();
 let enforced=2;
 for(let i=0;i<20;i++) if((await call(continuation)).tool_choice==='required') enforced++;
 assert.equal(enforced,12,'bounded per-turn enforcement');
 const next={...base,messages:[...continuation.messages,{role:'user',content:'next notification'}]};assert.equal((await call(next)).tool_choice,'required');
 state.pending=null;await save();assert.equal((await call(next)).tool_choice,'auto');
 await writeFile(file,'broken');await assert.rejects(call(next),/cannot read bridge state/);
 } finally {await rm(directory,{recursive:true,force:true});}
});
