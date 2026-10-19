"""Exercise CI's exact Git diff classification and required-check decision."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest
from unittest import mock

from scripts.ci import changes


ROOT = Path(__file__).resolve().parents[2]


class Changes(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.repo = Path(self.temporary.name)
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.name', 'CI fixture')
        self.git('config', 'user.email', 'ci@example.invalid')
        empty_hooks = self.repo / '.git' / 'empty-hooks'
        empty_hooks.mkdir()
        self.git('config', '--local', 'commit.gpgsign', 'false')
        self.git('config', '--local', 'core.hooksPath', str(empty_hooks))
        self.write('README.md', 'original\n')
        self.write('docs/old.md', 'old\n')
        self.write('app.py', 'print(1)\n')
        self.first = self.commit()

    def git(self, *args):
        return subprocess.run(['git', *args], cwd=self.repo, check=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              timeout=15).stdout.decode().strip()

    def write(self, name, content):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def commit(self):
        self.git('add', '-A')
        self.git('commit', '-qm', 'fixture')
        return self.git('rev-parse', 'HEAD')

    @staticmethod
    def pr(base, head):
        return {'pull_request': {'base': {'sha': base}, 'head': {'sha': head}}}

    @staticmethod
    def push(before, after):
        return {'before': before, 'after': after}

    def test_docs_only_including_new_deleted_and_many_paths(self):
        self.write('README.md', 'updated\n')
        self.write('docs/reference/deep.md', 'new\n')
        self.write('scripts/README.md', 'scripts\n')
        self.write('tooling/native/README.md', 'tooling\n')
        self.write('deploy/aws/README.md', 'deploy\n')
        (self.repo / 'docs/old.md').unlink()
        for index in range(305):
            self.write(f'docs/batch/{index:03}.md', 'note\n')
        head = self.commit()
        self.assertFalse(changes.needs_full(self.repo, 'pull_request', self.pr(self.first, head)))
        self.assertFalse(changes.needs_full(self.repo, 'push', self.push(self.first, head)))

    def test_code_and_mixed_changes_require_full(self):
        self.write('app.py', 'print(2)\n')
        code = self.commit()
        self.assertTrue(changes.needs_full(self.repo, 'push', self.push(self.first, code)))
        self.write('README.md', 'updated\n')
        mixed = self.commit()
        self.assertTrue(changes.needs_full(self.repo, 'push', self.push(self.first, mixed)))
        self.assertFalse(changes.needs_full(self.repo, 'push', self.push(code, mixed)))

    def test_other_markdown_and_non_markdown_under_docs_require_full(self):
        for path in ('third_party/LICENSE.md', 'generated/README.md',
                     'notes.md', 'docs/index.html', 'docs/tool.py', 'docs/data.json'):
            with self.subTest(path=path):
                before = self.git('rev-parse', 'HEAD')
                self.write(path, 'changed\n')
                after = self.commit()
                self.assertTrue(changes.needs_full(self.repo, 'push', self.push(before, after)))

    def test_rename_from_code_to_markdown_requires_full(self):
        self.write('docs/source.py', 'code\n')
        before = self.commit()
        (self.repo / 'docs/source.py').rename(self.repo / 'docs/source.md')
        after = self.commit()
        self.assertTrue(changes.needs_full(self.repo, 'pull_request', self.pr(before, after)))

    def test_executable_and_symlink_markdown_require_full(self):
        (self.repo / 'README.md').chmod(0o755)
        executable = self.commit()
        self.assertTrue(changes.needs_full(self.repo, 'push', self.push(self.first, executable)))
        (self.repo / 'docs/link.md').symlink_to('../README.md')
        linked = self.commit()
        self.assertTrue(changes.needs_full(self.repo, 'push', self.push(executable, linked)))

    def test_pr_uses_merge_base_instead_of_base_tip(self):
        self.git('checkout', '-qb', 'docs')
        self.write('docs/old.md', 'docs branch\n')
        head = self.commit()
        self.git('checkout', '-q', 'main')
        self.write('app.py', 'main branch code\n')
        base_tip = self.commit()
        self.assertFalse(changes.needs_full(self.repo, 'pull_request', self.pr(base_tip, head)))
        self.assertTrue(changes.needs_full(self.repo, 'push', self.push(head, base_tip)))

    def test_dispatch_initial_push_and_missing_commit_run_full(self):
        self.assertTrue(changes.needs_full(self.repo, 'workflow_dispatch', {}))
        self.assertTrue(changes.needs_full(
            self.repo, 'push', self.push('0' * 40, self.first)))
        self.assertTrue(changes.needs_full(
            self.repo, 'push', self.push('a' * 40, self.first)))
        self.assertTrue(changes.needs_full(
            self.repo, 'pull_request', self.pr('a' * 40, self.first)))
        self.assertTrue(changes.needs_full(
            self.repo, 'push', self.push(self.first, self.first)))

    def test_inherited_signing_and_hooks_do_not_run(self):
        hooks = self.repo / '.git' / 'inherited-hooks'
        hooks.mkdir()
        hook_marker = self.repo / '.git' / 'hook-ran'
        signing_marker = self.repo / '.git' / 'signing-ran'
        hook = hooks / 'pre-commit'
        hook.write_text(f'#!/bin/sh\ntouch "{hook_marker}"\nexit 91\n')
        hook.chmod(0o755)
        signer = self.repo / '.git' / 'inherited-gpg'
        signer.write_text(f'#!/bin/sh\ntouch "{signing_marker}"\nexit 92\n')
        signer.chmod(0o755)
        global_config = self.repo / '.git' / 'inherited.gitconfig'
        self.git('config', '--file', str(global_config), 'commit.gpgsign', 'true')
        self.git('config', '--file', str(global_config), 'core.hooksPath', str(hooks))
        self.git('config', '--file', str(global_config), 'gpg.program', str(signer))
        with mock.patch.dict(os.environ, {'GIT_CONFIG_GLOBAL': str(global_config)}):
            self.assertEqual(self.git('config', '--global', '--get', 'commit.gpgsign'), 'true')
            self.assertEqual(self.git('config', '--global', '--get', 'core.hooksPath'), str(hooks))
            self.write('docs/old.md', 'new\n')
            head = self.commit()
            self.assertFalse(changes.needs_full(
                self.repo, 'push', self.push(self.first, head)))
        self.assertFalse(hook_marker.exists())
        self.assertFalse(signing_marker.exists())

    def test_malformed_event_fails_without_output(self):
        output = self.repo / 'output'
        event = self.repo / 'event.json'
        environment = dict(os.environ, PYTHONPATH=str(ROOT))
        for payload in ('{', '{}', json.dumps(self.push('bad', self.first))):
            with self.subTest(payload=payload):
                event.write_text(payload)
                result = subprocess.run(
                    ['python3', '-m', 'scripts.ci.changes', 'push', str(event), str(output)],
                    cwd=self.repo, env=environment, capture_output=True, text=True,
                    timeout=15)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())

    def test_cli_writes_controlled_output(self):
        self.write('docs/old.md', 'updated\n')
        head = self.commit()
        output = self.repo / 'output'
        event = self.repo / 'event.json'
        event.write_text(json.dumps(self.pr(self.first, head)))
        subprocess.run(
            ['python3', '-m', 'scripts.ci.changes', 'pull_request', str(event), str(output)],
            cwd=self.repo, env=dict(os.environ, PYTHONPATH=str(ROOT)), check=True,
            capture_output=True, timeout=15)
        self.assertEqual(output.read_text(), 'full=false\n')


class RequiredCheck(unittest.TestCase):
    def test_final_job_rejects_failed_upstream_or_wrong_decision(self):
        workflow = (ROOT / '.github/workflows/ci.yml').read_text().splitlines()
        start = workflow.index('      - name: Check decision')
        run = workflow.index('        run: |', start)
        body = []
        for line in workflow[run + 1:]:
            if not line.startswith('          '):
                break
            body.append(line)
        script = textwrap.dedent('\n'.join(body))
        self.assertTrue(script)
        cases = (
            ('success', 'true', 'success', True),
            ('success', 'false', 'skipped', True),
            ('failure', 'false', 'skipped', False),
            ('cancelled', 'false', 'skipped', False),
            ('success', '', 'skipped', False),
            ('success', 'true', 'skipped', False),
            ('success', 'true', 'failure', False),
            ('success', 'false', 'success', False),
            ('success', 'false', 'cancelled', False),
        )
        for changes_result, full, validate_result, expected in cases:
            with self.subTest(case=(changes_result, full, validate_result)):
                env = dict(os.environ, CHANGES_RESULT=changes_result, FULL=full,
                           VALIDATE_RESULT=validate_result)
                result = subprocess.run(['bash', '-eo', 'pipefail', '-c', script],
                                        env=env, timeout=15)
                self.assertEqual(result.returncode == 0, expected)


if __name__ == '__main__':
    unittest.main()
