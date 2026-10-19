#!/usr/bin/env python3
"""Classify a complete Git event diff for the cheap CI decision job."""

import json
import re
import subprocess
import sys
from pathlib import Path


SHA = re.compile(r'[0-9a-f]{40}\Z')
RAW_HEADER = re.compile(
    rb':([0-7]{6}) ([0-7]{6}) [0-9a-f]{40} [0-9a-f]{40} ([A-Z])[0-9]*\Z')
TOP_LEVEL_DOCS = {b'README.md', b'CONTRIBUTING.md', b'SECURITY.md', b'GLOSSARY.md'}
README_DIRS = (b'scripts/', b'tooling/', b'deploy/')


def git(repo, *args):
    return subprocess.run(
        ['git', *args], cwd=repo, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        check=True).stdout


def event_sha(value):
    if not isinstance(value, str) or not SHA.fullmatch(value):
        raise ValueError('missing or invalid event SHA')
    return value


def event_refs(event_name, event):
    if not isinstance(event, dict):
        raise ValueError('invalid event payload')
    if event_name == 'workflow_dispatch':
        return None
    if event_name == 'pull_request':
        request = event['pull_request']
        return event_sha(request['base']['sha']), event_sha(request['head']['sha'])
    if event_name == 'push':
        before = event_sha(event['before'])
        after = event_sha(event['after'])
        return None if before == '0' * 40 else (before, after)
    raise ValueError(f'unsupported event: {event_name}')


def has_commit(repo, sha):
    try:
        git(repo, 'cat-file', '-e', f'{sha}^{{commit}}')
        return True
    except subprocess.CalledProcessError:
        return False


def allowed_doc(path):
    if path in TOP_LEVEL_DOCS:
        return True
    if path.startswith(b'docs/') and path.endswith(b'.md'):
        return True
    return path.endswith(b'/README.md') and path.startswith(README_DIRS)


def docs_only_diff(raw):
    if not raw:
        return False
    fields = raw.split(b'\0')
    if fields[-1] != b'' or len(fields[:-1]) % 2:
        raise ValueError('invalid Git raw diff')
    for header, path in zip(fields[0:-1:2], fields[1:-1:2]):
        match = RAW_HEADER.fullmatch(header)
        if match is None or not path:
            raise ValueError('invalid Git raw diff entry')
        old_mode, new_mode, status = match.groups()
        if (status not in (b'A', b'D', b'M') or
                old_mode not in (b'000000', b'100644') or
                new_mode not in (b'000000', b'100644') or
                not allowed_doc(path)):
            return False
    return True


def needs_full(repo, event_name, event):
    refs = event_refs(event_name, event)
    if refs is None:
        return True
    base, head = refs
    if not has_commit(repo, base) or not has_commit(repo, head):
        return True
    if event_name == 'pull_request':
        try:
            base = event_sha(git(repo, 'merge-base', base, head).decode().strip())
        except subprocess.CalledProcessError:
            return True
    raw = git(repo, 'diff', '--raw', '-z', '--no-renames', '--abbrev=40',
              '--no-ext-diff', '--no-textconv', base, head, '--')
    return not docs_only_diff(raw)


def main(argv):
    if len(argv) != 4:
        raise ValueError('usage: changes.py EVENT_NAME EVENT_PATH GITHUB_OUTPUT')
    _, event_name, event_path, output_path = argv
    event = json.loads(Path(event_path).read_text(encoding='utf-8'))
    full = needs_full(Path.cwd(), event_name, event)
    with Path(output_path).open('a', encoding='utf-8') as output:
        output.write(f'full={str(full).lower()}\n')
    print(f'Full validation required: {str(full).lower()}')


if __name__ == '__main__':
    try:
        main(sys.argv)
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        print(f'Cannot classify changes: {error}', file=sys.stderr)
        sys.exit(1)
