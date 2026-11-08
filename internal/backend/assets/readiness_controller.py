"""Live test controller: issue one challenge; observe actual worker evidence."""
import json
import subprocess
import sys
from ysg_mail.agent_enrollment import load_agent_settings
from ysg_mail.board_client import RemoteBoard

request=json.load(sys.stdin)
identity=request['identity'];deployment=request['deployment'];board=request['board']
topic='/b/'+board+'/hrx-readiness-'+deployment
controller=request['controller']
if request['action']=='challenge':
    body='@'+identity+' HRX readiness challenge '+deployment+'. Read this notification using its exact attention handle and all continuation fragments. Then use your native shell tool to post exactly READY '+deployment+' to '+topic+' with author '+identity+' and request ID hrx-ready-reply-'+deployment+'. After verifying the real post receipt, run the exact Handle ACK command supplied with this notification. Do not reply to other messages or begin any other work.'
    args=['pitput','--agent',controller,'create',topic,'--title','HRX fresh-container readiness','--author',controller,'--body',body,'--request-id','hrx-ready-challenge-'+deployment]
    result=subprocess.run(args,check=True,capture_output=True,text=True)
    print(result.stdout)
elif request['action']=='work':
    body=('@'+identity+' HRX assigned-work release for deployment '+deployment+'. '
          'Only the agent whose /opt/hrx/deployments/'+deployment+'/agent.json matches this deployment may execute it. '
          'Read /home/agent/.config/hrx/assigned-work.txt using your native tool and perform that user-authorized task once. '
          'This notice is the release signal after readiness; its work ID is '+request['work_id']+'. '
          'After processing this release, run the exact Handle ACK command: complete immediately executable work, or record a task awaiting a later notification and ACK acceptance so the bridge can deliver that notification. Acceptance does not mean the deferred task is complete. '
          'Do not execute it again on a duplicate notice; retain the work ID in session context.')
    result=subprocess.run(['pitput','--agent',controller,'post',topic,'--author',controller,'--body',body,
                           '--request-id','hrx-assigned-work-'+deployment],check=True,capture_output=True,text=True)
    print(json.dumps({'published':True,'work_id':request['work_id']}))
else:
    c=RemoteBoard(load_agent_settings(controller).remote)
    try:posts=c.request('GET','/v1/messages',params={'path':topic,'after':0,'limit':64})
    finally:c.close()
    replies=[p for p in posts if p['author']==identity and p['body'].strip()=='READY '+deployment]
    c=RemoteBoard(load_agent_settings(identity).remote)
    try:evidence=c.request('GET','/v1/monitors/'+request['monitor']+'/evidence',params={'source':'board','event_key':request['event_key'],'limit':25})
    finally:c.close()
    dispositions=sorted({e['fact']['disposition'] for e in evidence['evidence']})
    print(json.dumps({'reply_count':len(replies),'reply_path':replies[0]['path'] if replies else None,'dispositions':dispositions}))
