"""Go test logs remain live, readable, and subordinate to the test exit code."""

import io
import json
import os
import selectors
import subprocess
import sys
import unittest

from scripts.lib.go_test_output import render


class GoTestOutput(unittest.TestCase):
    def test_output_arrives_before_input_closes(self):
        process = subprocess.Popen(
            [sys.executable, '-m', 'scripts.lib.go_test_output'],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
        )
        try:
            event = {'Action': 'output', 'Package': 'example/admin', 'Output': 'failure detail\n'}
            process.stdin.write((json.dumps(event) + '\n').encode())
            process.stdin.flush()
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                self.assertTrue(selector.select(timeout=2), 'output was buffered until EOF')
                self.assertEqual(os.read(process.stdout.fileno(), 4096), b'[example/admin] failure detail\n')
            self.assertIsNone(process.poll(), 'renderer stopped before input ended')
            process.stdin.close()
            self.assertEqual(process.wait(timeout=3), 0)
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=3)
            process.stdin.close()
            process.stdout.close()

    def test_build_output_and_stack_lines_are_preserved(self):
        events = [
            {'Action': 'start', 'Package': 'example/admin'},
            {'Action': 'build-output', 'ImportPath': 'example/admin', 'Output': 'compile failed\n'},
            {'Action': 'output', 'Package': 'example/admin', 'Output': 'panic\n\tstack frame\n'},
        ]
        result = io.StringIO()
        render((json.dumps(event) for event in events), result)
        self.assertEqual(result.getvalue(),
                         '[example/admin] compile failed\n[example/admin] panic\n[example/admin] \tstack frame\n')

    def test_malformed_events_preserve_later_diagnostics_and_fail(self):
        for malformed in ['invalid event', 'null', '[]', '{"Output": 42}']:
            with self.subTest(malformed=malformed):
                event = json.dumps({'Package': 'example/admin', 'Output': 'panic\n\tstack frame\n'})
                result = subprocess.run(
                    [sys.executable, '-m', 'scripts.lib.go_test_output'],
                    input=malformed + '\n' + event + '\n',
                    capture_output=True, text=True, timeout=5, check=False,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, malformed + '\n[example/admin] panic\n'
                                 '[example/admin] \tstack frame\n')

    def test_pipeline_preserves_test_failure(self):
        event = json.dumps({'Action': 'output', 'Package': 'example/admin', 'Output': 'FAIL\n'})
        result = subprocess.run(
            ['bash', '-c', 'set -o pipefail; (printf "%s\\n" "$1"; exit 23) | '
             '"$2" -m scripts.lib.go_test_output', 'go-output-test', event, sys.executable],
            capture_output=True, text=True, timeout=5, check=False,
        )
        self.assertEqual(result.returncode, 23, result.stderr)
        self.assertEqual(result.stdout, '[example/admin] FAIL\n')


if __name__ == '__main__':
    unittest.main()
