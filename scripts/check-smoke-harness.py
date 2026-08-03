#!/usr/bin/env python3
"""check-smoke-harness — guard the one property that makes the ai-gateway smoke
a gate rather than a ritual: an aborted run must not report PASS.

Why this exists. `tests/scripts/smoke-gateway.py` computes its verdict from the
results it recorded, and exits 0 when nothing recorded a failure. A fatal abort
used to only *print* `[FAIL] Fatal: ...` — so a 2026-07-28 prod run that died in
P0 preflight (`GET /metrics` returned 401), having exercised not one model,
printed `Result: PASS — 3 passed 0 warnings 0 failed` and exited 0. Every caller
of the mandatory pre-"done" smoke would have read that as a clean run.

Two checks, because neither alone is sufficient:

  1. The verdict must be failure-sensitive — recording one failed result flips
     PASS to FAIL and the return value to False. Without this, check 2 would be
     asserting that the handler calls a function that does nothing.
  2. The `except RuntimeError` handler in main() must record a failure, not just
     log one. Checked against the parsed source: reproducing the abort for real
     needs a running gateway plus a CP login that succeeds and a /metrics call
     that does not, which is not available in CI. Driving the fatal through an
     unreachable gateway does NOT discriminate — that path already records a
     failure of its own, so it exits 1 either way.

Run: python3 scripts/check-smoke-harness.py
"""

from __future__ import annotations

import ast
import importlib.util
import os
import sys
import tempfile

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SMOKE_PATH = os.path.join(REPO_ROOT, "tests", "scripts", "smoke-gateway.py")

failures: list[str] = []


def load_smoke():
    """Import smoke-gateway.py as a module. Its work is behind a main guard, so
    importing runs no phases and touches no service."""
    sys.path.insert(0, os.path.join(REPO_ROOT, "tests", "lib"))
    spec = importlib.util.spec_from_file_location("smoke_gateway_under_check", SMOKE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def check_verdict_is_failure_sensitive(m) -> None:
    """One recorded failure must flip the verdict. The control half — a run with
    only a pass must still return True — is what makes the treatment meaningful."""
    tmp = tempfile.mkdtemp()
    m._results.clear()
    m._log_lines.clear()

    m.rec("P0", "harness-control").passed("control")
    control = m.render_report("nvk_harnesscheck", "t0", "t1", [], os.path.join(tmp, "control.md"))
    if control is not True:
        failures.append(
            "render_report returned %r for a run whose only result passed — the verdict is "
            "stuck at failure and cannot signal a clean run." % (control,)
        )

    m.rec("FATAL", "harness-treatment").failed("simulated abort")
    treatment = m.render_report("nvk_harnesscheck", "t0", "t1", [], os.path.join(tmp, "treatment.md"))
    if treatment is not False:
        failures.append(
            "render_report returned %r after a failed result was recorded — a failure does not "
            "flip the verdict, so no abort could ever make the smoke exit non-zero." % (treatment,)
        )


def check_fatal_handler_records_a_failure() -> None:
    """main()'s `except RuntimeError` must record a failed result. Logging alone
    leaves the counters untouched, which is the exact regression this guards."""
    tree = ast.parse(open(SMOKE_PATH).read(), filename=SMOKE_PATH)

    main_fn = next(
        (n for n in ast.walk(tree)
         if isinstance(n, ast.FunctionDef) and n.name == "main"),
        None,
    )
    if main_fn is None:
        failures.append("no main() in smoke-gateway.py — this check needs rewriting, not silencing.")
        return

    handlers = [
        h for n in ast.walk(main_fn) if isinstance(n, ast.Try)
        for h in n.handlers
        if isinstance(h.type, ast.Name) and h.type.id == "RuntimeError"
    ]
    if not handlers:
        failures.append(
            "main() no longer catches RuntimeError. If aborts now propagate, the process exits "
            "non-zero on its own and this check should be replaced — but confirm that before deleting it."
        )
        return

    for handler in handlers:
        records = any(
            isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "failed"
            and isinstance(node.func.value, ast.Call)
            and isinstance(node.func.value.func, ast.Name)
            and node.func.value.func.id == "rec"
            for node in ast.walk(handler)
        )
        if not records:
            failures.append(
                "the `except RuntimeError` handler at line %d does not call rec(...).failed(...). "
                "A fatal that only logs leaves fail_count at 0, so an aborted run reports PASS and "
                "exits 0 — the smoke stops being able to fail." % handler.lineno
            )


def main() -> int:
    if not os.path.exists(SMOKE_PATH):
        print("FAIL: %s not found" % SMOKE_PATH)
        return 1

    check_fatal_handler_records_a_failure()

    # Importing prints the module's banner; keep the check's own output readable
    # by letting it through rather than swallowing it — a surprise on stdout here
    # is itself worth seeing.
    module = load_smoke()
    check_verdict_is_failure_sensitive(module)

    print()
    if failures:
        print("check-smoke-harness: FAIL")
        for f in failures:
            print("  - %s" % f)
        return 1
    print("check-smoke-harness: OK — an aborted smoke run reports FAIL and exits non-zero")
    return 0


if __name__ == "__main__":
    sys.exit(main())
