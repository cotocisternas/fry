"""Docker-free checks for demo/SMTP configuration, telemetry and process lifetime."""
from contextlib import ExitStack
import io
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request
from unittest import mock

from scripts.dev import demo_admin
from scripts.lib.processes import (exit_signals, listening_ports, logged_process,
                                  reserved_loopback_port, scratch_directory)
from scripts.smoke import smtp

ROOT = Path(__file__).resolve().parents[2]
RESISTANT_CHILD = '''
import os, pathlib, signal, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
pathlib.Path(sys.argv[1]).write_text(str(os.getpid()))
time.sleep(60)
'''
RESISTANT_PARENT = '''
import pathlib, signal, subprocess, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
subprocess.Popen([sys.executable, '-c', sys.argv[1], sys.argv[2]])
while not pathlib.Path(sys.argv[2]).exists(): time.sleep(.01)
pathlib.Path(sys.argv[3]).touch()
time.sleep(60)
'''
EXITED_PARENT = '''
import pathlib, subprocess, sys, time
subprocess.Popen([sys.executable, '-c', sys.argv[1], sys.argv[2]])
while not pathlib.Path(sys.argv[2]).exists(): time.sleep(.01)
'''


def wait_for(path, timeout=3):
    deadline = time.monotonic() + timeout
    while not path.exists():
        if time.monotonic() >= deadline:
            raise TimeoutError('Probe did not create ' + str(path))
        time.sleep(.01)


def alive(pid):
    try:
        return Path('/proc', str(pid), 'stat').read_text().split()[2] != 'Z'
    except (FileNotFoundError, ProcessLookupError):
        return False


def cancellation_probe(directory):
    previous = {number: signal.getsignal(number) for number in (signal.SIGINT, signal.SIGTERM)}

    def cleanup():
        # Stand in for a checked Session.close, without starting Docker.
        (directory / 'cleanup-started').touch()
        time.sleep(.5)
        (directory / 'cleanup-finished').touch()

    try:
        with exit_signals(), ExitStack() as resources:
            resources.callback(cleanup)
            root = resources.enter_context(scratch_directory(directory, 'scenario-'))
            (directory / 'scratch-path').write_text(str(root))
            child = "import os,pathlib,sys,time; pathlib.Path(sys.argv[1]).write_text(str(os.getpid())); time.sleep(60)"
            with logged_process([sys.executable, '-c', child, str(directory / 'child.pid')],
                                root / 'child.log'):
                wait_for(directory / 'child.pid')
                (directory / 'ready').touch()
                time.sleep(60)
    finally:
        (directory / 'handlers-restored').write_text(str(all(
            signal.getsignal(number) == handler for number, handler in previous.items())))


class Configuration(unittest.TestCase):
    def test_demo_port_validation_rejects_bad_values_before_starting_dependencies(self):
        self.assertEqual(demo_admin.http_port('1024'), 1024)
        self.assertEqual(demo_admin.http_port('65535'), 65535)
        for value in ('0', '1023', '65536', '8080.5', 'http://localhost', ''):
            with self.subTest(value=value), self.assertRaises(ValueError):
                demo_admin.http_port(value)
        with mock.patch.dict(os.environ, {'FRY_DEMO_HTTP_PORT': '80'}), \
                mock.patch.object(demo_admin, 'Session') as session, \
                mock.patch('sys.stderr'):
            self.assertEqual(demo_admin.main([]), 1)
            session.assert_not_called()

    def test_configs_use_disposable_storage_and_matching_self_relay(self):
        root = Path('/synthetic/demo')
        config = demo_admin.demo_config(root, 41001, 8087)
        self.assertEqual(config['listen'], '127.0.0.1:41001')
        self.assertEqual(config['outbound']['relays'][0]['address'], '127.0.0.1:41001')
        self.assertEqual(config['data_directory'], '/synthetic/demo/data')
        self.assertEqual(config['admin']['origin'], 'http://127.0.0.1:8087')
        self.assertEqual(config['admin']['listen'], '127.0.0.1:8087')
        self.assertEqual(config['admin']['bootstrap_username'], 'operator')
        self.assertEqual(config['admin']['bootstrap_password_environment'], 'FRY_LOCAL_ADMIN_PASSWORD')
        self.assertEqual([route['address'] for route in config['routes']],
                         ['invoices@example.test', 'billing@example.test'])
        self.assertEqual(config['routes'][0]['inboxes'], config['routes'][1]['inboxes'])
        config = smtp.smoke_config(root, 41002)
        self.assertNotIn('telemetry', config)
        self.assertEqual(config['listen'], '127.0.0.1:41002')
        self.assertEqual(config['outbound']['relays'][0]['address'], '127.0.0.1:41002')
        self.assertEqual(config['outbound']['submission']['listen'], '127.0.0.1:0')
        self.assertEqual(config['imap']['listen'], '127.0.0.1:0')
        self.assertEqual(config['data_directory'], '/synthetic/demo/data')
        config = smtp.smoke_config(root, 41003, 'http://127.0.0.1:41004')
        self.assertEqual(config['telemetry'], {
            'enabled': True, 'endpoint': 'http://127.0.0.1:41004', 'allow_insecure': True,
            'trace_sample_ratio': 1.0, 'export_interval_seconds': 1})


class Telemetry(unittest.TestCase):
    def test_collector_records_both_signals_rejects_bad_requests_and_closes_listener(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with smtp.telemetry_collector(root) as origin:
                for path in ('traces', 'metrics'):
                    with urllib.request.urlopen(origin + '/v1/' + path, data=path.encode(), timeout=3) as response:
                        self.assertEqual(response.status, 200)
                    self.assertEqual((root / ('otlp-' + path)).read_bytes(), path.encode())
                for path, data in (('/unexpected', b'value'), ('/v1/traces', b'')):
                    with self.subTest(path=path), self.assertRaises(urllib.error.HTTPError) as error:
                        urllib.request.urlopen(origin + path, data=data, timeout=3)
                    self.assertEqual(error.exception.code, 400)
                    error.exception.close()
            with self.assertRaises(urllib.error.URLError):
                urllib.request.urlopen(origin + '/v1/traces', data=b'value', timeout=1)

    def test_privacy_and_required_telemetry_checks(self):
        database_url = 'postgres://fixture-secret@127.0.0.1:40000/fry_test'
        traces = b'smtp.receive smtp.submit imap.fetch postgresql.select'
        metrics = b'fry.operation.count fry.operation.duration'
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'otlp-traces').write_bytes(traces)
            (root / 'otlp-metrics').write_bytes(metrics)
            with mock.patch('sys.stdout'):
                smtp.verify_telemetry(root, database_url)
            for private in ('sender@example.net', 'robot@example.test', 'original bytes',
                            'process restart smoke', smtp.IMAP_PASSWORD, smtp.SMTP_PASSWORD,
                            database_url):
                (root / 'otlp-traces').write_bytes(traces + private.encode())
                with self.subTest(private=private), self.assertRaisesRegex(AssertionError, 'Private'):
                    smtp.verify_telemetry(root, database_url)
            for operation in traces.split():
                (root / 'otlp-traces').write_bytes(traces.replace(operation, b''))
                with self.subTest(operation=operation), self.assertRaisesRegex(AssertionError, 'Missing operation trace'):
                    smtp.verify_telemetry(root, database_url)
            (root / 'otlp-traces').write_bytes(traces)
            for instrument in metrics.split():
                (root / 'otlp-metrics').write_bytes(metrics.replace(instrument, b''))
                with self.subTest(instrument=instrument), self.assertRaisesRegex(AssertionError, 'Missing operation metric'):
                    smtp.verify_telemetry(root, database_url)


class SMTPWorkflow(unittest.TestCase):
    def test_restart_keeps_the_same_configuration(self):
        starts, _output = self.run_with_fixture_executable(cleanup_fails=False)
        self.assertEqual([start['initialize'] for start in starts], [True, False])
        self.assertEqual(starts[0]['config'], starts[1]['config'])

    def test_failed_database_cleanup_does_not_announce_success(self):
        _starts, output = self.run_with_fixture_executable(cleanup_fails=True)
        self.assertNotIn('passed', output)

    def run_with_fixture_executable(self, *, cleanup_fails):
        # Keep real process startup/restart and checked cleanup; replace only
        # the Go build, protocol traffic and disposable database adapters.
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'examples').mkdir()
            (root / 'examples/local-mail.json').write_text(
                (ROOT / 'examples/local-mail.json').read_text())
            history = root / 'starts.jsonl'
            session = mock.Mock(database_url='postgres://synthetic/fry_test', container='synthetic')
            if cleanup_fails:
                session.close.side_effect = RuntimeError('database cleanup failed')

            def capture_fixture(args, **_kwargs):
                if 'build' in args:
                    executable = Path(args[args.index('-o') + 1])
                    executable.write_text(f'#!{sys.executable}\n' + '''
import json, pathlib, signal, sys, time
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
config = json.loads(pathlib.Path(sys.argv[sys.argv.index('-config') + 1]).read_text())
''' + f'with pathlib.Path({str(history)!r}).open("a") as output:\n' + '''
    output.write(json.dumps({'config': config, 'initialize': '-init' in sys.argv}) + '\\n')
for name in ('SMTP', 'submission', 'IMAP'):
    print(name + ' listening on 127.0.0.1:41000', flush=True)
time.sleep(60)
''')
                    executable.chmod(0o700)
                    (executable.parent / 'otlp-traces').write_bytes(
                        b'smtp.receive smtp.submit imap.fetch postgresql.select')
                    (executable.parent / 'otlp-metrics').write_bytes(
                        b'fry.operation.count fry.operation.duration')
                    return subprocess.CompletedProcess(args, 0, b'', b'')
                round_number = len(history.read_text().splitlines())
                query = args[-1]
                multiplier = {'SELECT count(*) FROM fry_receipt': 3,
                              'SELECT count(*) FROM fry_destination': 2,
                              "SELECT count(*) FROM fry_outbound WHERE state='handed-off'": 1}[query]
                return subprocess.CompletedProcess(args, 0, str(round_number * multiplier).encode(), b'')

            output = io.StringIO()
            with mock.patch.object(smtp, 'ROOT', root), \
                    mock.patch.object(smtp, 'Session', return_value=session), \
                    mock.patch.object(smtp, 'capture', side_effect=capture_fixture), \
                    mock.patch.object(smtp, 'exercise_protocols'), \
                    mock.patch('sys.stdout', output):
                if cleanup_fails:
                    with self.assertRaisesRegex(RuntimeError, 'database cleanup failed'):
                        smtp.run(telemetry=True)
                else:
                    smtp.run(telemetry=True)
            session.close.assert_called_once_with()
            self.assertEqual(list(root.glob('.fry-smoke.*')), [])
            return [json.loads(line) for line in history.read_text().splitlines()], output.getvalue()


class ProcessLifetime(unittest.TestCase):
    def test_build_failure_cleans_scratch_and_checks_database_cleanup(self):
        for module in (demo_admin, smtp):
            for cleanup_fails in (False, True):
                with self.subTest(module=module.__name__, cleanup_fails=cleanup_fails), \
                        tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    session = mock.Mock(database_url='postgres://synthetic/fry_test')
                    if cleanup_fails:
                        session.close.side_effect = RuntimeError('database cleanup failed')
                    with mock.patch.object(module, 'ROOT', root), \
                            mock.patch.object(module, 'Session', return_value=session), \
                            mock.patch.object(module, 'capture', side_effect=RuntimeError('build failed')):
                        expected = 'database cleanup failed' if cleanup_fails else 'build failed'
                        with self.assertRaisesRegex(RuntimeError, expected):
                            if module is demo_admin:
                                module.run(8080)
                            else:
                                module.run()
                    session.close.assert_called_once_with()
                    self.assertEqual(list(root.iterdir()), [])

    def test_spawn_failure_closes_log_descriptor(self):
        with tempfile.TemporaryDirectory() as temporary:
            before = set(Path('/proc/self/fd').iterdir())
            with self.assertRaises(FileNotFoundError):
                with logged_process(['/no/such/fry-executable'], Path(temporary) / 'server.log'):
                    self.fail('Missing executable started')
            self.assertEqual(set(Path('/proc/self/fd').iterdir()), before)

    def test_early_exit_fails_startup_promptly(self):
        with tempfile.TemporaryDirectory() as temporary:
            log = Path(temporary) / 'server.log'
            started = time.monotonic()
            with logged_process([sys.executable, '-c', "print('fixture failed'); raise SystemExit(7)"], log) as process:
                with self.assertRaisesRegex(RuntimeError, 'status 7.*fixture failed'):
                    listening_ports(process, log, ('SMTP',), timeout=3)
            self.assertLess(time.monotonic() - started, 2)

    def test_startup_timeout_reaps_child_and_removes_scratch(self):
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaises(TimeoutError):
                with scratch_directory(temporary, 'startup-') as root:
                    log = root / 'server.log'
                    with logged_process([sys.executable, '-c', 'import time; time.sleep(60)'], log) as process:
                        listening_ports(process, log, ('SMTP',), timeout=.1)
            self.assertIsNotNone(process.returncode)
            self.assertFalse(root.exists())

    def test_resistant_child_and_descendant_are_killed_and_timeout_is_reported(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            child_pid, ready = root / 'child.pid', root / 'ready'
            started = time.monotonic()
            with self.assertRaisesRegex(TimeoutError, 'did not stop'):
                with logged_process([sys.executable, '-c', RESISTANT_PARENT, RESISTANT_CHILD,
                                     str(child_pid), str(ready)], root / 'server.log',
                                    stop_timeout=.1) as process:
                    wait_for(ready)
            self.assertLess(time.monotonic() - started, 3)
            self.assertEqual(process.returncode, -signal.SIGKILL)
            self.assert_stopped(int(child_pid.read_text()))

    def test_successful_wrapper_does_not_leave_background_descendants(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            child_pid = root / 'child.pid'
            with logged_process([sys.executable, '-c', EXITED_PARENT, RESISTANT_CHILD,
                                 str(child_pid)], root / 'server.log') as process:
                self.assertEqual(process.wait(timeout=3), 0)
            self.assert_stopped(int(child_pid.read_text()))

    def test_cleanup_failure_is_not_suppressed(self):
        with tempfile.TemporaryDirectory() as temporary:
            owned = tempfile.TemporaryDirectory(dir=temporary, prefix='failure-')
            try:
                # Inject at the cleanup API, not tempfile's platform-specific
                # choice of tree remover. Keep the real context-manager exit.
                with mock.patch('scripts.lib.processes.tempfile.TemporaryDirectory',
                                return_value=owned) as create, \
                        mock.patch.object(owned, 'cleanup',
                                          side_effect=OSError('checked cleanup failed')) as cleanup:
                    with self.assertRaisesRegex(OSError, 'checked cleanup failed'):
                        with scratch_directory(temporary, 'failure-') as root:
                            self.assertEqual(root, Path(owned.name))
                    cleanup.assert_called_once_with()
                    create.assert_called_once_with(dir=temporary, prefix='failure-',
                                                   ignore_cleanup_errors=False)
            finally:
                owned.cleanup()

    def test_port_is_reserved_until_handoff(self):
        with reserved_loopback_port() as port:
            with socket.socket() as competitor, self.assertRaises(OSError):
                competitor.bind(('127.0.0.1', port))
        with socket.socket() as next_owner:
            next_owner.bind(('127.0.0.1', port))

    def test_repeated_cancellation_during_cleanup_preserves_exit_and_restores_handlers(self):
        for first_signal in (signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=first_signal), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                process = subprocess.Popen(
                    [sys.executable, '-m', 'scripts.tests.scenarios', '--cancellation-probe', temporary],
                    cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
                    env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
                try:
                    wait_for(directory / 'ready')
                    process.send_signal(first_signal)
                    wait_for(directory / 'cleanup-started')
                    process.send_signal(signal.SIGTERM)
                    process.send_signal(signal.SIGINT)
                    out, err = process.communicate(timeout=5)
                    self.assertEqual(process.returncode, 128 + first_signal,
                                     (out + err).decode(errors='replace'))
                    self.assertTrue((directory / 'cleanup-finished').exists())
                    self.assertEqual((directory / 'handlers-restored').read_text(), 'True')
                    self.assertFalse(Path((directory / 'scratch-path').read_text()).exists())
                    self.assert_stopped(int((directory / 'child.pid').read_text()))
                finally:
                    if process.poll() is None:
                        os.killpg(process.pid, signal.SIGKILL)
                    process.communicate(timeout=3)
                    if (directory / 'child.pid').exists():
                        pid = int((directory / 'child.pid').read_text())
                        if alive(pid):
                            os.kill(pid, signal.SIGKILL)

    def assert_stopped(self, pid):
        deadline = time.monotonic() + 2
        while alive(pid) and time.monotonic() < deadline:
            time.sleep(.01)
        self.assertFalse(alive(pid), 'Scenario left a live child process')


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--cancellation-probe':
        cancellation_probe(Path(sys.argv[2]))
    else:
        unittest.main()
