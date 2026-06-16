"""Prompt dataset loading and rendering.

Datasets are JSONL files under ``benchmark/datasets/``:

* short_chat.jsonl       — {"prompt_id", "prompt"}
* streaming_stress.jsonl — {"prompt_id", "prompt"}
* long_context.jsonl     — {"prompt_id", "target_tokens", "instruction", "filler"}
                            (instruction/filler may contain {VU_ID}/{ITER_ID})

Token counts are approximated as ~4 characters/token — good enough for sizing
a ~16k-token prompt without pulling in a tokenizer dependency.
"""
from __future__ import annotations

import json
from pathlib import Path

DATASETS_DIR = Path(__file__).resolve().parent.parent / "datasets"

CHARS_PER_TOKEN = 4


def load_jsonl(name: str) -> list[dict]:
    path = DATASETS_DIR / name
    if not path.exists():
        raise FileNotFoundError(f"Dataset not found: {path}")
    rows: list[dict] = []
    with path.open() as f:
        for i, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError as e:
                raise ValueError(f"{name}:{i} is not valid JSON: {e}") from e
    if not rows:
        raise ValueError(f"Dataset {name} is empty")
    return rows


def approx_tokens(text: str) -> int:
    return max(1, len(text) // CHARS_PER_TOKEN)


def render_short(row: dict) -> str:
    return row["prompt"]


def render_long_context(row: dict, vu: int, iteration: int) -> str:
    """Expand a long-context template to ~target_tokens, embedding VU/ITER markers.

    The filler block is repeated (with markers substituted each time) until the
    text reaches the requested token budget, then the instruction is appended.
    """
    target_tokens = int(row.get("target_tokens", 16000))
    target_chars = target_tokens * CHARS_PER_TOKEN

    sub = {"{VU_ID}": str(vu), "{ITER_ID}": str(iteration)}

    def fill(s: str) -> str:
        for k, v in sub.items():
            s = s.replace(k, v)
        return s

    instruction = fill(row["instruction"])
    filler_unit = fill(row["filler"])

    body_parts: list[str] = []
    chars = 0
    n = 0
    while chars < target_chars:
        # Vary each block slightly so the body is not trivially compressible
        # and (when cache is disabled) never collides across iterations.
        block = f"[block {n}] {filler_unit}"
        body_parts.append(block)
        chars += len(block)
        n += 1
    body = "".join(body_parts)
    return f"{instruction}\n\n--- BEGIN DOCUMENT ---\n{body}\n--- END DOCUMENT ---"


def dataset_summary(name: str) -> dict:
    rows = load_jsonl(name)
    info = {"file": name, "count": len(rows)}
    if "target_tokens" in rows[0]:
        info["approx_tokens_each"] = rows[0].get("target_tokens")
    else:
        sample = rows[0].get("prompt", "")
        info["sample_prompt_id"] = rows[0].get("prompt_id")
        info["approx_tokens_sample"] = approx_tokens(sample)
    return info
