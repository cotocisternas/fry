#!/usr/bin/env python3
"""Prepare a fresh output-only Docker context with native, static Linux commands."""
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys


MODULE = 'github.com/cotocisternas/fry'
BINARIES = ('fry', 'fry-member', 'fry-migrate', 'fry-admin')
NOTICES = ('LICENSE', 'LICENSE.*', 'COPYING', 'NOTICE', 'NOTICE.*', 'AUTHORS')


def build_environment(arch):
    if arch not in ('amd64', 'arm64'):
        raise ValueError('architecture must be amd64 or arm64')
    environment = dict(os.environ)
    environment.pop('GOROOT', None)
    environment.update(GOENV='off', GOTOOLCHAIN='local', GOWORK='off',
                       GOFLAGS='-mod=readonly', CGO_ENABLED='0', GOOS='linux',
                       GOARCH=arch, GOAMD64='v1', GOARM64='v8.0',
                       GOEXPERIMENT='', GOFIPS140='off', GOPROXY='off', GOSUMDB='off',
                       GOPRIVATE='', GONOPROXY='', GONOSUMDB='', GOINSECURE='')
    return environment


def json_stream(text):
    decoder = json.JSONDecoder()
    while text.strip():
        value, end = decoder.raw_decode(text.lstrip())
        yield value
        text = text.lstrip()[end:]


def collect_notices(modules, destination):
    for module in modules:
        name = Path(module['Path'])
        if name.is_absolute() or '..' in name.parts:
            raise ValueError(f'invalid module path: {name}')
        source = module.get('Replace', module).get('Dir')
        if not source or not Path(source).is_dir():
            raise ValueError(f'module source unavailable for notices: {name}')
        target = destination / name
        target.mkdir(parents=True, exist_ok=True)
        for pattern in NOTICES:
            for notice in sorted(Path(source).glob(pattern)):
                if notice.is_symlink():
                    raise ValueError(f'notice must be a regular file: {notice}')
                if notice.is_file():
                    shutil.copyfile(notice, target / notice.name)
                    (target / notice.name).chmod(0o644)


def linked_modules(packages):
    modules = {}
    for package in packages:
        if module := package.get('Module'):
            modules[module['Path']] = module
    return [modules[name] for name in sorted(modules)]


def normalize_notices(destination):
    # COPY makes notices root-owned. Keep them readable by UID 65532 even when
    # the host creates its private staging tree with umask 077.
    for path in (destination, *destination.rglob('*')):
        path.chmod(0o755 if path.is_dir() else 0o644)


def verify_elf(binary, arch):
    with binary.open('rb') as stream:
        header = stream.read(64)
        if len(header) != 64 or header[:6] != b'\x7fELF\x02\x01':
            raise ValueError(f'{binary.name}: expected little-endian ELF64')
        if struct.unpack_from('<H', header, 18)[0] != {'amd64': 62, 'arm64': 183}[arch]:
            raise ValueError(f'{binary.name}: ELF architecture mismatch')
        offset = struct.unpack_from('<Q', header, 32)[0]
        size, count = struct.unpack_from('<HH', header, 54)
        if size != 56 or not 1 <= count <= 1024:
            raise ValueError(f'{binary.name}: invalid ELF program headers')
        stream.seek(offset)
        for _ in range(count):
            entry = stream.read(size)
            if len(entry) != size:
                raise ValueError(f'{binary.name}: truncated ELF program headers')
            if struct.unpack_from('<I', entry)[0] in (2, 3):
                raise ValueError(f'{binary.name}: dynamic linking is forbidden')


def verify_build_info(info, binary, arch, version):
    expected = {'-trimpath': 'true', '-buildmode': 'exe', '-compiler': 'gc',
                'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': arch,
                'GOAMD64' if arch == 'amd64' else 'GOARM64':
                    'v1' if arch == 'amd64' else 'v8.0'}
    settings = {item['Key']: item['Value'] for item in info['Settings']}
    if (info['GoVersion'] != version or info['Path'] != f'{MODULE}/cmd/{binary}'
            or any(settings.get(key) != value for key, value in expected.items())
            or settings.get('GOEXPERIMENT') or any(key.startswith('vcs.') for key in settings)):
        raise ValueError(f'{binary}: unexpected compiler, command or build settings')


def verify_context(context, arch):
    runtime = context / 'runtime' / f'linux-{arch}'
    expected = {'Dockerfile', '.dockerignore', 'runtime', f'runtime/linux-{arch}'}
    for path in context.rglob('*'):
        relative = path.relative_to(context).as_posix()
        if path.is_symlink():
            raise ValueError(f'symlink in packaging context: {relative}')
        if relative in expected:
            continue
        parts = path.relative_to(runtime).parts
        if (parts[0] not in ('bin', 'licenses', 'data')
                or (parts[0] == 'bin' and len(parts) > 1
                    and (len(parts) != 2 or parts[1] not in BINARIES))
                or (parts[0] == 'data' and len(parts) > 1)):
            raise ValueError(f'unexpected packaging input: {relative}')
    if {path.name for path in (runtime / 'bin').iterdir()} != set(BINARIES):
        raise ValueError('exactly four commands are required')
    for binary in BINARIES:
        path = runtime / 'bin' / binary
        if not path.is_file() or not os.access(path, os.X_OK):
            raise ValueError(f'command is missing or not executable: {binary}')
        verify_elf(path, arch)
    if not (runtime / 'licenses' / MODULE / 'LICENSE').is_file():
        raise ValueError('repository license is missing')
    if not (runtime / 'data').is_dir() or (runtime / 'data').stat().st_mode & 0o777 != 0o700:
        raise ValueError('private empty data directory is required')


def prepare(repo, context, arch, go):
    environment = build_environment(arch)

    def run(*arguments, timeout=180):
        return subprocess.check_output([go, *arguments], cwd=repo, env=environment,
                                       text=True, timeout=timeout)

    runtime = context / 'runtime' / f'linux-{arch}'
    # exist_ok=False prevents any caller from reusing an old binary directory.
    (runtime / 'bin').mkdir(parents=True)
    (runtime / 'licenses').mkdir()
    (runtime / 'data').mkdir(mode=0o700)
    manifests = {path: path.read_bytes() for pattern in ('go.mod', 'go.sum')
                 for path in (repo / pattern, *sorted((repo / 'third_party').glob(f'*/{pattern}')))}
    run('build', '-trimpath', '-buildvcs=false', '-o', str(runtime / 'bin') + '/',
        *(f'./cmd/{binary}' for binary in BINARIES), timeout=600)
    version = run('env', 'GOVERSION').strip()
    for binary in BINARIES:
        path = runtime / 'bin' / binary
        path.chmod(0o555)
        verify_build_info(json.loads(run('version', '-m', '-json', str(path))),
                          binary, arch, version)
    packages = json_stream(run('list', '-deps', '-json', *(f'./cmd/{binary}' for binary in BINARIES)))
    collect_notices(linked_modules(packages), runtime / 'licenses')
    # The standard library is linked too; retain its notices beside module notices.
    goroot = Path(run('env', 'GOROOT').strip())
    (runtime / 'licenses' / 'go').mkdir()
    for notice in ('LICENSE', 'PATENTS'):
        shutil.copyfile(goroot / notice, runtime / 'licenses' / 'go' / notice)
    normalize_notices(runtime / 'licenses')
    if any(path.read_bytes() != contents for path, contents in manifests.items()):
        raise ValueError('native preparation changed a module manifest; packaging refused')
    verify_context(context, arch)
    print(f'Verified four static linux/{arch} binaries and dependency notices ({version}).')


if __name__ == '__main__':
    try:
        if len(sys.argv) != 5:
            raise ValueError('usage: prepare.py REPO FRESH_CONTEXT amd64|arm64 PINNED_GO')
        prepare(Path(sys.argv[1]).resolve(), Path(sys.argv[2]).resolve(), sys.argv[3], sys.argv[4])
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        sys.exit(f'container: {error}')
