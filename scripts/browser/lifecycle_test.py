"""Focused real-fixture failure/cancellation checks; run after browser setup."""
from contextlib import contextmanager
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

from scripts.browser import run
from scripts.dev import demo_admin
from scripts.lib.processes import exit_signals, reserved_loopback_port

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]


def alive(pid):
    try:
        return Path('/proc', str(pid), 'stat').read_text().split()[2] != 'Z'
    except (FileNotFoundError, ProcessLookupError):
        return False


def descendants(pid):
    try:
        children = Path('/proc', str(pid), 'task', str(pid), 'children').read_text().split()
    except (FileNotFoundError, ProcessLookupError):
        return []
    return [child for value in children for child in (int(value), *descendants(int(value)))]


@contextmanager
def tracked_installation(state):
    sessions = []
    original = demo_admin.Session

    def session():
        value = original()
        sessions.append(value)
        return value

    with reserved_loopback_port() as port:
        admin_port = port
    with mock.patch.object(demo_admin, 'Session', side_effect=session):
        with demo_admin.installation(admin_port, announce=False) as (server, config):
            state.update(server=server.pid, scratch=str(Path(server.args[0]).parent),
                         container=sessions[0].container)
            yield config


def cancellation_probe(directory):
    state = {}
    with exit_signals(), tracked_installation(state):
        (directory / 'installation.json').write_text(json.dumps(state))
        environment = run.browser_environment()
        environment['FRY_BROWSER_READY_FILE'] = str(directory / 'browser.pid')
        run.browser_command(str(HERE / 'fixtures/wait.cjs'), environment, timeout=30)


class Lifecycle(unittest.TestCase):
    def assert_clean(self, state):
        self.assertFalse(alive(state['server']), 'FRY remains alive')
        self.assertFalse(Path(state['scratch']).exists(), 'Demo data/logs remain')
        result = subprocess.run(
            ['docker', 'container', 'ls', '--all', '--quiet', '--filter', 'id=' + state['container']],
            capture_output=True, check=True, timeout=10)
        self.assertFalse(result.stdout.strip(), 'Owned PostgreSQL remains')

    def test_browser_failure_removes_real_demo(self):
        state = {}
        with self.assertRaises(run.BrowserFailure), tracked_installation(state):
            run.browser_command(str(HERE / 'fixtures/fail.cjs'), run.browser_environment(), timeout=5)
        self.assert_clean(state)

    def test_failure_output_hides_raw_credentials_and_reports_nonzero(self):
        for error in (run.BrowserFailure('admin.cjs exited with status 7'),
                      RuntimeError('postgres://private:secret@localhost/private')):
            with self.subTest(error=type(error).__name__), \
                    mock.patch.object(run, 'run', side_effect=error), \
                    mock.patch('sys.stderr', new_callable=io.StringIO) as output:
                self.assertEqual(run.main(), 1)
                self.assertNotIn('secret', output.getvalue())
                self.assertNotIn('postgres://', output.getvalue())

    def test_sigterm_removes_browser_descendants_and_real_demo(self):
        with tempfile.TemporaryDirectory(prefix='fry-browser-lifecycle-') as temporary:
            directory = Path(temporary)
            probe = subprocess.Popen(
                [sys.executable, '-m', 'scripts.browser.lifecycle_test', '--cancel-probe', temporary],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=ROOT, start_new_session=True)
            owned = []
            state = None
            try:
                deadline = time.monotonic() + 60
                while not (directory / 'browser.pid').exists():
                    self.assertIsNone(probe.poll(), 'Cancellation probe exited before readiness')
                    self.assertLess(time.monotonic(), deadline, 'Cancellation probe startup timed out')
                    time.sleep(.05)
                state = json.loads((directory / 'installation.json').read_text())
                owned = descendants(probe.pid)
                self.assertGreaterEqual(len(owned), 3, 'Probe did not start Chromium descendants')
                os.kill(probe.pid, signal.SIGTERM)
                out, err = probe.communicate(timeout=60)
                self.assertEqual(probe.returncode, 143, 'Cancellation status was not preserved')
                self.assertFalse(out or err, 'Cancellation emitted private output')
                self.assertFalse([pid for pid in owned if alive(pid)], 'Owned descendants remain')
                self.assert_clean(state)
            finally:
                # Also bound and clean a red regression without touching other
                # developers' browsers or Docker resources.
                if probe.poll() is None:
                    os.kill(probe.pid, signal.SIGTERM)
                    try:
                        probe.communicate(timeout=60)
                    except subprocess.TimeoutExpired:
                        os.killpg(probe.pid, signal.SIGKILL)
                        probe.communicate(timeout=5)
                for pid in owned:
                    if alive(pid):
                        os.kill(pid, signal.SIGKILL)


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--cancel-probe':
        cancellation_probe(Path(sys.argv[2]))
    else:
        unittest.main()
