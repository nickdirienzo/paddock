#!/usr/bin/python3
"""Restrict investigation agent descendants to read-only source and writable scratch.

Paddock seeds the checkout before launching this wrapper.  OpenShell's policy
remains the outer boundary; this Landlock layer only removes write rights from
the coding agent, its daemon, and their children.  Failure is fatal.
"""

import ctypes
import os
import sys


class Ruleset(ctypes.Structure):
    _fields_ = [("handled_access_fs", ctypes.c_uint64)]


class PathRule(ctypes.Structure):
    _pack_ = 1
    _fields_ = [("allowed_access", ctypes.c_uint64), ("parent_fd", ctypes.c_int32)]


WRITE_FILE = 1 << 1
TRUNCATE = 1 << 14
WRITE_RIGHTS = WRITE_FILE | TRUNCATE | sum(1 << bit for bit in range(4, 14))
LIBC = ctypes.CDLL(None, use_errno=True)
LIBC.syscall.restype = ctypes.c_long


def syscall(number, *args):
    result = LIBC.syscall(number, *args)
    if result < 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))
    return result


def add_rule(ruleset_fd, path, rights):
    parent_fd = os.open(path, os.O_PATH | os.O_CLOEXEC)
    try:
        syscall(445, ruleset_fd, 1, ctypes.byref(PathRule(rights, parent_fd)), 0)
    finally:
        os.close(parent_fd)


def restrict():
    if syscall(444, 0, 0, 1) < 3:
        raise RuntimeError("Landlock ABI v3 is required for investigation")
    ruleset = Ruleset(WRITE_RIGHTS)
    ruleset_fd = syscall(444, ctypes.byref(ruleset), ctypes.sizeof(ruleset), 0)
    try:
        for path in ("/home/sandbox", "/tmp", "/sandbox/go-cache", "/sandbox/go-path"):
            add_rule(ruleset_fd, path, WRITE_RIGHTS)
        for path in ("/dev/null", "/dev/tty"):
            add_rule(ruleset_fd, path, WRITE_FILE | TRUNCATE)
        if LIBC.prctl(38, 1, 0, 0, 0) != 0:  # PR_SET_NO_NEW_PRIVS
            error = ctypes.get_errno()
            raise OSError(error, os.strerror(error))
        syscall(446, ruleset_fd, 0)
    finally:
        os.close(ruleset_fd)


def main():
    name = os.path.basename(sys.argv[0])
    if name in ("codex", "claude"):
        command = ["/usr/local/bin/" + name] + sys.argv[1:]
    else:
        command = sys.argv[1:]
    if not command or not os.path.isabs(command[0]):
        raise SystemExit("investigation guard needs an absolute executable")
    restrict()
    os.execv(command[0], command)


if __name__ == "__main__":
    main()
