"""Native-script adapters for the shared Go Testcontainers dependency runner.

Each Session owns one dedicated cluster. Restore tests can destroy the source
before creating a target without reimplementing Docker startup or readiness.
"""
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[2]
TOOLCHAIN = str(ROOT / 'scripts/toolchain.sh')
RUNNER = str(ROOT / 'scripts/test-deps.sh')


def image(service='postgres'):
    """Verify an explicitly pulled upstream pin without building or pulling."""
    reference = subprocess.check_output([RUNNER, 'image', service], cwd=ROOT, text=True, timeout=180).strip()
    tagged, digest = reference.rsplit('@', 1)
    assert re.fullmatch(r'sha256:[0-9a-f]{64}', digest), 'dependency image must have a pinned digest'
    inspected = json.loads(subprocess.check_output(
        ['docker', 'image', 'inspect', reference], text=True, timeout=10))[0]
    # Docker may omit its default registry and official-image namespace.
    expected = (tagged.rsplit(':', 1)[0] + '@' + digest).removeprefix('docker.io/').removeprefix('library/')
    known = {value.removeprefix('docker.io/').removeprefix('library/')
             for value in inspected.get('RepoDigests', []) or []}
    assert expected in known, 'local dependency image differs from the pinned upstream digest'
    assert inspected['Os'] == 'linux' and re.fullmatch(r'sha256:[0-9a-f]{64}', inspected['Id']), \
        'local dependency image lacks a Linux content identity'
    return reference


def native_command(args):
    """Route existing subprocess-based Go invocations through the pinned tools."""
    if args and args[0] == 'go':
        return (TOOLCHAIN, 'test-go' if len(args) > 1 and args[1] == 'test' else 'go', *args[1:])
    return args


def submission_result(value):
    # The administration CLI now emits protobuf JSON int64 strings. Normalize
    # only credential versions; storage byte counters retain their wire form.
    accounts = value if isinstance(value, list) else [value]
    for account in accounts:
        if isinstance(account, dict) and 'version' in account:
            raw = account['version']
            assert type(raw) is int or isinstance(raw, str) and re.fullmatch(r'[1-9][0-9]*', raw), 'invalid persisted credential version'
            account['version'] = int(raw)
            assert 1 <= account['version'] <= (1 << 63) - 1, 'invalid persisted credential version'
    return value


def signal_group(process, signum):
    try:
        os.killpg(process.pid, signum)
    except ProcessLookupError:
        pass


def abort_capture(process):
    # A wrapper may exit while a descendant retains stdout/stderr. Signal the
    # entire owned session, then force termination even if SIGTERM is ignored.
    signal_group(process, signal.SIGTERM)
    try:
        process.communicate(timeout=1)
    except subprocess.TimeoutExpired:
        pass
    finally:
        signal_group(process, signal.SIGKILL)
    try:
        process.communicate(timeout=1)
    except subprocess.TimeoutExpired:
        # Escaped descendants must not turn draining inherited pipes into an
        # unbounded wait. The direct child is still reaped with its own bound.
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream:
                stream.close()
        process.wait(timeout=1)


def capture(args, *, data=None, timeout=30, env=None, progress=None):
    process = subprocess.Popen(
        args, stdin=subprocess.PIPE if data is not None else subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, start_new_session=True)
    deadline = time.monotonic() + timeout
    pending = data
    try:
        while True:
            try:
                out, err = process.communicate(pending, timeout=min(20, max(.01, deadline-time.monotonic())))
                # Successful wrappers can also leave quiet background children.
                signal_group(process, signal.SIGKILL)
                return subprocess.CompletedProcess(args, process.returncode, out, err)
            except subprocess.TimeoutExpired:
                pending = None
                if time.monotonic() >= deadline:
                    raise
                if progress:
                    progress()
    except BaseException:
        abort_capture(process)
        raise


class Session:
    def __init__(self):
        self.container = None
        self.database_url = None
        self.closed = False
        self.files = tempfile.TemporaryDirectory(prefix='fry-testdeps-')
        self.log = tempfile.TemporaryFile()
        descriptor = Path(self.files.name) / 'session.json'
        self.process = subprocess.Popen(
            [RUNNER, 'run', '--', sys.executable, '-m', 'scripts.lib.testdeps', '--hold', str(descriptor)],
            cwd=ROOT, stdin=subprocess.PIPE, stdout=self.log, stderr=self.log,
            start_new_session=True)
        try:
            deadline = time.monotonic() + 180
            while not descriptor.exists():
                if self.process.poll() is not None:
                    raise RuntimeError('Testcontainers fixture failed: ' + self.diagnostic())
                if time.monotonic() >= deadline:
                    raise TimeoutError('Testcontainers fixture startup exceeded 180 seconds')
                time.sleep(.05)
            value = json.loads(descriptor.read_text())
            self.container = value['container']
            self.database_url = value['database_url']
            assert re.fullmatch(r'[0-9a-f]{64}', self.container), 'invalid fixture container ID'
            endpoint = urlsplit(self.database_url)
            assert endpoint.hostname == '127.0.0.1' and endpoint.port, 'fixture must use numeric loopback'
            assert endpoint.path == '/fry_test', 'unexpected fixture database'
        except BaseException as error:
            try:
                self.close()
            except Exception as cleanup_error:
                raise RuntimeError(str(error) + '; ' + str(cleanup_error)) from error
            raise

    def diagnostic(self):
        self.log.seek(0, os.SEEK_END)
        self.log.seek(max(0, self.log.tell() - 4096))
        detail = self.log.read(4096).decode(errors='replace').replace('fry-test-only', '<REDACTED>')
        return re.sub(r'postgres(?:ql)?://[^\s]+', '<REDACTED>', detail)

    def close(self):
        if self.closed:
            return
        # EOF releases the holder. The Go runner owns synchronous termination,
        # volume removal and cleanup errors, including on failed test commands.
        if self.process.stdin and not self.process.stdin.closed:
            self.process.stdin.close()
        try:
            try:
                status = self.process.wait(timeout=45)
            except subprocess.TimeoutExpired:
                signal_group(self.process, signal.SIGTERM)
                try:
                    self.process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    signal_group(self.process, signal.SIGKILL)
                    self.process.wait(timeout=5)
                raise RuntimeError('Testcontainers fixture cleanup exceeded its deadline')
            if status:
                raise RuntimeError('Testcontainers fixture runner/cleanup failed: ' + self.diagnostic())
            if self.container:
                remaining = subprocess.run(
                    ['docker', 'container', 'ls', '--all', '--no-trunc', '--format', '{{.ID}}',
                     '--filter', 'id=' + self.container],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=True)
                assert not remaining.stdout.strip(), 'database still exists after Testcontainers cleanup'
            self.closed = True
        finally:
            self.log.close()
            self.files.cleanup()


def hold(descriptor):
    value = {'container': os.environ['FRY_TEST_POSTGRES_CONTAINER'],
             'database_url': os.environ['FRY_TEST_DATABASE_URL']}
    path = Path(descriptor)
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(value))
    temporary.chmod(0o600)
    temporary.replace(path)
    # No server lifetime is hidden in Python: the runner terminates PostgreSQL
    # as soon as the owning test closes this pipe or exits.
    sys.stdin.buffer.read()


if __name__ == '__main__':
    if len(sys.argv) != 3 or sys.argv[1] != '--hold':
        raise SystemExit('Internal use: python3 -m scripts.lib.testdeps --hold DESCRIPTOR')
    hold(sys.argv[2])
