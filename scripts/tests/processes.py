#!/usr/bin/env python3
"""Docker-free regressions for bounded script command timeout/descendant cleanup."""
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

from scripts.smoke import offline_restore, submission_credentials

ROOT = Path(__file__).resolve().parents[2]
CHILD = """
import os, pathlib, signal, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
pathlib.Path(sys.argv[1]).write_text(str(os.getpid()))
print('grandchild owns output pipe', flush=True)
time.sleep(60)
"""
PARENT_EXIT = """
import pathlib, subprocess, sys, time
subprocess.Popen([sys.executable, '-c', sys.argv[1], sys.argv[2]], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
while not pathlib.Path(sys.argv[2]).exists(): time.sleep(.01)
"""
PARENT = """
import subprocess, sys, time
subprocess.Popen([sys.executable, '-c', sys.argv[1], sys.argv[2]])
time.sleep(60)
"""


def alive(pid):
    try:
        return Path('/proc', str(pid), 'stat').read_text().split()[2] != 'Z'
    except (FileNotFoundError, ProcessLookupError):
        return False


def probe(name, path):
    successful = name.endswith('-success')
    name = name.removesuffix('-success')
    fixture = {'offline-restore': offline_restore,
               'submission-credentials': submission_credentials}[name]
    try:
        fixture.run(sys.executable, '-c', PARENT_EXIT if successful else PARENT, CHILD, str(path), timeout=1)
    except (subprocess.TimeoutExpired, RuntimeError):
        pass
    else:
        assert successful, 'command unexpectedly completed'
    assert path.is_file(), 'resistant descendant never started'
    pid = int(path.read_text())
    deadline = time.monotonic() + 2
    while alive(pid) and time.monotonic() < deadline:
        time.sleep(.02)
    assert not alive(pid), 'completed/timed-out command left a live descendant'


class PackageImports(unittest.TestCase):
    def test_discovery_imports_without_starting_rehearsals(self):
        # Use a fresh interpreter so cached fixtures cannot conceal import-time
        # work. Discover the packages themselves, without a list of moved files.
        source = '''
import importlib
from pathlib import Path
import pkgutil
import signal
import sys
from types import ModuleType
from unittest import mock

import scripts

def reject_work(event, args):
    if event in {'subprocess.Popen', 'socket.__new__', 'os.system',
                 'os.posix_spawn', 'os.fork', 'os.mkdir'}:
        raise AssertionError('package import started work: ' + event)

sys.addaudithook(reject_work)
root = Path(scripts.__file__).resolve().parent
with mock.patch.object(signal, 'signal', side_effect=AssertionError('import installed signal handler')):
    modules = [importlib.import_module(info.name)
               for info in pkgutil.walk_packages(scripts.__path__, 'scripts.')]
assert modules, 'package discovery found no modules'
for module in modules:
    for value in vars(module).values():
        if isinstance(value, ModuleType) and getattr(value, '__file__', None):
            if Path(value.__file__).resolve().is_relative_to(root):
                assert value is sys.modules.get(value.__name__), 'fixture loaded outside package graph'
'''
        result = subprocess.run(
            [sys.executable, '-c', source], cwd=ROOT, capture_output=True, timeout=10,
            env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        self.assertEqual(result.returncode, 0,
                         (result.stdout + result.stderr).decode(errors='replace'))


class ProcessCleanup(unittest.TestCase):
    def test_offline_timeout_cleans_resistant_descendant(self):
        self.check_probe('offline-restore')

    def test_submission_timeout_cleans_resistant_descendant(self):
        self.check_probe('submission-credentials')

    def test_successful_commands_also_clean_background_descendants(self):
        for name in ('offline-restore-success', 'submission-credentials-success'):
            with self.subTest(name=name):
                self.check_probe(name)

    def check_probe(self, name):
        with tempfile.TemporaryDirectory(prefix='fry-process-regression-') as directory:
            path = Path(directory) / 'child.pid'
            process = subprocess.Popen(
                [sys.executable, '-m', 'scripts.tests.processes', '--probe', name, str(path)],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
                cwd=ROOT, env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
            try:
                out, err = process.communicate(timeout=10)
                self.assertEqual(process.returncode, 0, (out + err).decode(errors='replace'))
            finally:
                # The regression itself also cleans descendants when run red.
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                try:
                    process.communicate(timeout=2)
                except subprocess.TimeoutExpired:
                    if process.stdout: process.stdout.close()
                    if process.stderr: process.stderr.close()
                    process.wait(timeout=2)
                if path.exists():
                    pid = int(path.read_text())
                    if alive(pid):
                        os.kill(pid, signal.SIGKILL)


if __name__ == '__main__':
    if len(sys.argv) == 4 and sys.argv[1] == '--probe':
        probe(sys.argv[2], Path(sys.argv[3]))
    else:
        unittest.main()
