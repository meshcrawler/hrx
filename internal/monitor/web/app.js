'use strict';
const $ = id => document.getElementById(id);
let snapshot = null, paused = false, sortKey = 'last_activity', lastReceived = 0, fetchFailed = false, fetching = false;
const dateValue = value => { const n = Date.parse(value); return Number.isFinite(n) && n > 0 ? n : 0; };
const age = value => { const n = dateValue(value); if (!n) return 'Unknown'; const seconds = Math.max(0, Math.floor((Date.now()-n)/1000)); if (seconds < 5) return 'just now'; if (seconds < 60) return `${seconds}s ago`; if (seconds < 3600) return `${Math.floor(seconds/60)}m ${seconds%60}s ago`; if (seconds < 86400) return `${Math.floor(seconds/3600)}h ${Math.floor(seconds%3600/60)}m ago`; return `${Math.floor(seconds/86400)}d ago`; };
function el(tag, text, cls) { const node=document.createElement(tag); if(text!==undefined) node.textContent=text; if(cls) node.className=cls; return node; }
function timed(value) { const node=el('span',age(value),'age'); node.dataset.timestamp=value||''; node.title=dateValue(value)?new Date(value).toLocaleString():'No observation'; return node; }
function state(row) { if(row.status==='exited')return 'exited';return fetchFailed || row.source_status !== 'connected' ? 'offline' : row.status; }
const labels={running:'Running',exited:'Exited',untracked:'History only',unknown:'Unknown',stale:'Stale',offline:'Host unavailable'};
const inactive=new Set(['exited','untracked']);
const matchesState=(row,filter)=>filter==='all'||(filter?state(row)===filter:!inactive.has(state(row)));
const events={'launch':'launch','recovered-launch':'process started (recovered)','session-start':'session started','user-message':'user submitted','assistant-turn':'turn completed','tool-use':'tool completed','historical session':'saved session'};
function forumCell(row) {
 const cell=el('td',undefined,'forum-cell'), p=row.pitput;
 if(!p || typeof p.enabled!=='boolean') {cell.append(el('div','Unknown','forum-state'));return cell;}
 if(!p.enabled) {cell.append(el('div','None configured','forum-state'));return cell;}
 const boards=Array.isArray(p.boards)?p.boards.filter(b=>b&&typeof b.name==='string'):[];
 const chip=b=>{const node=el('span',b.name,'forum-chip');node.title=b.membership==='joined'?'Enrollment verified at launch; current membership and delivery unverified':'Configured in saved launch; membership and delivery unverified';return node;};
 cell.append(...boards.slice(0,2).map(chip));
 if(boards.length>2) {
  const details=el('details');details.dataset.kind='forums';
  details.append(el('summary',`+${boards.length-2} boards`),...boards.slice(2).map(chip));cell.append(details);
 }
 if(!boards.length)cell.append(el('div','No boards recorded','forum-state'));
 cell.append(el('div','Delivery unverified','sub'));
 cell.title=`Pitput identity: ${typeof p.identity==='string'?p.identity:'not recorded'}\nLaunch-time metadata only. No recent communication verified.`;
 return cell;
}
function metricCells(row) {
 const context=el('td',undefined,'metric-cell'), compactions=el('td',undefined,'metric-cell');
 const m=row.metrics?.session===row.session_id?row.metrics:null, c=m?.context, count=m?.compactions;
 const source=m?`${m.source}${m.version?' / '+m.version:''} / ${m.availability}`:'No native telemetry collected';
 const valid=c&&Number.isFinite(c.used_tokens)&&c.used_tokens>=0&&dateValue(c.observed_at);
 if(valid) {
  const limit=Number.isFinite(c.limit_tokens)&&c.limit_tokens>0?c.limit_tokens:null;
  const stale=Date.now()-dateValue(c.observed_at)>120000||row.source_status!=='connected'||fetchFailed;
  context.append(el('div',limit?`~${Math.round(c.used_tokens/limit*100)}%`:`~${c.used_tokens.toLocaleString()} tokens`,'metric-value'));
  context.append(el('div',`${stale?'Stale estimate':m.availability==='available'?'Estimate':m.availability==='catching-up'?'Catching up':'Partial history'}`,'sub'));
  context.title=`${c.used_tokens.toLocaleString()} / ${limit?limit.toLocaleString():'unknown limit'} tokens\nLatest native context estimate; not lifetime usage.\nObserved: ${new Date(c.observed_at).toLocaleString()}\n${source}`;
 } else {context.append(el('div','Unknown','metric-value'),el('div',m?.availability==='unsupported'?'Unsupported':'No current reading','sub'));context.title=source;}
 if(count&&Number.isSafeInteger(count.count)&&count.count>=0) {
  compactions.append(el('div',`${count.count.toLocaleString()} observed`,'metric-value'));
  compactions.title=`Completed compactions observed in available native session records. This is not a guaranteed lifetime total.\nDuplicate events and resumes do not add to the count.\nCollected: ${dateValue(m.collected_at)?new Date(m.collected_at).toLocaleString():'unknown'}\n${source}`;
  if(m.availability!=='available')compactions.append(el('div',m.availability==='catching-up'?'Catching up':'Partial history','sub'));
 } else {compactions.append(el('div','Unknown','metric-value'));compactions.title=source;}
 return [context,compactions];
}
const reporter=row=>row.source==='local'?'this collector':row.source;
function rowView(row) {
 const tr=el('tr'); tr.dataset.key=`${row.source}/${row.host_id}/${row.run_id}`;
 const identity=el('td'); identity.append(el('div',row.agent==='Identity not recorded'?'Unnamed session':row.agent||'Unnamed agent','agent'));
 const run=el('div', (row.session_id?'session ':'run ')+(row.session_id||row.run_id||'').slice(0,12),'run-label');run.title=`Session: ${row.session_id||'not recorded'}\nRun: ${row.run_id}`;identity.append(run);
 if(row.remote){identity.append(el('div',`Proxmox / ${(row.remote.stage||'reserved').replaceAll('-',' ')}${row.remote.failed?' — failed':''}`,'sub'));if(row.remote.failed)identity.append(el('div',`Inspect deployment ${row.remote.name}; use hrx remote logs before retrying.`,'sub'));}
 const presence=el('td');presence.append(el('span',row.remote&&state(row)==='unknown'?'Not live-tracked':labels[state(row)]||'Unknown',`badge ${state(row)}`));
 if(row.ownership){const lease=el('div',`Lease ${row.ownership.state} / #${row.ownership.generation}`,'sub');lease.title=row.ownership.expires_at?`Ownership expires: ${new Date(row.ownership.expires_at).toLocaleString()}\nOwnership and process liveness are separate.`:"Ownership has moved to a different run.";presence.append(lease);}
 const host=el('td');host.append(el('div',row.host||'Unknown host','host-label'),el('div',row.remote?`Proxmox${row.remote.vmid?' CT'+row.remote.vmid:''} / via ${reporter(row)}`:row.source,'sub'));host.title=row.remote?`Host ID: ${row.host_id}\nRemote guest; observed by the ${reporter(row)} controller, not by a collector on ${row.host}.`:`Host ID: ${row.host_id}`;
 const harness=el('td');const h=el('div',undefined,'harness');h.append(el('span',({claude:'Cl',codex:'Cx',opencode:'Oc'})[row.harness]||'--','harness-icon'),el('span',row.harness));harness.append(h);
 const model=el('td');model.append(el('div',row.model||'Unknown','model'));
 const activity=el('td');activity.append(timed(row.last_activity),el('div',row.remote?'controller stage update':events[row.activity]||'observed event','event'));
 const seen=el('td');seen.append(timed(row.last_seen),el('div',row.remote?'last controller observation; not live health':row.status==='untracked'?'not live-tracked':row.source_status!=='connected'?'last reported by host':'process observation','event'));
 const credential=el('td');const c=row.credential||{};
 if(c.provider==='gitlab') {
  const expired=dateValue(c.expires_at)>0&&dateValue(c.expires_at)<=Date.now();
  const details=el('details');details.append(el('summary',expired?'GitLab / expired':`GitLab / ${c.profile||'configured'}`,`credential-title ${expired?'warn':''}`));
  details.append(el('div',`${c.host||'Host unknown'} / ${(c.scopes||[]).join(', ')||'Scopes not recorded'} / expires ${c.expires_at||'unknown'}`,'credential-meta'));credential.append(details);
  credential.append(el('div',c.username||'Account unknown','credential-meta'));
  credential.title=`Host: ${c.host||'unknown'}\nScopes: ${(c.scopes||[]).join(', ')||'not recorded'}\nExpiry: ${c.expires_at||'not recorded'}\nRecorded at launch and updated when HRX refreshes the token; access not verified`;
 } else {credential.append(el('div',c.provider==='remote-lease'?'Scoped lease':c.provider==='unmanaged'?'No broker':'Unknown','credential-title'),el('div',c.provider==='remote-lease'?'Access not verified':c.provider==='unmanaged'?'Harness manages auth':'Not recorded','credential-meta'));}
 tr.append(identity,presence,host,harness,model,...metricCells(row),activity,seen,forumCell(row),credential);return tr;
}
function render() {
 if(!snapshot) return;
 const rows=snapshot.agents||[];
 $('live-count').textContent=rows.filter(r=>state(r)==='running').length;
 $('host-count').textContent=new Set(rows.map(r=>`${r.source}/${r.host_id}`)).size;
 $('agent-count').textContent=rows.length;
 const historical=rows.filter(r=>r.status==='untracked').length;
 $('history-note').hidden=!historical||!['all','untracked'].includes($('state-filter').value);
 $('history-note').textContent=`${historical} saved session${historical===1?'':'s'} without live tracking. Relaunch through HRX to report presence and activity; missing identities and credentials remain unknown.`;
 const selection=$('host-filter').value;const hosts=new Map(rows.map(r=>[`${r.source}/${r.host_id}`,r.remote?`${r.host} (Proxmox, via ${reporter(r)})`:`${r.host} (${r.source})`]));
 $('host-filter').replaceChildren(new Option('All hosts',''),...Array.from(hosts).sort((a,b)=>a[1].localeCompare(b[1])).map(([value,name])=>new Option(name,value)));
 if(hosts.has(selection)) $('host-filter').value=selection;
 $('sources').replaceChildren(...(snapshot.sources||[]).map(s=>{const chip=el('span',`${s.id} / ${s.status}`,'source'+(s.status==='connected'?'':' bad'));chip.title=s.error||'Read-only collector';return chip;}));
 const query=$('search').value.toLowerCase().trim(), host=$('host-filter').value, filter=$('state-filter').value;
 const filtered=rows.filter(r=>(!host||`${r.source}/${r.host_id}`===host)&&matchesState(r,filter)&&(!query||[r.agent,r.host,r.harness,r.model,r.credential?.username,r.credential?.host,...(Array.isArray(r.pitput?.boards)?r.pitput.boards.map(b=>b?.name):[])].join(' ').toLowerCase().includes(query)));
 filtered.sort((a,b)=>dateValue(b[sortKey])-dateValue(a[sortKey])||`${a.source}/${a.host_id}/${a.run_id}`.localeCompare(`${b.source}/${b.host_id}/${b.run_id}`));
 const expanded=new Set(Array.from($('rows').querySelectorAll('details[open]')).map(d=>`${d.closest('tr').dataset.key}/${d.dataset.kind||'credential'}`));
 $('rows').replaceChildren(...filtered.map(row=>{const tr=rowView(row);for(const d of tr.querySelectorAll('details'))d.open=expanded.has(`${tr.dataset.key}/${d.dataset.kind||'credential'}`);return tr;}));
 $('visible-count').textContent=`${filtered.length} in view`;
 const hiddenRows=filter?[]:rows.filter(r=>(!host||`${r.source}/${r.host_id}`===host)&&inactive.has(state(r)));
 const exited=hiddenRows.filter(r=>state(r)==='exited').length, history=hiddenRows.length-exited;
 $('hidden-note').hidden=!hiddenRows.length;
 $('hidden-text').textContent=`Showing active sessions. ${[exited&&`${exited} exited`,history&&`${history} history-only`].filter(Boolean).join(' and ')} hidden.`;
 $('empty').hidden=filtered.length>0;
 $('empty-message').textContent=rows.length?(filter?'Try a different search or filter.':'No active sessions right now. Choose All sessions to see finished runs.'):'Launch a harness through HRX to start collecting local observations.';
 status();
}
function status() {
 const old = lastReceived && Date.now()-lastReceived>10000;
 const bad=(snapshot?.sources||[]).some(s=>s.status!=='connected');
 $('connection-text').textContent=!lastReceived&&!fetchFailed?'Connecting':paused?'Updates paused':fetchFailed||old?'Connection interrupted':bad?'Partial connection':'Live observations';
 $('connection-dot').parentElement.className='connection '+(!paused&&!fetchFailed&&!old?'live':'error');
 $('notice').hidden=!(paused||fetchFailed||old||bad);
 $('notice').textContent=paused?'Updates paused. Displayed records are frozen; ages continue to advance.':fetchFailed||old?'Collector connection interrupted. Showing the last received snapshot; no new liveness is implied.':bad?'Some collectors are unavailable. Their rows retain the last reported timestamps and are marked unavailable.':'';
 $('footer-status').textContent=lastReceived?`Snapshot received ${age(new Date(lastReceived).toISOString())} \u00b7 sorted by ${sortKey==='last_activity'?'activity':'last seen'}`:'Waiting for first snapshot.';
}
async function refresh() {
 if(paused||fetching) return;
 fetching=true;
 try { const response=await fetch('api/v1/agents',{cache:'no-store',signal:AbortSignal.timeout(5000)});if(!response.ok)throw Error('fetch');const next=await response.json();if(next.version!==1||!Array.isArray(next.agents))throw Error('schema');if(paused)return;snapshot=next;lastReceived=Date.now();fetchFailed=false;render();}
 catch {fetchFailed=true;if(snapshot)render();else status();}
 finally {fetching=false;}
}
function restoreStatusFilter() {
 const value=(new URL(location.href).searchParams.get('status')||'').toLowerCase();
 $('state-filter').value=Object.hasOwn(labels,value)||value==='all'?value:'';
 render();
}
$('search').addEventListener('input',render);$('host-filter').addEventListener('change',render);
$('state-filter').addEventListener('change',()=>{
 const url=new URL(location.href), value=$('state-filter').value;
 if(value) url.searchParams.set('status',value);else url.searchParams.delete('status');
 history.replaceState(history.state,'',url);
 render();
});
$('show-all').addEventListener('click',()=>{$('state-filter').value='all';$('state-filter').dispatchEvent(new Event('change'));});
window.addEventListener('popstate',restoreStatusFilter);
restoreStatusFilter();
$('pause').addEventListener('click',()=>{paused=!paused;$('pause').textContent=paused?'Resume updates':'Pause updates';$('pause').setAttribute('aria-pressed',String(paused));status();if(!paused)refresh();});
$('sort').addEventListener('click',()=>{sortKey=sortKey==='last_activity'?'last_seen':'last_activity';$('sort').textContent=sortKey==='last_activity'?'Last event \u2193':'Sort: seen \u2193';render();});
setInterval(()=>{document.querySelectorAll('[data-timestamp]').forEach(node=>{node.textContent=age(node.dataset.timestamp);});status();},1000);
setInterval(refresh,3000);refresh();
