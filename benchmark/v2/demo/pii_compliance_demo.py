#!/usr/bin/env python3
"""
Nexus Compliance Demo — PII rejection in action.

What this shows:
  1. A clean prompt → Nexus relays to OpenAI → 200 with completion.
  2. A prompt containing a fake SSN → Nexus's pii-scanner hook rejects
     with HTTP 403 and the response header X-Nexus-Hook documents WHY.
  3. A prompt containing a fake credit card → same compliance rejection.

This is the artifact that proves the value James asked about: governance
that the thin pass-through gateways (LiteLLM, Bifrost) do not have.

Run:
  python demo/pii_compliance_demo.py
  # or to override the gateway URL / VK:
  NEXUS_BASE_URL=http://aws-host:3050 NEXUS_API_KEY=nvk_... \
    python demo/pii_compliance_demo.py
"""
from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

import httpx
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

# Auto-load benchmark/v2/.env.local for credentials.
try:
    from dotenv import load_dotenv
    env_local = Path(__file__).parent.parent / ".env.local"
    if env_local.exists():
        load_dotenv(env_local, override=False)
except ImportError:
    pass

console = Console()


# ──────────────────────────────────────────────────────────────────────
# Test cases. Each is (label, prompt, expected outcome).
# Fake PII values — deliberately NOT real numbers; the pii-scanner only
# checks regex shape, so a fake "123-45-6789" is enough to prove the hook.
# ──────────────────────────────────────────────────────────────────────
CASES = [
    {
        "label": "1. Clean prompt (control)",
        "prompt": "What is the capital of France? Reply in five words.",
        "expect_status": 200,
        "expect_hook": "passed",
    },
    {
        "label": "2. Prompt containing a fake SSN",
        "prompt": "Please summarize this customer record: John Doe SSN 123-45-6789, age 42.",
        "expect_status": 403,
        "expect_hook": "rejected:pii-scanner",
    },
    {
        "label": "3. Prompt containing a fake credit card",
        "prompt": "Help me write a thank-you note for order 4532-1234-5678-9010.",
        "expect_status": 403,
        "expect_hook": "rejected:pii-scanner",
    },
    {
        "label": "4. Prompt containing a fake phone number",
        "prompt": "Confirm appointment for John Doe at 555-123-4567.",
        "expect_status": 403,
        "expect_hook": "rejected:pii-scanner",
    },
    {
        "label": "5. Prompt containing a fake email",
        "prompt": "Please email the summary to alice@example-customer.com when ready.",
        "expect_status": 403,
        "expect_hook": "rejected:pii-scanner",
    },
]


def run() -> int:
    base_url = os.getenv("NEXUS_BASE_URL", "http://localhost:3050")
    vk = os.getenv("NEXUS_API_KEY", "")
    if not vk or not vk.startswith("nvk_"):
        console.print("[red]NEXUS_API_KEY is missing or wrong shape — set it in .env.local[/red]")
        return 2

    console.print(Panel.fit(
        f"[bold]Nexus Compliance Demo — PII Rejection[/bold]\n"
        f"target: {base_url}\nvirtual key: {vk[:12]}…\n"
        f"each call is a real request to the gateway",
        border_style="cyan",
    ))

    table = Table(title="Per-case outcome", show_lines=False, header_style="bold cyan")
    table.add_column("#", width=3)
    table.add_column("Case")
    table.add_column("HTTP")
    table.add_column("X-Nexus-Hook")
    table.add_column("Verdict")

    raw_log: list[dict] = []
    passes = 0
    fails = 0

    with httpx.Client(timeout=25.0) as client:
        for i, c in enumerate(CASES, start=1):
            body = {
                "model": "gpt-4o-mini",
                "messages": [{"role": "user", "content": c["prompt"]}],
                "max_tokens": 24,
                "stream": False,
            }
            t0 = time.perf_counter()
            try:
                r = client.post(
                    f"{base_url}/v1/chat/completions",
                    headers={
                        "Authorization": f"Bearer {vk}",
                        "Content-Type": "application/json",
                    },
                    json=body,
                )
            except Exception as e:
                table.add_row(str(i), c["label"], "EXC", "—", f"[red]error: {type(e).__name__}[/red]")
                fails += 1
                continue
            elapsed_ms = (time.perf_counter() - t0) * 1000.0
            hook_hdr = r.headers.get("X-Nexus-Hook", "(none)")
            status = r.status_code
            try:
                body_obj = r.json()
            except Exception:
                body_obj = {"raw": r.text[:200]}
            raw_log.append({
                "case": c["label"],
                "prompt": c["prompt"],
                "http_status": status,
                "x_nexus_hook": hook_hdr,
                "elapsed_ms": round(elapsed_ms, 1),
                "response": body_obj,
            })
            ok_status = (status == c["expect_status"])
            ok_hook = c["expect_hook"] in hook_hdr if c["expect_hook"] else True
            if ok_status and ok_hook:
                verdict = "[green]PASS[/green]"
                passes += 1
            else:
                verdict = (
                    f"[red]FAIL[/red] (expected {c['expect_status']} "
                    f"+ hook '{c['expect_hook']}')"
                )
                fails += 1
            table.add_row(str(i), c["label"], str(status), hook_hdr, verdict)

    console.print(table)

    out_path = Path(__file__).parent / "pii_demo_evidence.json"
    out_path.write_text(json.dumps({
        "base_url": base_url,
        "vk_prefix": vk[:12] + "…",
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "summary": {"cases": len(CASES), "pass": passes, "fail": fails},
        "cases": raw_log,
    }, indent=2))
    console.print(f"\n[dim]Evidence JSON written to {out_path}[/dim]")

    console.print(
        Panel.fit(
            f"[bold]{passes}/{len(CASES)} cases passed[/bold]\n"
            f"Compliance hook decisions are recorded in the X-Nexus-Hook response header.\n"
            f"LiteLLM and Bifrost will return 200 for ALL of cases 2-5 — no compliance layer.",
            border_style="green" if fails == 0 else "yellow",
        )
    )
    return 0 if fails == 0 else 1


if __name__ == "__main__":
    sys.exit(run())
