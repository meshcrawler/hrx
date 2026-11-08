"""Certificate-only checks: no native harness, server, or database is started."""
from pathlib import Path
import ssl
import subprocess
import tempfile
import unittest

from gateway import tls_material


def handshake(ca, cert, key, server_hostname):
    """An in-memory TLS handshake (no socket, no server process) using Python's own
    default client verification, which is strict X.509 from 3.13."""
    server_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    server_context.load_cert_chain(cert, key)
    client_context = ssl.create_default_context(cafile=str(ca))
    to_server, to_client = ssl.MemoryBIO(), ssl.MemoryBIO()
    server = server_context.wrap_bio(to_server, to_client, server_side=True)
    client = client_context.wrap_bio(to_client, to_server, server_hostname=server_hostname)
    done = {client: False, server: False}
    for _ in range(20):
        for side in (client, server):
            if not done[side]:
                try:
                    side.do_handshake()
                    done[side] = True
                except ssl.SSLWantReadError:
                    pass
        if all(done.values()):
            return
    raise AssertionError("handshake did not complete")


class TLSMaterialTests(unittest.TestCase):
    def test_server_leaf_has_separate_ca_and_valid_loopback_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            ca, cert, key = tls_material(root)

            def openssl(*args):
                return subprocess.run(["openssl", *map(str, args)], capture_output=True,
                                      text=True, timeout=10)

            handshake(ca, cert, key, "127.0.0.1")
            with self.assertRaises(ssl.SSLCertVerificationError):
                handshake(ca, cert, key, "127.0.0.2")
            leaf = openssl("x509", "-in", cert, "-noout", "-text")
            authority = openssl("x509", "-in", ca, "-noout", "-text")
            self.assertEqual(leaf.returncode, 0, leaf.stderr)
            self.assertEqual(authority.returncode, 0, authority.stderr)
            self.assertIn("CA:FALSE", leaf.stdout)
            self.assertIn("CA:TRUE", authority.stdout)
            self.assertIn("TLS Web Server Authentication", leaf.stdout)
            for text in (leaf.stdout, authority.stdout):
                self.assertIn("Subject Key Identifier", text)
            self.assertIn("Authority Key Identifier", leaf.stdout)
            self.assertNotEqual(ca.read_bytes(), cert.read_bytes())
            for private_key in (key, root / "ca-key.pem"):
                self.assertEqual(private_key.stat().st_mode & 0o777, 0o600)


if __name__ == "__main__":
    unittest.main()
