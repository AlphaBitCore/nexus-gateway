"""Shared base class for all scenarios."""
from __future__ import annotations
import json
from pathlib import Path
from engine.metrics import ScenarioMetrics
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter

DATASETS = Path(__file__).parent.parent / "datasets"

def load_prompts(filename: str) -> list[str]:
    data = json.loads((DATASETS / filename).read_text())
    if "prompts" in data:
        return data["prompts"]
    if "entries" in data:
        return [e["combined"] for e in data["entries"]]
    return []
