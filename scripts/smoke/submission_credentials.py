#!/usr/bin/env python3
"""Bounded real CLI/process SMTP credential smoke; no live mail or user storage."""
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import smtplib
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time

from scripts.lib import testdeps

ORG = '20000000000000000000000000000001'
OTHER = '20000000000000000000000000000002'


def progress(message):
    print('[submission credential smoke] ' + message, flush=True)


def run(*args, timeout=30, check=True, env=None):
    args = testdeps.native_command(args)
    result = testdeps.capture(args, timeout=timeout, env=env,
                              progress=lambda: progress('waiting for ' + ' '.join(args[:2])))
    if check and result.returncode:
        raise RuntimeError(args[0] + ' failed: ' + result.stderr.decode(errors='replace')[-4096:])
    return result


class Relay:
    """One bounded connection at a time; withhold first final250 until revoked."""
    def __init__(self):
        self.listener = socket.socket()
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen(4)
        self.listener.settimeout(.2)
        self.port = self.listener.getsockname()[1]
        self.release = threading.Event()
        self.received = threading.Event()
        self.stopped = threading.Event()
        self.messages = []
        self.errors = []
        self.connection = None
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def serve(self):
        try:
            while not self.stopped.is_set():
                try:
                    connection, address = self.listener.accept()
                except socket.timeout:
                    continue
                except OSError:
                    if self.stopped.is_set():
                        return
                    raise
                assert address[0] == '127.0.0.1'
                self.connection = connection
                with connection:
                    connection.settimeout(15)
                    with connection.makefile('rb') as reader:
                        connection.sendall(b'220 fixture.invalid synthetic relay\r\n')
                        for _ in range(32):
                            line = reader.readline(8193)
                            if not line:
                                break
                            assert len(line) <= 8192
                            verb = line.split(b' ', 1)[0].strip().upper()
                            if verb in (b'EHLO', b'HELO'):
                                connection.sendall(b'250 fixture.invalid\r\n')
                            elif verb in (b'MAIL', b'RCPT', b'RSET', b'NOOP'):
                                connection.sendall(b'250 ok\r\n')
                            elif verb == b'DATA':
                                connection.sendall(b'354 send synthetic original\r\n')
                                chunks = []
                                size = 0
                                while True:
                                    part = reader.readline(8193)
                                    assert part and len(part) <= 8192
                                    if part == b'.\r\n':
                                        break
                                    if part.startswith(b'..'):
                                        part = part[1:]
                                    chunks.append(part)
                                    size += len(part)
                                    assert size <= 1 << 20, 'relay input exceeded fixture cap'
                                self.messages.append(b''.join(chunks))
                                self.received.set()
                                assert self.release.wait(30), 'relay gate deadline expired'
                                connection.sendall(b'250 synthetic handoff complete\r\n')
                            elif verb == b'QUIT':
                                connection.sendall(b'221 bye\r\n')
                                break
                            else:
                                raise AssertionError('unexpected relay command')
                self.connection = None
        except BaseException as error:
            if not self.stopped.is_set():
                self.errors.append(str(error))

    def close(self):
        self.stopped.set()
        self.release.set()
        self.listener.close()
        if self.connection:
            try:
                self.connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.thread.join(timeout=3)
        assert not self.thread.is_alive(), 'fixture relay failed to stop'


class Smoke:
    def __init__(self, root):
        self.root = root
        self.name = None
        self.database = None
        self.old = secrets.token_urlsafe(24)
        self.new = secrets.token_urlsafe(24)
        self.dynamic_password = secrets.token_urlsafe(24)
        self.admin_password = secrets.token_urlsafe(24)
        self.imap_password = secrets.token_urlsafe(24)
        self.process = None
        self.log_file = None
        self.relay = None
        self.sessions = []
        self.round = 0
        self.expected = []
        self.report = {'profile': 'local-process-synthetic-submission-credentials', 'passed': False}
        self.env = os.environ.copy()
        for key in list(self.env):
            if key.startswith('FRY_'):
                del self.env[key]

    def redact(self, text):
        for value in [self.old, self.new, self.dynamic_password, self.admin_password, self.imap_password, 'fry-test-only']:
            text = text.replace(value, '<REDACTED>')
        return text

    def sql(self, query):
        return run('docker', 'exec', self.name, 'psql', '-U', 'postgres', '-d', 'fry_test',
                   '-v', 'ON_ERROR_STOP=1', '-Atc', query, timeout=10).stdout.decode().strip()

    def prepare(self):
        endpoint = run('docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}').stdout.decode().strip()
        assert endpoint.startswith('unix://'), 'local Docker Unix socket required'
        assert not os.environ.get('DOCKER_HOST') or os.environ['DOCKER_HOST'].startswith('unix://')
        progress('building actual fry and fry-admin binaries')
        (self.root / 'bin').mkdir()
        run('go', 'build', '-o', str(self.root / 'bin') + '/', './cmd/fry', './cmd/fry-admin', timeout=180)
        self.database = testdeps.Session()
        self.name = self.database.container
        self.env.update({'FRY_DATABASE_URL': self.database.database_url,
                         'FRY_LOCAL_ADMIN_PASSWORD': self.admin_password,
                         'FRY_LOCAL_IMAP_PASSWORD': self.imap_password})
        self.relay = Relay()
        certificate, key = self.root / 'smtp.pem', self.root / 'smtp.key'
        run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
            '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1',
            '-keyout', str(key), '-out', str(certificate))
        self.tls = ssl.create_default_context(cafile=str(certificate))
        reservations = []
        ports = []
        try:
            for _ in range(3):
                listener = socket.socket()
                listener.bind(('127.0.0.1', 0))
                reservations.append(listener)
                ports.append(listener.getsockname()[1])
            self.smtp_port, self.submission_port, self.admin_port = ports
        finally:
            for listener in reservations:
                listener.close()
        config = json.loads(Path('examples/local-admin.json').read_text())
        config.update(listen='127.0.0.1:' + str(self.smtp_port), data_directory=str(self.root / 'data'),
                      member_capacity_bytes=64 << 20)
        config['imap']['listen'] = '127.0.0.1:0'
        config['outbound']['relays'][0]['address'] = '127.0.0.1:' + str(self.relay.port)
        config['outbound']['submission']['listen'] = '127.0.0.1:' + str(self.submission_port)
        config['outbound']['submission'].update(transport='starttls-required',
                                               tls_cert_file=str(certificate), tls_key_file=str(key))
        config['admin']['listen'] = '127.0.0.1:' + str(self.admin_port)
        config['admin']['origin'] = 'http://127.0.0.1:' + str(self.admin_port)
        self.config = self.root / 'config.json'
        self.config.write_text(json.dumps(config))
        self.origin = config['admin']['origin']

    def start(self, bootstrap, initialize=False):
        self.round += 1
        self.log_path = self.root / ('server-' + str(self.round) + '.log')
        self.log_file = self.log_path.open('wb')
        env = self.env.copy()
        if bootstrap is not None:
            env['FRY_LOCAL_SMTP_PASSWORD'] = bootstrap
        else:
            env.pop('FRY_LOCAL_SMTP_PASSWORD', None)
        args = [str(self.root / 'bin/fry'), '-config', str(self.config)]
        if initialize:
            args.append('-init')
        self.process = subprocess.Popen(args, stdout=self.log_file, stderr=self.log_file, env=env)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            assert self.process.poll() is None, 'FRY exited: ' + self.log_path.read_text()[-4096:]
            log = self.log_path.read_text()
            if 'FRY SMTP listening' in log and 'FRY submission listening' in log and 'FRY administration' in log:
                return
            time.sleep(.1)
        raise AssertionError('FRY listener startup deadline: ' + self.log_path.read_text()[-4096:])

    def stop(self):
        for client in self.sessions:
            client.close()
        self.sessions.clear()
        if self.process:
            self.process.terminate()
            try:
                code = self.process.wait(timeout=25)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
                raise AssertionError('FRY did not close cleanly within its process bound')
            finally:
                self.process = None
                self.log_file.close()
                self.log_file = None
            assert code == 0, 'FRY shutdown failed: ' + self.log_path.read_text()[-4096:]
            assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '0'

    def cli(self, command, organization=ORG, name='robot', enabled=None, password=None,
            success=True, senders=None, domains=None, extra=()):
        env = self.env.copy()
        env['FRY_ADMIN_PASSWORD'] = self.admin_password
        if password is not None:
            env['FRY_NEW_SUBMISSION_PASSWORD'] = password
        args = [str(self.root / 'bin/fry-admin'), '-origin', self.origin,
                '-organization', organization, '-name', name]
        if enabled is not None:
            args.append('-enabled=' + str(enabled).lower())
        if senders is not None:
            args.extend(('-authorized-senders', ','.join(senders)))
        if domains is not None:
            args.extend(('-authorized-domains', ','.join(domains)))
        args.extend(extra)
        result = run(*args, command, timeout=15, check=False, env=env)
        output = result.stdout.decode(errors='replace') + result.stderr.decode(errors='replace')
        assert all(value not in output for value in [self.old, self.new, self.dynamic_password, self.admin_password]), 'CLI leaked a password'
        assert (result.returncode == 0) == success, 'unexpected CLI result: ' + output[-2048:]
        if not success:
            return None
        value = json.loads(result.stdout)
        return testdeps.submission_result(value) if command.startswith('submission-') else value

    def account(self, enabled, version=None, name='robot', senders=None, domains=None):
        values = self.cli('submission-accounts')
        matches = [value for value in values if value['username'] == name]
        assert len(matches) == 1
        value = matches[0]
        assert set(value) == {'username', 'organization', 'enabled', 'version', 'authorized_senders', 'authorized_domains'}
        assert value['organization'] == ORG and value['enabled'] is enabled
        if senders is not None:
            assert value['authorized_senders'] == senders
        if domains is not None:
            assert value['authorized_domains'] == domains
        if version is not None:
            assert value['version'] == version, 'bootstrap changed persisted credential version'
        return value['version']

    def auth(self, password, allowed=True, username='robot'):
        client = smtplib.SMTP('127.0.0.1', self.submission_port, timeout=10)
        self.sessions.append(client)
        client.ehlo()
        assert not client.has_extn('auth'), 'credentials advertised before required TLS'
        client.starttls(context=self.tls)
        try:
            code, _ = client.login(username, password)
        except smtplib.SMTPAuthenticationError as error:
            assert not allowed and error.smtp_code == 535, 'unexpected SMTP AUTH result'
            client.close()
            self.sessions.remove(client)
            return None
        assert allowed and code == 235, 'unexpected SMTP AUTH success'
        return client

    def original(self, index, sender='robot@example.test'):
        return (f'From: {sender}\r\nTo: destination@example.test\r\n'
                f'Subject: persisted submission credential smoke {index}\r\n\r\n'
                f'Exact synthetic queued original {index}.\r\n').encode()

    def send(self, index, username='robot', sender='robot@example.test', password=None):
        client = self.auth(password or (self.new if index > 1 else self.old), username=username)
        original = self.original(index, sender)
        assert client.sendmail(sender, ['destination@example.test'], original) == {}
        self.expected.append(original)
        client.quit()
        self.sessions.remove(client)

    def wait_handoffs(self, count):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            assert not self.relay.errors, 'synthetic relay failed: ' + repr(self.relay.errors)
            if self.sql("SELECT count(*) FROM fry_outbound WHERE state='handed-off'") == str(count):
                assert len(self.expected) == count and self.relay.messages == self.expected, 'relay original bytes changed or duplicated'
                return
            time.sleep(.1)
        raise AssertionError('accepted queue did not hand off within deadline')

    def verify(self):
        progress('checking CLI scope, rotation and stale authenticated sessions')
        self.start(self.old, initialize=True)
        self.cli('sender-override', extra=('-scope', 'example.test', '-reason', 'Disposable local fixture delegation'))
        self.cli('organization-save', name='Other synthetic organization', extra=('-id', OTHER))
        first = self.account(True)
        assert self.cli('submission-accounts', organization=OTHER) == []
        self.cli('submission-password-set', organization=OTHER, password=self.new, success=False)
        self.cli('submission-access-set', organization=OTHER, enabled=False, success=False)
        self.cli('submission-password-set', name='not-configured', password=self.new, success=False)
        assert self.account(True, first) == first
        stale = self.auth(self.old)
        self.send(1)
        assert self.relay.received.wait(10), 'queue did not reach controlled relay'
        assert self.sql("SELECT count(*) FROM fry_outbound WHERE state='handed-off'") == '0'
        self.cli('submission-password-set', password=self.new)
        rotated = self.account(True)
        assert rotated > first
        self.auth(self.old, allowed=False)
        assert stale.mail('robot@example.test')[0] == 535, 'rotated AUTH session remained authorized'
        pending = self.auth(self.new)
        assert pending.mail('robot@example.test')[0] == 250
        assert pending.rcpt('destination@example.test')[0] == 250
        idle = self.auth(self.new)
        self.cli('submission-access-set', enabled=False)
        disabled = self.account(False)
        assert disabled > rotated
        assert idle.mail('robot@example.test')[0] == 535, 'disabled AUTH session remained authorized'
        try:
            code, _ = pending.data(self.original(99))
        except smtplib.SMTPDataError as error:
            assert error.smtp_code == 535, 'unexpected stale DATA rejection'
        else:
            assert code == 535, 'credential revoked after MAIL was accepted at DATA: ' + str(code)
        self.auth(self.new, allowed=False)
        assert self.sql('SELECT count(*) FROM fry_receipt') == '1', 'stale session created a receipt'
        progress('releasing previously accepted queued mail after credential disable')
        self.relay.release.set()
        self.wait_handoffs(1)
        self.stop()
        progress('checking clean restart with stale bootstrap preserves password and disabled state')
        self.start(self.old)
        self.account(False, disabled)
        self.auth(self.old, allowed=False)
        self.auth(self.new, allowed=False)
        self.cli('submission-access-set', enabled=True)
        enabled = self.account(True)
        assert enabled > disabled
        self.auth(self.old, allowed=False)
        allowed = self.auth(self.new)
        assert allowed.mail('outside@example.test')[0] == 553, 'persisted sender restriction changed'
        self.send(2)
        self.wait_handoffs(2)
        self.cli('submission-access-set', enabled=False)
        disabled_again = self.account(False)
        self.stop()
        progress('checking clean restart without bootstrap secret preserves persisted controls')
        self.start(None)
        self.account(False, disabled_again)
        self.auth(self.old, allowed=False)
        self.auth(self.new, allowed=False)
        self.cli('submission-access-set', enabled=True)
        self.auth(self.old, allowed=False)
        self.send(3)
        self.wait_handoffs(3)
        assert self.sql('SELECT count(*) FROM fry_receipt') == '3'
        assert self.sql('SELECT count(*) FROM fry_submission_account') == '1'

        progress('creating a live SMTP account and changing its sender policy without restart')
        self.cli('submission-account-create', name='new-robot', password=self.dynamic_password,
                 senders=['robot@example.test'], domains=[], enabled=True)
        created = self.account(True, name='new-robot', senders=['robot@example.test'], domains=[])
        for organization in (ORG, OTHER):
            self.cli('submission-account-create', organization=organization, name='new-robot',
                     password=self.old, senders=['robot@example.test'], success=False)
        self.account(True, created, name='new-robot')
        self.auth(self.old, allowed=False, username='new-robot')
        idle = self.auth(self.dynamic_password, username='new-robot')
        pending = self.auth(self.dynamic_password, username='new-robot')
        assert pending.mail('robot@example.test')[0] == 250
        assert pending.rcpt('destination@example.test')[0] == 250
        self.cli('submission-policy-set', name='new-robot', senders=['new@example.test'], domains=[])
        changed = self.account(True, name='new-robot', senders=['new@example.test'], domains=[])
        assert changed > created
        assert idle.mail('robot@example.test')[0] == 535, 'policy change left stale AUTH usable'
        try:
            code, _ = pending.data(self.original(99))
        except smtplib.SMTPDataError as error:
            assert error.smtp_code == 535, 'unexpected policy-revoked DATA rejection'
        else:
            assert code == 535, 'policy-revoked transaction accepted DATA'
        assert self.sql('SELECT count(*) FROM fry_receipt') == '3'
        scoped = self.auth(self.dynamic_password, username='new-robot')
        assert scoped.mail('robot@example.test')[0] == 553, 'removed sender scope remained usable'
        self.send(4, username='new-robot', sender='new@example.test', password=self.dynamic_password)
        self.wait_handoffs(4)
        self.cli('submission-access-set', name='new-robot', enabled=False)
        disabled_dynamic = self.account(False, name='new-robot')
        self.cli('submission-account-create', name='new-robot', password=self.old,
                 senders=['robot@example.test'], enabled=True, success=False)
        self.account(False, disabled_dynamic, name='new-robot')
        self.cli('submission-access-set', name='new-robot', enabled=True)
        dynamic_version = self.account(True, name='new-robot')
        self.cli('submission-policy-set', senders=['narrow@example.test'], domains=[])
        narrowed = self.account(True, senders=['narrow@example.test'], domains=[])
        self.stop()

        progress('checking stale bootstrap cannot overwrite live sender policy or newly created accounts')
        self.start(self.old)
        self.account(True, narrowed, senders=['narrow@example.test'], domains=[])
        self.account(True, dynamic_version, name='new-robot', senders=['new@example.test'], domains=[])
        self.auth(self.old, allowed=False)
        scoped = self.auth(self.new)
        assert scoped.mail('robot@example.test')[0] == 553, 'bootstrap widened persisted sender policy'
        self.send(5, sender='narrow@example.test')
        self.wait_handoffs(5)
        self.stop()

        progress('starting with no bootstrap accounts and provisioning another organization on the default relay')
        config = json.loads(self.config.read_text())
        config['outbound']['submission']['accounts'] = []
        self.config.write_text(json.dumps(config))
        self.start(None)
        self.account(True, narrowed, senders=['narrow@example.test'], domains=[])
        self.account(True, dynamic_version, name='new-robot', senders=['new@example.test'], domains=[])
        self.send(6, username='new-robot', sender='new@example.test', password=self.dynamic_password)
        self.wait_handoffs(6)
        self.cli('sender-override', organization=OTHER,
                 extra=('-scope', 'other.example.test', '-reason', 'Disposable second-organization delegation'))
        self.cli('submission-account-create', organization=OTHER, name='other-robot', password=self.dynamic_password,
                 senders=[], domains=['other.example.test'], enabled=True)
        other_accounts = self.cli('submission-accounts', organization=OTHER)
        assert len(other_accounts) == 1 and other_accounts[0]['username'] == 'other-robot'
        assert other_accounts[0]['authorized_senders'] == [] and other_accounts[0]['authorized_domains'] == ['other.example.test']
        other = self.auth(self.dynamic_password, username='other-robot')
        assert other.mail('new@example.test')[0] == 553, 'cross-organization sender scope escaped'
        self.send(7, username='other-robot', sender='other@other.example.test', password=self.dynamic_password)
        self.wait_handoffs(7)
        assert self.sql('SELECT count(*) FROM fry_receipt') == '7'
        assert self.sql('SELECT count(*) FROM fry_submission_account') == '3'
        # The CLI already checks every response for fixture passwords. Listing
        # audited operations must also omit secrets and password hashes.
        audit = json.dumps(self.cli('audit'))
        assert 'password_hash' not in audit and self.dynamic_password not in audit
        self.stop()
        self.report.update({'passed': True, 'accepted_and_handed_off': 7, 'clean_restarts': 4,
                            'stale_auth_mail_rejected': True, 'stale_mail_data_rejected': True,
                            'stale_and_absent_bootstrap_preserved_state': True,
                            'cli_wrong_org_and_missing_account_rejected': True,
                            'persisted_sender_scope_preserved': True,
                            'live_account_and_policy_without_restart': True,
                            'policy_revocation_fenced_mail_and_data': True,
                            'disabled_username_reserved': True,
                            'empty_bootstrap_and_new_org_default_relay': True,
                            'verified_starttls_required': True})

    def cleanup(self):
        errors = []
        try:
            self.stop()
        except Exception as error:
            errors.append(str(error))
        if self.relay:
            try:
                self.relay.close()
            except Exception as error:
                errors.append(str(error))
        if self.database:
            try:
                self.database.close()
            except Exception as error:
                errors.append(str(error))
        if errors:
            raise RuntimeError('Cleanup failed: ' + '; '.join(errors))


def interrupted(signum, _frame):
    raise SystemExit(128 + signum)


def main():
    if len(sys.argv) != 1:
        raise SystemExit('Usage: python3 -m scripts.smoke.submission_credentials')
    for dependency in ('docker', 'openssl'):
        if shutil.which(dependency) is None:
            raise SystemExit('Required command unavailable: ' + dependency)
    os.umask(0o077)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    with tempfile.TemporaryDirectory(prefix='fry-submission-smoke-') as directory:
        smoke = Smoke(Path(directory))
        try:
            smoke.prepare()
            smoke.verify()
        except Exception as error:
            print(smoke.redact(str(error)), file=sys.stderr)
            raise SystemExit(1)
        finally:
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            try:
                smoke.cleanup()
            except Exception as error:
                print(smoke.redact(str(error)), file=sys.stderr)
                raise SystemExit(1)
    print(json.dumps(smoke.report, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
