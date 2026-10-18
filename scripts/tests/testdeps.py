"""Docker-free checks for upstream image identity and explicit preparation."""
import json
import subprocess
import unittest
from unittest import mock

from scripts.lib import testdeps

DIGEST = 'sha256:' + 'a' * 64
PIN = 'docker.io/library/postgres:18.6-trixie@' + DIGEST


def local_image(**overrides):
    return {'RepoDigests': ['postgres@' + DIGEST], 'Id': 'sha256:' + 'b' * 64,
            'Os': 'linux', **overrides}


class UpstreamImages(unittest.TestCase):
    def test_accepts_upstream_without_fry_recipe_labels(self):
        for known in ('postgres@', 'library/postgres@', 'docker.io/library/postgres@'):
            with self.subTest(known=known), mock.patch.object(
                    testdeps.subprocess, 'check_output',
                    side_effect=[PIN + '\n', json.dumps([local_image(RepoDigests=[known + DIGEST])])]) as command:
                self.assertEqual(testdeps.image(), PIN)
                self.assertEqual([call.args[0] for call in command.call_args_list],
                                 [[testdeps.RUNNER, 'image', 'postgres'], ['docker', 'image', 'inspect', PIN]])

    def test_rejects_wrong_or_missing_upstream_provenance(self):
        for known in ([], None, ['postgres@sha256:' + 'c' * 64],
                      ['example.test/postgres@' + DIGEST], ['another/postgres@' + DIGEST]):
            with self.subTest(known=known), mock.patch.object(
                    testdeps.subprocess, 'check_output',
                    side_effect=[PIN, json.dumps([local_image(RepoDigests=known)])]), \
                    self.assertRaisesRegex(AssertionError, 'pinned upstream digest'):
                testdeps.image()

    def test_requires_linux_immutable_local_identity(self):
        for overrides in ({'Id': 'postgres:18'}, {'Os': 'windows'}):
            with self.subTest(overrides=overrides), mock.patch.object(
                    testdeps.subprocess, 'check_output',
                    side_effect=[PIN, json.dumps([local_image(**overrides)])]), \
                    self.assertRaisesRegex(AssertionError, 'Linux content identity'):
                testdeps.image()

    def test_missing_image_fails_without_implicit_pull(self):
        missing = subprocess.CalledProcessError(1, ['docker', 'image', 'inspect', PIN])
        with mock.patch.object(testdeps.subprocess, 'check_output', side_effect=[PIN, missing]) as command, \
                self.assertRaises(subprocess.CalledProcessError):
            testdeps.image()
        self.assertEqual([call.args[0] for call in command.call_args_list],
                         [[testdeps.RUNNER, 'image', 'postgres'], ['docker', 'image', 'inspect', PIN]])


if __name__ == '__main__':
    unittest.main()
