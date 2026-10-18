#!/usr/bin/env python3
"""Exercise advisory quotas through real CLI, SMTP, IMAP and worker processes.

Reuse only the existing disposable process fixture; its credential-specific
verification is not invoked. No existing installation or external mail is used.
"""
import imaplib
import json
import os
from pathlib import Path
import re
import signal
import smtplib
import sys
import tempfile
import time
import traceback

from scripts.smoke import submission_credentials as fixture_module


class QuotaSmoke(fixture_module.Smoke):
    def usage(self):
        result = self.cli('storage-usage')
        for key in ('retained_bytes', 'retained_originals', 'policy_version'):
            assert isinstance(result[key], str) and re.fullmatch(r'[0-9]+', result[key]), 'inexact storage JSON'
        return result

    def await_alert(self, active, limit):
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            usage = self.usage()
            assert usage['limit_bytes'] == limit, 'quota configuration changed'
            if usage['alert_active'] is active and not usage['sample_stale'] and usage['sampled_policy_version'] == usage['policy_version']:
                return usage
            time.sleep(.2)
        raise AssertionError('background quota sample did not catch up')

    def incoming(self, body):
        with smtplib.SMTP('127.0.0.1', self.smtp_port, timeout=10) as client:
            assert client.sendmail('sender@example.net', ['invoices@example.test'], body) == {}, 'advisory quota blocked receipt'

    def mailbox(self):
        port = int(re.search(r'IMAP listening on 127\.0\.0\.1:(\d+)', self.log_path.read_text())[1])
        client = imaplib.IMAP4('127.0.0.1', port, timeout=10)
        assert client.login('invoices', self.imap_password)[0] == 'OK'
        return client

    def verify(self):
        self.start(self.old, initialize=True)
        initial = self.usage()
        assert initial['retained_bytes'] == initial['retained_originals'] == '0'
        assert initial['limit_bytes'] is None and not initial['alert_active']
        self.cli('storage-quota-set', extra=('-limit-bytes', '1'))
        self.await_alert(False, '1')
        originals = [f'Subject: advisory quota {n}\r\n\r\n'.encode() + bytes([65 + n]) * 1000 + b'\r\n' for n in range(3)]
        fixture_module.progress('accepting mail beyond an advisory storage threshold')
        self.incoming(originals[0])
        # Make this policy due immediately after the receipt: no sleeps or clock
        # rewriting to bypass the real background worker's sampling schedule.
        self.cli('storage-quota-set', extra=('-limit-bytes', '1'))
        self.await_alert(True, '1')
        for body in originals[1:]:
            self.incoming(body)
        total = str(sum(map(len, originals)))
        usage = self.usage()
        assert usage['retained_originals'] == '3' and usage['retained_bytes'] == total
        deadline = time.monotonic() + 15
        with self.mailbox() as mail:
            while time.monotonic() < deadline:
                status, count = mail.select('INBOX')
                if status == 'OK' and count == [b'3']:
                    break
                time.sleep(.1)
            else:
                raise AssertionError('accepted mail did not publish')
            status, values = mail.uid('FETCH', '1:*', '(UID BODY.PEEK[])')
            actual = [part[1] for part in values if isinstance(part, tuple)]
            assert status == 'OK' and sorted(actual) == sorted(originals), 'over-quota originals changed'
            assert mail.create('Copies')[0] == 'OK'
            assert mail.uid('COPY', '1', 'Copies')[0] == 'OK'
            after_copy = self.usage()
            assert after_copy['retained_bytes'] == total and after_copy['retained_originals'] == '3', 'COPY counted bytes twice'
            assert mail.uid('STORE', '1:*', '+FLAGS.SILENT', '(\\Deleted)')[0] == 'OK'
            assert mail.expunge()[0] == 'OK'
            assert mail.select('Copies')[0] == 'OK'
            assert mail.uid('STORE', '1:*', '+FLAGS.SILENT', '(\\Deleted)')[0] == 'OK'
            assert mail.expunge()[0] == 'OK'
        retained = self.usage()
        assert retained['retained_bytes'] == total and retained['retained_originals'] == '3', 'mailbox deletion bypassed recovery retention'
        self.cli('storage-quota-set', extra=('-limit-bytes', total))
        self.await_alert(False, total)
        self.cli('storage-quota-set', extra=('-limit-bytes', '1'))
        self.await_alert(True, '1')
        self.cli('storage-quota-set', extra=('-disable-limit',))
        disabled = self.await_alert(False, None)
        fixture_module.progress('checking persisted quota and retained bytes after restart')
        self.stop()
        self.start(self.old)
        restarted = self.usage()
        for key in ('retained_bytes', 'retained_originals', 'limit_bytes', 'policy_version', 'alert_active', 'sampled_policy_version'):
            assert restarted[key] == disabled[key], 'quota restart changed ' + key
        assert self.sql('SELECT count(*) FROM fry_receipt') == '3'
        self.stop()
        self.report = {'passed': True, 'profile': 'local-process-advisory-storage-quotas',
                       'accepted_originals': 3, 'verified_originals': 3, 'retained_bytes': total,
                       'accepted_above_quota': True, 'copy_does_not_duplicate_usage': True,
                       'expunge_preserves_recovery_usage': True, 'equal_threshold_resolves_alert': True,
                       'disable_resolves_alert': True, 'clean_restarts': 1}


def main():
    if len(sys.argv) != 1:
        raise SystemExit('Usage: python3 -m scripts.smoke.storage_quotas')
    os.umask(0o077)
    for number in (signal.SIGTERM, signal.SIGINT):
        signal.signal(number, fixture_module.interrupted)
    with tempfile.TemporaryDirectory(prefix='fry-storage-quota-smoke-') as directory:
        fixture = QuotaSmoke(Path(directory))
        try:
            fixture.prepare()
            fixture.verify()
        except Exception as error:
            frame = traceback.extract_tb(error.__traceback__)[-1]
            print(f'Quota smoke failed at {frame.lineno}: ' + fixture.redact(str(error))[:500], file=sys.stderr)
            raise SystemExit(1)
        finally:
            for number in (signal.SIGTERM, signal.SIGINT):
                signal.signal(number, signal.SIG_IGN)
            try:
                fixture.cleanup()
            except Exception as error:
                print('Quota smoke cleanup failed: ' + fixture.redact(str(error))[:500], file=sys.stderr)
                raise SystemExit(1)
    print(json.dumps(fixture.report, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
