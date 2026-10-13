"""Verify old executable on offline copies, without loading production config or tokens.

Only the copies in a fresh temporary directory are opened by either executable.
Output is aggregate checks; database contents and application logs are not printed.
"""
import argparse
import base64
import collections
from contextlib import closing
import gzip
import hashlib
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.request


def legacy_rows(path):
    result = {}
    with closing(sqlite3.connect(f"file:{path.as_posix()}?mode=ro", uri=True)) as con:
        for (table,) in con.execute("SELECT name FROM sqlite_master WHERE type='table'"):
            if table.startswith(("moderation_", "sqlite_")):
                continue
            rows = con.execute('SELECT * FROM "' + table.replace('"', '""') + '"').fetchall()
            result[table] = collections.Counter(
                hashlib.sha256(json.dumps(row, default=str).encode()).hexdigest() for row in rows
            )
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-binary", type=Path, required=True)
    parser.add_argument("--migrator", type=Path, required=True)
    parser.add_argument("--base-cwd", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("databases", type=Path, nargs="+")
    args = parser.parse_args()
    results = []
    # Deliberately omit all inherited bot/OpenAI/auth configuration.
    env = {k: v for k, v in os.environ.items() if k.upper() in
           {"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE"}}
    with tempfile.TemporaryDirectory(prefix="moderation-rollback-") as temp:
        temp = Path(temp)
        for index, source in enumerate(args.databases):
            copy = temp / f"copy-{index}.db"
            with closing(sqlite3.connect(f"file:{source.resolve().as_posix()}?mode=ro", uri=True)) as src:
                with closing(sqlite3.connect(copy)) as dst:
                    src.backup(dst)
            before = legacy_rows(copy)
            with (temp / f"migration-{index}.log").open("wb") as log:
                subprocess.run([str(args.migrator.resolve()), "--copy-db", str(copy)],
                               stdout=log, stderr=subprocess.STDOUT, env=env, check=True)
            assert legacy_rows(copy) == before, "migration changed legacy rows"
            with closing(sqlite3.connect(copy)) as con:
                assert con.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
                journal_before = con.execute("SELECT * FROM moderation_actions").fetchall()
                assert journal_before, "unfinished fixture missing"
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            command = [str(args.base_binary.resolve()), "--db=" + str(copy),
                       "--instance-id=tg-spam", "--history-duration=876000h", "--telegram.token=", "--telegram.group=",
                       "--server.enabled", f"--server.listen=127.0.0.1:{port}",
                       "--server.auth=offline-compat"]
            authorization = "Basic " + base64.b64encode(b"tg-spam:offline-compat").decode()
            with (temp / f"old-binary-{index}.log").open("wb") as log:
                process = subprocess.Popen(command, cwd=args.base_cwd, stdout=log,
                                           stderr=subprocess.STDOUT, env=env)
                try:
                    expires = time.monotonic() + 30
                    for endpoint in ("/ping", "/users/", "/samples", "/dictionary/", "/download/backup"):
                        while True:
                            if process.poll() is not None:
                                raise RuntimeError("old executable stopped; private log retained during probe")
                            try:
                                request = urllib.request.Request(f"http://127.0.0.1:{port}{endpoint}",
                                    headers={"Authorization": authorization})
                                with urllib.request.urlopen(request, timeout=3) as response:
                                    payload = response.read()
                                    assert response.status == 200
                                    if endpoint == "/download/backup":
                                        if payload[:2] == b"\x1f\x8b":
                                            payload = gzip.decompress(payload)
                                        assert b"moderation_actions" in payload
                                break
                            except (OSError, urllib.error.URLError):
                                if time.monotonic() >= expires:
                                    raise RuntimeError("old executable read probe timed out") from None
                                time.sleep(0.2)
                finally:
                    process.terminate()
                    process.wait(timeout=10)
            with closing(sqlite3.connect(copy)) as con:
                assert con.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
                assert con.execute("SELECT * FROM moderation_actions").fetchall() == journal_before
            after = legacy_rows(copy)
            # Startup may initialize configured default corpus, but cannot lose existing rows.
            lost = {table: sum((rows - after.get(table, collections.Counter())).values()) for table, rows in before.items()}
            assert not any(lost.values()), f"old startup changed legacy rows: {lost}"
            results.append({"database": source.name, "migration_preserved_legacy_rows": True,
                            "old_binary_read_endpoints": 5, "unfinished_actions_unchanged": True,
                            "integrity": "ok", "telegram_calls": 0})
    args.report.write_text(json.dumps(results, indent=2), encoding="utf-8")
    print(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()
