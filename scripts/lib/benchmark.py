"""Private benchmark evidence, bounded commands, and verified fixture teardown.

The Go Testcontainers runner remains the sole owner of dependency lifecycle.
Benchmark modules keep their own workloads and report assertions.
"""
from contextlib import contextmanager
import ctypes
import errno
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile

from scripts.lib import testdeps

ROOT = testdeps.ROOT
FAILURE_LIMIT = 65536
SECRETS = ('fry-test-only', 'synthetic-load-only-password')


class UsageError(ValueError):
    pass


def redact(raw, secrets=(), limit=FAILURE_LIMIT):
    if isinstance(raw, bytes):
        raw = raw.decode('utf-8', errors='replace')
    for secret in (*secrets, *SECRETS):
        if secret:
            raw = raw.replace(secret, '<REDACTED>')
    raw = re.sub(r'postgres(?:ql)?://[^\s\"\'<>]+', '<REDACTED>', raw)
    # Decode before returning so a byte cut never retains an invalid UTF-8 tail.
    return raw.encode('utf-8')[-limit:].decode('utf-8', errors='ignore')


def diagnostic(path, secrets=(), limit=FAILURE_LIMIT):
    fallback = 'Benchmark fixture failed before test diagnostics were available.\n'
    if path is None or path.is_symlink() or not path.is_file():
        return fallback
    # Include enough overlap to redact credentials straddling the retained tail.
    overlap = max((len(s.encode()) for s in (*secrets, *SECRETS)), default=0)
    with path.open('rb') as source:
        source.seek(0, os.SEEK_END)
        source.seek(max(0, source.tell() - limit - overlap))
        return redact(source.read(limit + overlap), secrets, limit)


def new_output(value):
    path = Path(value).absolute()
    assert path.parent.is_dir(), 'output parent must exist'
    assert not path.exists() and not path.is_symlink(), 'output must be new'
    return path


def private_json(path, value):
    """Publish complete JSON atomically, without replacing any existing path."""
    path = Path(path)
    descriptor, temporary = tempfile.mkstemp(prefix='.fry-benchmark-report-', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'w') as output:
            json.dump(value, output, indent=2)
            output.write('\n')
            output.flush()
            os.fsync(output.fileno())
        os.link(temporary, path)
    finally:
        os.unlink(temporary)


def public_values(value, secrets=(), forbidden_keys=False):
    """Reject credential-bearing reports rather than silently editing evidence."""
    forbidden = {'args', 'arguments', 'dsn', 'password', 'body', 'raw_message', 'environment'}
    if isinstance(value, dict):
        for key, item in value.items():
            if forbidden_keys:
                assert key.lower() not in forbidden, 'private value must not be retained'
            public_values(key, secrets)
            public_values(item, secrets, forbidden_keys)
    elif isinstance(value, list):
        for item in value:
            public_values(item, secrets, forbidden_keys)
    elif isinstance(value, str):
        assert 'postgres://' not in value and 'postgresql://' not in value, 'fixture credential leaked'
        assert all(not secret or secret not in value for secret in (*secrets, *SECRETS)), 'fixture credential leaked'


def command(*args, timeout=30, env=None, data=None, check=True):
    result = testdeps.capture(testdeps.native_command(args), timeout=timeout, env=env, data=data)
    if check and result.returncode:
        raise RuntimeError(Path(args[0]).name + ' failed: ' + redact(result.stderr, limit=4096))
    return result


def remove_private(path):
    """Bound cleanup independently of fixture/test deadlines and verify removal."""
    command('rm', '-rf', '--', str(path), timeout=10)
    assert not path.exists() and not path.is_symlink(), 'private fixture cleanup failed'


@contextmanager
def signals_ignored():
    previous = {sig: signal.signal(sig, signal.SIG_IGN) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        yield
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)


@contextmanager
def signals_blocked():
    """Defer delivery while registering a newly allocated owned resource."""
    previous = signal.pthread_sigmask(signal.SIG_BLOCK, (signal.SIGINT, signal.SIGTERM))
    try:
        yield
    finally:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous)


@contextmanager
def evidence_directory(source, parent):
    """Keep only validated evidence while the primary scratch tree is removed."""
    path = Path(tempfile.mkdtemp(prefix='.fry-benchmark-evidence-', dir=parent))
    try:
        # The destination is our own empty directory. mv also handles output
        # parents on a different filesystem without unbounded Python copying.
        command('mv', '-T', '--', str(source), str(path), timeout=10)
        yield path
    finally:
        with signals_ignored():
            if path.exists() or path.is_symlink():
                remove_private(path)


def publish_directory(source, target):
    """Atomically move private evidence without clobbering even an empty directory.

    FRY's native scripts require Linux; renameat2 supplies the no-replace directory
    operation Python's rename interface lacks. Both paths are on the output FS.
    """
    library = ctypes.CDLL(None, use_errno=True)
    rename = library.renameat2
    rename.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
    rename.restype = ctypes.c_int
    if rename(-100, os.fsencode(source), -100, os.fsencode(target), 1):
        code = ctypes.get_errno()
        if code == errno.EEXIST:
            raise FileExistsError(code, 'output must be new', str(target))
        raise OSError(code, os.strerror(code), str(target))
    assert not source.exists(), 'private evidence staging directory remains'


class Run:
    """One benchmark's private scratch, child processes, and optional Session."""
    def __init__(self, name, *, failure_directory=None, failure_limit=FAILURE_LIMIT):
        self.name = name
        self.failure_directory = failure_directory
        self.failure_limit = failure_limit
        self.root = None
        self.database = None
        self.children = []
        self.secrets = []
        self.log = None

    def __enter__(self):
        try:
            with signals_blocked():
                self.root = Path(tempfile.mkdtemp(prefix='.fry-' + self.name + '-', dir=ROOT))
            return self
        except BaseException:
            if self.root:
                with signals_ignored():
                    remove_private(self.root)
            raise

    def fixture(self):
        self.database = testdeps.Session()
        self.secrets.append(self.database.database_url)
        return self.database

    def environment(self, **values):
        environment = {**os.environ, **values}
        environment['TMPDIR'] = str(self.root)
        if self.database:
            environment['FRY_TEST_DATABASE_URL'] = self.database.database_url
        return environment

    def run(self, *args, timeout=30, env=None, data=None, log=None, check=True):
        if log:
            self.log = self.root / log
        try:
            result = command(*args, timeout=timeout, env=env, data=data, check=False)
        except subprocess.TimeoutExpired as error:
            if log:
                self.log.write_bytes((error.stdout or b'') + (error.stderr or b''))
            raise TimeoutError(Path(args[0]).name + ' exceeded its deadline') from error
        if log:
            self.log.write_bytes(result.stdout + result.stderr)
        if check and result.returncode:
            detail = redact(result.stdout + result.stderr, self.secrets, self.failure_limit)
            raise RuntimeError(Path(args[0]).name + ' failed: ' + detail)
        return result

    def start(self, *args, env, log):
        self.log = self.root / log
        with self.log.open('wb') as output:
            with signals_blocked():
                child = subprocess.Popen(testdeps.native_command(args), stdin=subprocess.DEVNULL,
                                         stdout=output, stderr=output, env=env, start_new_session=True)
                self.children.append(child)
        return child

    def cleanup(self):
        failures = []
        for child in reversed(self.children):
            try:
                try:
                    testdeps.signal_group(child, signal.SIGTERM)
                    child.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    pass
                finally:
                    testdeps.signal_group(child, signal.SIGKILL)
            except Exception as error:
                failures.append(error)
            finally:
                try:
                    child.wait(timeout=5)
                except Exception as error:
                    failures.append(error)
        if self.database:
            try:
                self.database.close()
            except Exception as error:
                failures.append(error)
            finally:
                # Session bounds its own close; also reap the owned runner group
                # when an early wrapper exit or close failure leaves descendants.
                try:
                    testdeps.signal_group(self.database.process, signal.SIGKILL)
                    self.database.process.wait(timeout=5)
                except Exception as error:
                    failures.append(error)
        return failures

    def __exit__(self, kind, error, traceback):
        with signals_ignored():
            failures = self.cleanup()
            # Read private logs before removal, but retain only a bounded,
            # sanitized tail if either the command or teardown failed.
            try:
                detail = diagnostic(self.log, self.secrets, self.failure_limit)
            except Exception as diagnostic_error:
                failures.append(diagnostic_error)
                detail = 'Benchmark diagnostics unavailable.\n'
            try:
                remove_private(self.root)
            except Exception as cleanup_error:
                failures.append(cleanup_error)
            if error or failures:
                messages = [str(value) for value in (error, *failures) if value is not None]
                detail = redact(detail + '\n' + '\n'.join(messages), self.secrets, self.failure_limit)
                if self.failure_directory:
                    try:
                        self.failure_directory.mkdir(mode=0o700, exist_ok=True)
                        path = self.failure_directory / 'failure.log'
                        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                        with os.fdopen(descriptor, 'w') as output:
                            output.write(detail)
                        print('Sanitized failure diagnostics: ' + str(path), file=sys.stderr)
                    except Exception as diagnostic_error:
                        print(redact(str(diagnostic_error), self.secrets), file=sys.stderr)
                else:
                    print(detail, file=sys.stderr)
                if error is not None:
                    error.benchmark_reported = True
            if failures and error is None:
                failure = RuntimeError('Benchmark fixture cleanup failed: ' + redact(
                    '; '.join(str(value) for value in failures), self.secrets))
                failure.benchmark_reported = True
                raise failure
        return False


def entrypoint(main):
    """Install signal handling only while executing, and preserve 130/143 exits."""
    def interrupted(signum, _frame):
        # A second signal must not interrupt bounded fixture teardown.
        for sig in (signal.SIGINT, signal.SIGTERM):
            signal.signal(sig, signal.SIG_IGN)
        raise SystemExit(128 + signum)

    os.chdir(ROOT)
    os.umask(0o077)
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        try:
            result = main(sys.argv[1:])
            return result or 0
        except UsageError as error:
            print(str(error), file=sys.stderr)
            return 2
        except Exception as error:
            if not getattr(error, 'benchmark_reported', False):
                print(redact(str(error)), file=sys.stderr)
            return 1
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)
