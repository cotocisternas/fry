"""Run and restart the executable against disposable synthetic SMTP/IMAP storage."""
import argparse
from contextlib import ExitStack, contextmanager
from http.server import BaseHTTPRequestHandler, HTTPServer
import imaplib
import json
import os
from pathlib import Path
import smtplib
import sys
import threading
import time

from scripts.lib.processes import (exit_signals, listening_ports, logged_process,
                                  reserved_loopback_port, scratch_directory)
from scripts.lib.testdeps import Session, capture, native_command

ROOT = Path(__file__).resolve().parents[2]
IMAP_PASSWORD = 'disposable-smoke-password'
SMTP_PASSWORD = 'disposable-submission-password'
INCOMING = b'From: sender@example.net\r\nSubject: process restart smoke\r\n\r\noriginal bytes\r\n'
SUBMISSION = b'From: robot@example.test\r\nSubject: queued submission smoke\r\n\r\noriginal bytes\r\n'


@contextmanager
def telemetry_collector(root):
    """Bind the collector directly, with no guessed port or startup subprocess."""
    failures = []

    class Collector(BaseHTTPRequestHandler):
        def setup(self):
            # Bound header/body reads too, so a partial request cannot hold up
            # collector shutdown after the executable has stopped.
            self.request.settimeout(2)
            super().setup()

        def log_message(self, *_args):
            pass

        def do_POST(self):
            self.connection.settimeout(2)
            try:
                length = int(self.headers.get('Content-Length', '0'))
            except ValueError:
                length = 0
            if self.path not in ('/v1/traces', '/v1/metrics') or not 0 < length <= 2**20:
                self.send_error(400)
                return
            try:
                data = self.rfile.read(length)
                if len(data) != length:
                    self.send_error(400)
                    return
                with (root / ('otlp-' + self.path.rsplit('/', 1)[1])).open('ab') as output:
                    output.write(data)
                self.send_response(200)
                self.send_header('Content-Length', '0')
                self.end_headers()
            except OSError as error:
                failures.append(error)

    with HTTPServer(('127.0.0.1', 0), Collector) as server:
        thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': .05},
                                  name='fry-smoke-otlp', daemon=True)
        thread.start()
        try:
            yield f'http://127.0.0.1:{server.server_port}'
        finally:
            server.shutdown()
            thread.join(timeout=3)
            if thread.is_alive():
                raise RuntimeError('Telemetry collector did not stop')
            if failures:
                raise RuntimeError('Telemetry collector request failed') from failures[0]


def smoke_config(root, smtp_port, collector=None):
    config = json.loads((ROOT / 'examples/local-mail.json').read_text())
    address = f'127.0.0.1:{smtp_port}'
    config.update(listen=address, data_directory=str(root / 'data'))
    config['outbound']['relays'][0]['address'] = address
    config['outbound']['submission']['listen'] = '127.0.0.1:0'
    config['imap']['listen'] = '127.0.0.1:0'
    if collector:
        config['telemetry'] = dict(enabled=True, endpoint=collector, allow_insecure=True,
                                   trace_sample_ratio=1.0, export_interval_seconds=1)
    return config


def exercise_protocols(ports, round_number):
    with smtplib.SMTP('127.0.0.1', ports['SMTP'], timeout=10) as smtp:
        smtp.sendmail('sender@example.net', ['invoices@example.test', 'billing@example.test'],
                      INCOMING)
    with smtplib.SMTP('127.0.0.1', ports['submission'], timeout=10) as submission:
        submission.login('robot', SMTP_PASSWORD)
        submission.sendmail('robot@example.test', ['invoices@example.test'], SUBMISSION)
    with imaplib.IMAP4('127.0.0.1', ports['IMAP'], timeout=10) as mailbox:
        mailbox.login('invoices', IMAP_PASSWORD)
        deadline = time.monotonic() + 10
        while True:
            status, count = mailbox.select('INBOX')
            if status == 'OK' and int(count[0]) == 2 * round_number:
                break
            if time.monotonic() >= deadline:
                raise AssertionError('SMTP receipt did not become visible through IMAP')
            time.sleep(.1)
        status, payload = mailbox.uid('FETCH', '1:*', '(UID BODY.PEEK[])')
        messages = [part[1] for part in payload if isinstance(part, tuple)]
        if status != 'OK' or len(messages) != 2 * round_number:
            raise AssertionError('Unexpected IMAP message count after process restart')
        if any(message not in {INCOMING, SUBMISSION} for message in messages):
            raise AssertionError('IMAP changed original message bytes')


def sql_count(session, query):
    result = capture(('docker', 'exec', session.container, 'psql', '-U', 'postgres', '-d',
                      'fry_test', '-Atc', query), timeout=10)
    result.check_returncode()
    value = result.stdout.decode().strip()
    if not value.isascii() or not value.isdigit():
        raise RuntimeError('Database count query did not return an integer')
    return int(value)


def verify_database(session, round_number):
    if sql_count(session, 'SELECT count(*) FROM fry_receipt') != round_number * 3:
        raise AssertionError('Unexpected receipt count')
    if sql_count(session, 'SELECT count(*) FROM fry_destination') != round_number * 2:
        raise AssertionError('Alias deduplication failed')
    deadline = time.monotonic() + 5
    while sql_count(session, "SELECT count(*) FROM fry_outbound WHERE state='handed-off'") != round_number:
        if time.monotonic() >= deadline:
            raise AssertionError('Queued relay handoff did not complete')
        time.sleep(.1)


def verify_telemetry(root, database_url):
    traces = (root / 'otlp-traces').read_bytes()
    metrics = (root / 'otlp-metrics').read_bytes()
    for operation in (b'smtp.receive', b'smtp.submit', b'imap.fetch', b'postgresql.select'):
        if operation not in traces:
            raise AssertionError('Missing operation trace: ' + operation.decode())
    for instrument in (b'fry.operation.count', b'fry.operation.duration'):
        if instrument not in metrics:
            raise AssertionError('Missing operation metric: ' + instrument.decode())
    for private in ('sender@example.net', 'robot@example.test', 'original bytes',
                    'process restart smoke', IMAP_PASSWORD, SMTP_PASSWORD, database_url):
        if private.encode() in traces + metrics:
            raise AssertionError('Private mail/configuration data reached collector')


def run(*, telemetry=False):
    with ExitStack() as resources:
        session = Session()
        resources.callback(session.close)
        root = resources.enter_context(scratch_directory(ROOT, '.fry-smoke.'))
        environment = {**os.environ, 'FRY_DATABASE_URL': session.database_url,
                       'FRY_LOCAL_IMAP_PASSWORD': IMAP_PASSWORD,
                       'FRY_LOCAL_SMTP_PASSWORD': SMTP_PASSWORD}
        collector = resources.enter_context(telemetry_collector(root)) if telemetry else None
        executable = root / 'fry'
        result = capture(native_command(('go', '-C', str(ROOT), 'build', '-o', str(executable),
                                         './cmd/fry')), timeout=180)
        result.check_returncode()
        config_path, log_path = root / 'config.json', root / 'server.log'
        # Relay bindings persist across restarts and reject address changes.
        with reserved_loopback_port() as port:
            config_path.write_text(json.dumps(smoke_config(root, port, collector)))
        for round_number in (1, 2):
            command = [str(executable), '-config', str(config_path)]
            if round_number == 1:
                command.append('-init')
            with logged_process(command, log_path, env=environment, cwd=ROOT) as process:
                ports = listening_ports(process, log_path, ('SMTP', 'submission', 'IMAP'))
                exercise_protocols(ports, round_number)
                verify_database(session, round_number)
            if process.returncode:
                raise RuntimeError(f'FRY exited with status {process.returncode} during restart')
        if telemetry:
            verify_telemetry(root, session.database_url)
    print('Executable incoming SMTP, authenticated submission, queued relay, IMAP bytes and restart passed.')
    if telemetry:
        print('Executable OTLP traces/metrics and mail-data privacy passed.')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args(argv)
    try:
        with exit_signals():
            run(telemetry=os.environ.get('FRY_SMOKE_TELEMETRY', '0') == '1')
    except Exception as error:
        print('Executable SMTP smoke failed: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
