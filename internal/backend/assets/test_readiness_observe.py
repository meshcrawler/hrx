import copy
import pathlib
import unittest

from readiness_observe import native_binding_matches, claude_transcript_evidence


class NativeBinding(unittest.TestCase):
    def test_canonical_board_and_exact_root_project_session(self):
        directory=str(pathlib.Path('.').resolve())
        session={'id':'native-123','location':{'directory':directory},'time':{}}
        mapping={'thread_id':'native-123','project_directory':directory,'board':'/b/experiment'}
        config={'opencode_project_directory':directory}
        self.assertTrue(native_binding_matches(session,mapping,config,'experiment'))
        self.assertFalse(native_binding_matches(session,mapping,config,'other'))
        for changes in [{'id':'other'},{'parentID':'parent'},{'time':{'archived':1}},
                        {'location':{'directory':'/another-project'}},{'location':{}}]:
            changed=copy.deepcopy(session);changed.update(changes)
            self.assertFalse(native_binding_matches(changed,mapping,config,'experiment'))
        self.assertFalse(native_binding_matches(session,mapping,{'opencode_project_directory':'/other'},'experiment'))


class ClaudeNativeEvidence(unittest.TestCase):
    def test_only_successful_tool_results_count(self):
        def record(kind,parts):
            return {'type':kind,'sessionId':'native','cwd':'/workspace','isSidechain':False,'message':{'content':parts}}
        records=[record('assistant',[{'type':'tool_use','id':'r','name':'Bash','input':{'command':'pitput attention read exact-handle'}},
                                     {'type':'tool_use','id':'p','name':'Bash','input':{'command':'pitput post /b/test'}}]),
                 record('user',[{'type':'tool_result','tool_use_id':'r','is_error':False},
                                {'type':'tool_result','tool_use_id':'p','is_error':True}])]
        bound,actions=claude_transcript_evidence(records,'native','/workspace')
        self.assertTrue(bound);self.assertEqual(actions,['read'])
        for field,value in [('sessionId','other'),('cwd','/other'),('isSidechain',True)]:
            altered=copy.deepcopy(records);altered[0][field]=value
            with self.assertRaises(ValueError):claude_transcript_evidence(altered,'native','/workspace')
        self.assertEqual(claude_transcript_evidence([],'native','/workspace'),(False,[]))


if __name__=='__main__':unittest.main()

class CodexNativeEvidence(unittest.TestCase):
    def test_current_codex_mapping_without_config_path(self):
        import tempfile, json, hashlib
        from unittest.mock import patch
        from types import SimpleNamespace
        from readiness_observe import observe_codex
        with tempfile.TemporaryDirectory() as tmp:
            home=pathlib.Path(tmp);identity='host/worker/agent';session='native';board='work'
            directory=home/'.local/state/hrx/pitput'/hashlib.sha256(identity.encode()).hexdigest()/session/board
            directory.mkdir(parents=True)
            index=home/'.local/state/hrx/sessions/index.json';index.parent.mkdir(parents=True)
            index.write_text(json.dumps({'sessions':[{'native_id':session,'harness':'codex','agent_identity':identity,'project':str(home/'workspace')}]}))
            (directory/'native-observer.json').write_text(json.dumps({'endpoint':'unix:///fixture','session':session,'project':str(home/'workspace')}))
            mapping={'agent_id':identity,'thread_id':session,'board':'/b/work','codex_binary':str(directory/'codex-queue')}
            evidence=SimpleNamespace(returncode=0,stdout=json.dumps({'native_binding':True,'native_actions':['read']}))
            result={}
            with patch('readiness_observe.subprocess.run',return_value=evidence) as run:
                observe_codex(mapping,{'handled':[]},identity,board,result,home)
                self.assertTrue(result['native_binding']);self.assertEqual(run.call_count,1)
                mapping['codex_binary']='/other/codex-queue'
                observe_codex(mapping,{},identity,board,{},home)
                self.assertEqual(run.call_count,1)
