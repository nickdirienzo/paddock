#!/usr/bin/python3
"""Protect Paddock's control process from same-UID agent ptrace."""

import ctypes
import os
import runpy
import sys


libc = ctypes.CDLL(None, use_errno=True)
if libc.prctl(4, 0, 0, 0, 0) != 0:  # PR_SET_DUMPABLE
    error = ctypes.get_errno()
    raise OSError(error, os.strerror(error))

sys.argv = ["/usr/local/bin/paddock-agent"] + sys.argv[1:]
runpy.run_path(sys.argv[0], run_name="__main__")
