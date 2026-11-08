import datetime
import pathlib
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

from agent_config import validate,launch_argv,private_file,native_session_alive,resume_arguments,launcher,replace_lease


def config(harness='opencode'):
    return {'deployment':'a'*32,'harness':harness,'model':'claude-sonnet-5-5' if harness=='claude' else 'test-model',
            'identity':'test/worker/agent','boards':['experiment'], 'gateway_url':'https://gateway.example/v1',
            'pitput':{'url':'https://forum.example','token':'private-agent-token'},
            'lease':{'key':'private-gateway-key','expires_at':(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=5)).isoformat()},
            'initial_prompt':'Read the readiness challenge and wait for authorized work.'}


class AgentConfig(unittest.TestCase):
    def test_native_arguments_keep_profile_and_identity(self):
        data=config();validate(data);args=launch_argv(data,'/home/agent/workspace')
        self.assertEqual(args[:3],['/usr/local/bin/hrx','--profile','remote'])
        self.assertIn(data['identity'],args);self.assertIn('--pitput-targeted',args)
        self.assertEqual(args[-2:],['--prompt',data['initial_prompt']])
        self.assertNotIn(data['lease']['key'],' '.join(args))

    def test_opencode_readiness_waits_for_actual_notification(self):
        data=config();data['readiness_only']=True
        args=launch_argv(data,'/home/agent/workspace')
        self.assertNotIn('--prompt',args)
        self.assertNotIn(data['initial_prompt'],args)
        self.assertIn('--pitput',args)
        self.assertIn('--pitput-targeted',args)

    def test_claude_requires_oauth_and_uses_managed_onboarding(self):
        data=config('claude')
        with self.assertRaisesRegex(ValueError,'OAuth'):validate(data)
        data['lease']['claude_code_oauth_token']='private-subscription';validate(data)
        args=launch_argv(data,'/home/agent/workspace')
        self.assertIn('--claude-config-dir',args);self.assertIn('theme,trust,bypass',args)
        self.assertNotIn('private-subscription',' '.join(args))

    def test_expiry_and_missing_boards_refused(self):
        data=config();data['lease']['expires_at']='2000-01-01T00:00:00Z'
        with self.assertRaisesRegex(ValueError,'expired'):validate(data)
        data=config();data['boards']=[]
        with self.assertRaisesRegex(ValueError,'boards'):validate(data)

    def test_guest_host_mapping_rejects_injected_lines(self):
        data=config();data['hosts']={'gateway.example':'192.0.2.10'}
        validate(data)
        for mapping in [{'gateway.example\nother':'192.0.2.10'},
                        {'gateway.example':'192.0.2.10\n127.0.0.1 other'}]:
            data['hosts']=mapping
            with self.assertRaises(ValueError):validate(data)

    def test_retained_dead_pane_is_not_running(self):
        for status, output, expected in [(0,'0\n',True),(0,'1\n',False),(1,'',False)]:
            with patch('agent_config.subprocess.run',return_value=SimpleNamespace(returncode=status,stdout=output)):
                self.assertEqual(native_session_alive(['runuser','-u','agent','--'],'hrx-test'),expected)
        with patch('agent_config.subprocess.run',return_value=SimpleNamespace(returncode=0,stdout='0\n0\n')):
            with self.assertRaisesRegex(ValueError,'pane layout'):
                native_session_alive([], 'hrx-test')

    def test_resume_requires_exact_consistent_board_bindings(self):
        saved=dict(config(),boards=['one','two'])
        mappings=[{'agent_id':saved['identity'],'board':'/b/'+b,
                   'project_directory':'/home/agent/workspace','thread_id':'ses_123abc'}
                  for b in saved['boards']]
        args=resume_arguments(saved,mappings)
        self.assertEqual(args[-2:],['--resume','ses_123abc'])
        self.assertNotIn(saved['initial_prompt'],args)
        self.assertNotIn('--prompt',args)
        compile(launcher(args),'launcher','exec')
        for field,value in [('thread_id','ses_other'),('agent_id','other/worker/agent'),
                            ('project_directory','/tmp/other'),('board','/b/wrong')]:
            bad=[dict(m) for m in mappings];bad[1][field]=value
            with self.assertRaises(ValueError):resume_arguments(saved,bad)
        with self.assertRaises(ValueError):resume_arguments(saved,[])
        saved['harness']='claude'
        with self.assertRaises(ValueError):resume_arguments(saved,mappings)

    def test_lease_install_requires_stopped_native_session(self):
        import json
        data=config();ident=data['deployment']
        saved={'deployment':ident,'session':'hrx-'+ident,'harness':'opencode'}
        with patch('agent_config.pathlib.Path.read_text',return_value=json.dumps(saved)), \
             patch('agent_config.native_session_alive',return_value=True), \
             patch('agent_config.private_file') as write:
            with self.assertRaisesRegex(ValueError,'stop native'):replace_lease(data)
            write.assert_not_called()
        with patch('agent_config.pathlib.Path.read_text',return_value=json.dumps(saved)), \
             patch('agent_config.native_session_alive',return_value=False), \
             patch('agent_config.pwd.getpwnam',return_value=object()), \
             patch('agent_config.private_file') as write:
            result=replace_lease(data)
            self.assertTrue(result['credential_installed']);self.assertFalse(result['ready'])
            self.assertNotIn('private-',json.dumps(result))
            write.assert_called_once()
            write.reset_mock();data['lease']['expires_at']='2000-01-01T00:00:00Z'
            with self.assertRaisesRegex(ValueError,'expired'):replace_lease(data)
            write.assert_not_called()

    def test_claude_resume_rearms_monitor_without_repeating_task(self):
        saved=config('claude');saved['boards']=['one']
        native='12345678-1234-1234-1234-123456789abc'
        mapping={'backend':'claude-task','agent_id':saved['identity'],'board':'/b/one',
                 'claude_home':'/home/agent/.config/hrx/claude-native','thread_id':native}
        args=resume_arguments(saved,[mapping])
        self.assertEqual(args[args.index('--resume')+1],native)
        self.assertNotIn(saved['initial_prompt'],args)
        self.assertIn('Do not repeat completed assignments',args[-1])
        mapping['claude_home']='/other'
        with self.assertRaises(ValueError):resume_arguments(saved,[mapping])

    def test_private_write_refuses_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp);(root/'alias').symlink_to(root,target_is_directory=True)
            with self.assertRaisesRegex(ValueError,'symlink'):private_file(root/'alias'/'secret','value',None)


if __name__=='__main__':unittest.main()

class CodexResume(unittest.TestCase):
    def test_exact_native_id_and_home_are_required(self):
        native='11111111-2222-3333-4444-555555555555'
        saved=config('codex')
        mapping={'agent_id':saved['identity'],'board':'/b/experiment','thread_id':native,'codex_home':'/home/agent/.codex'}
        args=resume_arguments(saved,[mapping])
        self.assertEqual(args[-2:],['--resume',native]);self.assertNotIn(saved['initial_prompt'],args)
        mapping['codex_home']='/other'
        with self.assertRaisesRegex(ValueError,'binding mismatch'):resume_arguments(saved,[mapping])
