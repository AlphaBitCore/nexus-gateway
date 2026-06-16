"""Machine / run metadata capture (stdlib only, no psutil dependency)."""
from __future__ import annotations

import os
import platform
import socket


def _total_ram_bytes() -> int | None:
    """Best-effort physical RAM in bytes, cross-platform, no third-party deps."""
    # Linux / most Unixes expose these sysconf names.
    try:
        return os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES")
    except (ValueError, AttributeError, OSError):
        pass
    # macOS fallback via sysctl.
    try:
        import subprocess

        out = subprocess.run(
            ["sysctl", "-n", "hw.memsize"],
            capture_output=True, text=True, timeout=5,
        )
        if out.returncode == 0 and out.stdout.strip().isdigit():
            return int(out.stdout.strip())
    except Exception:
        pass
    return None


def machine_info() -> dict:
    ram = _total_ram_bytes()
    return {
        "hostname": socket.gethostname(),
        "platform": platform.platform(),
        "python_version": platform.python_version(),
        "cpu_count": os.cpu_count(),
        "ram_bytes": ram,
        "ram_gb": round(ram / (1024 ** 3), 2) if ram else None,
    }
