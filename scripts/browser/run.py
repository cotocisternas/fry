"""Run both existing Chromium suites with a real, owned disposable demo."""
import os
from pathlib import Path
import subprocess
import sys

from scripts.dev.demo_admin import installation
from scripts.lib.processes import exit_signals, reserved_loopback_port
from scripts.lib.testdeps import capture

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
SUITES = ('admin.cjs', 'member-storage.cjs')


class BrowserFailure(Exception):
    """Only this fixed, locally constructed status is safe for CI output."""


def browser_environment():
    environment = {**os.environ, 'CI': '1',
                   'PLAYWRIGHT_MODULE': str(HERE / 'node_modules/playwright'),
                   'PLAYWRIGHT_BROWSERS_PATH': str(ROOT / '.artifacts/browser/browsers')}
    # The required gate always uses the locked browser, with no interactive
    # inspector, externally injected Node code, or protocol logging of secrets.
    for name in ('CHROMIUM_PATH', 'FRY_DEMO_ORIGIN', 'PWDEBUG', 'DEBUG',
                 'DEBUG_FILE', 'NODE_OPTIONS', 'NODE_PATH'):
        environment.pop(name, None)
    return environment


def browser_command(name, environment, *, timeout):
    result = capture(('node', str(HERE / name)), env=environment, timeout=timeout)
    if result.returncode:
        # Playwright errors can contain filled credentials, HTTP bodies or
        # cookies. Keep raw stdout/stderr private and publish a fixed summary.
        raise BrowserFailure(f'{name} exited with status {result.returncode}')
    return result


def run():
    environment = browser_environment()
    print('Browser gate: checking pinned native Chromium', flush=True)
    result = browser_command('preflight.cjs', environment, timeout=30)
    print(result.stdout.decode().strip(), flush=True)
    print('Browser gate: starting disposable seeded FRY/PostgreSQL', flush=True)
    # The demo needs the public origin before binding. Reserve a random port
    # until startup; a competing bind fails startup instead of testing a fixed
    # external installation. The demo owns a separate random SMTP listener.
    with reserved_loopback_port() as port:
        admin_port = port
    with installation(admin_port, announce=False) as (process, config):
        environment['FRY_DEMO_ORIGIN'] = config['admin']['origin']
        for name in SUITES:
            if process.poll() is not None:
                raise RuntimeError('Disposable FRY exited before ' + name)
            print('Browser gate: running ' + name, flush=True)
            browser_command(name, environment, timeout=200)
            if process.poll() is not None:
                raise RuntimeError('Disposable FRY exited during ' + name)
            print('PASS: ' + name, flush=True)
    print('PASS: both Chromium suites; disposable demo and PostgreSQL removed', flush=True)


def main():
    try:
        with exit_signals():
            run()
    except subprocess.TimeoutExpired:
        print('FAIL: browser command exceeded its deadline; cleanup completed.', file=sys.stderr)
        return 1
    except BrowserFailure as error:
        print('FAIL: ' + str(error) + '; raw logs withheld.', file=sys.stderr)
        return 1
    except Exception as error:
        # Avoid printing demo/fixture logs or exception text carrying a DSN.
        print('FAIL: browser gate (' + type(error).__name__ + '); raw logs withheld.', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
