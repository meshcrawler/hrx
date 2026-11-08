import hashlib
import io
import json
import pathlib
import tarfile
import tempfile
import unittest

from bootstrap import receive, InvalidBundle, install_binary, install_codex_resources
from unittest.mock import patch


def bundle(change=None):
    files={'hrx':b'hrx binary', 'opencode':b'harness binary', 'pitput-0.1-py3-none-any.whl':b'wheel'}
    kinds=['hrx','harness','wheel']
    manifest={'schema':1,'harness':'opencode','artifacts':[{'name':name,'kind':kind,'version':'0.1','sha256':hashlib.sha256(data).hexdigest(),'size':len(data)} for (name,data),kind in zip(files.items(),kinds)]}
    members=[('manifest.json',json.dumps(manifest).encode(),tarfile.REGTYPE)]+[(n,d,tarfile.REGTYPE) for n,d in files.items()]
    if change:change(members)
    result=io.BytesIO()
    with tarfile.open(fileobj=result,mode='w') as archive:
        for name,data,kind in members:
            member=tarfile.TarInfo(name);member.size=len(data);member.type=kind
            if kind==tarfile.SYMTYPE:member.linkname='/etc/passwd'
            archive.addfile(member,io.BytesIO(data))
    result.seek(0);return result


class Receive(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=pathlib.Path(self.temp.name)/'cache'

    def test_verified_and_repeated_bundle(self):
        path,manifest=receive(bundle(),self.root)
        self.assertEqual((path/'hrx').read_bytes(),b'hrx binary')
        self.assertEqual(receive(bundle(),self.root)[0],path)
        (path/'hrx').write_bytes(b'modified')
        with self.assertRaisesRegex(InvalidBundle,'modified'):receive(bundle(),self.root)

    def test_repeat_install_never_rewrites_live_binary(self):
        root=pathlib.Path(self.temp.name)
        source=root/'source';target=root/'target';source.write_bytes(b'binary')
        digest=hashlib.sha256(b'binary').hexdigest()
        install_binary(source,target,digest)
        with patch('bootstrap.shutil.copyfile',side_effect=AssertionError('rewrote live executable')):
            install_binary(source,target,digest)
        target.write_bytes(b'changed')
        with self.assertRaisesRegex(InvalidBundle,'modified'):install_binary(source,target,digest)
        self.assertEqual(target.read_bytes(),b'changed')

    def test_codex_companions_preserve_native_layout_and_integrity(self):
        root=pathlib.Path(self.temp.name);source=root/'source';source.mkdir()
        artifacts=[]
        for name in ['codex-code-mode-host','bwrap','rg']:
            data=name.encode();(source/name).write_bytes(data)
            artifacts.append({'name':name,'kind':'codex-resource','sha256':hashlib.sha256(data).hexdigest()})
        runtime=root/'runtime';manifest={'harness':'codex','artifacts':artifacts}
        install_codex_resources(source,runtime,manifest)
        for name,path in [('codex-code-mode-host','bin/codex-code-mode-host'),('bwrap','codex-resources/bwrap'),('rg','codex-path/rg')]:
            self.assertEqual((runtime/path).read_bytes(),name.encode())
            self.assertEqual((runtime/path).stat().st_mode&0o777,0o755)
        (runtime/'bin/codex-code-mode-host').write_bytes(b'tampered')
        with self.assertRaisesRegex(InvalidBundle,'modified'):
            install_codex_resources(source,runtime,manifest)

    def test_interrupted_binary_copy_can_retry_without_repair(self):
        root=pathlib.Path(self.temp.name)
        source=root/'source';target=root/'target';source.write_bytes(b'complete binary')
        digest=hashlib.sha256(source.read_bytes()).hexdigest()
        def interrupted_copy(source, temporary):
            pathlib.Path(temporary).write_bytes(b'partial')
            raise OSError('interrupted copy')
        with patch('bootstrap.shutil.copyfile',side_effect=interrupted_copy):
            with self.assertRaisesRegex(OSError,'interrupted copy'):
                install_binary(source,target,digest)
        self.assertFalse(target.exists())
        install_binary(source,target,digest)
        self.assertEqual(target.read_bytes(),source.read_bytes())
        self.assertEqual(target.stat().st_mode&0o777,0o755)

    def test_receiver_rejects_codex_without_companions(self):
        def change(members):
            manifest=json.loads(members[0][1]);manifest['harness']='codex'
            members[0]=('manifest.json',json.dumps(manifest).encode(),tarfile.REGTYPE)
        with self.assertRaisesRegex(InvalidBundle,'requires pinned companion'):
            receive(bundle(change),self.root)
        self.assertEqual(list(self.root.iterdir()),[])

    def test_rejects_bad_archive_before_publication(self):
        changes=[lambda m:m.__setitem__(1,('../escape',b'x',tarfile.REGTYPE)),
                 lambda m:m.__setitem__(1,('hrx',b'hrx binary',tarfile.SYMTYPE)),
                 lambda m:m.__setitem__(1,('hrx',b'BAD binary',tarfile.REGTYPE)),
                 lambda m:m.pop(),lambda m:m.append(m[1])]
        for change in changes:
            with self.subTest(change=change),self.assertRaises((InvalidBundle,tarfile.TarError)):
                receive(bundle(change),self.root)
            self.assertEqual(list(self.root.iterdir()),[])


if __name__=='__main__':unittest.main()
