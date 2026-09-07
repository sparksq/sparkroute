#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Qualify an actual release archive on its native controller platform."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import socket
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


def port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


def request(url, *, body=None, token=None):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    with urllib.request.urlopen(urllib.request.Request(url, data=json.dumps(body).encode() if body else None, headers=headers), timeout=10) as reply:
        return json.load(reply)


def smoke(archive: Path):
    pins = dict(line.split(None, 1)[::-1] for line in (archive.parent / 'checksums.txt').read_text().splitlines())
    assert hashlib.sha256(archive.read_bytes()).hexdigest() == pins[archive.name], 'archive checksum differs'
    expected = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    executable = 'sparkroute.exe' if os.name == 'nt' else 'sparkroute'
    with tempfile.TemporaryDirectory(prefix='sparkroute-smoke-') as temporary:
        root = Path(temporary)
        binary = root / executable
        if archive.suffix == '.zip':
            with zipfile.ZipFile(archive) as source:
                binary.write_bytes(source.read(executable))
                metadata = json.loads(source.read('build-info.json'))
        else:
            with tarfile.open(archive) as source:
                binary.write_bytes(source.extractfile(executable).read())
                metadata = json.load(source.extractfile('build-info.json'))
        binary.chmod(0o700)
        info = json.loads(subprocess.check_output([str(binary), '--build-info'], text=True))
        assert info == metadata
        assert info['commit'] == expected
        assert info['source'] == 'https://github.com/sparksq/sparkroute/tree/' + expected
        calls = []

        class Upstream(BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                calls.append(body)
                reply = json.dumps({'id': 'smoke', 'object': 'chat.completion', 'model': body['model'], 'choices': []}).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(reply)))
                self.end_headers()
                self.wfile.write(reply)

            def log_message(self, *_args):
                pass

        upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        token = root / 'admin-token'
        token.write_text('smoke-test-token')
        token.chmod(0o600)
        seed = root / 'seed.json'
        seed.write_text(json.dumps({
            'providers': [{'name': 'fixture', 'type': 'openai_compatible', 'base_url': f'http://127.0.0.1:{upstream.server_port}/v1'}],
            'deployments': [{'name': 'warm', 'provider': 'fixture', 'model': 'upstream'}],
            'virtual_models': [{'name': 'assistant', 'aliases': ['coding'], 'pools': [{'targets': [{'deployment': 'warm', 'weight': 1}]}]}],
        }))
        data_port, admin_port = port(), port()
        data, admin = f'http://127.0.0.1:{data_port}', f'http://127.0.0.1:{admin_port}'
        with (root / 'gateway.log').open('w+') as log:
            process = subprocess.Popen([
                str(binary), '-config-source=sqlite', '-config-sqlite=' + str(root / 'config.db'),
                '-config-bootstrap=' + str(seed), '-data-address=127.0.0.1:' + str(data_port),
                '-client-credentials-sqlite=' + str(root / 'credentials.db'),
                '-ledger-sqlite=' + str(root / 'ledger.db'),
                '-trace-storage=filesystem', '-trace-filesystem=' + str(root / 'traces'),
                '-admin-address=127.0.0.1:' + str(admin_port), '-operations-address=127.0.0.1:0',
                '-admin-auth-mode=token-file', '-admin-token-file=' + str(token), '-list-aliases',
            ], stdout=log, stderr=subprocess.STDOUT)
            try:
                deadline = time.monotonic() + 30
                while True:
                    try:
                        bootstrap = request(admin + '/v1/ui/bootstrap', token='smoke-test-token')
                        break
                    except urllib.error.URLError:
                        if process.poll() is not None or time.monotonic() >= deadline:
                            log.seek(0)
                            raise RuntimeError('gateway failed to start: ' + log.read()) from None
                        time.sleep(0.1)
                assert bootstrap['build'] == info
                assert bootstrap['features']['provider_auth']
                sign_in = request(admin + '/v1/provider-auth/openai/smoke', token='smoke-test-token')
                assert sign_in['state'] == 'signed_out'
                try:
                    request(admin + '/v1/ui/bootstrap')
                    raise AssertionError('admin accepted unauthenticated request')
                except urllib.error.HTTPError as error:
                    assert error.code == 401
                names = {model['id'] for model in request(data + '/v1/models')['data']}
                assert {'assistant', 'coding'} <= names
                request(data + '/v1/chat/completions', body={'model': 'coding', 'messages': [{'role': 'user', 'content': 'hi'}]})
                assert calls[0]['model'] == 'upstream'
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)
                upstream.shutdown()
                upstream.server_close()
                thread.join(timeout=5)
        print('Qualified', archive.name, 'version', info['version'], 'source', info['commit'])


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    smoke(parser.parse_args().archive.resolve())
