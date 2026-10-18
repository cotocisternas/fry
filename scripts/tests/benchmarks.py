"""Docker-free benchmark report, failure, process, and cleanup regressions."""
from contextlib import ExitStack, redirect_stderr, redirect_stdout
import copy
import gzip
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest import mock

from scripts.bench import acceptance, hotstore_retained, load_mail, mailbox_publication, query_plans, retained_reclaim
from scripts.lib import benchmark as bench
from scripts.lib import testdeps

ROOT = Path(__file__).resolve().parents[2]
DSN = 'postgres://postgres:fry-test-only@127.0.0.1:12345/fry_test'


def acceptance_report(mode='baseline', storage='legacy', size=65536):
    value = dict(mode=mode, storage_profile=storage, member_capacity_bytes=256 << 20,
                 segment_max_records=2048 if storage == 'bounded' else 0,
                 segment_max_bytes=96 << 20 if storage == 'bounded' else 0,
                 sql_trace_enabled=mode != 'baseline', runtime_profiles_enabled=mode == 'profile',
                 runtime_trace_enabled=mode == 'trace', messages=1024, message_bytes=65536,
                 workers=16, attempted=1024, succeeded=1024, errors=0,
                 verified_receipts=1024, verified_bodies=1024, max_concurrent_appends=16,
                 max_message_bytes=size, lease_seconds=30, sql_bucket_overflow=0,
                 pool={'max_connections': 16}, sql=[] if mode == 'baseline' else [{'count': 1}],
                 acquire_trace={'count': 0 if mode == 'baseline' else 1},
                 profile_block_rate_ns=10000 if mode == 'profile' else 0,
                 profile_mutex_fraction=5 if mode == 'profile' else 0)
    if storage == 'bounded':
        lanes = 16 if size == 65536 else 11
        value.update(verified_capacity_files=2, verified_capacity_targets=2048,
                     capacity_lane_version=1, capacity_lanes_per_file=lanes, verified_capacity_lanes=2 * lanes)
    return value


def acceptance_artifacts(root, mode='baseline', storage='legacy', size=65536):
    root.mkdir(mode=0o700)
    bench.private_json(root / 'report.json', acceptance_report(mode, storage, size))
    names = {'profile': ('cpu.pprof', 'block.pprof', 'mutex.pprof'),
             'trace': ('execution.trace',)}.get(mode, ())
    for name in names:
        path = root / name
        path.write_bytes(gzip.compress(b'private synthetic profile') if name.endswith('.pprof') else b'synthetic trace')
        path.chmod(0o600)


def publication_text():
    return '\n'.join('test prefix PUBLICATION_MEASUREMENT_JSON ' + json.dumps(
        {'limit': limit, 'messages': 1024, 'verified_originals': 1024}) for limit in (1, 16, 64, 128))


def reclaim_report():
    return {'records': 128, 'message_bytes': 128, 'live_records': 64, 'expired_records': 64,
            'verified_live_direct_from_rewritten_member': 64, 'finished_removed_receipts': 64}


def reclaim_text(value=None):
    return 'test prefix RETAINED_RECLAIM_MEASUREMENT_JSON ' + json.dumps(value or reclaim_report())


def load_report(status=0):
    return {'requested': 32, 'message_bytes': 1024, 'concurrency': 4,
            'accepted': 32 - status, 'deferred': 0, 'failed': status,
            'verified_headers': 32 - status, 'verified_fetch_samples': 1, 'metadata': {},
            'errors': ['IMAP verification failed'] if status else []}


class BenchmarkTests(unittest.TestCase):
    def setUp(self):
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.root = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix='fry-bench-test-')))
        self.stack.enter_context(mock.patch.object(bench, 'ROOT', self.root))
        self.stdout = self.stack.enter_context(redirect_stdout(io.StringIO()))
        self.stderr = self.stack.enter_context(redirect_stderr(io.StringIO()))

    def fixture(self, failure=False, output=None):
        process = mock.Mock()
        process.wait.return_value = 0

        def close():
            self.assertEqual(signal.getsignal(signal.SIGTERM), signal.SIG_IGN)
            self.assertEqual(signal.getsignal(signal.SIGINT), signal.SIG_IGN)
            if output:
                self.assertFalse(output.exists(), 'success report was published before fixture cleanup')
            if failure:
                raise RuntimeError('fixture close failed ' + DSN)

        fixture = SimpleNamespace(container='0' * 64, database_url=DSN, process=process, close=mock.Mock(side_effect=close))
        self.stack.enter_context(mock.patch.object(testdeps, 'Session', return_value=fixture))
        self.stack.enter_context(mock.patch.object(testdeps, 'signal_group'))
        return fixture

    def test_json_publication_is_private_atomic_and_never_overwrites(self):
        target = self.root / 'report.json'
        bench.private_json(target, {'passed': True})
        self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        for destination in (target, self.root / 'link.json'):
            if destination != target:
                destination.symlink_to(target)
            with self.subTest(destination=destination), self.assertRaises(FileExistsError):
                bench.private_json(destination, {'passed': False})
        self.assertEqual(json.loads(target.read_text()), {'passed': True})
        self.assertEqual(list(self.root.glob('.fry-benchmark-report-*')), [])
        with self.assertRaises(TypeError):
            bench.private_json(self.root / 'bad.json', {'bad': object()})
        self.assertFalse((self.root / 'bad.json').exists())
        self.assertEqual(list(self.root.glob('.fry-benchmark-report-*')), [])

    def test_directory_publication_refuses_empty_directory_and_dangling_link(self):
        source = self.root / 'source'
        source.mkdir()
        (source / 'report.json').write_text('{}')
        target = self.root / 'target'
        target.mkdir()
        with self.assertRaises(FileExistsError):
            bench.publish_directory(source, target)
        self.assertTrue(source.exists())
        target.rmdir()
        target.symlink_to(self.root / 'missing')
        with self.assertRaises(FileExistsError):
            bench.publish_directory(source, target)
        target.unlink()
        bench.publish_directory(source, target)
        self.assertFalse(source.exists())
        self.assertTrue((target / 'report.json').is_file())

    def test_new_output_rejects_dangling_symlink(self):
        target = self.root / 'output'
        target.symlink_to(self.root / 'missing')
        with self.assertRaises(AssertionError):
            bench.new_output(target)

    def test_diagnostics_are_bounded_and_redact_boundary_credentials(self):
        secret = 'postgres://postgres:' + 's' * 120 + '@127.0.0.1/fry_test'
        log = self.root / 'test.log'
        log.write_bytes(b'x' * 100000 + (secret + '\n' + 'z' * 65520).encode())
        detail = bench.diagnostic(log, (secret,))
        self.assertLessEqual(len(detail.encode()), 65536)
        self.assertNotIn('s' * 20, detail)
        log.write_bytes(('fry-test-only synthetic-load-only-password ' + DSN + ' ' + 'é' * 50000).encode())
        detail = bench.diagnostic(log, (DSN,))
        self.assertLessEqual(len(detail.encode()), 65536)
        self.assertNotIn('fry-test-only', detail)
        self.assertNotIn('synthetic-load-only-password', detail)
        self.assertNotIn('postgres://', detail)
        link = self.root / 'linked.log'
        link.symlink_to(log)
        self.assertIn('before test diagnostics', bench.diagnostic(link))

    def test_unreadable_diagnostic_still_cleans_and_refuses_success(self):
        target = self.root / 'report.json'
        fixture = self.fixture(output=target)
        with mock.patch.object(bench, 'diagnostic', side_effect=PermissionError('unreadable log')):
            with self.assertRaisesRegex(RuntimeError, 'unreadable log'):
                with bench.Run('unreadable') as run:
                    run.fixture()
                bench.private_json(target, {'passed': True})
        fixture.close.assert_called_once()
        self.assertFalse(run.root.exists())
        self.assertFalse(target.exists())

    def test_close_failure_cleans_scratch_redacts_and_prevents_report(self):
        target = self.root / 'report.json'
        fixture = self.fixture(failure=True, output=target)
        with self.assertRaisesRegex(RuntimeError, 'fixture close failed'):
            with bench.Run('failed-close') as run:
                run.fixture()
            bench.private_json(target, {'passed': True})
        self.assertFalse(run.root.exists())
        self.assertFalse(target.exists())
        fixture.process.wait.assert_called_once_with(timeout=5)
        self.assertNotIn(DSN, self.stderr.getvalue())
        self.assertNotIn('fry-test-only', self.stderr.getvalue())

    def test_scratch_cleanup_failure_prevents_publication(self):
        target = self.root / 'report.json'
        self.fixture(output=target)
        with mock.patch.object(bench, 'remove_private', side_effect=TimeoutError('scratch cleanup exceeded deadline')):
            with self.assertRaisesRegex(RuntimeError, 'scratch cleanup exceeded deadline'):
                with bench.Run('failed-remove') as run:
                    run.fixture()
                bench.private_json(target, {'passed': True})
        self.assertTrue(run.root.exists())
        self.assertFalse(target.exists())

    def test_startup_failure_removes_private_scratch(self):
        with mock.patch.object(testdeps, 'Session', side_effect=RuntimeError('startup failed ' + DSN)):
            with self.assertRaises(RuntimeError):
                with bench.Run('startup') as run:
                    run.fixture()
        self.assertFalse(run.root.exists())
        self.assertNotIn(DSN, self.stderr.getvalue())

    def test_retained_evidence_stash_removed_when_fixture_cleanup_fails(self):
        self.fixture(failure=True)
        with self.assertRaises(RuntimeError):
            with ExitStack() as stack:
                with bench.Run('staged') as run:
                    run.fixture()
                    source = run.root / 'results'
                    source.mkdir()
                    bench.private_json(source / 'report.json', {'passed': True})
                    stage = stack.enter_context(bench.evidence_directory(source, self.root))
                bench.publish_directory(stage, self.root / 'output')
        self.assertFalse(stage.exists())
        self.assertFalse(run.root.exists())
        self.assertFalse((self.root / 'output').exists())

    def test_acceptance_artifact_validation_all_modes_and_capacity_lanes(self):
        for mode, storage, size in (('baseline', 'legacy', 65536), ('profile', 'bounded', 65536), ('trace', 'bounded', 1048576)):
            root = self.root / mode
            acceptance_artifacts(root, mode, storage, size)
            report = acceptance.validate(root, mode, storage, size, DSN)
            self.assertEqual(report['verified_bodies'], 1024)
        value = json.loads((self.root / 'trace/report.json').read_text())
        value['verified_capacity_lanes'] -= 1
        (self.root / 'trace/report.json').write_text(json.dumps(value))
        with self.assertRaises(AssertionError):
            acceptance.validate(self.root / 'trace', 'trace', 'bounded', 1048576, DSN)

    def test_acceptance_rejects_incomplete_workload_and_nested_private_values(self):
        root = self.root / 'baseline'
        acceptance_artifacts(root)
        for field, value in (('verified_receipts', 1023), ('runtime_trace_enabled', True),
                             ('nested', {'Password': 'secret'}), ('nested', {'value': DSN})):
            report = acceptance_report()
            report[field] = value
            (root / 'report.json').write_text(json.dumps(report))
            with self.subTest(field=field), self.assertRaises(AssertionError):
                acceptance.validate(root, 'baseline', 'legacy', 65536, DSN)

    def test_acceptance_rejects_unexpected_public_symlink_and_empty_artifacts(self):
        root = self.root / 'profile'
        acceptance_artifacts(root, 'profile')
        path = root / 'cpu.pprof'
        valid = path.read_bytes()
        for kind in ('extra', 'public', 'symlink', 'empty'):
            with self.subTest(kind=kind):
                if kind == 'extra':
                    (root / 'unexpected').write_text('bad')
                elif kind == 'public':
                    path.chmod(0o644)
                elif kind == 'symlink':
                    path.unlink()
                    path.symlink_to(root / 'block.pprof')
                else:
                    path.write_bytes(b'')
                with self.assertRaises(AssertionError):
                    acceptance.validate(root, 'profile', 'legacy', 65536, DSN)
                (root / 'unexpected').unlink(missing_ok=True)
                path.unlink()
                path.write_bytes(valid)
                path.chmod(0o600)

    def test_acceptance_rejects_compressed_credentials_and_expansion_over_budget(self):
        root = self.root / 'profile'
        acceptance_artifacts(root, 'profile')
        path = root / 'cpu.pprof'
        path.write_bytes(gzip.compress(DSN.encode()))
        with self.assertRaisesRegex(AssertionError, 'credential leaked'):
            acceptance.validate(root, 'profile', 'legacy', 65536, DSN)
        path.write_bytes(gzip.compress(b'x' * 4097))
        with mock.patch.object(acceptance, 'ARTIFACT_LIMIT', 4096):
            with self.assertRaisesRegex(AssertionError, 'expanded profile'):
                acceptance.validate(root, 'profile', 'legacy', 65536, DSN)

    def test_publication_and_reclaim_require_all_original_verifications(self):
        report = mailbox_publication.validate(publication_text())
        self.assertEqual([case['limit'] for case in report['cases']], [1, 16, 64, 128])
        for text in ('', publication_text().replace('1024', '1023'), publication_text() + '\n' + publication_text()):
            with self.subTest(text=text[:40]), self.assertRaises(AssertionError):
                mailbox_publication.validate(text)
        retained_reclaim.validate(reclaim_text(), 128, 128)
        for key in ('records', 'live_records', 'expired_records', 'verified_live_direct_from_rewritten_member', 'finished_removed_receipts'):
            report = reclaim_report()
            report[key] -= 1
            with self.subTest(key=key), self.assertRaises(AssertionError):
                retained_reclaim.validate(reclaim_text(report), 128, 128)
        with self.assertRaises(AssertionError):
            retained_reclaim.validate(reclaim_text() + '\n' + reclaim_text(), 128, 128)

    def test_hotstore_phase_verification_and_exact_artifact_set(self):
        root = self.root / 'hotstore'
        root.mkdir()
        report = {'passed': True, 'fixture_removed': True, 'records': 1024, 'message_bytes': 1024,
                  'encoded_file_bytes': 1200000, 'phases': {}}
        for phase in ('open', 'inventory'):
            value = {'phase': phase, 'records': 1024, 'message_bytes': 1024, 'verified_bodies': 1024,
                     'verified_locators': 1024, 'elapsed_ms': 1, 'encoded_file_bytes': 1200000}
            report['phases'][phase] = value
            bench.private_json(root / (phase + '.json'), value)
        bench.private_json(root / 'report.json', report)
        hotstore_retained.validate(root, 1024, 1024)
        for change in ('fixture', 'bodies', 'bytes', 'elapsed', 'extra'):
            value = copy.deepcopy(report)
            if change == 'fixture': value['fixture_removed'] = False
            if change == 'bodies': value['phases']['open']['verified_bodies'] -= 1
            if change == 'bytes': value['phases']['inventory']['encoded_file_bytes'] -= 1
            if change == 'elapsed': value['phases']['open']['elapsed_ms'] = 0
            if change == 'extra': (root / 'unexpected').write_text('{}')
            (root / 'report.json').write_text(json.dumps(value))
            with self.subTest(change=change), self.assertRaises(AssertionError):
                hotstore_retained.validate(root, 1024, 1024)

    def test_publication_and_reclaim_publish_only_after_verified_cleanup(self):
        for module, args, text in ((mailbox_publication, [], publication_text()),
                                   (retained_reclaim, ['128', '128'], reclaim_text())):
            for failed in (False, True):
                with self.subTest(module=module.__name__, failed=failed), ExitStack() as stack:
                    target = self.root / f'{module.__name__}-{failed}.json'
                    fixture = self.fixture(failure=failed, output=target)
                    command = stack.enter_context(mock.patch.object(bench.Run, 'run', return_value=subprocess.CompletedProcess([], 0, text.encode(), b'')))
                    if failed:
                        with self.assertRaises(RuntimeError):
                            module.main([*args, str(target)])
                    else:
                        module.main([*args, str(target)])
                        self.assertTrue(target.is_file())
                    fixture.close.assert_called_once()
                    self.assertEqual(command.call_args.kwargs['timeout'], 300 if module is mailbox_publication else 360)
                    self.assertEqual(list(self.root.glob('.fry-*')), [])

    def test_load_validation_preserves_failing_client_report(self):
        failed = load_report(1)
        self.assertIs(load_mail.validate(failed, 32, 1024, 4, 1), failed)
        with self.assertRaises(AssertionError):
            load_mail.validate(load_report(1), 32, 1024, 4, 0)
        report = load_report()
        report['verified_headers'] = 31
        with self.assertRaises(AssertionError):
            load_mail.validate(report, 32, 1024, 4, 0)

    def test_query_plan_failed_cleanup_refuses_even_warning_report(self):
        target = self.root / 'plans.json'
        fixture = self.fixture(failure=True, output=target)
        with mock.patch.object(query_plans, 'queries', return_value=('candidate', 'wrap', 'batch', 'hash')), \
                mock.patch.object(query_plans.Check, 'setup', lambda check: check.owner.fixture()), \
                mock.patch.object(query_plans.Check, 'explain', return_value={'warnings': ['queue sort present']}), \
                mock.patch.object(query_plans.Check, 'sql', return_value='synthetic version'), \
                mock.patch.object(testdeps, 'image', return_value='synthetic image'):
            with self.assertRaises(RuntimeError):
                query_plans.main([str(target)])
        self.assertFalse(target.exists())
        fixture.close.assert_called_once()


def alive(pid):
    try:
        return Path('/proc', str(pid), 'stat').read_text().split()[2] != 'Z'
    except (FileNotFoundError, ProcessLookupError):
        return False


def worker(mode, path):
    if mode in ('resistant', 'sleep'):
        if mode == 'resistant':
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
        path.write_text(str(os.getpid()))
        time.sleep(60)
        return
    subprocess.Popen([sys.executable, '-m', 'scripts.tests.benchmarks', '--worker', 'resistant', str(path)],
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    deadline = time.monotonic() + 3
    while not path.exists() and time.monotonic() < deadline:
        time.sleep(.01)
    assert path.exists(), 'resistant descendant never started'
    if mode == 'timeout':
        time.sleep(60)
    raise SystemExit(7 if mode == 'failure' else 0)


def signal_probe(directory):
    bench.ROOT = directory
    fixture_process = mock.Mock()
    fixture_process.wait.return_value = 0
    original_signal_group = testdeps.signal_group

    def signal_group(process, signum):
        if process is not fixture_process:
            original_signal_group(process, signum)

    def close():
        assert signal.getsignal(signal.SIGTERM) == signal.SIG_IGN
        assert signal.getsignal(signal.SIGINT) == signal.SIG_IGN
        (directory / 'closing').write_text('fixture close started')
        time.sleep(.25)
        (directory / 'closed').write_text('fixture close completed')

    fixture = SimpleNamespace(process=fixture_process, database_url=DSN, container='0' * 64, close=close)

    def main(_args):
        with bench.Run('signal') as run:
            run.fixture()
            (directory / 'scratch').write_text(str(run.root))
            run.run(sys.executable, '-m', 'scripts.tests.benchmarks', '--worker', 'sleep', str(directory / 'child.pid'),
                    timeout=30, env={**os.environ, 'PYTHONPATH': str(ROOT)})
        bench.private_json(directory / 'report.json', {'passed': True})

    with mock.patch.object(testdeps, 'Session', return_value=fixture), \
            mock.patch.object(testdeps, 'signal_group', side_effect=signal_group):
        raise SystemExit(bench.entrypoint(main))


class BenchmarkProcesses(unittest.TestCase):
    def test_capture_reaps_descendants_on_success_failure_and_timeout(self):
        for mode in ('success', 'failure', 'timeout'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory(prefix='fry-benchmark-process-') as directory:
                path = Path(directory) / 'child.pid'
                try:
                    try:
                        bench.command(sys.executable, '-m', 'scripts.tests.benchmarks', '--worker', mode, str(path), timeout=1)
                    except (RuntimeError, subprocess.TimeoutExpired):
                        self.assertNotEqual(mode, 'success')
                    else:
                        self.assertEqual(mode, 'success')
                    self.assertTrue(path.is_file())
                    deadline = time.monotonic() + 2
                    while alive(int(path.read_text())) and time.monotonic() < deadline:
                        time.sleep(.02)
                    self.assertFalse(alive(int(path.read_text())), 'benchmark command left a live descendant')
                finally:
                    if path.exists() and alive(int(path.read_text())):
                        os.kill(int(path.read_text()), signal.SIGKILL)

    def test_signal_paths_finish_cleanup_before_exit_and_never_publish(self):
        for signum in (signal.SIGINT, signal.SIGTERM):
            with self.subTest(signum=signum), tempfile.TemporaryDirectory(prefix='fry-benchmark-signal-') as directory:
                root = Path(directory)
                process = subprocess.Popen([sys.executable, '-m', 'scripts.tests.benchmarks', '--signal-probe', str(root)],
                                           cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
                                           env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
                try:
                    deadline = time.monotonic() + 5
                    while not (root / 'child.pid').exists() and time.monotonic() < deadline:
                        self.assertIsNone(process.poll())
                        time.sleep(.01)
                    self.assertTrue((root / 'child.pid').exists(), 'signal probe never started')
                    os.kill(process.pid, signum)
                    deadline = time.monotonic() + 5
                    while not (root / 'closing').exists() and time.monotonic() < deadline:
                        time.sleep(.01)
                    self.assertTrue((root / 'closing').exists())
                    os.kill(process.pid, signum)  # A second signal cannot skip close.
                    out, err = process.communicate(timeout=5)
                    self.assertEqual(process.returncode, 128 + signum, (out + err).decode())
                    self.assertTrue((root / 'closed').exists())
                    self.assertFalse(Path((root / 'scratch').read_text()).exists())
                    self.assertFalse((root / 'report.json').exists())
                    self.assertFalse(alive(int((root / 'child.pid').read_text())))
                finally:
                    testdeps.signal_group(process, signal.SIGKILL)
                    process.communicate(timeout=2)
                    if (root / 'child.pid').exists() and alive(int((root / 'child.pid').read_text())):
                        os.kill(int((root / 'child.pid').read_text()), signal.SIGKILL)


if __name__ == '__main__':
    if len(sys.argv) == 4 and sys.argv[1] == '--worker':
        worker(sys.argv[2], Path(sys.argv[3]))
    elif len(sys.argv) == 3 and sys.argv[1] == '--signal-probe':
        signal_probe(Path(sys.argv[2]))
    else:
        unittest.main()
