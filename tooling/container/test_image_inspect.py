"""Docker inspection compatibility must preserve explicit platform validation."""
import json
import subprocess
import unittest
from unittest.mock import Mock, patch

from image_inspect import ImageNotFound, docker, inspect_image


class ImageInspectionTests(unittest.TestCase):
    def fixture(self, *, platform_flag=True, apis='1.56 1.56', os_name='linux',
                arch='amd64', selected_arch='arm64'):
        def command(*arguments):
            if arguments == ('image', 'inspect', '--help'):
                return 'Options: --format --platform' if platform_flag else 'Options: --format'
            if arguments[0] == 'version':
                return apis
            if arguments[:2] != ('image', 'inspect'):
                self.fail(f'unexpected Docker mutation: {arguments}')
            if '--platform' in arguments:
                if not platform_flag:
                    raise subprocess.CalledProcessError(125, ['docker', *arguments],
                                                        stderr='unknown flag: --platform')
                return json.dumps([{'Os': os_name, 'Architecture': selected_arch}])
            return json.dumps([{'Os': os_name, 'Architecture': arch}])
        return Mock(side_effect=command)

    def test_modern_cli_selects_foreign_platform_instead_of_default_native_image(self):
        command = self.fixture()
        inspection = inspect_image('fry:fixture', 'arm64', command)
        self.assertEqual(inspection['Architecture'], 'arm64')
        self.assertEqual(command.call_args.args,
                         ('image', 'inspect', '--platform', 'linux/arm64', 'fry:fixture'))

    def test_legacy_cli_can_validate_foreign_metadata_without_host_architecture(self):
        command = self.fixture(platform_flag=False, arch='arm64')
        self.assertEqual(inspect_image('fry:fixture', 'arm64', command)['Architecture'], 'arm64')
        self.assertEqual(command.call_args.args, ('image', 'inspect', 'fry:fixture'))

    def test_old_client_api_old_server_api_and_explicit_api_limit_use_checked_fallback(self):
        for apis in ('1.48 1.56', '1.56 1.48', '1.48 1.48'):
            with self.subTest(apis=apis):
                command = self.fixture(apis=apis)
                self.assertEqual(inspect_image('fry:fixture', 'amd64', command)['Architecture'], 'amd64')
                self.assertEqual(command.call_args.args, ('image', 'inspect', 'fry:fixture'))
                with self.assertRaisesRegex(ValueError, 'platform mismatch'):
                    inspect_image('fry:fixture', 'arm64', command)

    def test_both_inspection_modes_reject_wrong_os_and_wrong_architecture(self):
        for platform_flag in (True, False):
            for os_name, arch in (('windows', 'arm64'), ('linux', 'amd64')):
                with self.subTest(platform_flag=platform_flag, os_name=os_name, arch=arch):
                    command = self.fixture(platform_flag=platform_flag, os_name=os_name,
                                           arch=arch, selected_arch=arch)
                    with self.assertRaisesRegex(ValueError, 'platform mismatch'):
                        inspect_image('fry:fixture', 'arm64', command)

    def test_missing_image_is_distinct_from_other_docker_failures_and_never_retried(self):
        for message, expected in (('Error response from daemon: No such image: fry:fixture', ImageNotFound),
                                  ('permission denied connecting to Docker', subprocess.CalledProcessError),
                                  ('unknown flag: --platform', subprocess.CalledProcessError)):
            with self.subTest(message=message):
                command = self.fixture(platform_flag=False)
                command.side_effect = ['Options: --format',
                                       subprocess.CalledProcessError(1, ['docker', 'image', 'inspect'],
                                                                     stderr=message)]
                with self.assertRaises(expected):
                    inspect_image('fry:fixture', 'amd64', command)
                self.assertEqual(command.call_count, 2)

    def test_capability_probe_failure_stops_before_inspection(self):
        for failure in (subprocess.TimeoutExpired('docker', 30),
                        subprocess.CalledProcessError(1, ['docker'], stderr='permission denied')):
            with self.subTest(failure=failure):
                command = Mock(side_effect=failure)
                with self.assertRaises(type(failure)):
                    inspect_image('fry:fixture', 'amd64', command)
                self.assertEqual(command.call_count, 1)

    def test_supported_platform_inspect_failure_is_not_retried_without_selection(self):
        failure = subprocess.CalledProcessError(1, ['docker', 'image', 'inspect'],
                                                stderr='requested platform is unavailable')
        command = Mock(side_effect=['Options: --platform', '1.56 1.56', failure])
        with self.assertRaises(subprocess.CalledProcessError):
            inspect_image('fry:fixture', 'arm64', command)
        self.assertEqual(command.call_count, 3)
        self.assertEqual(command.call_args.args,
                         ('image', 'inspect', '--platform', 'linux/arm64', 'fry:fixture'))

    def test_invalid_api_versions_and_metadata_fail_closed(self):
        for apis in ('', '1.56', 'bad 1.56', '1.56 bad'):
            with self.subTest(apis=apis), self.assertRaises(ValueError):
                inspect_image('fry:fixture', 'amd64', self.fixture(apis=apis))
        for metadata in ([], [{}, {}], {}, [None], [{}]):
            with self.subTest(metadata=metadata), self.assertRaises(ValueError):
                command = Mock(side_effect=['Options: --format', json.dumps(metadata)])
                inspect_image('fry:fixture', 'amd64', command)

    def test_docker_commands_have_bounded_timeout_and_capture_diagnostics(self):
        with patch('image_inspect.subprocess.check_output', return_value='fixture') as execute:
            self.assertEqual(docker('image', 'inspect', 'fry:fixture'), 'fixture')
        self.assertEqual(execute.call_args.kwargs['timeout'], 30)
        self.assertEqual(execute.call_args.kwargs['stderr'], subprocess.PIPE)


if __name__ == '__main__':
    unittest.main()
