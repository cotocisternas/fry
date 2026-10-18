#!/usr/bin/env python3
"""Disposable real SMTP/IMAP and mTLS-owner scheduling rehearsal.

Reuse the offline rehearsal's process, credential, database and checked-cleanup
fixtures. No backup is claimed here; all traffic and endpoints are synthetic.
"""
import hashlib
from pathlib import Path
import smtplib
import sys
import time
import traceback

from scripts.smoke import offline_restore as fixture


class AutomaticRotation(fixture.Rehearsal):
    def verify_unaccepted_retry(self, body, error):
        if isinstance(error, smtplib.SMTPDataError):
            assert 400 <= error.smtp_code < 500, 'unexpected permanent SMTP rejection'
        else:
            assert isinstance(error, smtplib.SMTPServerDisconnected)
        # A transport failure is generally ambiguous. This isolated fixture can
        # inspect authoritative state before retrying; it does not claim SMTP
        # clients in production can safely infer non-acceptance from a reset.
        deadline = time.monotonic() + 5
        while self.sql("SELECT count(*) FROM fry_attempt WHERE state='preparing'") != '0':
            assert time.monotonic() < deadline, 'unresolved SMTP attempt before retry'
            time.sleep(.05)
        sha = hashlib.sha256(body).hexdigest()
        assert self.sql("SELECT count(*) FROM fry_receipt WHERE encode(sha256,'hex')='" + sha + "'") == '0', 'transport failure followed acceptance; do not blindly retry'

    def execute(self):
        self.prepare()
        # Profiles have not yet been registered by the remote member processes.
        # One lane holds four records; total storage retains spare rewrite space.
        for member in self.member_configs:
            member.update(segment_max_records=4, segment_max_bytes=5 << 20)
        self.config['storage_lifecycle'].update(reclaim_hot=False, rotate_hot=False,
                                                reclaim_interval_seconds=1)
        self.start_all()
        fixture.progress('receiving the first four originals with automatic rotation disabled')
        bodies = [f'Subject: automatic rotation {i}\r\n\r\nExact original {i} across scheduled file rotation.\r\n'.encode()
                  for i in range(12)]
        for body in bodies[:4]:
            self.incoming(body)
        self.mailbox_snapshot(bodies[:4])
        fixture.progress('checking the fifth SMTP transaction against a full file')
        try:
            self.incoming(bodies[4])
        except (smtplib.SMTPDataError, smtplib.SMTPServerDisconnected) as error:
            self.verify_unaccepted_retry(bodies[4], error)
            assert isinstance(error, smtplib.SMTPDataError) and error.smtp_code == 451, 'definite pre-read capacity rejection must return SMTP 451'
            disconnects = int(isinstance(error, smtplib.SMTPServerDisconnected))
        else:
            raise AssertionError('bounded full file accepted without rotation')
        assert self.sql("SELECT count(*) FROM fry_attempt WHERE state='accepted'") == '4'
        assert self.sql("SELECT count(*) FROM fry_member_maintenance WHERE kind='auto-rotate'") == '0', 'rotation enabled without opt-in'

        fixture.progress('enabling scheduled rotation after verified capacity deferral')
        self.stop_process(self.coordinator)
        self.config['storage_lifecycle'].update(reclaim_hot=True, rotate_hot=True)
        self.start_coordinator()
        deferrals = 1
        for body in bodies[4:]:
            deadline = time.monotonic() + 40
            while True:
                try:
                    self.incoming(body)
                    break
                except (smtplib.SMTPDataError, smtplib.SMTPServerDisconnected) as error:
                    self.verify_unaccepted_retry(body, error)
                    deferrals += 1
                    disconnects += int(isinstance(error, smtplib.SMTPServerDisconnected))
                assert time.monotonic() < deadline, 'scheduled rotation did not resume intake'
                time.sleep(.2)
        fixture.progress('twelve originals accepted; verifying mailbox and rotation history')
        self.mailbox_snapshot(bodies)
        assert self.sql("SELECT count(*) FROM fry_attempt WHERE state='accepted'") == '12'
        assert int(self.sql("SELECT count(*) FROM fry_member_maintenance WHERE kind='auto-rotate' AND state='complete'")) >= 4
        assert int(self.sql("SELECT count(*) FROM fry_hot_rewrite WHERE kind='rotate' AND state='complete'")) >= 4
        assert int(self.sql("SELECT count(*) FROM fry_member_file WHERE state='sealed'")) >= 4

        # Stop scheduling normally before independently inspecting each replica.
        # Existing sealed files remain readable when automatic rotation is off.
        self.stop_process(self.coordinator)
        self.config['storage_lifecycle'].update(reclaim_hot=False, rotate_hot=False)
        self.start_coordinator()
        expected = self.mailbox_snapshot(bodies)
        for stopped in (0, 1):
            self.stop_process(self.members[stopped])
            assert self.mailbox_snapshot(bodies) == expected, 'sole replica changed mailbox or originals'
            self.members[stopped] = self.start_member(stopped)
            self.stop_process(self.coordinator)
            self.start_coordinator()
        self.stop_all(expected_closed_owners=4)
        assert self.sql("SELECT count(*) FROM fry_member_profile WHERE enabled AND max_records=4 AND max_file_bytes=5242880 AND lane_count=1") == '2'
        assert self.sql("""SELECT count(*) FROM fry_target t JOIN fry_member_file f
            ON f.member=t.member AND f.file_id=t.incarnation
            WHERE NOT t.bytes_removed AND f.state IN ('current','sealed')
                AND t.capacity_settled AND t.capacity_lane=0""") == '24'
        assert self.sql("""SELECT count(*) FROM fry_member_file_lane l
            JOIN fry_member_file f USING(member,file_id)
            WHERE f.state IN ('current','sealed') AND (
                l.reserved_records<>(SELECT count(*) FROM fry_target t WHERE t.member=l.member AND t.incarnation=l.file_id AND NOT t.bytes_removed)
                OR l.reserved_bytes<>COALESCE((SELECT sum(capacity_reserved_bytes) FROM fry_target t WHERE t.member=l.member AND t.incarnation=l.file_id AND NOT t.bytes_removed),0))""") == '0', 'live lane census differs'
        return dict(passed=True, profile='synthetic-scheduled-automatic-rotation',
                    accepted_originals=12, independent_replicas_verified=2,
                    unaccepted_smtp_attempts=deferrals, smtp_disconnects=disconnects,
                    explicit_temporary_replies=deferrals-disconnects, opt_in_verified=True,
                    current_record_limit=4, current_byte_limit=5 << 20,
                    completed_rotations=int(self.sql("SELECT count(*) FROM fry_hot_rewrite WHERE kind='rotate' AND state='complete'")),
                    postgres_version=self.version,
                    source_sha256={name: fixture.digest(Path(name)) for name in
                        ('scripts/smoke/automatic_rotation.py', 'scripts/smoke/offline_restore.py',
                         'internal/smtpserver/server.go',
                         'cmd/fry/lifecycle.go', 'internal/receiptstore/automatic_rotation_linux.go',
                         'db/schema/receiptstore.sql')},
                    binary_sha256={name: fixture.digest(self.root / 'bin' / name) for name in ('fry', 'fry-member')})


if __name__ == '__main__':
    fixture.Rehearsal = AutomaticRotation
    fixture.progress = lambda message: print('[automatic rotation] ' + message, flush=True)
    try:
        fixture.main()
    except Exception as error:
        message = str(error)
        for private in fixture.PRIVATE_VALUES:
            message = message.replace(private, '<REDACTED>')
        frame = traceback.extract_tb(error.__traceback__)[-1]
        location = f'{Path(frame.filename).name}:{frame.lineno} ({frame.name})'
        print('Automatic rotation rehearsal failed at ' + location + ': ' + type(error).__name__ + ': ' + message.split('\n')[0][:300], file=sys.stderr)
        raise SystemExit(1)
