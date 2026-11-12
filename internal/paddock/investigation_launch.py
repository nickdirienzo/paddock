#!/usr/bin/python3
"""Apply the investigation guard to the launcher before starting the agent."""

import runpy
import sys

runpy.run_path('/sandbox/.paddock/guard/exec.py')['restrict']()
sys.argv = ["/usr/local/bin/paddock-agent"] + sys.argv[1:]
runpy.run_path(sys.argv[0], run_name="__main__")
