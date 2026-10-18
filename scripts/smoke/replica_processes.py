#!/usr/bin/env python3
"""Invoked by scripts/smoke/replicas.sh; owns only that script's disposable fixtures."""
from datetime import datetime, timezone
import hashlib
import http.client
import imaplib
import json
import os
from pathlib import Path
import re
import signal
import smtplib
import socket
import ssl
import subprocess
import sys
import time

def run(*args, data=None):
    return subprocess.run(args, input=data, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, check=True, timeout=30).stdout


def sql(query):
    return run('docker', 'exec', container, 'psql', '-U', 'postgres', '-d',
               'fry_test', '-Atc', query).decode().strip()


def openssl(*args, data=None):
    return run('openssl', *args, data=data)


def certificate(name, server=False):
    key, csr, cert = [root / f'{name}.{suffix}' for suffix in ('key', 'csr', 'crt')]
    openssl('req', '-new', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256',
            '-nodes', '-keyout', str(key), '-out', str(csr), '-subj', f'/CN={name}')
    ext = root / f'{name}.ext'
    ext.write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n'
                   + ('extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1\n' if server
                      else 'extendedKeyUsage=clientAuth\n'))
    openssl('x509', '-req', '-in', str(csr), '-CA', str(root / 'ca.crt'), '-CAkey',
            str(root / 'ca.key'), '-CAcreateserial', '-out', str(cert), '-days', '1',
            '-extfile', str(ext))
    public = openssl('x509', '-in', str(cert), '-pubkey', '-noout')
    digest = hashlib.sha256(openssl('pkey', '-pubin', '-outform', 'DER', data=public)).hexdigest()
    return dict(ca_file=str(root / 'ca.crt'), certificate_file=str(cert), key_file=str(key)), digest


def start(binary, config, name, pattern, extra=()):
    path = root / f'{name}.log'
    stream = path.open('w')
    logs.append(stream)
    args = [str(root / binary)] + ([] if config is None else ['-config', str(config)]) + list(extra)
    process = subprocess.Popen(args, stdout=stream,
                               stderr=subprocess.STDOUT)
    processes.append(process)
    for _ in range(150):
        text = path.read_text()
        match = re.search(pattern, text)
        if match:
            return process, int(match[1]), text
        if process.poll() is not None:
            raise AssertionError(f'{name} exited early: {text}')
        time.sleep(.1)
    raise AssertionError(f'{name} startup timeout: {path.read_text()}')


def stop(process):
    process.terminate()
    assert process.wait(timeout=20) == 0


def imap_connect(port):
    if config['imap'].get('transport') == 'tls':
        return imaplib.IMAP4_SSL('127.0.0.1', port,
                                ssl_context=ssl.create_default_context(cafile=str(root / 'ca.crt')),
                                timeout=15)
    return imaplib.IMAP4('127.0.0.1', port, timeout=15)


def interrupted(signum, frame):
    raise SystemExit(128 + signum)


def main():
    global root, container, config, processes, logs
    root = Path(sys.argv[1]).resolve()
    container = sys.argv[2]
    processes = []
    logs = []

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        openssl('req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256',
                '-nodes', '-keyout', str(root / 'ca.key'), '-out', str(root / 'ca.crt'),
                '-days', '1', '-subj', '/CN=FRY disposable replica CA',
                '-addext', 'basicConstraints=critical,CA:TRUE',
                '-addext', 'keyUsage=critical,keyCertSign,cRLSign')
        coordinator_tls, coordinator_key = certificate('coordinator')
        config = json.loads(Path('examples/local-smtp.json').read_text())
        config.update(listen='127.0.0.1:0', replica_tls=coordinator_tls)
        config.pop('data_directory')
        config.pop('member_capacity_bytes')
        config['imap']['listen'] = '127.0.0.1:0'
        outage_file = root / 's3-unavailable'
        fixture_process, fixture_port, _ = start('fry-s3-fixture', None, 's3-fixture',
            r'FRY S3 fixture listening on http://127\.0\.0\.1:(\d+)',
            ('-unavailable-file', str(outage_file)))
        config['storage_lifecycle'] = dict(
            objects=dict(region='sa-east-1', bucket='fry-test', endpoint=f'http://127.0.0.1:{fixture_port}',
                         use_path_style=True, loopback_anonymous=True),
            scratch_directory=str(root / 'pack-scratch'), max_pack_bytes=2*1024*1024,
            max_staging_bytes=8*1024*1024, pack_lease_seconds=3, interval_seconds=1,
            work_timeout_seconds=20, max_actions=5, max_receipts=128,
            reclaim_hot=True, reclaim_interval_seconds=1)
        member_configs = []
        for i, member in enumerate(config['members']):
            tls, fingerprint = certificate(f'member-{i}', server=True)
            member.update(server_key=fingerprint, url='https://127.0.0.1:1')
            member_config = dict(id=member['id'], domain=member['domain'], listen='127.0.0.1:0',
                                 data_directory=str(root / f'member-{i}'), max_message_bytes=1048576,
                                 capacity_bytes=1073741824, max_in_flight=8, max_requests=16,
                                 timeout_seconds=30, tls=tls, coordinator_keys=[coordinator_key])
            path = root / f'member-{i}.json'
            path.write_text(json.dumps(member_config))
            member_configs.append(path)
        coordinator_config = root / 'coordinator.json'
        coordinator_config.write_text(json.dumps(config))
        run(str(root / 'fry'), '-config', str(coordinator_config), '-init-only')
        expected = []
        for generation in (1, 2):
            members = []
            for i, path in enumerate(member_configs):
                process, port, _ = start('fry-member', path, f'member-{i}-generation-{generation}',
                                         r'listening with mutual TLS on 127\.0\.0\.1:(\d+)')
                members.append(process)
                config['members'][i]['url'] = f'https://127.0.0.1:{port}'
            coordinator_config.write_text(json.dumps(config))
            coordinator, smtp_port, text = start('fry', coordinator_config, f'coordinator-{generation}',
                                                  r'SMTP listening on 127\.0\.0\.1:(\d+)')
            imap_port = int(re.search(r'IMAP listening on 127\.0\.0\.1:(\d+)', text)[1])
            body = f'From: sender@example.net\r\nSubject: replica process {generation}\r\n\r\n'.encode() + (b'x' * 600 + b'\r\n') * 500
            with smtplib.SMTP('127.0.0.1', smtp_port, timeout=15) as smtp:
                smtp.sendmail('sender@example.net', ['invoices@example.test', 'billing@example.test'], body)
            expected.append(body)

            def verify_mail():
                with imap_connect(imap_port) as mail:
                    mail.login('invoices', os.environ['FRY_LOCAL_IMAP_PASSWORD'])
                    for _ in range(100):
                        status, count = mail.select('INBOX')
                        if status == 'OK' and int(count[0]) == len(expected):
                            break
                        time.sleep(.1)
                    else:
                        raise AssertionError('mailbox publication timeout')
                    status, values = mail.uid('FETCH', '1:*', '(UID BODY.PEEK[])')
                    assert status == 'OK'
                    assert [v[1] for v in values if isinstance(v, tuple)] == expected

            verify_mail()
            for _ in range(150):
                packed = int(sql("SELECT count(*) FROM fry_receipt WHERE pack_id IS NOT NULL"))
                hot = int(sql("SELECT count(*) FROM fry_target WHERE NOT bytes_removed"))
                if packed == len(expected) and hot == 0:
                    break
                time.sleep(.1)
            else:
                raise AssertionError('automatic packing/hot reclamation did not complete')
            verify_mail()  # Every hot copy is gone; these bytes come from S3 ranges.
            assert sql('SELECT count(*) FROM fry_target WHERE locator IS NOT NULL AND owner_epoch>0') == str(2 * generation)
            if generation == 1:
                stop(coordinator)
                for member in members:
                    stop(member)
                assert sql('SELECT count(*) FROM fry_member_owner WHERE closed') == '2'
            else:
                outage_file.touch()
                time.sleep(.2)
                outage_body = b'Subject: object storage outage\r\n\r\nretained in hot replicas\r\n'
                with smtplib.SMTP('127.0.0.1', smtp_port, timeout=15) as smtp:
                    smtp.sendmail('sender@example.net', ['invoices@example.test'], outage_body)
                expected.append(outage_body)
                time.sleep(1.2)
                with imap_connect(imap_port) as mail:
                    mail.login('invoices', os.environ['FRY_LOCAL_IMAP_PASSWORD'])
                    assert mail.select('INBOX')[1] == [str(len(expected)).encode()]
                    status, values = mail.uid('FETCH', str(len(expected)), '(BODY.PEEK[])')
                    assert status == 'OK' and [v[1] for v in values if isinstance(v, tuple)] == [outage_body]
                assert sql("SELECT count(*) FROM fry_receipt WHERE pack_id IS NULL") == '1'
                outage_file.unlink()
                for _ in range(200):
                    if sql("SELECT count(*) FROM fry_receipt WHERE pack_id IS NULL") == '0':
                        break
                    time.sleep(.1)
                else:
                    raise AssertionError('packing did not recover from the S3 outage')
                verify_mail()
                # The real migration CLI shares private replica transport with an
                # active coordinator; it never opens the members' local files.
                migration_config = {key: config[key] for key in (
                    'replica_tls', 'max_message_bytes', 'recovery_retention_hours',
                    'max_in_flight', 'members')}
                target_inbox = '30000000000000000000000000000009'
                job = '70000000000000000000000000000009'
                migration_config.update(organization=config['routes'][0]['organization'],
                                        inbox=target_inbox, job=job, max_messages=1,
                                        max_windows=8, timeout_seconds=30,
                                        source=dict(address=f'127.0.0.1:{imap_port}',
                                                    username='invoices',
                                                    password_environment='FRY_LOCAL_IMAP_PASSWORD',
                                                    loopback_plaintext=True))
                migration_path = root / 'migration.json'
                migration_path.write_text(json.dumps(migration_config))

                def migrate(action, *extra):
                    return json.loads(run(str(root / 'fry-migrate'), '-config',
                                          str(migration_path), '-action', action, *extra))

                progress = migrate('begin')
                assert progress['Progress']['CopiedMessages'] == 1
                assert not progress['Run']['Complete']
                for _ in range(10):
                    if progress['Run']['Complete']:
                        break
                    progress = migrate('resume')
                assert progress['Run']['Complete']
                assert progress['Progress']['Phase'] == 'copying'
                assert sql(f"SELECT migration_job FROM fry_inbox WHERE id='{target_inbox}'") == job
                with imap_connect(imap_port) as mail:
                    mail.login('invoices', os.environ['FRY_LOCAL_IMAP_PASSWORD'])
                    assert mail.select('INBOX')[0] == 'OK'
                    assert mail.uid('STORE', '1', '+FLAGS.SILENT', '(\\Seen processed)')[0] == 'OK'
                    assert mail.create('Empty')[0] == 'OK'
                    assert mail.subscribe('Empty')[0] == 'OK'
                freeze = root / 'freeze.json'
                freeze.write_text(json.dumps(dict(
                    actor='synthetic-process-test', reason='exercise final copy',
                    barrier='this test has closed its only mutating client and sends no more source mail',
                    arrival_handoff='no external senders exist in this disposable fixture',
                    started_at=datetime.now(timezone.utc).isoformat())))
                progress = migrate('begin-final', '-freeze', str(freeze))
                for _ in range(10):
                    if progress['Run']['Complete']:
                        break
                    progress = migrate('resume')
                assert progress['Run']['Complete'] and progress['Progress']['Phase'] == 'ready'
                pass_generation = progress['Progress']['Generation']
                rejected = subprocess.run([str(root / 'fry-migrate'), '-config', str(migration_path),
                                           '-action', 'activate', '-generation', str(pass_generation-1)],
                                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
                assert rejected.returncode != 0
                assert migrate('activate', '-generation', str(pass_generation))['Progress']['Phase'] == 'complete'
                assert migrate('activate', '-generation', str(pass_generation))['Progress']['Phase'] == 'complete'
                stop(coordinator)
                config['imap']['accounts'].append(dict(username='imported',
                    password_environment='FRY_LOCAL_IMAP_PASSWORD',
                    organization=config['routes'][0]['organization'], inbox=target_inbox))
                # Final restart exposes explicit TLS modes using a generated private
                # CA; storage and migration stay the same across listener changes.
                edge_tls, _ = certificate('mail-edge', server=True)
                public_transport = dict(transport='tls', tls_cert_file=edge_tls['certificate_file'],
                                        tls_key_file=edge_tls['key_file'], handshake_timeout_seconds=2)
                config.update(public_transport)
                config['transport'] = 'starttls'
                config['imap'].update(public_transport)
                with socket.socket() as smtp_probe:
                    smtp_probe.bind(('127.0.0.1', 0))
                    config['listen'] = f'127.0.0.1:{smtp_probe.getsockname()[1]}'
                config['outbound'] = dict(default_relay='50000000000000000000000000000001',
                    retry_hours=1, notice_retry_hours=1,
                    relays=[dict(id='50000000000000000000000000000001',
                                 address=config['listen'], transport='loopback')],
                    submission=dict(public_transport, listen='127.0.0.1:0',
                        accounts=[dict(username='robot', password_environment='FRY_LOCAL_IMAP_PASSWORD',
                            organization=config['routes'][0]['organization'],
                            authorized_senders=['robot@example.test'])]))
                config['outbound']['submission']['transport'] = 'starttls-required'
                with socket.socket() as probe:
                    probe.bind(('127.0.0.1', 0))
                    admin_port = probe.getsockname()[1]
                config['admin'] = dict(public_transport, listen=f'127.0.0.1:{admin_port}',
                    origin=f'https://127.0.0.1:{admin_port}', session_minutes=60,
                    bootstrap_username='operator', bootstrap_password_environment='FRY_LOCAL_IMAP_PASSWORD',
                    bootstrap_content_access=True)
                coordinator_config.write_text(json.dumps(config))
                coordinator, smtp_port, text = start('fry', coordinator_config, 'coordinator-migrated',
                                                      r'SMTP listening on 127\.0\.0\.1:(\d+)')
                imap_port = int(re.search(r'IMAP listening on 127\.0\.0\.1:(\d+)', text)[1])
                with imap_connect(imap_port) as mail:
                    mail.login('imported', os.environ['FRY_LOCAL_IMAP_PASSWORD'])
                    assert mail.select('INBOX')[1] == [str(len(expected)).encode()]
                    status, values = mail.uid('FETCH', '1:*', '(UID BODY.PEEK[])')
                    assert status == 'OK'
                    assert [v[1] for v in values if isinstance(v, tuple)] == expected
                    status, values = mail.uid('FETCH', '1', '(FLAGS)')
                    assert status == 'OK' and b'processed' in values[0] and b'\\Seen' in values[0]
                    assert mail.select('Empty')[1] == [b'0']
                verify_mail()  # Source memberships and bytes were left intact.
                edge_context = ssl.create_default_context(cafile=str(root / 'ca.crt'))
                with smtplib.SMTP('127.0.0.1', smtp_port, timeout=15) as smtp:
                    smtp.ehlo()
                    assert smtp.has_extn('starttls') and not smtp.has_extn('auth')
                    smtp.starttls(context=edge_context)
                    assert smtp.ehlo()[0] == 250 and not smtp.has_extn('starttls')
                    assert smtp.noop()[0] == 250
                https = http.client.HTTPSConnection('127.0.0.1', admin_port, context=edge_context, timeout=15)
                https.request('POST', '/api/login', json.dumps(dict(username='operator',
                    password=os.environ['FRY_LOCAL_IMAP_PASSWORD'])),
                    {'Content-Type': 'application/json', 'Origin': config['admin']['origin']})
                response = https.getresponse()
                assert response.status == 200, response.read()
                cookie = response.getheader('Set-Cookie')
                assert cookie.startswith('__Host-fry-admin=') and 'Secure' in cookie and 'HttpOnly' in cookie
                identity = json.loads(response.read())
                assert identity['csrf']
                https.request('GET', '/api/session', headers={'Cookie': cookie.split(';')[0]})
                response = https.getresponse()
                assert response.status == 200, response.read()
                response.read()
                def admin_post(path, payload):
                    https.request('POST', '/api/' + path, json.dumps(payload),
                        {'Content-Type': 'application/json', 'Origin': config['admin']['origin'],
                         'Cookie': cookie.split(';')[0], 'X-FRY-CSRF': identity['csrf']})
                    response = https.getresponse()
                    raw = response.read()
                    assert response.status == 200, raw
                    return json.loads(raw)

                submission_port = int(re.search(r'submission listening on 127\.0\.0\.1:(\d+)', text)[1])
                organization = config['routes'][0]['organization']
                with smtplib.SMTP('127.0.0.1', submission_port, timeout=15) as submission:
                    submission.ehlo()
                    assert not submission.has_extn('auth')
                    submission.starttls(context=edge_context)
                    submission.login('robot', os.environ['FRY_LOCAL_IMAP_PASSWORD'])
                    assert submission.mail('robot@example.test')[0] == 553
                    admin_post('sender-override', dict(organization=organization,
                        scope='robot@example.test', reason='Synthetic public submission process test'))
                    sent = b'From: robot@example.test\r\nSubject: verified submission\r\n\r\naccepted under recorded authority\r\n'
                    submission.sendmail('robot@example.test', ['invoices@example.test'], sent)
                    expected.append(sent)
                    verify_mail()  # Queued handoff through the selected relay back to MX.
                    assert submission.mail('robot@example.test')[0] == 250
                    assert submission.rcpt('invoices@example.test')[0] == 250
                    admin_post('sender-revoke', dict(organization=organization,
                        scope='robot@example.test', reason='Synthetic in-flight revocation'))
                    try:
                        code, _ = submission.data(b'Subject: revoked\r\n\r\nnever accepted\r\n')
                    except smtplib.SMTPDataError as error:
                        assert error.smtp_code == 553
                    else:
                        assert code == 553, 'revoked sender accepted DATA'
                    assert submission.mail('robot@example.test')[0] == 553
                for _ in range(100):
                    if sql("SELECT count(*) FROM fry_outbound WHERE state='handed-off'") == '1':
                        break
                    time.sleep(.1)
                else:
                    raise AssertionError('verified submission did not complete relay handoff')
                https.close()
                # A real abrupt process death does not manufacture safe takeover.
                members[0].kill()
                assert members[0].wait(timeout=5) != 0
                verify_mail()  # Falls back to the surviving member.
                with smtplib.SMTP('127.0.0.1', smtp_port, timeout=15) as smtp:
                    try:
                        smtp.sendmail('sender@example.net', ['invoices@example.test'], b'Subject: unavailable\r\n\r\nnot accepted\r\n')
                    except smtplib.SMTPDataError as error:
                        assert 400 <= error.smtp_code < 500
                    except smtplib.SMTPServerDisconnected:
                        pass  # Unconfirmed cancellation closes SMTP without a success reply.
                    else:
                        raise AssertionError('SMTP accepted without both storage members')
                assert sql('SELECT count(*) FROM fry_receipt') == str(2 * len(expected))
                restart = subprocess.run([str(root / 'fry-member'), '-config', str(member_configs[0])],
                                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15)
                assert restart.returncode != 0, 'unclean generation was silently adopted'
                assert sql('SELECT count(*) FROM fry_member_owner WHERE epoch=2 AND NOT closed') == '2'
                stop(coordinator)
                stop(members[1])
        print('Separate-process mTLS replication, SMTP/IMAP bytes, clean restart, automatic packing/reclamation, S3 outage buffering, staged migration CLI, public SMTP/IMAP TLS, HTTPS sender authorization/revocation, queued relay, crash fallback, deferred receipt and fenced restart passed.')
    except BaseException:
        for path in sorted(root.glob('*.log')):
            print(f'{path.name}:\n{path.read_text()}', file=sys.stderr)
        raise
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        for stream in logs:
            stream.close()


if __name__ == '__main__':
    main()
