"""Docker-free regressions for the container rehearsal's crash fencing check."""
from pathlib import Path
import smtplib
import tempfile
import unittest
from unittest import mock

from scripts.smoke import container_processes as containers

OWNERS = ('member-a|1|previous-a|t\nmember-a|2|current-a|f\n'
          'member-b|1|previous-b|t\nmember-b|2|current-b|f')
SANITIZED = 'command failed; check configuration and service health\n'


class CrashFencing(unittest.TestCase):
    def exercise(self, *, owners_after=OWNERS, restart_code=1, restart_log=SANITIZED):
        with tempfile.TemporaryDirectory(prefix='fry-crash-check-test-') as directory:
            rehearsal = containers.Rehearsal('fry:synthetic', Path(directory))
            restarted = False
            snapshots = []

            def docker(*args):
                nonlocal restarted
                if args == ('start', rehearsal.members[0]):
                    restarted = True
                elif args != ('kill', '--signal', 'KILL', rehearsal.members[0]):
                    self.fail('unexpected Docker operation: ' + repr(args))

            def sql(query):
                if query == ('SELECT member,epoch,instance,closed FROM fry_member_owner '
                             'ORDER BY member,epoch'):
                    rows = owners_after if restarted else OWNERS
                    snapshots.append(rows)
                    return rows
                if query.startswith('SELECT pg_try_advisory_lock('):
                    return 't'
                if query.startswith('SELECT count(*) FROM fry_member_owner WHERE member='):
                    return '1'
                values = {'SELECT count(*) FROM fry_receipt': '2',
                          'SELECT count(*) FROM fry_member_owner WHERE epoch=2 AND NOT closed': '2',
                          'SELECT max(epoch) FROM fry_member_owner': '2',
                          'SELECT count(*) FROM fry_member_owner WHERE NOT closed': '1'}
                self.assertIn(query, values, 'unexpected SQL query')
                return values[query]

            client = mock.Mock()
            client.sendmail.side_effect = smtplib.SMTPDataError(451, b'member unavailable')
            previous_logs = 'listening with mutual TLS\n'
            with mock.patch.object(containers, 'docker', side_effect=docker) as command, \
                    mock.patch.object(rehearsal, 'sql', side_effect=sql), \
                    mock.patch.object(rehearsal, 'wait_exit', side_effect=[137, restart_code]), \
                    mock.patch.object(rehearsal, 'verify') as verify, \
                    mock.patch.object(rehearsal, 'smtp', return_value=client), \
                    mock.patch.object(rehearsal, 'all_logs',
                                      side_effect=[previous_logs, previous_logs + restart_log]), \
                    mock.patch.object(rehearsal, 'stop') as stop:
                rehearsal.crash_check()
            self.assertEqual(snapshots, [OWNERS, OWNERS])
            self.assertEqual(command.call_args_list,
                             [mock.call('kill', '--signal', 'KILL', rehearsal.members[0]),
                              mock.call('start', rehearsal.members[0])])
            verify.assert_called_once_with()
            client.sendmail.assert_called_once()
            client.close.assert_called_once_with()
            self.assertEqual(stop.call_args_list,
                             [mock.call(rehearsal.coordinator), mock.call(rehearsal.members[1])])

    def test_sanitized_failure_preserves_the_existing_owner_generation(self):
        self.exercise()

    def test_refused_restart_must_not_change_owner_instances_or_history(self):
        for owners in (OWNERS.replace('current-a', 'replacement-a'),
                       OWNERS.replace('previous-a|t', 'previous-a|f')):
            with self.subTest(owners=owners), \
                    self.assertRaisesRegex(AssertionError, 'refused restart changed member owner rows'):
                self.exercise(owners_after=owners)

    def test_failed_restart_must_not_have_started_listening(self):
        with self.assertRaisesRegex(AssertionError, 'listening with mutual TLS'):
            self.exercise(restart_log=SANITIZED + 'listening with mutual TLS\n')

    def test_silent_success_is_not_a_refused_restart(self):
        with self.assertRaisesRegex(AssertionError, 'unclean generation was silently adopted'):
            self.exercise(restart_code=0, restart_log='')


if __name__ == '__main__':
    unittest.main()
