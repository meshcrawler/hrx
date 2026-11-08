import copy
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

from proxmox import Host, Refused, validate
from test_proxmox import request


class RelayLifecycle(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.state = 'inactive'
        self.address = '192.0.2.21'
        self.calls = []
        self.dhcp_pending = 0
        self.network_state = "ActiveState=activating\nSubState=start\n"
        self.client_state = "absent"
        self.restarts = 0
        self.host = Host(self.tmp.name, self.run_command)
        self.record = {'deployment': 'a'*32, 'vmid': 138}
        self.spec = {'relay': {'listen_address': '192.0.2.10',
                             'upstream_address': '192.0.2.20', 'upstream_port': 443}}

    def run_command(self, args):
        self.calls.append(args)
        if args[:2] == ['systemctl','show']:
            return self.state
        if args[0] == 'pct':
            if 'systemctl' in args:
                if 'restart' in args:
                    self.restarts += 1
                    self.dhcp_pending = 0
                    return ''
                return self.network_state
            if 'python3' in args:
                return self.client_state
            if self.dhcp_pending:
                self.dhcp_pending -= 1
                return '[]'
            return json.dumps([{'addr_info': [{'scope':'global','local':self.address}]}])
        if args[:2] == ['systemctl','stop']:
            self.state = 'inactive'
        elif args[0] == 'systemd-run':
            self.state = 'active'
        else:
            raise AssertionError(args)
        return ''

    def test_retries_reuse_service_and_address_change_replaces_it(self):
        self.host.relay(self.record,self.spec,True)
        self.host.relay(self.record,self.spec,True)
        self.assertEqual(sum(c[0]=='systemd-run' for c in self.calls),1)
        self.assertEqual(self.record['relay_port'],20138)
        self.address='192.0.2.22'
        self.host.relay(self.record,self.spec,True)
        self.assertEqual(sum(c[0]=='systemd-run' for c in self.calls),2)
        source=next(pathlib.Path(self.tmp.name).glob('relay-*.py')).read_text()
        self.assertIn(self.address,source)
        self.assertNotIn('192.0.2.21',source)
        compile(source,'relay','exec')
        self.host.relay(self.record,self.spec,False)
        self.host.relay(self.record,self.spec,False)
        self.assertEqual(self.state,'inactive')
        self.assertNotIn('relay_port',self.record)
        self.assertEqual(list(pathlib.Path(self.tmp.name).glob('relay-*.py')),[])

    def test_waits_for_dhcp_and_bounds_observation(self):
        self.dhcp_pending=2
        with patch('proxmox.time.sleep') as sleep:
            self.host.relay(self.record,self.spec,True)
            self.assertEqual(sleep.call_count,2)
        self.host.relay(self.record,self.spec,False)
        self.dhcp_pending=31
        with patch('proxmox.time.sleep') as sleep:
            with self.assertRaisesRegex(Refused,'DHCP observation deadline'):
                self.host.relay(self.record,self.spec,True)
            self.assertEqual(sleep.call_count,30)
        self.assertEqual(self.state,'inactive')

    def test_recovers_only_terminal_networking_without_live_client(self):
        self.network_state='ActiveState=active\nSubState=exited\n'
        self.dhcp_pending=31
        with patch('proxmox.time.sleep'):
            self.host.relay(self.record,self.spec,True)
        self.assertEqual(self.restarts,1)
        self.host.relay(self.record,self.spec,False)
        self.client_state='active';self.dhcp_pending=31
        with patch('proxmox.time.sleep'):
            with self.assertRaises(Refused):self.host.relay(self.record,self.spec,True)
        self.assertEqual(self.restarts,1)

    def test_unbounded_or_injected_destinations_refused(self):
        for field,value in [('listen_address','0.0.0.0'),('upstream_address','host; command'),
                            ('upstream_port',65536),('upstream_port',True)]:
            r=request();r['spec'].update(copy.deepcopy(self.spec))
            r['spec']['relay'][field]=value
            with self.assertRaises(Refused):validate(r)

    def test_relay_configuration_changes_deployment_digest(self):
        r=request();before=validate(r)
        r['spec'].update(self.spec)
        self.assertNotEqual(before,validate(r))


if __name__ == '__main__':
    unittest.main()
