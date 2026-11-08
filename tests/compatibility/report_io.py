"""Atomic authoritative JSON publication for compatibility evidence."""
import json
import os
from pathlib import Path
import tempfile


def atomic_json(path, document):
    staged = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False,
                                         prefix=".report-", encoding="utf-8") as stream:
            staged = Path(stream.name)
            stream.write(json.dumps(document, indent=2) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(staged, path)
    except Exception:
        if staged is not None:
            try:
                staged.unlink(missing_ok=True)
            except OSError:
                pass
        raise
