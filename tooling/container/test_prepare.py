"""Focused packaging gates: foreign/dynamic binaries, stale output, and notices."""
import importlib.util
import json
import os
from pathlib import Path
import struct
import subprocess
import shutil
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('container_prepare', Path(__file__).with_name('prepare.py'))
prepare = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(prepare)


def elf(arch='amd64', segment=1):
    header = bytearray(64)
    header[:6] = b'\x7fELF\x02\x01'
    struct.pack_into('<H', header, 18, {'amd64': 62, 'arm64': 183}[arch])
    struct.pack_into('<Q', header, 32, 64)
    struct.pack_into('<HH', header, 54, 56, 1)
    entry = bytearray(56)
    struct.pack_into('<I', entry, 0, segment)
    return header + entry


class PackagingTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.context = Path(self.temporary.name)

    def binary(self, contents):
        path = self.context / 'command'
        path.write_bytes(contents)
        return path

    def context_fixture(self):
        runtime = self.context / 'runtime/linux-amd64'
        (runtime / 'bin').mkdir(parents=True)
        (runtime / 'licenses' / prepare.MODULE).mkdir(parents=True)
        (runtime / 'licenses' / prepare.MODULE / 'LICENSE').write_text('FRY license')
        (runtime / 'data').mkdir(mode=0o700)
        for binary in prepare.BINARIES:
            path = runtime / 'bin' / binary
            path.write_bytes(elf())
            path.chmod(0o555)
        (self.context / 'Dockerfile').write_text('packaging recipe')
        (self.context / '.dockerignore').write_text('output allowlist')
        return runtime

    def test_static_elf_for_both_architectures(self):
        for arch in ('amd64', 'arm64'):
            with self.subTest(arch=arch):
                prepare.verify_elf(self.binary(elf(arch)), arch)

    def test_foreign_architecture_cannot_package(self):
        with self.assertRaisesRegex(ValueError, 'architecture mismatch'):
            prepare.verify_elf(self.binary(elf('arm64')), 'amd64')

    def test_dynamic_linking_cannot_package(self):
        for segment in (2, 3):
            with self.subTest(segment=segment), self.assertRaisesRegex(ValueError, 'dynamic linking'):
                prepare.verify_elf(self.binary(elf(segment=segment)), 'amd64')

    def test_malformed_binary_cannot_package(self):
        for contents in (b'#!/bin/sh\n', elf()[:-1]):
            with self.subTest(contents=contents[:6]), self.assertRaises(ValueError):
                prepare.verify_elf(self.binary(contents), 'amd64')

    def test_source_or_secret_in_context_is_rejected(self):
        self.context_fixture()
        for name in ('go.mod', '.env'):
            path = self.context / name
            path.write_text('must not enter Docker')
            with self.subTest(name=name), self.assertRaises(ValueError):
                prepare.verify_context(self.context, 'amd64')
            path.unlink()

    def test_only_complete_four_binary_context_is_accepted(self):
        runtime = self.context_fixture()
        prepare.verify_context(self.context, 'amd64')
        (runtime / 'bin/fry-admin').unlink()
        with self.assertRaisesRegex(ValueError, 'four commands'):
            prepare.verify_context(self.context, 'amd64')

    def test_data_payload_or_symlink_is_rejected(self):
        runtime = self.context_fixture()
        path = runtime / 'data/secret'
        path.write_text('must not package data')
        with self.assertRaisesRegex(ValueError, 'unexpected packaging input'):
            prepare.verify_context(self.context, 'amd64')
        path.unlink()
        (runtime / 'licenses/leak').symlink_to('/etc/passwd')
        with self.assertRaisesRegex(ValueError, 'symlink'):
            prepare.verify_context(self.context, 'amd64')

    def test_reusing_prepared_outputs_is_rejected_before_go_runs(self):
        self.context_fixture()
        with patch.object(prepare.subprocess, 'check_output') as command:
            with self.assertRaises(FileExistsError):
                prepare.prepare(ROOT, self.context, 'amd64', '/unused/pinned-go')
            command.assert_not_called()

    def test_ambient_go_configuration_is_ignored(self):
        ambient = {'GOFLAGS': '-race -tags personal', 'GOOS': 'darwin', 'GOARCH': 'arm',
                   'GOENV': '/private/go.env', 'GOWORK': '/private/go.work', 'GOROOT': '/bad/go',
                   'CGO_ENABLED': '1', 'GOTOOLCHAIN': 'auto', 'GOAMD64': 'v4',
                   'GOARM64': 'v9.5', 'GOEXPERIMENT': 'experiment', 'GOFIPS140': 'latest'}
        ambient.update(GOPROXY='https://unreviewed.invalid', GOSUMDB='unreviewed.invalid',
                       GOPRIVATE='*', GONOPROXY='*', GONOSUMDB='*', GOINSECURE='*')
        with patch.dict(os.environ, ambient):
            environment = prepare.build_environment('arm64')
        for key, value in {'GOFLAGS': '-mod=readonly', 'CGO_ENABLED': '0', 'GOOS': 'linux',
                           'GOARCH': 'arm64', 'GOENV': 'off', 'GOWORK': 'off',
                           'GOTOOLCHAIN': 'local', 'GOAMD64': 'v1', 'GOARM64': 'v8.0',
                           'GOEXPERIMENT': '', 'GOFIPS140': 'off', 'GOPROXY': 'off',
                           'GOSUMDB': 'off', 'GOPRIVATE': '', 'GONOPROXY': '',
                           'GONOSUMDB': '', 'GOINSECURE': ''}.items():
            self.assertEqual(environment[key], value, key)
        self.assertNotIn('GOROOT', environment)

    def test_local_replacement_notices_keep_original_module_path(self):
        upstream = self.context / 'upstream'
        replacement = self.context / 'replacement'
        upstream.mkdir()
        replacement.mkdir()
        (upstream / 'LICENSE').write_text('old upstream notice')
        for name in ('LICENSE', 'NOTICE.patch', 'AUTHORS'):
            (replacement / name).write_text('patched module ' + name)
        (replacement / 'private.key').write_text('never package')
        modules = [{'Path': 'example.org/patched/v2', 'Dir': str(upstream),
                    'Replace': {'Dir': str(replacement)}}]
        destination = self.context / 'licenses'
        prepare.collect_notices(modules, destination)
        target = destination / 'example.org/patched/v2'
        self.assertEqual({path.name for path in target.iterdir()}, {'LICENSE', 'NOTICE.patch', 'AUTHORS'})
        self.assertEqual((target / 'LICENSE').read_text(), 'patched module LICENSE')

    def test_missing_module_sources_do_not_silently_drop_notices(self):
        with self.assertRaisesRegex(ValueError, 'source unavailable'):
            prepare.collect_notices([{'Path': 'example.org/missing'}], self.context / 'licenses')

    def test_restrictive_umask_keeps_all_notices_readable_and_data_private(self):
        original = os.umask(0o077)
        try:
            source = self.context / 'module-source'
            source.mkdir()
            (source / 'LICENSE').write_text('dependency license')
            destination = self.context / 'licenses'
            prepare.collect_notices([{'Path': 'example.org/module', 'Dir': str(source)}], destination)
            (destination / 'go').mkdir()
            for notice in ('LICENSE', 'PATENTS'):
                shutil.copyfile(source / 'LICENSE', destination / 'go' / notice)
            data = self.context / 'data'
            data.mkdir(mode=0o700)
            prepare.normalize_notices(destination)
        finally:
            os.umask(original)
        for path in (destination, *destination.rglob('*')):
            self.assertEqual(path.stat().st_mode & 0o777, 0o755 if path.is_dir() else 0o644, str(path))
        self.assertEqual(self.context.stat().st_mode & 0o777, 0o700)
        self.assertEqual(data.stat().st_mode & 0o777, 0o700)

    def test_module_metadata_stream_supports_spaces_in_paths(self):
        modules = [{'Path': 'example.org/one', 'Dir': '/a directory'}, {'Path': 'example.org/two'}]
        encoded = '\n'.join(json.dumps(item, indent=2) for item in modules)
        self.assertEqual(list(prepare.json_stream(encoded)), modules)

    def test_linked_modules_deduplicate_packages_and_exclude_unused_graph_entries(self):
        first = {'Path': 'example.org/linked', 'Dir': '/cached/linked'}
        patched = {'Path': 'example.org/patched', 'Replace': {'Dir': '/local/patch'}}
        packages = [{'ImportPath': 'fmt', 'Standard': True},
                    {'ImportPath': 'example.org/linked/one', 'Module': first},
                    {'ImportPath': 'example.org/linked/two', 'Module': first},
                    {'ImportPath': 'example.org/patched', 'Module': patched}]
        self.assertEqual(prepare.linked_modules(packages), [first, patched])

    def test_missing_base_gives_explicit_setup_without_building(self):
        fake = self.context / 'docker'
        fake.write_text('''#!/usr/bin/env bash
printf "%s\\n" "$*" >> "$DOCKER_CALLS"
if [[ $* == 'image inspect --help' ]]; then echo 'Options: --format'; exit 0; fi
if [[ $1 == version ]]; then echo '1.48 1.48'; exit 0; fi
echo 'Error response from daemon: No such image: fixture' >&2
exit 1
''')
        fake.chmod(0o755)
        calls = self.context / 'calls'
        environment = dict(os.environ, PATH=f'{self.context}:{os.environ["PATH"]}', DOCKER_CALLS=str(calls))
        result = subprocess.run([str(ROOT / 'scripts/container.sh'), 'fry:test', 'arm64'],
                                env=environment, capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 2)
        self.assertIn('docker pull --platform linux/arm64 gcr.io/distroless/', result.stderr)
        self.assertTrue(all(line.startswith(('image inspect ', 'version '))
                            for line in calls.read_text().splitlines()))

    def preflight(self, os_name='linux', arch='arm64', failure=''):
        root = Path(tempfile.mkdtemp(dir=self.context))
        (root / 'scripts').mkdir()
        shutil.copyfile(ROOT / 'scripts/container.sh', root / 'scripts/container.sh')
        shutil.copyfile(ROOT / 'Dockerfile', root / 'Dockerfile')
        shutil.copytree(ROOT / 'tooling/container', root / 'tooling/container',
                        ignore=shutil.ignore_patterns('__pycache__'))
        # Stop at the native compiler boundary; this test never compiles or builds.
        (root / 'scripts/toolchain.sh').write_text('exit 73\n')
        fake = root / 'docker'
        fake.write_text('''#!/usr/bin/env bash
printf "%s\\n" "$*" >> "$DOCKER_CALLS"
if [[ $* == 'image inspect --help' ]]; then echo 'Options: --format'; exit 0; fi
if [[ $1 == version ]]; then echo '1.48 1.48'; exit 0; fi
if [[ $* == *--platform* ]]; then echo 'unknown flag: --platform' >&2; exit 125; fi
if [[ -n $DOCKER_FAILURE ]]; then echo "$DOCKER_FAILURE" >&2; exit 1; fi
printf '[{"Os":"%s","Architecture":"%s"}]\\n' "$DOCKER_OS" "$DOCKER_ARCH"
''')
        fake.chmod(0o755)
        calls = root / 'calls'
        environment = dict(os.environ, PATH=f'{root}:{os.environ["PATH"]}', DOCKER_CALLS=str(calls),
                           DOCKER_OS=os_name, DOCKER_ARCH=arch, DOCKER_FAILURE=failure)
        result = subprocess.run(['bash', str(root / 'scripts/container.sh'), 'fry:test', 'arm64'],
                                env=environment, capture_output=True, text=True, timeout=10)
        self.assertTrue(all(line.startswith(('image inspect ', 'version '))
                            for line in calls.read_text().splitlines()))
        return result

    def test_docker_28_preflight_accepts_explicitly_cached_foreign_platform(self):
        result = self.preflight()
        self.assertEqual(result.returncode, 73, result.stderr)
        self.assertNotIn('unknown flag', result.stderr)

    def test_docker_28_preflight_rejects_wrong_os_or_architecture(self):
        for os_name, arch in (('windows', 'arm64'), ('linux', 'amd64')):
            with self.subTest(os_name=os_name, arch=arch):
                result = self.preflight(os_name, arch)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn('platform mismatch', result.stderr)
                self.assertNotIn('cache the pinned runtime', result.stderr)

    def test_inspection_failure_is_not_reported_as_missing_base(self):
        result = self.preflight(failure='permission denied connecting to Docker')
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn('permission denied', result.stderr)
        self.assertNotIn('cache the pinned runtime', result.stderr)

    def test_unsupported_architecture_stops_before_docker(self):
        result = subprocess.run([str(ROOT / 'scripts/container.sh'), 'fry:test', 'ppc64le'],
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 2)
        self.assertIn('ARCH must be amd64 or arm64', result.stderr)


if __name__ == '__main__':
    unittest.main()
