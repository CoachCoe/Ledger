#!/usr/bin/env python3
"""Measure the shipped daemon via its public Unix HTTP API (no agent SDK).

Usage: python3 tools/benchmark-observer.py /abs/ledger /abs/repo [--seconds 60]
The repo must be disposable, have a commit, and have ignored node_modules installed.
It retains .ledger/worktrees/results for inspection; it always stops its daemon.
"""
import argparse
import base64
import concurrent.futures
import http.client
import json
import os
from pathlib import Path
import random
import socket
import subprocess
import sys
import tempfile
import threading
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('ledger', type=Path)
p.add_argument('repo', type=Path)
p.add_argument('--seconds', type=int, default=60)
a = p.parse_args()
repo, ledger = a.repo.resolve(), a.ledger.resolve()
if not (repo / 'node_modules').is_dir():
    p.error('install node_modules in the disposable source repository first')
if (repo / '.ledger').exists():
    p.error('use a disposable repository without existing Ledger state')
config = tempfile.mkdtemp(prefix='ledger-bench-keys-', dir='/tmp')
env = dict(os.environ, XDG_CONFIG_HOME=config)
socket_path = str(repo / '.ledger/runtime/ledgerd.sock')

class Connection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(30)
        self.sock.connect(socket_path)

def rpc(operation, args=None, session=''):
    op = '' if operation.endswith('.status') else 'op_' + base64.b32hexencode(os.urandom(16)).decode().lower().rstrip('=')
    request = dict(api_version=1, operation=operation, operation_id=op, session_id=session, input=args or {})
    conn = Connection('ledger')
    try:
        conn.request('POST', '/v1', json.dumps(request), {'Content-Type': 'application/json'})
        response = json.loads(conn.getresponse().read())
    finally:
        conn.close()
    if not response['ok']:
        raise RuntimeError(response['error'])
    result = response['result']
    return result.get('result', result)

def cli(*args):
    return subprocess.run([str(ledger), *args], cwd=repo, env=env, check=True, capture_output=True, text=True)

cli('init')
cli('system', 'status')
workspaces = []
try:
    intent = rpc('intent.create', dict(description='Real-project watcher benchmark', criteria=[dict(id='review', statement='Measure observation latency', verification=dict(kind='judgment', reviewer='any'))]))
    for i in range(10):
        session = rpc('session.create', dict(agent_name=f'bench-{i}'))
        ws = rpc('workspace.fork', dict(intent_id=intent['intent_id'], capability_id=session['capability_id']), session['session_id'])
        path = Path(ws['path'])
        copy = ['cp', '-c', '-R'] if sys.platform == 'darwin' else ['cp', '-R']
        subprocess.run([*copy, str(repo / 'node_modules'), str(path)], check=True)
        workspaces.append(ws)
    # Warm all private indexes after setup and ignored dependency-copy events.
    time.sleep(3)
    for ws in workspaces:
        rpc('workspace.status', dict(workspace_id=ws['workspace_id']))
    lock = threading.Lock()
    pending = {}
    latencies = []
    failures = []
    stopped = threading.Event()
    start = time.monotonic()
    deadline = start + a.seconds

    def writer(i, ws):
        rng = random.Random(i)
        counter = 0
        while not stopped.wait(rng.uniform(1, 3)) and time.monotonic() < deadline:
            rel = f'ledger-bench-{i}-{counter}.ts'
            with lock:
                (Path(ws['path']) / rel).write_text(f'export const x = {counter};\n')
                pending[(ws['workspace_id'], rel)] = time.monotonic()
            counter += 1

    def reader(ws):
        while not stopped.is_set():
            try:
                status = rpc('workspace.status', dict(workspace_id=ws['workspace_id']))
                with lock:
                    now = time.monotonic()
                    for path in status.get('write_set', []):
                        key = (ws['workspace_id'], path)
                        if key in pending:
                            latencies.append((now - pending.pop(key)) * 1000)
            except Exception as exc:
                with lock:
                    failures.append(str(exc))
                return
            stopped.wait(.1)

    with concurrent.futures.ThreadPoolExecutor(max_workers=20) as pool:
        futures = [pool.submit(writer, i, ws) for i, ws in enumerate(workspaces)]
        futures += [pool.submit(reader, ws) for ws in workspaces]
        # Drain the last debounce window instead of excluding late edits.
        while time.monotonic() < deadline + 3:
            time.sleep(.1)
        stopped.set()
        for future in futures:
            future.result()
    samples = sorted(latencies)
    percentile = lambda q: round(samples[min(len(samples)-1, int(q*len(samples)))], 2) if samples else None
    result = dict(platform=sys.platform, cpu_count=os.cpu_count(), source_commit=subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip(), workspaces=10, duration_seconds=a.seconds, samples=len(samples), p50_ms=percentile(.5), p95_ms=percentile(.95), max_ms=percentile(1), undetected=len(pending), errors=failures)
    result['passed'] = bool(samples) and not pending and not failures and result['max_ms'] <= 2000
    (repo / '.ledger/observer-benchmark.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))
    if not result['passed']:
        sys.exit(1)
finally:
    cli('daemon', 'stop')
