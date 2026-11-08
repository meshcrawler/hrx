import hashlib
import io
import json
import os
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from release_work import release, stage_claude


class ReleaseWork(unittest.TestCase):
    def setUp(self):
        try:
            import tomllib
        except ImportError:
            self.skipTest('guest release helper requires Python 3.11+')
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=pathlib.Path(self.tmp.name).resolve();self.base=self.root/'deployments';self.home=self.root/'home'
        self.ident='a'*32;self.identity='host/worker/agent';self.session='ses_test'
        self.request={'deployment':self.ident,'identity':self.identity,'session':self.session,'board':'test','event_key':'ready-event','prompt':'assigned task'}
        (self.home/'workspace').mkdir(parents=True);(self.base/self.ident).mkdir(parents=True)
        (self.base/self.ident/'agent.json').write_text(json.dumps({'deployment':self.ident,'identity':self.identity,'harness':'opencode'}))
        key=hashlib.sha256(self.identity.encode()).hexdigest()
        self.state_path=self.home/'.local/state/hrx/pitput'/key/'boards/test/state'/key/'state.json';self.state_path.parent.mkdir(parents=True)
        password=self.root/'password';password.write_text('private-local-fixture')
        config=self.root/'bridge.toml';config.write_text('opencode_url="http://localhost"\nopencode_password_file='+json.dumps(str(password))+'\n')
        self.state={'mapping':{'agent_id':self.identity,'thread_id':self.session,'project_directory':str(self.home/'workspace'),'config_path':str(config)},'pending':None,'handled':[{'event_key':'ready-event'}]}
        self.save();self.messages=[];self.posts=0;self.lose_reply=False
        self.user=SimpleNamespace(pw_uid=os.getuid(),pw_gid=os.getgid())

    def save(self):self.state_path.write_text(json.dumps(self.state))

    def api(self, request, timeout):
        if request.data:
            body=json.loads(request.data);self.posts+=1
            self.messages.append({'id':body['id'],'text':body['text']})
            if self.lose_reply:raise OSError('lost response after acceptance')
            data={'id':body['id'],'sessionID':self.session,'delivery':'queue'}
        elif '/message?' in request.full_url:data=self.messages
        elif request.full_url.endswith('/inbox'):data=[]
        else:data={'id':self.session,'location':{'directory':str(self.home/'workspace')},'time':{}}
        return io.BytesIO(json.dumps({'data':data}).encode())

    def run_release(self):return release(self.request,self.base,self.home,self.user)

    def test_once_and_lost_reply_recovery(self):
        with patch('release_work.urllib.request.urlopen',side_effect=self.api):
            self.lose_reply=True
            with self.assertRaises(OSError):self.run_release()
            self.lose_reply=False
            first=self.run_release();second=self.run_release()
        self.assertEqual(first,second);self.assertEqual(self.posts,1)
        self.assertTrue(json.loads((self.base/self.ident/'work.json').read_text())['released'])
        self.assertNotIn('private-local-fixture',(self.base/self.ident/'work.json').read_text())

    def test_no_release_without_retained_readiness_or_with_pending_work(self):
        for changes in [{'handled':[]},{'pending':{'event':'new-work'}}]:
            self.state.update(changes);self.save()
            with patch('release_work.urllib.request.urlopen',side_effect=AssertionError('premature API call')):
                with self.assertRaises(ValueError):self.run_release()
        self.assertFalse((self.base/self.ident/'work.json').exists())

    def test_changed_assignment_is_not_released_again(self):
        with patch('release_work.urllib.request.urlopen',side_effect=self.api):
            self.run_release();self.request['prompt']='different task'
            with self.assertRaisesRegex(ValueError,'changed'):self.run_release()
        self.assertEqual(self.posts,1)


class ClaudeStage(unittest.TestCase):
    setUp=ReleaseWork.setUp
    save=ReleaseWork.save
    def stage_fixture(self):
        (self.base/self.ident/'agent.json').write_text(json.dumps({'deployment':self.ident,'identity':self.identity,'harness':'claude'}))
        self.state['mapping'].update(backend='claude-task',board='/b/test',claude_home=str(self.home/'.claude'),claude_task_generation='generation')
        self.save()
        (self.state_path.parent.parent/'claude-active-generation').write_text(json.dumps({'session':self.session,'generation':'generation'}))
        index=self.home/'.local/state/hrx/sessions/index.json';index.parent.mkdir(parents=True)
        index.write_text(json.dumps({'sessions':[{'native_id':self.session,'harness':'claude','agent_identity':self.identity,'project':str(self.home/'workspace'),'claude_config_dir':str(self.home/'.claude')}]}))
        (self.home/'.config/hrx').mkdir(parents=True)

    def test_private_staging_retries_and_rejects_changed_task(self):
        self.stage_fixture()
        first=stage_claude(self.request,self.base,self.home,self.user)
        self.state['pending']={'event':'assigned-work'};self.save()
        self.assertEqual(first,stage_claude(self.request,self.base,self.home,self.user))
        task=self.home/'.config/hrx/assigned-work.txt'
        self.assertEqual(task.read_text(),'assigned task');self.assertEqual(task.stat().st_mode&0o777,0o600)
        self.assertNotIn('assigned task',json.dumps(first))
        self.request['prompt']='changed'
        with self.assertRaisesRegex(ValueError,'changed'):stage_claude(self.request,self.base,self.home,self.user)

    def test_generation_and_readiness_are_required(self):
        self.stage_fixture();self.state['handled']=[];self.save()
        with self.assertRaisesRegex(ValueError,'readiness'):stage_claude(self.request,self.base,self.home,self.user)
        self.state['handled']=[{'event_key':'ready-event'}];self.save()
        (self.state_path.parent.parent/'claude-active-generation').write_text(json.dumps({'session':'other','generation':'generation'}))
        with self.assertRaisesRegex(ValueError,'generation'):stage_claude(self.request,self.base,self.home,self.user)


if __name__=='__main__':unittest.main()

class CodexStage(unittest.TestCase):
    setUp=ReleaseWork.setUp
    save=ReleaseWork.save

    def test_verified_binding_private_task_and_stable_retry(self):
        from release_work import stage_notice
        (self.base/self.ident/'agent.json').write_text(json.dumps({'deployment':self.ident,'identity':self.identity,'harness':'codex'}))
        key=hashlib.sha256(self.identity.encode()).hexdigest()
        directory=self.home/'.local/state/hrx/pitput'/key/self.session/'test'
        directory.mkdir(parents=True)
        self.state['mapping'].update(codex_binary=str(directory/'codex-queue'),board='/b/test');self.save()
        (directory/'native-observer.json').write_text(json.dumps({'endpoint':'unix:///fixture','session':self.session,'project':str(self.home/'workspace')}))
        index=self.home/'.local/state/hrx/sessions/index.json';index.parent.mkdir(parents=True)
        index.write_text(json.dumps({'sessions':[{'native_id':self.session,'harness':'codex','agent_identity':self.identity,'project':str(self.home/'workspace')}]}))
        (self.home/'.config/hrx').mkdir(parents=True)
        with patch('release_work.subprocess.run',return_value=SimpleNamespace(returncode=0,stdout='{"native_binding":false}')):
            with self.assertRaisesRegex(ValueError,'binding unavailable'):
                stage_notice(self.request,self.base,self.home,self.user)
        self.assertFalse((self.base/self.ident/'work.json').exists())
        with patch('release_work.subprocess.run',return_value=SimpleNamespace(returncode=0,stdout='{"native_binding":true}')):
            first=stage_notice(self.request,self.base,self.home,self.user)
            self.assertEqual(first,stage_notice(self.request,self.base,self.home,self.user))
            task=self.home/'.config/hrx/assigned-work.txt'
            self.assertEqual(task.read_text(),'assigned task');self.assertEqual(task.stat().st_mode&0o777,0o600)
            self.request['prompt']='changed'
            with self.assertRaisesRegex(ValueError,'changed'):
                stage_notice(self.request,self.base,self.home,self.user)
