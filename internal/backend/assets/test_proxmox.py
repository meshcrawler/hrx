import copy
import json
import pathlib
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor

from proxmox import Host, Refused


class FakeProxmox:
    def __init__(self):
        self.guests = {}
        self.creates = 0
        self.releases = []
        self.fail_after_create = False
        self.fail_after_destroy = False

    def __call__(self, args):
        if args[:3] == ['pvesh', 'get', '/cluster/status']:
            return json.dumps([{'type': 'node', 'local': 1, 'name': 'testhost'}])
        if args[:3] == ['pvesh', 'get', '/cluster/nextid']:
            return '3000'
        if args[:3] == ['pvesh', 'get', '/cluster/resources']:
            return json.dumps([{'vmid': n, 'node': 'testhost', 'type': 'lxc', 'status': g['status']} for n,g in self.guests.items()])
        if args[0] == 'pvesh' and args[2].endswith('/status/current'):
            return json.dumps({'status': self.guests[int(args[2].split('/')[-3])]['status']})
        if args[0] == 'pvesh':
            return json.dumps(self.guests[int(args[2].split('/')[-2])]['config'])
        operation, vmid = args[1], int(args[2])
        if operation == 'exec':
            assert self.guests[vmid]['status'] == 'running'
            self.releases.append(vmid)
            return ''
        if operation == 'create':
            if vmid in self.guests:
                raise Refused('VMID collision')
            self.creates += 1
            self.guests[vmid] = {'status': 'stopped', 'config': {'description': args[args.index('--description')+1]+'\n', 'unprivileged': 1}}
            if self.fail_after_create:
                raise Refused('lost response after effect')
        elif operation == 'destroy':
            del self.guests[vmid]
            if self.fail_after_destroy:
                raise Refused('lost destroy response')
        else:
            self.guests[vmid]['status'] = {'start': 'running', 'stop': 'stopped'}[operation]
        return ''


def request():
    return {'operation': 'ensure', 'deployment': 'a'*32, 'spec': {
        'template': 'local:vztmpl/debian.tar.zst', 'storage': 'local-lvm',
        'bridge': 'vmbr1', 'cores': 2, 'memory_mib': 2048, 'disk_gib': 8,
        'tags': ['test'], 'launch_digest': 'b'*64}}


class Lifecycle(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.fake = FakeProxmox()
        self.host = Host(pathlib.Path(self.temp.name)/'state', self.fake)
        self.req = request()

    def test_stop_and_destroy_release_only_running_owned_guest(self):
        self.host.handle(self.req)
        for op in ['start','stop','stop','start','destroy','destroy']:
            self.req['operation']=op
            self.host.handle(self.req)
        self.assertEqual(self.fake.releases,[3000,3000])

    def test_repeated_and_concurrent_ensure_create_once(self):
        with ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(lambda _:self.host.handle(self.req), range(8)))
        self.assertEqual({r['vmid'] for r in results}, {3000})
        self.assertEqual(self.fake.creates, 1)

    def test_concurrent_distinct_deployments_reserve_distinct_ids(self):
        def create(i):
            r=copy.deepcopy(self.req);r['deployment']=format(i,'032x')
            return self.host.handle(r)['vmid']
        with ThreadPoolExecutor(max_workers=4) as pool:
            ids=list(pool.map(create,range(8)))
        self.assertEqual(len(set(ids)),8)

    def test_recover_create_after_lost_response(self):
        self.fake.fail_after_create = True
        with self.assertRaises(Refused):self.host.handle(self.req)
        self.fake.fail_after_create = False
        self.assertEqual(self.host.handle(self.req)['vmid'], 3000)
        self.assertEqual(self.fake.creates, 1)

    def test_foreign_ownership_is_never_adopted_or_destroyed(self):
        self.host.handle(self.req)
        self.fake.guests[3000]['config']['description'] = 'unrelated owner'
        for op in ['ensure','start','stop','destroy']:
            with self.subTest(op=op):
                self.req['operation']=op
                with self.assertRaisesRegex(Refused,'ownership'):self.host.handle(self.req)
        self.assertIn(3000,self.fake.guests)

    def test_active_proxmox_task_blocks_mutation(self):
        self.host.handle(self.req)
        self.fake.guests[3000]['config']['lock']='create'
        self.req['operation']='destroy'
        with self.assertRaisesRegex(Refused,'locked'):self.host.handle(self.req)
        self.assertIn(3000,self.fake.guests)

    def test_guest_execution_requires_owned_running_container(self):
        self.host.handle(self.req)
        self.req['operation']='guest';self.req['command']=['python3','-c','pass']
        calls=[];self.host.guest=calls.append
        with self.assertRaisesRegex(Refused,'running'):self.host.handle(self.req)
        self.fake.guests[3000]['status']='running'
        self.host.handle(self.req)
        self.assertEqual(calls,[['pct','exec','3000','--','python3','-c','pass']])
        self.fake.guests[3000]['config']['description']='different owner'
        with self.assertRaisesRegex(Refused,'ownership'):self.host.handle(self.req)
        self.assertEqual(len(calls),1)

    def test_changed_spec_rejected(self):
        self.host.handle(self.req)
        self.req['spec']['launch_digest']='c'*64
        with self.assertRaisesRegex(Refused,'different desired'):self.host.handle(self.req)

    def test_lifecycle_and_destroy_recovery(self):
        self.host.handle(self.req)
        self.req['operation']='start'
        self.assertEqual(self.host.handle(self.req)['container_status'],'running')
        self.req['operation']='destroy';self.fake.fail_after_destroy=True
        with self.assertRaises(Refused):self.host.handle(self.req)
        self.req['operation']='inspect'
        self.assertEqual(self.host.handle(self.req)['phase'],'destroying')
        self.req['operation']='start'
        with self.assertRaisesRegex(Refused,'destruction'):self.host.handle(self.req)
        self.req['operation']='destroy'
        self.assertEqual(self.host.handle(self.req)['phase'],'destroyed')
        self.req['operation']='ensure'
        with self.assertRaisesRegex(Refused,'destroyed'):self.host.handle(self.req)

    def test_no_silent_recreate(self):
        self.host.handle(self.req)
        del self.fake.guests[3000]
        with self.assertRaisesRegex(Refused,'disappeared'):self.host.handle(self.req)

    def test_distinct_deployments_and_tombstones_reserve_ids(self):
        self.host.handle(self.req)
        self.req['operation']='destroy';self.host.handle(self.req)
        self.req['operation']='ensure';self.req['deployment']='c'*32
        self.assertEqual(self.host.handle(self.req)['vmid'],3001)

    def test_range_skips_cluster_occupied_ids(self):
        self.fake.guests[3000] = {'status': 'stopped', 'config': {}}
        self.assertEqual(self.host.handle(self.req)['vmid'], 3001)

    def test_range_exhaustion_has_no_create_effect(self):
        for vmid in range(3000, 3100):
            self.fake.guests[vmid] = {'status': 'stopped', 'config': {}}
        with self.assertRaisesRegex(Refused, 'range 3000–3099 exhausted'):
            self.host.handle(self.req)
        self.assertEqual(self.fake.creates, 0)

    def test_existing_legacy_id_is_preserved(self):
        self.host.handle(self.req)
        record_path = self.host.root / (self.req['deployment'] + '.json')
        record = json.loads(record_path.read_text())
        record['vmid'] = 162
        record_path.write_text(json.dumps(record))
        self.fake.guests[162] = self.fake.guests.pop(3000)
        self.assertEqual(self.host.handle(self.req)['vmid'], 162)
        self.assertEqual(self.fake.creates, 1)

    def test_invalid_spec_cannot_create(self):
        for key,value in [('bridge','vmbr1,firewall=0'),('cores',True),('tags',['a;b']),('template','../bad')]:
            r=copy.deepcopy(self.req);r['spec'][key]=value
            with self.subTest(key=key),self.assertRaises(Refused):self.host.handle(r)
        self.assertEqual(self.fake.creates,0)


if __name__ == '__main__':unittest.main()
