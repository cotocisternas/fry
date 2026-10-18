#!/usr/bin/env python3
"""Bounded disposable Docker rehearsal. Invoke through scripts/smoke.sh containers.

The image must already exist. This runner never builds or changes an existing
installation; every Docker resource is generated and recorded before creation.
"""
import hashlib
import imaplib
import json
from pathlib import Path
import secrets
import shutil
import signal
import smtplib
import ssl
import subprocess
import sys
import tempfile
import time

from scripts.lib import testdeps

PASSWORD = 'synthetic-container-imap-only'
ORG = '20000000000000000000000000000001'
INBOX = '30000000000000000000000000000001'


def run(*args, timeout=30, check=True, data=None):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError(f'{args[0]} operation failed: '
                           + result.stderr.decode(errors='replace')[-4096:])
    return result


def docker(*args, **kwargs):
    return run('docker', *args, **kwargs).stdout.decode().strip()


class Rehearsal:
    def __init__(self, image, root):
        self.image = image
        self.postgres_source = None
        self.root = root
        self.runtime = root / 'runtime'
        self.signing = root / 'signing'
        self.runtime.mkdir(mode=0o755)
        self.signing.mkdir(mode=0o700)
        self.token = 'fry-container-smoke-' + secrets.token_hex(6)
        self.network = self.token + '-network'
        self.ingress_network = self.token + '-ingress'
        self.label = 'fry.smoke.run=' + self.token
        self.containers = []
        self.volumes = []
        self.created_networks = []
        self.db = self.token + '-postgres'
        self.coordinator = self.token + '-coordinator'
        self.members = [self.token + '-member-a', self.token + '-member-b']
        self.expected = []
        self.context = None

    def inspect(self, name):
        return json.loads(docker('inspect', name))[0]

    def all_logs(self, name):
        result = run('docker', 'logs', '--tail', '200', name, check=False)
        return (result.stdout + result.stderr).decode(errors='replace')

    def state(self, name):
        return self.inspect(name)['State']

    def wait_log(self, name, marker, previous=0, seconds=30):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            state = self.state(name)
            text = self.all_logs(name)
            if not state['Running']:
                raise AssertionError(f'{name} exited {state["ExitCode"]}: {text[-4096:]}')
            if text.count(marker) > previous:
                return
            time.sleep(.1)
        raise AssertionError(f'{name} startup timed out: {self.all_logs(name)[-4096:]}')

    def container(self, name, alias, image, options=(), command=(), binary=None, ingress=False):
        self.containers.append(name)
        args = (['create'] if ingress else ['run', '--detach']) + [
            '--pull=never', '--name', name, '--label', self.label, '--restart=no',
            '--network', self.ingress_network if ingress else self.network,
            '--network-alias', alias]
        if binary:
            args += ['--read-only', '--cap-drop=ALL',
                     '--security-opt=no-new-privileges:true',
                     '--mount', f'type=bind,src={self.runtime},dst=/etc/fry,readonly',
                     '--env', 'FRY_DATABASE_URL=postgres://postgres:fry-test-only@database:5432/fry_test?sslmode=disable',
                     '--env', 'FRY_CONTAINER_IMAP_PASSWORD=' + PASSWORD,
                     '--entrypoint', '/usr/local/bin/' + binary]
        docker(*args, *options, image, *command)
        if ingress:
            # Attach private dependencies before starting the process. Internal-only
            # Docker networks do not reliably support published host ports.
            docker('network', 'connect', '--alias', alias, self.network, name)
            docker('start', name)
        if binary:
            details = self.inspect(name)
            host = details['HostConfig']
            assert details['Config']['User'] in ('65532', '65532:65532'), 'image default UID changed'
            assert host['ReadonlyRootfs'] and 'ALL' in host['CapDrop']
            assert any(v.startswith('no-new-privileges') for v in host['SecurityOpt'])

    def sql(self, query):
        return docker('exec', self.db, 'psql', '-U', 'postgres', '-d', 'fry_test',
                      '-v', 'ON_ERROR_STOP=1', '-Atc', query, timeout=10)

    def certificate(self, name, server=False):
        key = self.runtime / (name + '.key')
        csr = self.signing / (name + '.csr')
        cert = self.runtime / (name + '.crt')
        run('openssl', 'req', '-new', '-newkey', 'ec', '-pkeyopt',
            'ec_paramgen_curve:P-256', '-nodes', '-keyout', str(key),
            '-out', str(csr), '-subj', '/CN=' + name)
        extension = self.signing / (name + '.ext')
        text = 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n'
        if server:
            text += ('extendedKeyUsage=serverAuth\nsubjectAltName=DNS:' + name
                     + ',DNS:localhost,IP:127.0.0.1\n')
        else:
            text += 'extendedKeyUsage=clientAuth\n'
        extension.write_text(text)
        run('openssl', 'x509', '-req', '-in', str(csr), '-CA', str(self.runtime / 'ca.crt'),
            '-CAkey', str(self.signing / 'ca.key'), '-CAcreateserial',
            '-CAserial', str(self.signing / 'ca.srl'), '-out', str(cert),
            '-days', '1', '-extfile', str(extension))
        public = run('openssl', 'x509', '-in', str(cert), '-pubkey', '-noout').stdout
        der = run('openssl', 'pkey', '-pubin', '-outform', 'DER', data=public).stdout
        return {'ca_file': '/etc/fry/ca.crt', 'certificate_file': '/etc/fry/' + cert.name,
                'key_file': '/etc/fry/' + key.name}, hashlib.sha256(der).hexdigest()

    def configure(self):
        run('openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256',
            '-nodes', '-keyout', str(self.signing / 'ca.key'), '-out', str(self.runtime / 'ca.crt'),
            '-days', '1', '-subj', '/CN=FRY disposable container CA',
            '-addext', 'basicConstraints=critical,CA:TRUE',
            '-addext', 'keyUsage=critical,keyCertSign,cRLSign')
        client_tls, client_key = self.certificate('coordinator-client')
        self.certificate('coordinator', server=True)
        members = []
        for i, alias in enumerate(('member-a', 'member-b')):
            tls, key = self.certificate(alias, server=True)
            member_id = f'{i + 1:032x}'
            members.append({'id': member_id, 'domain': 'zone-' + str(i + 1),
                            'url': 'https://' + alias + ':8443', 'server_key': key})
            self.write_config(alias + '.json', {
                'id': member_id, 'domain': 'zone-' + str(i + 1),
                'listen': '0.0.0.0:8443', 'data_directory': '/var/lib/fry',
                'max_message_bytes': 1048576, 'capacity_bytes': 67108864,
                'max_in_flight': 4, 'max_requests': 16, 'timeout_seconds': 10,
                'tls': tls, 'coordinator_keys': [client_key]})
        edge = {'tls_cert_file': '/etc/fry/coordinator.crt',
                'tls_key_file': '/etc/fry/coordinator.key', 'handshake_timeout_seconds': 5}
        config = {
            'domain': 'mail.container.test', 'listen': '0.0.0.0:2525',
            'transport': 'starttls', **edge, 'max_message_bytes': 1048576,
            'max_connections': 16, 'max_in_flight': 4, 'recovery_retention_hours': 168,
            'replica_tls': client_tls, 'members': members,
            'routes': [{'address': 'invoices@example.test', 'organization': ORG,
                        'inboxes': [INBOX]}],
            'imap': {'listen': '0.0.0.0:1993', 'transport': 'tls', **edge,
                     'accounts': [{'username': 'invoices',
                                   'password_environment': 'FRY_CONTAINER_IMAP_PASSWORD',
                                   'organization': ORG, 'inbox': INBOX}]}}
        self.write_config('coordinator.json', config)
        # Synthetic fixture keys only. The signing CA private key stays outside
        # the mount. Runtime files must be readable by the image's default UID.
        for path in self.runtime.iterdir():
            path.chmod(0o444)
        self.runtime.chmod(0o555)
        self.context = ssl.create_default_context(cafile=str(self.runtime / 'ca.crt'))

    def write_config(self, name, value):
        (self.runtime / name).write_text(json.dumps(value, indent=2) + '\n')

    def setup(self):
        docker('image', 'inspect', self.image)
        self.postgres_source = testdeps.image()
        self.configure()
        self.created_networks.append(self.network)
        docker('network', 'create', '--internal', '--label', self.label, self.network)
        self.created_networks.append(self.ingress_network)
        docker('network', 'create', '--label', self.label, self.ingress_network)
        self.container(self.db, 'database', self.postgres_source,
                       options=('--env', 'POSTGRES_PASSWORD=fry-test-only',
                                '--env', 'POSTGRES_DB=fry_test'))
        for _ in range(60):
            if run('docker', 'exec', self.db, 'pg_isready', '-h', '127.0.0.1',
                   '-U', 'postgres', '-d', 'fry_test', timeout=5, check=False).returncode == 0:
                break
            if not self.state(self.db)['Running']:
                raise AssertionError('PostgreSQL fixture exited: ' + self.all_logs(self.db))
            time.sleep(.5)
        else:
            raise AssertionError('PostgreSQL fixture did not become ready')
        # Fresh-schema initialization is deliberate and is never rerun on restart.
        init = self.token + '-initialize'
        self.container(init, 'initializer', self.image,
                       command=('-config', '/etc/fry/coordinator.json', '-init-only'), binary='fry')
        self.wait_exit(init, 30, expected=0)
        for name, alias in zip(self.members, ('member-a', 'member-b')):
            volume = name + '-data'
            self.volumes.append(volume)
            docker('volume', 'create', '--label', self.label, volume)
            self.container(name, alias, self.image,
                           options=('--mount', f'type=volume,src={volume},dst=/var/lib/fry'),
                           command=('-config', '/etc/fry/' + alias + '.json'), binary='fry-member')
            self.wait_log(name, 'listening with mutual TLS')
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '2'
        self.container(self.coordinator, 'coordinator', self.image,
                       options=('--publish', '127.0.0.1::2525', '--publish', '127.0.0.1::1993'),
                       command=('-config', '/etc/fry/coordinator.json'), binary='fry', ingress=True)
        self.wait_log(self.coordinator, 'FRY SMTP listening')

    def wait_exit(self, name, seconds, expected=None):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            state = self.state(name)
            if not state['Running']:
                if expected is not None:
                    assert state['ExitCode'] == expected, self.all_logs(name)[-4096:]
                return state['ExitCode']
            time.sleep(.1)
        raise AssertionError(name + ' did not exit within its bound')

    def port(self, private):
        value = docker('port', self.coordinator, str(private) + '/tcp')
        assert value.startswith('127.0.0.1:') and '\n' not in value, value
        return int(value.split(':')[-1])

    def smtp(self):
        client = smtplib.SMTP('127.0.0.1', self.port(2525), timeout=15)
        client.ehlo()
        assert client.has_extn('starttls'), 'incoming SMTP did not advertise STARTTLS'
        client.starttls(context=self.context)
        client.ehlo()
        assert not client.has_extn('auth'), 'incoming SMTP advertised submission AUTH'
        return client

    def send(self, index):
        raw = (f'From: sender@example.net\r\nTo: invoices@example.test\r\n'
               f'Subject: container rehearsal {index}\r\n'
               'Date: Tue, 13 Oct 2026 12:00:00 +0000\r\n\r\n'
               f'Exact synthetic original {index}.\r\n').encode()
        with self.smtp() as client:
            assert client.sendmail('sender@example.net', ['invoices@example.test'], raw) == {}
        self.expected.append(raw)

    def verify(self):
        deadline = time.monotonic() + 15
        with imaplib.IMAP4_SSL('127.0.0.1', self.port(1993), ssl_context=self.context,
                              timeout=20) as client:
            assert client.login('invoices', PASSWORD)[0] == 'OK'
            while True:
                status, counts = client.select('INBOX', readonly=True)
                assert status == 'OK'
                if int(counts[0]) == len(self.expected):
                    break
                assert time.monotonic() < deadline, 'accepted mail did not become visible'
                time.sleep(.1)
            status, values = client.uid('SEARCH', None, 'ALL')
            assert status == 'OK'
            uids = values[0].split()
            assert len(uids) == len(self.expected)
            originals = []
            for uid in uids:
                status, values = client.uid('FETCH', uid, '(BODY.PEEK[])')
                assert status == 'OK'
                literals = [v[1] for v in values if isinstance(v, tuple)]
                assert len(literals) == 1, values
                originals.append(literals[0])
            assert sorted(originals) == sorted(self.expected), 'original bytes changed'

    def stop(self, name):
        docker('stop', '--time', '25', name, timeout=35)
        assert self.wait_exit(name, 2) == 0, self.all_logs(name)[-4096:]

    def clean_restart(self):
        self.stop(self.coordinator)
        for name in self.members:
            self.stop(name)
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE closed') == '2'
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '0'
        for name in self.members:
            previous = self.all_logs(name).count('listening with mutual TLS')
            docker('start', name)
            self.wait_log(name, 'listening with mutual TLS', previous)
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE epoch=2 AND NOT closed') == '2'
        previous = self.all_logs(self.coordinator).count('FRY SMTP listening')
        docker('start', self.coordinator)
        self.wait_log(self.coordinator, 'FRY SMTP listening', previous)
        self.verify()
        self.send(2)
        self.verify()

    def crash_check(self):
        docker('kill', '--signal', 'KILL', self.members[0])
        assert self.wait_exit(self.members[0], 5) != 0
        self.verify()  # The surviving copy remains readable over authenticated TLS.
        before = self.sql('SELECT count(*) FROM fry_receipt')
        client = self.smtp()
        try:
            try:
                client.sendmail('sender@example.net', ['invoices@example.test'],
                                b'Subject: unavailable member\r\n\r\nnot accepted\r\n')
            except smtplib.SMTPDataError as error:
                assert 400 <= error.smtp_code < 500, error.smtp_code
            except smtplib.SMTPServerDisconnected:
                pass  # No successful acknowledgement; retry remains necessary.
            else:
                raise AssertionError('accepted new mail without both durable copies')
        finally:
            client.close()
        assert self.sql('SELECT count(*) FROM fry_receipt') == before
        # Distinguish durable unclosed-generation fencing from an old live PG
        # session still holding its advisory owner lock. Each psql probe exits,
        # releasing a successfully acquired probe lock before restart.
        digest = hashlib.sha256(b'fry/member-owner/' + bytes.fromhex(f'{1:032x}')).digest()
        owner_key = int.from_bytes(digest[:8], 'big', signed=True)
        deadline = time.monotonic() + 5
        while self.sql(f'SELECT pg_try_advisory_lock({owner_key})') != 't':
            assert time.monotonic() < deadline, 'dead member advisory lock remained held'
            time.sleep(.1)
        assert self.sql("SELECT count(*) FROM fry_member_owner WHERE member='"
                        + f'{1:032x}' + "' AND epoch=2 AND NOT closed") == '1'
        owners_query = ('SELECT member,epoch,instance,closed FROM fry_member_owner '
                        'ORDER BY member,epoch')
        owners_before = self.sql(owners_query)
        previous_logs = self.all_logs(self.members[0])
        # This same container, configuration and volume already served mail.
        # Never clear owner flags, replace roots, or manufacture a safe takeover.
        docker('start', self.members[0])
        code = self.wait_exit(self.members[0], 15)
        assert code != 0, 'unclean generation was silently adopted'
        restart_logs = self.all_logs(self.members[0])
        assert restart_logs.startswith(previous_logs), 'restart log prefix unexpectedly changed'
        restart_logs = restart_logs[len(previous_logs):]
        assert 'listening with mutual TLS' not in restart_logs, restart_logs
        # Command errors are sanitized; prove refusal through unchanged owners.
        assert self.sql(owners_query) == owners_before, 'refused restart changed member owner rows'
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE epoch=2 AND NOT closed') == '2'
        assert self.sql('SELECT max(epoch) FROM fry_member_owner') == '2'
        self.stop(self.coordinator)
        self.stop(self.members[1])
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '1'

    def cleanup(self):
        errors = []
        for name in reversed(self.containers):
            try:
                result = run('docker', 'rm', '--force', '--volumes', name,
                             timeout=20, check=False)
                if result.returncode:
                    errors.append('container ' + name)
            except subprocess.TimeoutExpired:
                errors.append('container ' + name)
        for name in self.volumes:
            try:
                result = run('docker', 'volume', 'rm', name, timeout=15, check=False)
                if result.returncode:
                    errors.append('volume ' + name)
            except subprocess.TimeoutExpired:
                errors.append('volume ' + name)
        for network in reversed(self.created_networks):
            try:
                result = run('docker', 'network', 'rm', network, timeout=15, check=False)
                if result.returncode:
                    errors.append('network ' + network)
            except subprocess.TimeoutExpired:
                errors.append('network ' + network)
        if errors:
            print('Fixture cleanup could not remove: ' + ', '.join(errors), file=sys.stderr)
            if sys.exc_info()[0] is None:
                raise RuntimeError('Docker fixture cleanup incomplete')


def interrupted(signum, _frame):
    raise SystemExit(128 + signum)


def main():
    if len(sys.argv) != 2:
        raise SystemExit('Usage: python3 -m scripts.smoke.container_processes IMAGE_TAG')
    for name in ('docker', 'openssl'):
        if shutil.which(name) is None:
            raise SystemExit('Required command unavailable: ' + name)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    with tempfile.TemporaryDirectory(prefix='fry-container-smoke-') as directory:
        rehearsal = Rehearsal(sys.argv[1], Path(directory))
        try:
            rehearsal.setup()
            rehearsal.send(1)
            rehearsal.verify()
            rehearsal.clean_restart()
            rehearsal.crash_check()
            print('Container rehearsal passed: verified TLS SMTP/IMAP originals; '
                  'read-only nonroot coordinator and two mTLS members; retained-volume '
                  'clean restart; surviving-copy read; no acceptance with a member down; '
                  'unclean restart fenced. No AWS or production capacity claim.')
        finally:
            # Every resource name is unique to this invocation. Do not use global
            # Docker pruning or touch any preexisting container, network or volume.
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            rehearsal.cleanup()


if __name__ == '__main__':
    main()
