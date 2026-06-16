"""
Pre-flight config parity validator (S-10).

Checks that all gateways being compared have equivalent settings
before any comparison test runs. Halts with a clear error if mismatches
are found in caching, timeout, or model configuration.
"""
from __future__ import annotations

import sys
from dataclasses import dataclass
from typing import Optional

from rich.console import Console
from rich.table import Table

from engine.models import GatewayFullConfig

console = Console()


@dataclass
class ParityCheck:
    field: str
    expected: str
    actual: dict[str, str]  # gateway_name -> value

    @property
    def passed(self) -> bool:
        return len(set(self.actual.values())) <= 1

    @property
    def mismatch_detail(self) -> str:
        return ", ".join(f"{k}={v}" for k, v in self.actual.items())


def validate_config_parity(
    configs: list[GatewayFullConfig],
    required_caching: bool,
    halt_on_failure: bool = True,
) -> list[ParityCheck]:
    """
    Validate that all gateway configs have equivalent settings.
    Returns list of checks; raises SystemExit if halt_on_failure and any fail.
    """
    checks: list[ParityCheck] = []

    # 1. Caching state
    checks.append(ParityCheck(
        field="caching_enabled",
        expected=str(required_caching),
        actual={c.gateway.name: str(c.features.caching_enabled) for c in configs},
    ))

    # 2. Timeout
    checks.append(ParityCheck(
        field="timeout_seconds",
        expected="equivalent",
        actual={c.gateway.name: str(c.request.timeout_seconds) for c in configs},
    ))

    # 3. Max retries (must be 0 for fair benchmark)
    checks.append(ParityCheck(
        field="max_retries",
        expected="0",
        actual={c.gateway.name: str(c.request.max_retries) for c in configs},
    ))

    # 4. Streaming mode
    checks.append(ParityCheck(
        field="stream",
        expected="equivalent",
        actual={c.gateway.name: str(c.request.stream) for c in configs},
    ))

    # Print parity report table
    table = Table(title="[bold]Pre-flight Config Parity Report[/bold]", show_lines=True)
    table.add_column("Field", style="cyan")
    table.add_column("Status", style="bold")
    for c in configs:
        table.add_column(c.gateway.name)

    all_passed = True
    for check in checks:
        passed = check.passed
        status = "[green]✅ PASS[/green]" if passed else "[red]❌ MISMATCH[/red]"
        if not passed:
            all_passed = False
        row = [check.field, status] + [check.actual.get(c.gateway.name, "?") for c in configs]
        table.add_row(*row)

    console.print(table)

    if not all_passed:
        failing = [c.field for c in checks if not c.passed]
        msg = (
            f"\n[red bold]Config parity FAILED on: {', '.join(failing)}[/red bold]\n"
            "All gateways must have identical settings for a fair comparison run.\n"
            "Fix the YAML configs and re-run."
        )
        console.print(msg)
        if halt_on_failure:
            sys.exit(1)

    # Extra: warn if any caching_enabled != required_caching
    for config in configs:
        if config.features.caching_enabled != required_caching:
            console.print(
                f"[red]WARNING: {config.gateway.name} caching_enabled="
                f"{config.features.caching_enabled} but expected {required_caching}[/red]"
            )
            if halt_on_failure:
                sys.exit(1)

    console.print("[green]✅ All config parity checks passed. Starting benchmark.[/green]\n")
    return checks
