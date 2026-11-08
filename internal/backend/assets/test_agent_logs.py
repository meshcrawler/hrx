import unittest
from agent_logs import sanitize, LIMIT


class AgentLogs(unittest.TestCase):
    def test_credentials_and_terminal_sequences_are_removed(self):
        source='\x1b[31mprivate-key\x1b[0m\nAuthorization: Bearer unknown-token\nsk-123456789012345\nuseful failure'
        result=sanitize(source,['private-key'])
        for value in ['private-key','unknown-token','sk-123456789012345','\x1b']:
            self.assertNotIn(value,result)
        self.assertIn('useful failure',result)

    def test_redacts_before_truncation(self):
        secret='unique-secret-across-tail-boundary'
        result=sanitize('prefix'+secret+'x'*(LIMIT-5),[secret])
        self.assertLessEqual(len(result),LIMIT)
        self.assertNotIn('boundary',result)
        self.assertNotIn('prefix',result)
