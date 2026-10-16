#!/usr/bin/env python3
"""Run synthetic catalog tests with an isolated PostgreSQL server and verified TLS."""
import os
import secrets
import subprocess
import tempfile
import time
from pathlib import Path

root = Path(__file__).resolve().parents[1]
name = 'research-postgres-test-' + secrets.token_hex(6)
image = 'postgres:18.1@sha256:1090bc3a8ccfb0b55f78a494d76f8d603434f7e4553543d6e807bc7bd6bbd17f'
# Override for another reviewed official PostgreSQL image or architecture.
image = os.environ.get('POSTGRES_TEST_IMAGE', image)

def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)

with tempfile.TemporaryDirectory(prefix=name) as directory:
    path = Path(directory)
    password = secrets.token_hex(32)
    (path / 'password').write_text(password)
    (path / 'username').write_text('desk_owner')
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
        '-subj', '/CN=localhost', '-addext', 'subjectAltName=DNS:localhost',
        '-keyout', str(path / 'server.key'), '-out', str(path / 'server.crt'),
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    # Root copies the mounted inputs; PostgreSQL receives its own private key files.
    command = '''cp /tls/server.crt /tmp/server.crt
cp /tls/server.key /tmp/server.key
cp /tls/password /tmp/pg-password
chown postgres:postgres /tmp/server.crt /tmp/server.key /tmp/pg-password
chmod 600 /tmp/server.key /tmp/pg-password
exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/tmp/server.crt -c ssl_key_file=/tmp/server.key
'''
    try:
        run('docker', 'run', '--detach', '--name', name, '--publish', '127.0.0.1:5432:5432',
            '--mount', f'type=bind,src={path},dst=/tls,readonly',
            '--env', 'POSTGRES_USER=desk_owner', '--env', 'POSTGRES_DB=research',
            '--env', 'POSTGRES_PASSWORD_FILE=/tmp/pg-password',
            '--entrypoint', 'bash', image, '-ceu', command, stdout=subprocess.DEVNULL)
        for _ in range(60):
            ready = subprocess.run(['docker', 'exec', name, 'pg_isready', '-h', '127.0.0.1', '-U', 'desk_owner', '-d', 'research'],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if ready.returncode == 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError('Synthetic PostgreSQL server did not become ready')
        env = dict(os.environ, RESEARCH_POSTGRES_INTEGRATION='true', RESEARCH_POSTGRES_HOST='localhost',
                   RESEARCH_POSTGRES_DATABASE='research', RESEARCH_POSTGRES_USERNAME_FILE=str(path / 'username'),
                   RESEARCH_POSTGRES_PASSWORD_FILE=str(path / 'password'), RESEARCH_POSTGRES_CA_FILE=str(path / 'server.crt'))
        run('go', 'test', '-race', '-run', 'TestPostgres', '-count=1', '-v', '.', cwd=root, env=env)
    finally:
        subprocess.run(['docker', 'rm', '--force', '--volumes', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
