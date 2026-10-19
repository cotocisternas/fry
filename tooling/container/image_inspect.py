"""Inspect local images across Docker CLI/API generations without pulling."""
import json
import subprocess
import sys


class ImageNotFound(ValueError):
    """The requested local image is absent, rather than Docker being unavailable."""


def docker(*arguments, timeout=30):
    return subprocess.check_output(['docker', *arguments], text=True, timeout=timeout,
                                   stderr=subprocess.PIPE)


def api_version(value):
    parts = value.split('.')
    if len(parts) != 2 or not all(part.isascii() and part.isdigit() for part in parts):
        raise ValueError(f'invalid Docker API version: {value!r}')
    return tuple(int(part) for part in parts)


def inspect_image(image, arch, command=docker):
    if arch not in ('amd64', 'arm64'):
        raise ValueError('architecture must be amd64 or arm64')
    # Help detects CLI support; both API versions also matter when the client is
    # newer than the daemon or DOCKER_API_VERSION limits the negotiated API.
    help_text = command('image', 'inspect', '--help')
    arguments = []
    if '--platform' in help_text.split():
        versions = command('version', '--format', '{{.Client.APIVersion}} {{.Server.APIVersion}}').split()
        if len(versions) != 2:
            raise ValueError('cannot determine Docker client/server API versions')
        if all(api_version(value) >= (1, 49) for value in versions):
            arguments = ['--platform', f'linux/{arch}']
    try:
        result = json.loads(command('image', 'inspect', *arguments, image))
    except subprocess.CalledProcessError as error:
        if error.stderr and 'No such image:' in error.stderr:
            raise ImageNotFound(f'local image is missing: {image}') from error
        raise
    if not isinstance(result, list) or len(result) != 1 or not isinstance(result[0], dict):
        raise ValueError('expected one local image inspection')
    inspection = result[0]
    actual = f'{inspection.get("Os")}/{inspection.get("Architecture")}'
    if actual != f'linux/{arch}':
        raise ValueError(f'image platform mismatch for {image}: expected linux/{arch}, found {actual}')
    return inspection


def main():
    try:
        if len(sys.argv) != 3:
            raise ValueError('usage: image_inspect.py LOCAL_IMAGE amd64|arm64')
        inspection = inspect_image(sys.argv[1], sys.argv[2])
        print(f'{inspection["Os"]}/{inspection["Architecture"]}')
        return 0
    except ImageNotFound as error:
        print(f'container image: {error}', file=sys.stderr)
        return 3
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        detail = getattr(error, 'stderr', None) or str(error)
        print(f'container image: {detail.strip()}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
