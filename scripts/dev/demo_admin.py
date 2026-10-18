"""Disposable localhost administration demo with synthetic mail and credentials."""
import argparse
from contextlib import ExitStack, contextmanager
import http.cookiejar
import json
import os
from pathlib import Path
import smtplib
import sys
import urllib.request

from scripts.lib.processes import (exit_signals, listening_ports, logged_process,
                                  reserved_loopback_port, scratch_directory)
from scripts.lib.testdeps import Session, capture, native_command

ROOT = Path(__file__).resolve().parents[2]
IMAP_PASSWORD = 'disposable-demo-imap'
SMTP_PASSWORD = 'disposable-demo-smtp'
ADMIN_PASSWORD = 'disposable-demo-admin'
SUBJECTS = ('Invoice 1042 / example partner', 'Delivery confirmation / example partner')


def http_port(value):
    try:
        port = int(value)
    except (TypeError, ValueError) as error:
        raise ValueError('FRY_DEMO_HTTP_PORT must be an integer from 1024 to 65535') from error
    if not 1024 <= port <= 65535:
        raise ValueError('FRY_DEMO_HTTP_PORT must be an integer from 1024 to 65535')
    return port


def demo_config(root, smtp_port, admin_port):
    config = json.loads((ROOT / 'examples/local-admin.json').read_text())
    address = f'127.0.0.1:{smtp_port}'
    config.update(listen=address, data_directory=str(root / 'data'))
    config['outbound']['relays'][0]['address'] = address
    config['outbound']['submission']['listen'] = '127.0.0.1:0'
    config['imap']['listen'] = '127.0.0.1:0'
    config['admin']['listen'] = f'127.0.0.1:{admin_port}'
    config['admin']['origin'] = f'http://127.0.0.1:{admin_port}'
    return config


def seed_demo(ports, config, *, announce=True):
    with smtplib.SMTP('127.0.0.1', ports['SMTP'], timeout=10) as smtp:
        for subject in SUBJECTS:
            smtp.sendmail('partner@example.net', ['invoices@example.test'],
                          ('From: partner@example.net\r\nTo: invoices@example.test\r\n'
                           f'Subject: {subject}\r\n\r\nSynthetic demonstration message.\r\n').encode())
    origin = config['admin']['origin']
    client = urllib.request.build_opener(
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def post(path, data, csrf=''):
        request = urllib.request.Request(
            origin + '/api/' + path, json.dumps(data).encode(),
            headers={'Origin': origin, 'Content-Type': 'application/json', 'X-FRY-CSRF': csrf})
        with client.open(request, timeout=10) as response:
            return json.load(response)

    who = post('login', {'username': 'operator', 'password': ADMIN_PASSWORD})
    post('organizations', {'id': config['routes'][0]['organization'],
                           'name': 'Example Logistics'}, who['csrf'])
    post('logout', {}, who['csrf'])
    if announce:
        print(f'Disposable FRY demo: {origin}', flush=True)
        print('Fixture login: operator / disposable-demo-admin', flush=True)
        print('Ctrl-C removes all demo data.', flush=True)


@contextmanager
def installation(admin_port, *, announce=True):
    """Own a seeded demo until the caller finishes; check all teardown paths."""
    with ExitStack() as resources:
        session = Session()
        resources.callback(session.close)
        root = resources.enter_context(scratch_directory(ROOT, '.fry-admin-demo.'))
        environment = {**os.environ, 'FRY_DATABASE_URL': session.database_url,
                       'FRY_LOCAL_IMAP_PASSWORD': IMAP_PASSWORD,
                       'FRY_LOCAL_SMTP_PASSWORD': SMTP_PASSWORD,
                       'FRY_LOCAL_ADMIN_PASSWORD': ADMIN_PASSWORD,
                       'FRY_DEMO_HTTP_PORT': str(admin_port)}
        executable = root / 'fry'
        result = capture(native_command(('go', '-C', str(ROOT), 'build', '-o', str(executable),
                                         './cmd/fry')), timeout=180)
        result.check_returncode()
        config_path, log_path = root / 'config.json', root / 'server.log'
        with reserved_loopback_port() as port:
            config = demo_config(root, port, admin_port)
            config_path.write_text(json.dumps(config))
        with logged_process([str(executable), '-init', '-config', str(config_path)],
                            log_path, env=environment, cwd=ROOT) as process:
            ports = listening_ports(process, log_path, ('SMTP', 'administration'), timeout=15)
            seed_demo(ports, config, announce=announce)
            yield process, config


def run(admin_port):
    with installation(admin_port) as (process, _config):
        status = process.wait()
        if status:
            raise RuntimeError(f'FRY demo exited with status {status}')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args(argv)
    try:
        port = http_port(os.environ.get('FRY_DEMO_HTTP_PORT', '8080'))
        with exit_signals():
            run(port)
    except Exception as error:
        print('Administration demo failed: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
