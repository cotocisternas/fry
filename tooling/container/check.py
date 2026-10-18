#!/usr/bin/env python3
"""Bounded image filesystem and command-startup checks; no database or network."""
import json
import platform
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import uuid

from prepare import BINARIES, MODULE, verify_elf


def docker(*arguments, timeout=30):
    return subprocess.check_output(['docker', *arguments], text=True, timeout=timeout)


def check(image, arch):
    if arch not in ('amd64', 'arm64'):
        raise ValueError('architecture must be amd64 or arm64')
    # Resolve the local immutable image first; subsequent commands never pull it.
    inspection = json.loads(docker('image', 'inspect', '--platform', f'linux/{arch}', image))[0]
    # Docker's containerd store can report a platform manifest as Id when
    # --platform is used; container creation resolves the local index identity.
    identity = json.loads(docker('image', 'inspect', image))[0]['Id']
    config = inspection['Config']
    if (inspection['Os'] != 'linux' or inspection['Architecture'] != arch
            or config['User'] != '65532:65532' or config['WorkingDir'] != '/var/lib/fry'
            or config['Entrypoint'] != ['/usr/local/bin/fry'] or config['Cmd'] != ['-help']
            or config.get('StopSignal') != 'SIGTERM'):
        raise ValueError('image platform or runtime configuration differs from packaging contract')
    name = 'fry-package-check-' + uuid.uuid4().hex[:12]
    security = ('--pull=never', '--platform', f'linux/{arch}', '--read-only', '--network=none',
                '--cap-drop=ALL', '--security-opt=no-new-privileges', '--pids-limit=64')
    created = False
    execution = False
    try:
        docker('create', '--name', name, *security, identity)
        created = True
        with tempfile.TemporaryDirectory(prefix='fry-package-check-') as temporary:
            directory = Path(temporary)
            archive = directory / 'rootfs.tar'
            docker('export', '--output', str(archive), name, timeout=60)
            with tarfile.open(archive) as filesystem:
                data = filesystem.getmember('var/lib/fry')
                if not data.isdir() or (data.uid, data.gid, data.mode) != (65532, 65532, 0o700):
                    raise ValueError('runtime data directory ownership/mode differs')
                ca = filesystem.getmember('etc/ssl/certs/ca-certificates.crt')
                if not ca.isfile() or ca.size == 0:
                    raise ValueError('system TLS roots are missing')
                for notice in filesystem.getmembers():
                    if notice.name == 'usr/share/fry/licenses' or notice.name.startswith('usr/share/fry/licenses/'):
                        if notice.mode != (0o755 if notice.isdir() else 0o644):
                            raise ValueError(f'notice permissions prevent nonroot access: {notice.name}')
                for module in (MODULE, 'github.com/emersion/go-smtp',
                               'github.com/emersion/go-imap/v2', 'go'):
                    notice = filesystem.getmember(f'usr/share/fry/licenses/{module}/LICENSE')
                    if not notice.isfile() or notice.size == 0:
                        raise ValueError(f'license is missing: {module}')
                for binary in BINARIES:
                    member = filesystem.getmember(f'usr/local/bin/{binary}')
                    if not member.isfile() or member.mode != 0o555:
                        raise ValueError(f'binary is missing or has unexpected permissions: {binary}')
                    path = directory / binary
                    with filesystem.extractfile(member) as source, path.open('wb') as output:
                        while chunk := source.read(1024 * 1024):
                            output.write(chunk)
                    verify_elf(path, arch)
        host_arch = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}.get(platform.machine())
        if host_arch != arch:
            execution = False
        else:
            for binary in BINARIES:
                command_name = name + '-' + binary
                command_created = False
                try:
                    docker('create', '--name', command_name, *security,
                           '--entrypoint', '/usr/local/bin/' + binary, identity, '-help')
                    command_created = True
                    # Go flag help is written to stderr; a zero exit establishes startup.
                    output = docker('start', '--attach', command_name)
                    if output:
                        print(output, end='')
                finally:
                    if command_created:
                        docker('rm', '--force', command_name)
            execution = True
    finally:
        if created:
            docker('rm', '--force', name)
    if execution:
        print(f'Image checks passed for {image} ({identity}, linux/{arch}): four static commands, '
              'licenses, TLS roots, private nonroot data directory, restricted -help startup '
              'and checked cleanup.')
    else:
        print(f'Filesystem/static-binary checks and cleanup passed for linux/{arch}; '
              f'command execution skipped on {platform.machine()} host.')


if __name__ == '__main__':
    try:
        if len(sys.argv) != 3:
            raise ValueError('usage: check.py LOCAL_IMAGE amd64|arm64')
        check(sys.argv[1], sys.argv[2])
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        sys.exit(f'container check: {error}')
