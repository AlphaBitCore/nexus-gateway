"""Unit tests for the S-02 dataset preflight guard (Task 2).

Asserts the named failure mode (under-padded dataset → hard stop) and the
happy path (a real long-context dataset passes silently). Stdlib unittest only
— no new dependencies. Run from benchmark/v2/:

    python -m pytest tests/test_s02_preflight.py        # if pytest present
    python tests/test_s02_preflight.py                  # plain unittest
"""
from __future__ import annotations

import contextlib
import io
import sys
import unittest
from pathlib import Path

# Make benchmark/v2/ importable when run directly.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scenarios.s02_long_context import (  # noqa: E402
    estimate_tokens,
    validate_dataset_tokens,
    MIN_PROMPT_TOKENS,
)


class TestS02DatasetPreflight(unittest.TestCase):
    def test_valid_dataset_passes_without_error(self):
        # ~12,000 words → ~15,600 estimated tokens, comfortably over the 10k floor.
        prompts = [" ".join(["word"] * 12_000) for _ in range(3)]
        # Each prompt must clear the threshold for this to be a valid fixture.
        self.assertGreaterEqual(estimate_tokens(prompts[0]), MIN_PROMPT_TOKENS)
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            validate_dataset_tokens(prompts, "/tmp/long_context_v2.json")  # must NOT raise
        out = buf.getvalue()
        self.assertIn("dataset preflight", out)
        self.assertIn("✓", out)

    def test_stub_dataset_raises_systemexit_with_message(self):
        stub = ["[REQUEST-abc] Analyze the semiconductor industry."]  # ~41-token stub
        self.assertLess(estimate_tokens(stub[0]), MIN_PROMPT_TOKENS)
        buf = io.StringIO()
        with self.assertRaises(SystemExit) as ctx:
            with contextlib.redirect_stdout(buf):
                validate_dataset_tokens(stub, "/path/to/long_context_v2.json")
        # Non-zero exit, and the message matches the documented format.
        self.assertNotEqual(ctx.exception.code, 0)
        out = buf.getvalue()
        self.assertIn("PREFLIGHT FAILED", out)
        self.assertIn("estimated tokens", out)
        self.assertIn(f"expected >= {MIN_PROMPT_TOKENS:,}", out)
        self.assertIn("Dataset file: /path/to/long_context_v2.json", out)
        self.assertIn("pad_long_context_dataset.py", out)

    def test_empty_dataset_raises_systemexit(self):
        with self.assertRaises(SystemExit):
            with contextlib.redirect_stdout(io.StringIO()):
                validate_dataset_tokens([], "/path/to/long_context_v2.json")


if __name__ == "__main__":
    unittest.main(verbosity=2)
