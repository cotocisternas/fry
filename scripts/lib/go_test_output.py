"""Render Go's streaming test events without waiting for a package to finish."""

import json
import sys


def render(source, destination):
    for line in source:
        event = json.loads(line)
        output = event.get("Output")
        if output is not None:
            package = event.get("Package") or event.get("ImportPath", "go")
            for part in output.splitlines(keepends=True):
                destination.write(f"[{package}] {part}")
            destination.flush()


if __name__ == "__main__":
    render(sys.stdin, sys.stdout)
