"""Run hub store tests against a disposable real PostgreSQL instance."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    executable = shutil.which("initdb")
    candidates = sorted(Path("/opt/homebrew/opt").glob("postgresql*/bin/initdb"))
    pg_bin = Path(os.environ.get("PG_BIN") or
                  (str(Path(executable).parent) if executable else str(candidates[-1].parent) if candidates else ""))
    if not (pg_bin / "initdb").is_file():
        raise SystemExit("PostgreSQL binaries required: set PG_BIN")
    with tempfile.TemporaryDirectory(prefix="hrx-hub-pg-", dir="/tmp") as directory:
        root = Path(directory).resolve()
        data, socket = root / "data", root / "socket"
        socket.mkdir(mode=0o700)
        with (root / "setup.log").open("w") as log:
            run([str(pg_bin / "initdb"), "-D", str(data), "-U", "fixture", "--auth=trust",
                 "--no-locale", "--encoding=UTF8"], stdout=log, stderr=log)
            run([str(pg_bin / "pg_ctl"), "-D", str(data), "-l", str(root / "postgres.log"),
                 "-o", f"-k {socket} -c listen_addresses=''", "-w", "start"], stdout=log, stderr=log)
            try:
                env = dict(os.environ, HRX_HUB_TEST_DSN=f"host={socket} user=fixture dbname=postgres sslmode=disable")
                run(["go", "test", "-race", "./internal/hub", "-run", "^TestPostgres", "-count=1", "-timeout=90s"], env=env)
            finally:
                run([str(pg_bin / "pg_ctl"), "-D", str(data), "-m", "immediate", "stop"], stdout=log, stderr=log)


if __name__ == "__main__":
    main()
