"""Image-check cleanup must succeed before publishing a passing result."""
from contextlib import redirect_stdout
import io
import json
import subprocess
import tarfile
import unittest
from unittest.mock import patch

import check


class CleanupTests(unittest.TestCase):
    def fixture_docker(self, cleanup_failure=None):
        events = []
        inspection = {'Id': 'sha256:fixture', 'Os': 'linux', 'Architecture': 'amd64',
                      'Config': {'User': '65532:65532', 'WorkingDir': '/var/lib/fry',
                                 'Entrypoint': ['/usr/local/bin/fry'], 'Cmd': ['-help'],
                                 'StopSignal': 'SIGTERM'}}

        def docker(*arguments, timeout=30):
            events.append(arguments)
            if arguments[:2] == ('image', 'inspect'):
                return json.dumps([inspection])
            if arguments[0] == 'export':
                with tarfile.open(arguments[2], 'w') as archive:
                    directory = tarfile.TarInfo('var/lib/fry')
                    directory.type = tarfile.DIRTYPE
                    directory.mode = 0o700
                    directory.uid = directory.gid = 65532
                    archive.addfile(directory)
                    paths = [('etc/ssl/certs/ca-certificates.crt', 0o644)]
                    paths += [(f'usr/share/fry/licenses/{module}/LICENSE', 0o644)
                              for module in (check.MODULE, 'github.com/emersion/go-smtp',
                                             'github.com/emersion/go-imap/v2', 'go')]
                    paths += [(f'usr/local/bin/{binary}', 0o555) for binary in check.BINARIES]
                    for path, mode in paths:
                        member = tarfile.TarInfo(path)
                        member.mode = mode
                        member.size = 7
                        archive.addfile(member, io.BytesIO(b'fixture'))
            if arguments[0] == 'rm':
                command_container = any(arguments[-1].endswith('-' + binary) for binary in check.BINARIES)
                if cleanup_failure == ('command' if command_container else 'export'):
                    raise subprocess.CalledProcessError(1, ['docker', *arguments])
            return ''

        return docker, events

    def run_check(self, failure=None):
        docker, events = self.fixture_docker(failure)
        output = io.StringIO()
        with patch.object(check, 'docker', side_effect=docker), \
                patch.object(check, 'verify_elf'), \
                patch.object(check.platform, 'machine', return_value='x86_64'), \
                redirect_stdout(output):
            if failure:
                with self.assertRaises(subprocess.CalledProcessError):
                    check.check('fry:fixture', 'amd64')
                self.assertNotIn('checks passed', output.getvalue())
            else:
                def record_print(*args, **kwargs):
                    events.append(('print', *args))
                with patch('builtins.print', side_effect=record_print):
                    check.check('fry:fixture', 'amd64')
        return events

    def test_export_container_cleanup_failure_cannot_report_success(self):
        events = self.run_check('export')
        self.assertEqual(events[-1][0], 'rm')

    def test_command_cleanup_failure_still_removes_export_container(self):
        events = self.run_check('command')
        self.assertEqual([event[0] for event in events[-2:]], ['rm', 'rm'])
        self.assertFalse(events[-1][-1].endswith('-fry'))

    def test_success_is_printed_only_after_all_owned_containers_are_removed(self):
        events = self.run_check()
        self.assertEqual(sum(event[0] == 'rm' for event in events), 5)
        self.assertEqual([event[0] for event in events[-2:]], ['rm', 'print'])
        self.assertIn('checked cleanup', events[-1][1])


if __name__ == '__main__':
    unittest.main()
