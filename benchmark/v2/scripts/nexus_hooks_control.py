#!/usr/bin/env python3
"""
nexus_hooks_control.py — thin CLI over engine.hooks_control.

  python scripts/nexus_hooks_control.py status   # read gateway runtime hook state
  python scripts/nexus_hooks_control.py off       # disable compliance hooks (CP API) + prove convergence
  python scripts/nexus_hooks_control.py on        # re-enable

Toggling delegates to scripts/hooks_toggle.sh (the proven OAuth + PUT + poll);
this wrapper adds the structured runtime read + convergence verdict. No secret
values are printed. See HOOKS_MODE_METHODOLOGY.md for why DB edits are invalid.
"""
import sys
from pathlib import Path

# Allow running as a script from anywhere: put the v2 root on the path.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from engine.hooks_control import main  # noqa: E402

if __name__ == "__main__":
    raise SystemExit(main())
