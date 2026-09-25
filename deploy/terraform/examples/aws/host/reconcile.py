#!/usr/bin/env python3
"""Entry point for lux-reconcile.service: runs from the config repo checkout."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from luxhost.reconcile import main  # noqa: E402

if __name__ == "__main__":
    sys.exit(main())
