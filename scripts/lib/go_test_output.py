"""Render Go's streaming test events without waiting for a package to finish."""

import json
import sys


def render(source, destination):
    valid = True
    for line in source:
        try:
            event = json.loads(line)
            if not isinstance(event, dict):
                raise ValueError("test event must be an object")
            output = event.get("Output")
            package = event.get("Package") or event.get("ImportPath") or "go"
            if output is not None and not isinstance(output, str):
                raise ValueError("test output must be a string")
            if not isinstance(package, str):
                raise ValueError("test package must be a string")
        except ValueError:
            # Keep draining: an unexpected tool diagnostic must not hide the
            # test failure or break the producer's pipe before it can report it.
            valid = False
            destination.write(line)
            destination.flush()
            continue
        if output is not None:
            for part in output.splitlines(keepends=True):
                destination.write(f"[{package}] {part}")
            destination.flush()
    return valid


if __name__ == "__main__":
    sys.exit(0 if render(sys.stdin, sys.stdout) else 1)
