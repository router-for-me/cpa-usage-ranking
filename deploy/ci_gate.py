#!/usr/bin/env python3
"""Fail closed unless change detection succeeded and required CI jobs passed."""

import os
import sys


REQUIRED = ("BACKEND_RESULT", "FRONTEND_RESULT", "CONTAINER_RESULT")


def main():
    full_check = os.environ.get("FULL_CHECK")
    if full_check not in ("true", "false"):
        print("FULL_CHECK must be explicitly 'true' or 'false'", file=sys.stderr)
        return 1
    if os.environ.get("CHANGES_RESULT") != "success":
        print("Required check CHANGES_RESULT did not succeed", file=sys.stderr)
        return 1

    allowed = {"success"} if full_check == "true" else {"success", "skipped"}
    for name in REQUIRED:
        if os.environ.get(name) not in allowed:
            print(f"Required check {name} did not succeed", file=sys.stderr)
            return 1
    print("All required checks succeeded")
    return 0


if __name__ == "__main__":
    sys.exit(main())
