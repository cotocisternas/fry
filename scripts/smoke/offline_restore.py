#!/usr/bin/env python3
"""Synthetic offline, hot-only restore. No online/crash/cold/HA guarantee.

Every database, process, file and endpoint belongs to this invocation. Original
writers and their database/data are removed before replacement startup; copied
ownership rows are never edited to manufacture clean closure.
"""
from datetime import datetime, timezone
import hashlib
import imaplib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import stat
import smtplib
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import traceback

from scripts.lib import testdeps

ORG = '20000000000000000000000000000001'
PRIVATE_VALUES = ['fry-test-only']


def progress(message):
    print('[offline restore] ' + message, flush=True)


def run(*args, data=None, timeout=30, check=True, env=None):
    args = testdeps.native_command(args)
    result = testdeps.capture(args, data=data, timeout=timeout, env=env)
    if check and result.returncode:
        raise RuntimeError(Path(args[0]).name + ' failed: ' + result.stderr.decode(errors='replace')[-2048:])
    return result


def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        while chunk := stream.read(65536):
            h.update(chunk)
    return h.hexdigest()


def inventory(root):
    assert root.is_dir() and not root.is_symlink()
    result = {'.': {'type': 'directory', 'mode': stat.S_IMODE(root.stat().st_mode)}}
    for path in sorted(root.rglob('*')):
        assert not path.is_symlink(), 'backup contains a symlink'
        relative = path.relative_to(root).as_posix()
        assert '..' not in Path(relative).parts and not Path(relative).is_absolute()
        mode = stat.S_IMODE(path.stat().st_mode)
        if path.is_dir():
            result[relative] = {'type': 'directory', 'mode': mode}
        else:
            assert path.is_file(), 'backup contains a non-regular file'
            result[relative] = {'type': 'file', 'mode': mode, 'size': path.stat().st_size, 'sha256': digest(path)}
    return result


def verify_archive(root, manifest):
    actual = inventory(root)
    assert actual == manifest, 'backup file set/size/SHA256 mismatch before target startup'


class Relay:
    """Before enable: 451 at MAIL, never DATA. After enable: bounded local sink."""
    def __init__(self):
        self.socket = socket.socket()
        self.socket.bind(('127.0.0.1', 0))
        self.port = self.socket.getsockname()[1]
        self.socket.listen(4)
        self.socket.settimeout(.2)
        self.enabled = threading.Event()
        self.stop = threading.Event()
        self.messages, self.errors = [], []
        self.connection = None
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def serve(self):
        try:
            while not self.stop.is_set():
                try:
                    connection, peer = self.socket.accept()
                except socket.timeout:
                    continue
                except OSError:
                    if self.stop.is_set():
                        return
                    raise
                assert peer[0] == '127.0.0.1'
                self.connection = connection
                with connection:
                    connection.settimeout(10)
                    with connection.makefile('rb') as reader:
                        connection.sendall(b'220 synthetic.invalid\r\n')
                        for _ in range(32):
                            line = reader.readline(8193)
                            if not line:
                                break
                            assert len(line) <= 8192
                            verb = line.split(b' ', 1)[0].strip().upper()
                            if verb in (b'EHLO', b'HELO'):
                                connection.sendall(b'250 synthetic.invalid\r\n')
                            elif verb == b'MAIL' and not self.enabled.is_set():
                                connection.sendall(b'451 synthetic checkpoint: no handoff\r\n')
                            elif verb in (b'MAIL', b'RCPT', b'RSET', b'NOOP'):
                                connection.sendall(b'250 ok\r\n')
                            elif verb == b'DATA':
                                assert self.enabled.is_set(), 'DATA before checkpoint'
                                connection.sendall(b'354 continue\r\n')
                                parts, size = [], 0
                                while True:
                                    part = reader.readline(8193)
                                    assert part and len(part) <= 8192
                                    if part == b'.\r\n':
                                        break
                                    if part.startswith(b'..'):
                                        part = part[1:]
                                    size += len(part)
                                    assert size <= 1 << 20
                                    parts.append(part)
                                self.messages.append(b''.join(parts))
                                connection.sendall(b'250 synthetic handoff\r\n')
                            elif verb == b'QUIT':
                                connection.sendall(b'221 bye\r\n')
                                break
                            else:
                                raise AssertionError('unexpected relay command')
                self.connection = None
        except BaseException as error:
            if not self.stop.is_set():
                self.errors.append(type(error).__name__)

    def close(self):
        self.stop.set()
        self.socket.close()
        if self.connection:
            try:
                self.connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.thread.join(timeout=12)
        assert not self.thread.is_alive(), 'relay did not stop'
        assert not self.errors, 'relay fixture failed'


class Rehearsal:
    def __init__(self, root):
        self.root = root
        self.names, self.processes, self.logs = [], [], []
        self.databases = {}
        self.relay = None
        self.env = os.environ.copy()
        for key in list(self.env):
            if key.startswith('FRY_'):
                del self.env[key]
        self.admin_password, self.imap_password, self.smtp_password = [secrets.token_urlsafe(24) for _ in range(3)]
        PRIVATE_VALUES.extend((self.admin_password, self.imap_password, self.smtp_password))
        self.env.update(FRY_LOCAL_ADMIN_PASSWORD=self.admin_password,
                        FRY_LOCAL_IMAP_PASSWORD=self.imap_password)
        self.members = []
        self.coordinator = None
        self.round = 0

    def sql(self, query, db=None):
        return run('docker', 'exec', db or self.db, 'psql', '-X', '-U', 'postgres', '-d', 'fry_test',
                   '-v', 'ON_ERROR_STOP=1', '-Atc', query, timeout=10).stdout.decode().strip()

    def database(self, stage):
        session = testdeps.Session()
        name = session.container
        self.names.append(name)
        self.databases[name] = session
        self.db = name
        self.env['FRY_DATABASE_URL'] = session.database_url
        return name

    def cert(self, name, server):
        base = self.root / name
        run('openssl', 'req', '-new', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes',
            '-keyout', str(base.with_suffix('.key')), '-out', str(base.with_suffix('.csr')), '-subj', '/CN=' + name)
        extension = base.with_suffix('.ext')
        extension.write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n' +
                             ('extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1\n' if server else 'extendedKeyUsage=clientAuth\n'))
        run('openssl', 'x509', '-req', '-in', str(base.with_suffix('.csr')), '-CA', str(self.root / 'ca.crt'),
            '-CAkey', str(self.root / 'ca.key'), '-CAcreateserial', '-out', str(base.with_suffix('.crt')), '-days', '1', '-extfile', str(extension))
        public = run('openssl', 'x509', '-in', str(base.with_suffix('.crt')), '-pubkey', '-noout').stdout
        fingerprint = hashlib.sha256(run('openssl', 'pkey', '-pubin', '-outform', 'DER', data=public).stdout).hexdigest()
        return {'ca_file': str(self.root / 'ca.crt'), 'certificate_file': str(base.with_suffix('.crt')), 'key_file': str(base.with_suffix('.key'))}, fingerprint

    def prepare(self):
        endpoint = run('docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}').stdout.decode().strip()
        assert endpoint.startswith('unix://') and (not os.getenv('DOCKER_HOST') or os.getenv('DOCKER_HOST').startswith('unix://')), 'local Docker socket required'
        (self.root / 'bin').mkdir()
        run('go', 'build', '-o', str(self.root / 'bin') + '/', './cmd/fry', './cmd/fry-admin', './cmd/fry-member', './internal/testutil/cmd/member-maintenance', timeout=180)
        self.database('source')
        self.version = self.sql('SHOW server_version')
        run('openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes',
            '-keyout', str(self.root / 'ca.key'), '-out', str(self.root / 'ca.crt'), '-days', '1', '-subj', '/CN=synthetic-offline-restore',
            '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign')
        coordinator_tls, coordinator_key = self.cert('coordinator', False)
        self.relay = Relay()
        config = json.loads(Path('examples/local-admin.json').read_text())
        config.pop('data_directory')
        config.pop('member_capacity_bytes')
        config.update(listen='127.0.0.1:0', replica_tls=coordinator_tls)
        config['storage_lifecycle'] = dict(interval_seconds=1, work_timeout_seconds=10,
            max_actions=2, max_receipts=32, reclaim_hot=False)
        config['imap']['listen'] = '127.0.0.1:0'
        config['outbound']['submission'].update(listen='127.0.0.1:0', accounts=[])
        config['outbound']['relays'][0]['address'] = f'127.0.0.1:{self.relay.port}'
        reservation = socket.socket()
        reservation.bind(('127.0.0.1', 0))
        self.admin_port = reservation.getsockname()[1]
        reservation.close()
        self.origin = f'http://127.0.0.1:{self.admin_port}'
        config['admin'].update(listen=f'127.0.0.1:{self.admin_port}', origin=self.origin)
        self.source = self.root / 'original-data'
        self.source.mkdir(mode=0o700)
        self.member_configs = []
        for i, member in enumerate(config['members']):
            tls, fingerprint = self.cert('member-' + str(i), True)
            member.update(server_key=fingerprint, url='https://127.0.0.1:1')
            self.member_configs.append(dict(id=member['id'], domain=member['domain'], listen='127.0.0.1:0',
                data_directory=str(self.source / ('member-' + str(i))), max_message_bytes=1048576,
                capacity_bytes=64 << 20, segment_max_records=128, segment_max_bytes=20 << 20,
                max_in_flight=8, max_requests=16, timeout_seconds=20,
                tls=tls, coordinator_keys=[coordinator_key]))
        self.config = config
        self.path = self.root / 'coordinator.json'
        self.write_config()
        run(str(self.root / 'bin/fry'), '-config', str(self.path), '-init-only', env=self.env)

    def write_config(self):
        self.path.write_text(json.dumps(self.config))

    def start(self, binary, config, markers):
        self.round += 1
        path = self.root / f'process-{self.round}.log'
        stream = path.open('wb')
        self.logs.append(stream)
        process = subprocess.Popen([str(self.root / 'bin' / binary), '-config', str(config)], stdout=stream, stderr=stream, env=self.env)
        self.processes.append(process)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            text = path.read_text()
            assert process.poll() is None, binary + ' exited during startup: ' + text[-2048:]
            if all(marker in text for marker in markers):
                return process, text
            time.sleep(.1)
        raise AssertionError(binary + ' startup timeout')

    def start_member(self, i):
        path = self.root / f'member-{i}.json'
        path.write_text(json.dumps(self.member_configs[i]))
        process, text = self.start('fry-member', path, ['listening with mutual TLS'])
        port = int(re.search(r'listening with mutual TLS on 127\.0\.0\.1:(\d+)', text)[1])
        self.member_configs[i]['listen'] = f'127.0.0.1:{port}'
        self.config['members'][i]['url'] = f'https://127.0.0.1:{port}'
        return process

    def start_all(self):
        self.members = [self.start_member(i) for i in range(2)]
        self.start_coordinator()

    def start_coordinator(self):
        self.write_config()
        self.coordinator, text = self.start('fry', self.path, ['FRY SMTP listening', 'FRY IMAP listening', 'FRY submission listening', 'FRY administration'])
        self.smtp_port = int(re.search(r'SMTP listening on 127\.0\.0\.1:(\d+)', text)[1])
        self.imap_port = int(re.search(r'IMAP listening on 127\.0\.0\.1:(\d+)', text)[1])
        self.submission_port = int(re.search(r'submission listening on 127\.0\.0\.1:(\d+)', text)[1])

    @staticmethod
    def stop_process(process):
        process.terminate()
        try:
            code = process.wait(timeout=25)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            raise AssertionError('clean shutdown timed out')
        assert code == 0, 'clean shutdown failed'

    def stop_all(self, expected_closed_owners=2):
        self.stop_process(self.coordinator)
        for member in self.members:
            self.stop_process(member)
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '0'
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE closed') == str(expected_closed_owners), 'unexpected closed owner history'
        assert self.sql('SELECT count(*) FROM fry_read_pin') == '0'
        assert self.sql('SELECT count(*) FROM fry_member_read WHERE NOT closed') == '0'
        assert self.sql("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend' AND pid<>pg_backend_pid()") == '0', 'application DB sessions remain'
        assert self.sql('SELECT count(*) FROM fry_pack') == '0', 'hot-only fixture unexpectedly contains packs'

    def cli(self, command, username='restored-user', password=None, domains=None):
        env = self.env.copy()
        env['FRY_ADMIN_PASSWORD'] = self.admin_password
        if password is not None:
            env['FRY_NEW_SUBMISSION_PASSWORD'] = password
        args = [str(self.root / 'bin/fry-admin'), '-origin', self.origin, '-organization', ORG, '-name', username]
        if domains is not None:
            args += ['-authorized-domains', ','.join(domains)]
        args.append(command)
        result = run(*args, env=env, timeout=15)
        value = json.loads(result.stdout)
        return testdeps.submission_result(value) if command.startswith('submission-') else value

    def incoming(self, body):
        with smtplib.SMTP('127.0.0.1', self.smtp_port, timeout=10) as client:
            assert client.sendmail('sender@source.test', ['invoices@example.test'], body) == {}

    def imap(self):
        client = imaplib.IMAP4('127.0.0.1', self.imap_port, timeout=15)
        assert client.login('invoices', self.imap_password)[0] == 'OK'
        return client

    def mailbox_snapshot(self, expected, folder='INBOX'):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            with self.imap() as client:
                assert client.select(folder)[0] == 'OK'
                ids = client.uid('SEARCH', None, 'ALL')[1][0].split()
                if len(ids) != len(expected):
                    time.sleep(.1)
                    continue
                result = {}
                for uid in ids:
                    status, values = client.uid('FETCH', uid, '(UID FLAGS INTERNALDATE BODY.PEEK[])')
                    assert status == 'OK'
                    literal = [item for item in values if isinstance(item, tuple)]
                    assert len(literal) == 1 and literal[0][1] in expected, 'restored original differs'
                    metadata, body = literal[0]
                    result[uid.decode()] = {'sha256': hashlib.sha256(body).hexdigest(),
                        'flags': sorted(v.decode() for v in imaplib.ParseFlags(metadata)),
                        'internal_date': re.search(rb'INTERNALDATE "([^"]+)"', metadata)[1].decode()}
                assert sorted(v['sha256'] for v in result.values()) == sorted(hashlib.sha256(b).hexdigest() for b in expected)
                return {'uidvalidity': client.response('UIDVALIDITY')[1][0].decode(),
                        'uidnext': client.response('UIDNEXT')[1][0].decode(), 'messages': result}
        raise AssertionError('mailbox publication/restore timed out')

    def authority_snapshot(self):
        tables = self.sql("SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'fry_%' ORDER BY tablename").splitlines()
        assert all(re.fullmatch(r'fry_[a-z0-9_]+', table) for table in tables)
        sequences = self.sql("SELECT sequencename FROM pg_sequences WHERE schemaname='public' ORDER BY sequencename").splitlines()
        assert all(re.fullmatch(r'fry_[a-z0-9_]+', name) for name in sequences)
        return {'tables': {table: hashlib.sha256(self.sql(f"SELECT COALESCE(jsonb_agg(v ORDER BY v::text),'[]'::jsonb) FROM (SELECT to_jsonb(t) v FROM {table} t) q").encode()).hexdigest() for table in tables},
                'sequences': {name: self.sql(f"SELECT last_value::text||':'||is_called::text FROM public.{name}") for name in sequences}}

    def download(self, receipt, expected, replica):
        output = self.root / f'verify-{replica}-{receipt}.eml'
        env = self.env.copy()
        env['FRY_ADMIN_PASSWORD'] = self.admin_password
        run(str(self.root / 'bin/fry-admin'), '-origin', self.origin, '-organization', ORG,
            '-id', receipt, '-out', str(output), 'download', env=env, timeout=20)
        assert output.read_bytes() == expected, 'independent replica original differs'
        output.unlink()

    def remove_database(self, name):
        # Session.close waits for Testcontainers cleanup and verifies absence.
        # Repeated cleanup of a source destroyed before recovery is harmless.
        self.databases[name].close()

    def member_maintenance(self, member, action):
        peer = next(config for config in self.config['members'] if config['id'] == member['id'])
        path = self.root / ('maintenance-' + member['id'] + '.json')
        path.write_text(json.dumps(dict(peer, tls=self.config['replica_tls'])))
        result = run(str(self.root / 'bin/member-maintenance'), '-config', str(path), action,
                     env=self.env, timeout=90)
        return json.loads(result.stdout)

    def rotate_members(self):
        # The native helper exercises the generated private gRPC transport with
        # the same coordinator certificate, release identity and server pin.
        targets = self.sql("SELECT jsonb_agg(to_jsonb(t) ORDER BY member,attempt) FROM fry_target t")
        for member in self.member_configs:
            result = self.member_maintenance(member, 'rotate')
            identity, after = result['before'], result['after']
            assert identity['member'] == member['id'], 'rotation identity lookup failed'
            assert result['states'] == ['complete', 'complete', 'complete'], 'rotation/status/replay failed'
            assert after['incarnation'] != identity['incarnation'], 'rotation did not change current file'
            assert after['member'] == identity['member'] and after['owner_epoch'] == identity['owner_epoch'], 'rotation changed member/process identity'
        assert self.sql("SELECT jsonb_agg(to_jsonb(t) ORDER BY member,attempt) FROM fry_target t") == targets, 'rotation changed historical targets'

    def reclaim_sealed_members(self):
        for member in self.member_configs:
            source_directory = self.sql("SELECT directory FROM fry_member_file WHERE state='sealed' AND member='" + member['id'] + "'")
            assert source_directory == '.', 'fixture must reclaim the first sealed source'
            source = Path(member['data_directory']) / source_directory / 'data.fry'
            assert source.is_file(), 'sealed source absent before reclamation'
            result = self.member_maintenance(member, 'reclaim-sealed')
            identity, after = result['before'], result['after']
            assert identity['member'] == member['id'], 'sealed reclamation identity lookup failed'
            assert result['states'] == ['complete', 'complete', 'complete'], 'sealed reclamation/status/replay failed'
            assert after == identity, 'sealed reclamation changed active writer identity'
            assert not source.exists(), 'retired sealed source remains on disk'

    def check_capacity(self, originals):
        assert self.sql("SELECT count(*) FROM fry_member_profile WHERE enabled AND max_records=128 AND max_file_bytes=20971520 AND max_bytes=67108864 AND lane_version=1 AND lane_count=2") == '2', 'bounded profiles changed'
        assert self.sql("SELECT count(*) FROM fry_target WHERE NOT bytes_removed AND capacity_settled AND capacity_reserved_bytes IS NOT NULL AND capacity_lane IS NOT NULL AND locator IS NOT NULL") == str(2 * originals), 'target capacity was not settled'
        assert self.sql(f"""SELECT count(*) FROM fry_member_file f
            CROSS JOIN LATERAL (SELECT count(*) n,sum(record_ceiling) rc,sum(byte_ceiling) bc,
                sum(reserved_records) records,sum(reserved_bytes) bytes
                FROM fry_member_file_lane l WHERE l.member=f.member AND l.file_id=f.file_id) usage
            WHERE f.state IN ('current','sealed') AND usage.n=2
                AND usage.rc=128 AND usage.bc=20971520-64
                AND usage.records=(SELECT count(*) FROM fry_target t WHERE t.member=f.member AND t.incarnation=f.file_id AND NOT t.bytes_removed)
                AND usage.bytes=COALESCE((SELECT sum(t.capacity_reserved_bytes) FROM fry_target t
                    WHERE t.member=f.member AND t.incarnation=f.file_id AND NOT t.bytes_removed),0)
                AND ((f.state='current' AND f.charged_bytes=20971520 AND usage.records={originals - 4})
                    OR (f.state='sealed' AND f.charged_bytes=64+usage.bytes AND f.sealed_records=usage.records))""") == '6', 'bounded file occupancy changed'
        assert self.sql("SELECT count(*) FROM fry_member_file WHERE state='sealed'") == '4', 'sealed catalog changed'
        assert self.sql("SELECT sum(sealed_records) FROM fry_member_file WHERE state='sealed'") == '8', 'sealed census changed'
        assert self.sql("SELECT count(*) FROM fry_hot_rewrite WHERE kind='rotate' AND state='complete'") == '4', 'rotation history changed'
        assert self.sql("SELECT count(*) FROM fry_hot_rewrite WHERE kind='sealed-reclaim' AND state='removed'") == '2', 'sealed reclamation history changed'
        assert self.sql("SELECT count(*) FROM fry_member_file WHERE state='removed' AND charged_bytes=0") == '2', 'reclaimed source accounting changed'
        assert self.sql("SELECT count(*) FROM fry_target WHERE bytes_removed") == '2', 'expired physical-copy proof changed'
        assert self.sql("SELECT count(*) FROM fry_receipt WHERE lifecycle='removed'") == '1', 'expired receipt was resurrected'
        assert self.sql("""SELECT count(*) FROM fry_member_file_lane l
            JOIN fry_member_file f USING(member,file_id)
            CROSS JOIN LATERAL (SELECT count(*) n,COALESCE(sum(capacity_reserved_bytes),0) bytes
                FROM fry_target t WHERE t.member=l.member AND t.incarnation=l.file_id
                    AND t.capacity_lane=l.lane AND NOT t.bytes_removed) targets
            WHERE f.state IN ('current','sealed') AND l.reserved_records=targets.n AND l.reserved_bytes=targets.bytes""") == '12', 'lane target census changed'
        roots = {member['id']: Path(member['data_directory']) for member in self.member_configs}
        for row in self.sql("SELECT member||':'||directory||':'||charged_bytes FROM fry_member_file WHERE state='sealed'").splitlines():
            member, directory, length = row.split(':')
            assert directory == '.' or re.fullmatch(r'generation-[0-9a-f]+', directory), 'invalid sealed directory'
            assert (roots[member] / directory / 'data.fry').stat().st_size == int(length), 'sealed physical charge changed'
        for row in self.sql("SELECT member||':'||directory FROM fry_member_file WHERE state='removed'").splitlines():
            member, directory = row.split(':')
            assert not (roots[member] / directory / 'data.fry').exists(), 'removed source bytes reappeared'

    def execute(self):
        self.prepare()
        self.start_all()
        originals = [f'Subject: offline restore {i}\r\n\r\nExact synthetic original {i}.\r\n'.encode() for i in range(3)]
        for body in originals:
            self.incoming(body)
        disposable = b'Subject: expire before backup\r\n\r\nSynthetic original that must not survive reclamation.\r\n'
        self.incoming(disposable)
        self.mailbox_snapshot(originals + [disposable])
        self.rotate_members()
        with self.imap() as client:
            assert client.select('INBOX')[0] == 'OK'
            assert client.uid('STORE', '4', '+FLAGS.SILENT', '(\\Deleted)')[0] == 'OK'
            assert client.expunge()[0] == 'OK'
        disposable_sha = hashlib.sha256(disposable).hexdigest()
        # Simulate only passage of recovery time. Real expunge, lifecycle expiry
        # and member reclamation establish removal authority; no state/closure
        # proof or ownership rows are synthesized by this fixture.
        self.sql("UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 second' "
                 "WHERE kind='recovery' AND receipt=(SELECT id FROM fry_receipt WHERE encode(sha256,'hex')='" + disposable_sha + "')")
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if self.sql("SELECT count(*) FROM fry_receipt WHERE lifecycle='retiring'") == '1':
                break
            time.sleep(.1)
        else:
            raise AssertionError('expired fixture receipt did not retire')
        self.reclaim_sealed_members()
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if self.sql("SELECT count(*) FROM fry_receipt WHERE lifecycle='removed'") == '1':
                break
            time.sleep(.1)
        else:
            raise AssertionError('reclaimed fixture receipt did not finish cleanup')
        self.mailbox_snapshot(originals)
        with self.imap() as client:
            assert client.create('Archive')[0] == 'OK'
            assert client.subscribe('Archive')[0] == 'OK'
            assert client.select('INBOX')[0] == 'OK'
            assert client.uid('STORE', '1', '+FLAGS.SILENT', '(\\Seen \\Flagged)')[0] == 'OK'
            appended = b'Subject: dated APPEND\r\n\r\nExact dated folder original.\r\n'
            assert client.append('Archive', '(\\Seen)', datetime(2020, 2, 3, 4, 5, 6, tzinfo=timezone.utc), appended)[0] == 'OK'
        self.rotate_members()
        self.cli('submission-account-create', password=self.smtp_password, domains=['stale.test'])
        self.cli('submission-policy-set', domains=['example.test'])
        accounts = self.cli('submission-accounts')
        assert len(accounts) == 1 and accounts[0]['version'] == 2
        expected_mail = self.mailbox_snapshot(originals)
        expected_archive = self.mailbox_snapshot([appended], 'Archive')
        outbound = b'From: robot@example.test\r\nSubject: pending restore\r\n\r\nNever handed off before backup.\r\n'
        with smtplib.SMTP('127.0.0.1', self.submission_port, timeout=10) as client:
            client.login('restored-user', self.smtp_password)
            assert client.sendmail('robot@example.test', ['destination@example.test'], outbound) == {}
        # Stop immediately after a confirmed retry clears the three-minute
        # claim lease, before its next due opportunity. Never rewrite lease debt.
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if self.sql("SELECT count(*) FROM fry_outbound WHERE state='queued' AND lease_until IS NULL AND response_code=451 AND available_at>clock_timestamp()+interval '2 seconds'") == '1':
                break
            time.sleep(.05)
        else:
            raise AssertionError('no acknowledged pre-DATA retry checkpoint')
        progress('stopping every original writer and checking closure')
        self.stop_all()
        assert not self.relay.messages and not self.relay.errors
        assert self.sql("SELECT count(*) FROM fry_outbound WHERE state='queued' AND lease_until IS NULL") == '1'
        source_db = self.db
        bodies = {hashlib.sha256(body).hexdigest(): body for body in originals + [appended, outbound]}
        receipt_bodies = {}
        for pair in self.sql("SELECT id||':'||encode(sha256,'hex') FROM fry_receipt WHERE lifecycle<>'removed' ORDER BY id").splitlines():
            receipt, sha = pair.split(':')
            receipt_bodies[receipt] = bodies[sha]
        assert len(receipt_bodies) == 5
        self.check_capacity(5)
        state = self.authority_snapshot()
        backup = self.root / 'backup'
        backup.mkdir(mode=0o700)
        dump = run('docker', 'exec', source_db, 'pg_dump', '-U', 'postgres', '-d', 'fry_test', '--format=custom', '--no-owner', '--no-acl', timeout=30).stdout
        (backup / 'database.dump').write_bytes(dump)
        original_files = inventory(self.source)
        shutil.copytree(self.source, backup / 'members', copy_function=shutil.copy2)
        assert inventory(backup / 'members') == original_files, 'member backup differs from frozen source'
        configs = backup / 'creation-config'
        configs.mkdir(mode=0o700)
        for path in (self.path, self.root / 'member-0.json', self.root / 'member-1.json'):
            shutil.copy2(path, configs / path.name)
        manifest = inventory(backup)
        assert any(path.startswith('members/') for path in manifest)
        (self.root / 'manifest.json').write_text(json.dumps(manifest, sort_keys=True))
        verify_archive(backup, manifest)
        # Exercise rejection on disposable copies; preserve the good archive.
        for failure in ('missing', 'corrupt'):
            trial = self.root / ('negative-' + failure)
            shutil.copytree(backup, trial)
            target = trial / 'database.dump'
            if failure == 'missing':
                target.unlink()
            else:
                target = trial / next(path for path, meta in manifest.items() if path.startswith('members/') and meta['type'] == 'file' and meta['size'] > 0)
                with target.open('r+b') as stream:
                    first = stream.read(1)
                    stream.seek(0)
                    stream.write(bytes([first[0] ^ 1]))
            try:
                verify_archive(trial, manifest)
            except AssertionError:
                pass
            else:
                raise AssertionError('negative archive passed verification')
            shutil.rmtree(trial)
        progress('removing original database and member roots before fresh restore')
        self.remove_database(source_db)
        shutil.rmtree(self.source)
        assert not self.source.exists() and all(p.poll() == 0 for p in self.processes)
        self.database('restored')
        assert self.sql('SHOW server_version') == self.version
        verify_archive(backup, manifest)
        run('docker', 'exec', '-i', self.db, 'pg_restore', '-U', 'postgres', '-d', 'fry_test', '--exit-on-error', '--single-transaction', '--no-owner', '--no-acl', data=(backup / 'database.dump').read_bytes(), timeout=30)
        assert self.authority_snapshot() == state, 'logical database state changed during restore'
        restored = self.root / 'restored-data'
        shutil.copytree(backup / 'members', restored, copy_function=shutil.copy2)
        assert inventory(restored) == inventory(backup / 'members')
        for i in range(2):
            self.member_configs[i]['data_directory'] = str(restored / ('member-' + str(i)))
        # No -init: preserved member IDs/incarnations and catalog remain authority.
        self.start_all()
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE epoch=2 AND NOT closed') == '2'
        assert self.mailbox_snapshot(originals) == expected_mail
        # Fallback must not conceal a missing restored replica.
        for stopped, survivor in ((0, 1), (1, 0)):
            self.stop_process(self.members[stopped])
            assert self.mailbox_snapshot(originals) == expected_mail, f'replica {survivor} INBOX verification failed'
            assert self.mailbox_snapshot([appended], 'Archive') == expected_archive, f'replica {survivor} Archive verification failed'
            for receipt, body in receipt_bodies.items():
                self.download(receipt, body, survivor)
            self.members[stopped] = self.start_member(stopped)
            # The remote adapter captures the owner's generation at startup.
            # Refresh normally while BOTH members are up before the next test.
            self.stop_process(self.coordinator)
            self.start_coordinator()
        restored_accounts = self.cli('submission-accounts')
        assert restored_accounts == accounts
        self.relay.enabled.set()
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if self.sql("SELECT count(*) FROM fry_outbound WHERE state='handed-off'") == '1':
                break
            assert not self.relay.errors
            time.sleep(.2)
        else:
            raise AssertionError('restored queued handoff timeout')
        assert self.relay.messages == [outbound], 'synthetic queued bytes differ or duplicate'
        # Fresh AUTH proves the persisted secret survives without any seed user.
        with smtplib.SMTP('127.0.0.1', self.submission_port, timeout=10) as client:
            assert client.login('restored-user', self.smtp_password)[0] == 235
            assert client.mail('robot@stale.test')[0] == 553, 'stale bootstrap sender policy returned'
            assert client.mail('robot@example.test')[0] == 250
            assert client.rset()[0] == 250
        new_body = b'Subject: after restore\r\n\r\nFresh accepted original.\r\n'
        self.incoming(new_body)
        final_mail = self.mailbox_snapshot(originals + [new_body])
        assert final_mail['uidvalidity'] == expected_mail['uidvalidity']
        assert int(final_mail['uidnext']) == int(expected_mail['uidnext']) + 1
        assert final_mail['messages'][str(int(expected_mail['uidnext']))]['sha256'] == hashlib.sha256(new_body).hexdigest()
        # All authoritative state matched before startup, including Archive's UID,
        # flags/date, audit, security, refs/work and both original byte locators.
        with self.imap() as client:
            assert client.select('Archive')[0] == 'OK'
            status, values = client.uid('FETCH', '1', '(UID FLAGS INTERNALDATE BODY.PEEK[])')
            literal = [item for item in values if isinstance(item, tuple)]
            assert status == 'OK' and len(literal) == 1 and literal[0][1] == appended
            # IMAP permits a space-padded day; the wire encoder uses that form.
            # Compare the timestamp and parsed flags, not one date spelling.
            raw_date = re.search(rb'INTERNALDATE "([^"]+)"', literal[0][0])[1].decode()
            assert datetime.strptime(raw_date.strip(), '%d-%b-%Y %H:%M:%S %z') == datetime(2020, 2, 3, 4, 5, 6, tzinfo=timezone.utc), 'restored APPEND timestamp changed'
            assert b'\\Seen' in imaplib.ParseFlags(literal[0][0]), 'restored Archive flags changed'
        self.stop_process(self.coordinator)
        for member in self.members:
            self.stop_process(member)
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '0'
        assert self.sql('SELECT count(*) FROM fry_read_pin') == '0'
        self.check_capacity(6)
        progress('restore checks passed; removing all temporary resources')
        return {'passed': True, 'profile': 'synthetic-offline-hot-only-restore', 'backed_up_originals_verified': 5, 'new_originals_verified': 1,
                'independent_hot_replicas_verified': 2, 'restored_sender_policy_enforced': True, 'negative_archives_rejected': 2,
                'queued_before_any_relay_acceptance': True, 'synthetic_restored_handoffs': 1,
                'postgres_version': self.version,
                'bounded_profiles_and_settled_occupancy_verified': True,
                'capacity_lane_version': 1, 'capacity_lanes_per_file': 2,
                'sealed_files_verified': 4, 'rotations_verified': 4, 'rotation_replay_and_fixed_targets_verified': True,
                'sealed_reclaims_verified': 2, 'expired_originals_absent_after_restore': 1,
                'segment_max_records': 128, 'segment_max_bytes': 20 << 20, 'member_capacity_bytes': 64 << 20,
                'backup_files': sum(meta['type'] == 'file' for meta in manifest.values()),
                'backup_directories': sum(meta['type'] == 'directory' for meta in manifest.values()),
                'source_sha256': {name: digest(Path(name)) for name in ('scripts/smoke/offline_restore.py', 'scripts/smoke.sh', 'db/schema/receiptstore.sql', 'db/schema/admin.sql')},
                'binary_sha256': {name: digest(self.root / 'bin' / name) for name in ('fry', 'fry-admin', 'fry-member', 'member-maintenance')}}

    def cleanup(self):
        errors = []
        for process in reversed(self.processes):
            try:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=25)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
                        errors.append('process required forced cleanup')
                if process.returncode != 0:
                    errors.append('process exited unsuccessfully')
            except BaseException:
                errors.append('process cleanup failed')
        for stream in self.logs:
            try:
                stream.close()
            except BaseException:
                errors.append('log cleanup failed')
        if self.relay:
            try:
                self.relay.close()
            except BaseException:
                errors.append('relay cleanup failed')
        for name in self.names:
            try:
                self.remove_database(name)
            except BaseException:
                errors.append('database cleanup failed')
        assert not errors, '; '.join(errors)


def main():
    os.umask(0o077)
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, lambda number, frame: (_ for _ in ()).throw(SystemExit(128 + number)))
    root = Path(tempfile.mkdtemp(prefix='fry-offline-restore-'))
    fixture = Rehearsal(root)
    report = None
    try:
        report = fixture.execute()
    finally:
        for signum in (signal.SIGINT, signal.SIGTERM):
            signal.signal(signum, signal.SIG_IGN)
        try:
            fixture.cleanup()
        finally:
            shutil.rmtree(root)
            assert not root.exists(), 'temporary backup/data cleanup incomplete'
    # Success is emitted only after processes, databases and backup roots vanish.
    print(json.dumps(report, sort_keys=True), flush=True)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Only synthetic resources are used; avoid printing raw server logs/DSNs.
        message = str(error)
        for private in PRIVATE_VALUES:
            message = message.replace(private, '<REDACTED>')
        frame = traceback.extract_tb(error.__traceback__)[-1]
        location = f'{Path(frame.filename).name}:{frame.lineno} ({frame.name})'
        print('Offline restore rehearsal failed at ' + location + ': ' + type(error).__name__ + ': ' + message.split('\n')[0][:300], file=sys.stderr)
        raise SystemExit(1)
