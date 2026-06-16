"""JSON result serialization."""
from __future__ import annotations
import json
from pathlib import Path
from engine.metrics import ScenarioMetrics


def write(results: list[ScenarioMetrics], env_info: dict, output_dir: str, run_id: str) -> Path:
    Path(output_dir).mkdir(parents=True, exist_ok=True)
    out: dict = {
        "run_id": run_id,
        "environment": env_info,
        "results": [m.to_dict() for m in results],
    }
    path = Path(output_dir) / f"results_{run_id}.json"
    path.write_text(json.dumps(out, indent=2))
    return path
