"""Small lifetime helpers for the demo and executable SMTP rehearsal."""
from contextlib import contextmanager
from pathlib import Path
import re
import signal
import socket
import subprocess
import tempfile
import time

from scripts.lib.testdeps import signal_group


@contextmanager
def exit_signals():
    """Use only from a command's main; restore the caller's handlers afterward."""
    previous = {}

    def interrupted(signum, _frame):
        # A second Ctrl-C must not interrupt child or database cleanup.
        for cancellation in (signal.SIGINT, signal.SIGTERM):
            signal.signal(cancellation, signal.SIG_IGN)
        raise SystemExit(128 + signum)

    try:
        for signum in (signal.SIGINT, signal.SIGTERM):
            previous[signum] = signal.signal(signum, interrupted)
        yield
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)


def stop_group(process, *, timeout=10):
    """Reap the child and kill its remaining descendants, with bounded waits."""
    signal_group(process, signal.SIGTERM)
    exceeded = False
    try:
        process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        exceeded = True
    finally:
        # A successful wrapper may leave children, including ones ignoring TERM.
        signal_group(process, signal.SIGKILL)
        process.wait(timeout=2)
    if exceeded:
        raise TimeoutError(f'Child process did not stop within {timeout} seconds')
    return process.returncode


@contextmanager
def logged_process(command, log_path, *, env=None, cwd=None, stop_timeout=10):
    with Path(log_path).open('wb') as output:
        process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=output,
                                   stderr=output, env=env, cwd=cwd, start_new_session=True)
        try:
            yield process
        finally:
            stop_group(process, timeout=stop_timeout)


def listening_ports(process, log_path, names, *, timeout=10):
    """Wait for all requested FRY listeners, failing promptly on child exit."""
    deadline = time.monotonic() + timeout
    while True:
        log = Path(log_path).read_text(errors='replace')
        status = process.poll()
        if status is not None:
            raise RuntimeError(f'FRY exited with status {status} before startup: ' + log[-4096:])
        ports = {}
        for name in names:
            match = re.search(re.escape(name) + r' listening on 127\.0\.0\.1:(\d+)', log)
            if match:
                ports[name] = int(match[1])
        if len(ports) == len(names):
            return ports
        if time.monotonic() >= deadline:
            raise TimeoutError('FRY did not start: ' + log[-4096:])
        time.sleep(.05)


@contextmanager
def reserved_loopback_port():
    """Hold the self-relay port until immediately before FRY starts.

    FRY needs the relay address in its config and cannot inherit this socket, so
    a small close-to-bind handoff is unavoidable without changing the app.
    """
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        yield reservation.getsockname()[1]


@contextmanager
def scratch_directory(parent, prefix):
    with tempfile.TemporaryDirectory(dir=parent, prefix=prefix,
                                     ignore_cleanup_errors=False) as temporary:
        root = Path(temporary)
        yield root
    if root.exists():
        raise RuntimeError('Temporary scenario files remain after cleanup: ' + str(root))
