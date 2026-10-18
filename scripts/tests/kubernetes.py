"""Docker-free check that the Kubernetes security hold precedes side effects."""
import sys
import unittest
from unittest import mock

from scripts.smoke import kubernetes


class SecurityHold(unittest.TestCase):
    def test_entrypoint_stops_before_commands_or_resource_creation(self):
        with mock.patch.object(kubernetes, 'NODE', None), \
                mock.patch.object(sys, 'argv', ['kubernetes', 'fry:local-test']), \
                mock.patch.object(kubernetes, 'execute') as execute, \
                mock.patch.object(kubernetes.shutil, 'which') as which, \
                mock.patch.object(kubernetes.tempfile, 'TemporaryDirectory') as temporary, \
                mock.patch.object(kubernetes, 'Rehearsal') as rehearsal, \
                self.assertRaisesRegex(SystemExit, r'Security hold: Kubernetes >=1\.37\.1'):
            kubernetes.main()
        execute.assert_not_called()
        which.assert_not_called()
        temporary.assert_not_called()
        rehearsal.assert_not_called()


if __name__ == '__main__':
    unittest.main()
