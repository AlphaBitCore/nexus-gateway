"""benchmark/v3 — schema validity + shape tests (stdlib unittest + jsonschema).

Asserts the canonical run schema is itself a valid Draft-07 schema, that a clean
run validates against it, and that representative shape violations are caught.
Requires jsonschema (benchmark/v3/requirements.txt); skips cleanly if absent.

    python tests/test_schema.py
"""
from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path

V3_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(V3_ROOT))

from tests.test_validator import valid_run  # noqa: E402  (reuse the canonical fixture)

SCHEMA_PATH = V3_ROOT / "shared" / "schemas" / "run.schema.json"
PROV_SCHEMA_PATH = V3_ROOT / "shared" / "schemas" / "provenance.schema.json"

try:
    import jsonschema  # noqa: E402
    HAVE_JSONSCHEMA = True
except ModuleNotFoundError:
    HAVE_JSONSCHEMA = False


@unittest.skipUnless(HAVE_JSONSCHEMA, "jsonschema not installed")
class RunSchema(unittest.TestCase):
    def setUp(self):
        self.schema = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
        self.validator = jsonschema.Draft7Validator(self.schema)

    def test_schema_itself_is_valid(self):
        jsonschema.Draft7Validator.check_schema(self.schema)  # raises if malformed

    def test_clean_run_validates(self):
        errors = list(self.validator.iter_errors(valid_run()))
        self.assertEqual(errors, [], errors)

    def test_missing_required_top_level_field_fails(self):
        r = valid_run()
        del r["upstream"]
        self.assertTrue(list(self.validator.iter_errors(r)))

    def test_unknown_enum_value_fails(self):
        r = valid_run()
        r["run_mode"] = "warp_speed"
        self.assertTrue(list(self.validator.iter_errors(r)))

    def test_additional_property_rejected(self):
        r = valid_run()
        r["gateway"]["surprise"] = "typo"  # additionalProperties:false
        self.assertTrue(list(self.validator.iter_errors(r)))

    def test_story_enum_enforced(self):
        r = valid_run()
        r["story"] = "C"
        self.assertTrue(list(self.validator.iter_errors(r)))


@unittest.skipUnless(HAVE_JSONSCHEMA, "jsonschema not installed")
class ProvenanceSchema(unittest.TestCase):
    def test_provenance_schema_is_valid(self):
        schema = json.loads(PROV_SCHEMA_PATH.read_text(encoding="utf-8"))
        jsonschema.Draft7Validator.check_schema(schema)


if __name__ == "__main__":
    unittest.main(verbosity=2)
