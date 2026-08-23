"""Self-tests for smoke-gateway.py's own assertions.

The smoke suite spends real money, so its assertions cannot be exercised by
running it. That is exactly why they need testing another way: an assertion
nobody can run is an assertion nobody has checked, and this suite has already
shipped two that reported PASS while their own condition failed.

These tests drive the assertion functions directly with synthetic responses and
observe what each one RECORDS — not what it prints. Free to run, no provider, no
network:

    python3 -m unittest discover -s tests/scripts -p 'test_*.py'
"""

import importlib.util
import json
import pathlib
import re
import unittest

_SRC = pathlib.Path(__file__).with_name("smoke-gateway.py")
_spec = importlib.util.spec_from_file_location("smoke_gateway", _SRC)
smoke = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(smoke)


class _StubGW:
    """Returns one canned response, and remembers the body it was handed."""

    def __init__(self, response):
        self.response = response
        self.seen_body = None

    def _post_sync(self, path, body, timeout, endpoint=None, extra_headers=None):
        self.seen_body = body
        return self.response


def run_case(response, must_contain="NOT idempotent", body=None):
    """Drive _mm_comprehension_case once and return the Result it recorded."""
    before = len(smoke._results)
    gw = _StubGW(response)
    smoke._mm_comprehension_case(
        gw, "case", "/v1/chat/completions",
        body if body is not None else {"model": "m", "messages": [{"role": "user", "content": "which one?"}]},
        {}, must_contain, 10,
    )
    recorded = smoke._results[before:]
    assert len(recorded) == 1, f"expected exactly one recorded result, got {len(recorded)}"
    return recorded[0]


def ok200(payload):
    return {"status": 200, "data": payload}


class ComprehensionVerdict(unittest.TestCase):
    """The anchor is only reachable by reading the media — so is the PASS."""

    def test_anchor_in_the_answer_passes(self):
        r = run_case(ok200({"choices": [{"message": {"content": "SQLite is NOT idempotent."}}]}))
        self.assertIs(r.ok, True, "an answer carrying the anchor must pass")

    def test_gemini_shape_is_read_too(self):
        r = run_case(ok200({"candidates": [{"content": {"parts": [{"text": "it is NOT idempotent"}]}}]}))
        self.assertIs(r.ok, True, "the Gemini-native arm must be able to pass")

    def test_wrong_answer_fails(self):
        r = run_case(ok200({"choices": [{"message": {"content": "All three are idempotent."}}]}))
        self.assertIs(r.ok, False, "a confident wrong answer is the defect this check exists for")

    def test_empty_text_is_its_own_named_failure(self):
        """A 200 with nothing in it is no answer, not a wrong one.

        This is the shape the media program was created for. Reporting it as
        'answered wrong' sends the reader after a model-quality problem when the
        response body was empty.
        """
        for payload in ({"choices": [{"message": {"content": ""}}]},
                        {"choices": [{"message": {"content": "   \n "}}]},
                        {"choices": [{"message": {"content": None}}]}):
            with self.subTest(payload=payload):
                r = run_case(ok200(payload))
                self.assertIs(r.ok, False)
                self.assertIn("EMPTY", r.note,
                              "empty text must be named as such, not folded into 'answered wrong'")

    def test_unparseable_envelope_fails_instead_of_searching_itself(self):
        """The regression this file was written for.

        The old code fell back to `answer = json.dumps(data)[:400]` and searched
        the whole envelope for the anchor. Here the anchor appears in the
        envelope — in a field the model never wrote — while no assistant text
        exists at all. The old code reported 'the model read the media'.
        """
        r = run_case(ok200({"id": "resp_1", "system_fingerprint": "build-NOT idempotent"}))
        self.assertIs(r.ok, False,
                      "an anchor found in the envelope rather than in an answer is not evidence")
        self.assertIn("could not locate", r.note)

    def test_anchor_present_in_the_request_is_a_test_defect(self):
        """A prompt that names the answer measures echo, not comprehension."""
        r = run_case(
            ok200({"choices": [{"message": {"content": "NOT idempotent"}}]}),
            body={"model": "m", "messages": [{"role": "user", "content": "is it NOT idempotent?"}]},
        )
        self.assertIs(r.ok, False)
        self.assertIn("TEST DEFECT", r.note)

    def test_rate_limit_is_not_verified_and_not_a_pass(self):
        r = run_case({"status": 429, "data": {}})
        self.assertTrue(r.warn, "a 429 verified nothing and must not read as a pass")

    def test_non_200_fails(self):
        r = run_case({"status": 500, "data": {"error": {"message": "boom"}}})
        self.assertIs(r.ok, False)


if __name__ == "__main__":
    unittest.main()


class EveryMediaArmIsAnchored(unittest.TestCase):
    """Structural: no media-bearing ingress case may run without a comprehension anchor.

    The requirement is 'every media/file request is backed by the AI's
    understanding', not 'the four cases named comprehension are'. The arm that
    prompted this — a Cohere vision model sent a 190-byte red square with the
    prompt 'one word', which answered "I'm not sure what you're asking for with
    just the word 'one'" — passed, because nothing looked at the answer.

    Read from the source rather than by running: the smoke bills real providers,
    so the only affordable time to check that an arm is anchored is before it runs.
    """

    # The one arm allowed to run without an anchor, and only because of a fact
    # about the catalog rather than a tolerance for a red: NOT ONE catalog model
    # declares video among its inputModalities, so a comprehension anchor here
    # would assert a capability nothing on offer claims. The carve-out is named
    # rather than inferred from "it has fewer arguments", so adding a second
    # unanchored arm still fails.
    _UNANCHORED_BY_DESIGN = {"media/gemini/video"}

    def test_no_ingress_case_lacks_an_anchor(self):
        import ast
        src = _SRC.read_text()
        tree = ast.parse(src)
        unanchored = []
        seen_carveouts = set()
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            fn = node.func
            if getattr(fn, "id", None) != "_mm_ingress_case":
                continue
            label = node.args[2].value if isinstance(node.args[2], ast.Constant) else "?"
            # (gw, cp, label, path, body, headers, mime, size, modality, timeout, anchor)
            if len(node.args) < 11:
                if label in self._UNANCHORED_BY_DESIGN:
                    seen_carveouts.add(label)
                    continue
                unanchored.append(f"{label} @L{node.lineno}")
        self.assertEqual(
            unanchored, [],
            "these media arms assert our bookkeeping but never check that the model "
            "received anything:\n  " + "\n  ".join(unanchored))
        # A carve-out for an arm that has since been anchored (or renamed, or
        # deleted) would silently widen this gate for whatever takes its place.
        self.assertEqual(
            seen_carveouts, self._UNANCHORED_BY_DESIGN,
            f"the unanchored carve-out list names arms that no longer match: "
            f"{sorted(self._UNANCHORED_BY_DESIGN - seen_carveouts)}")

    def test_the_prompt_never_contains_the_answer(self):
        """A prompt naming the number would make every arm pass on echo alone."""
        src = _SRC.read_text()
        for number in ("19452", "38617", "52903", "74128"):
            for marker in ("_MM_Q = ", "_MM_AUDIO_Q = "):
                line = src.split(marker, 1)[1].split("\n", 1)[0]
                self.assertNotIn(number, line,
                                 f"the shared prompt names {number}; the anchor must live only in the media")

    def test_fixtures_actually_carry_their_numbers(self):
        """The anchors are only evidence if the bytes really contain them."""
        media = _SRC.parent.parent / "fixtures" / "media"
        self.assertIn("52903", (media / "doc.md").read_text(),
                      "the markdown fixture lost its reference number")
        self.assertIn(b"(38617) Tj", (media / "doc.pdf").read_bytes(),
                      "the PDF fixture lost its reference number")
        for name in ("ocr-19452.png", "clip.mp4"):
            self.assertGreater((media / name).stat().st_size, 500,
                               f"{name} looks empty; regenerate with gen-media-fixtures.py")


class VideoGenerationArmIsOptInAndPriced(unittest.TestCase):
    """The video arm bills a real render, so its structure is the safeguard.

    Nothing here calls the provider. What is asserted is the shape that keeps a
    full-suite run from spending money nobody chose to spend: the phase is
    unreachable without an explicit spec, the spec has no default to inherit,
    and the skip path says why rather than passing silently.
    """

    def test_the_phase_is_unreachable_without_an_explicit_spec(self):
        src = _SRC.read_text()
        self.assertRegex(
            src, r"if args\.video_generate:\s*\n\s*phase_video_generation\(",
            "phase_video_generation must be gated on the flag; an ungated call bills every run")

    def test_the_flag_has_no_default_clip(self):
        src = _SRC.read_text()
        block = src.split('"--video-generate"', 1)[1].split("ap.add_argument", 1)[0]
        self.assertIn('default=""', block,
                      "--video-generate must default to empty; a default clip is money with no decision behind it")

    def test_skipping_records_a_reason_rather_than_passing_silently(self):
        src = _SRC.read_text()
        self.assertIn("skipped: video generation bills a real render", src,
                      "the skip branch must name why, or a never-run arm reads as covered")

    def test_a_malformed_spec_fails_before_anything_is_submitted(self):
        """MODEL:SECONDS is validated first, so a typo cannot start a render."""
        src = _SRC.read_text()
        body = src.split("def phase_video_generation", 1)[1].split("\ndef ", 1)[0]
        validate = body.index("--video-generate expects MODEL:SECONDS")
        submit = body.index('gw._post_sync("/v1/videos"')
        self.assertLess(validate, submit,
                        "the spec check must precede the submit, or a bad spec still spends")

    def test_content_is_checked_for_a_container_signature(self):
        """A 200 with a JSON error body must not pass as a video."""
        body = _SRC.read_text().split("def phase_video_generation", 1)[1].split("\ndef ", 1)[0]
        self.assertIn("ftyp", body,
                      "the content assertion must look for the mp4 container, not merely a 200")

    def test_a_failed_render_is_reported_as_a_failure(self):
        body = _SRC.read_text().split("def phase_video_generation", 1)[1].split("\ndef ", 1)[0]
        for terminal in ("failed", "cancelled"):
            self.assertIn(terminal, body,
                          f"a {terminal} job must be a failure — the money was spent either way")


class MediaArmsGetABudgetLargeEnoughToAnswer(unittest.TestCase):
    """A comprehension arm with a tiny budget tests the budget, not the model.

    Measured against gemini-2.5-flash on the OCR fixture through the live
    gateway: max_tokens=8 returns empty text with finish_reason=length, 64
    returns "19" — the first two digits of 19452, still truncated — and 512
    returns "19452" with finish_reason=stop. Reasoning models spend the budget
    before emitting any visible text, so a small cap does not shorten the
    answer, it deletes it. Two arms were reported red for exactly this.
    """

    def test_the_shared_budget_leaves_room_for_a_thinking_preamble(self):
        src = _SRC.read_text()
        m = re.search(r"^_MM_BUDGET = (\d+)", src, re.M)
        self.assertIsNotNone(m, "_MM_BUDGET must exist; media arms must not hardcode a cap")
        self.assertGreaterEqual(
            int(m.group(1)), 256,
            "a budget under 256 truncates a reasoning model before it emits any answer — "
            "measured: 8 gives empty, 64 gives a partial number, 512 gives the whole one")

    def test_no_media_arm_hardcodes_a_small_cap(self):
        """The budget lives in one place, so raising it cannot miss an arm."""
        src = _SRC.read_text().split("\n")
        media_region = "\n".join(src[6500:6900])
        stragglers = re.findall(r'"max_tokens": (\d+)', media_region)
        small = [v for v in stragglers if int(v) < 256]
        self.assertEqual(
            small, [],
            f"media arms still hardcode small budgets {small}; use _MM_BUDGET so one "
            f"change covers every arm")


class TheMediaArmsReachTheRowByTheOnlyIdTheCallerGets(unittest.TestCase):
    """The response header is a TRACE id; the per-event endpoints take a PK.

    Until the gateway minted a distinct primary key, traffic_event.id and
    trace_id held the same value, so handing the response header to
    /api/admin/traffic/:id/normalized worked by coincidence. When they
    diverged, every media arm's lookup 404'd — and because the fetch polls for
    30s to let the async writer catch up, a permanently wrong key was
    indistinguishable from "not yet". Twenty-one arms reported NOT VERIFIED and
    the run still said PASS.

    So this pins two things: the trace id is resolved to a row id through the
    filter an operator would use, and a row that EXISTS but cannot be read is a
    failure, never a timeout.
    """

    class _StubCP:
        def __init__(self, rows, normalized):
            self.rows = rows
            self.normalized = normalized
            self.paths = []

        def get(self, path):
            self.paths.append(path)
            if "/normalized" in path:
                return self.normalized
            return (200, {"data": self.rows})

    def test_resolves_the_trace_id_before_asking_for_the_payload(self):
        cp = self._StubCP(
            rows=[{"id": "row-pk"}],
            normalized=(200, {"requestNormalized": {"content": []}}),
        )
        got = smoke._mm_normalized(cp, "trace-abc", "request", deadline_s=0)

        self.assertIsNotNone(got, "a resolvable trace must yield the payload")
        self.assertIn(
            "requestId=trace-abc",
            cp.paths[0],
            "the trace id must be resolved through the requestId filter, "
            f"not used as a row key; asked for {cp.paths[0]!r}",
        )
        self.assertIn(
            "/traffic/row-pk/normalized",
            cp.paths[-1],
            "the payload must be fetched under the RESOLVED row id; "
            f"asked for {cp.paths[-1]!r}",
        )

    def test_a_row_that_cannot_be_read_is_a_failure_not_a_timeout(self):
        cp = self._StubCP(rows=[{"id": "row-pk"}], normalized=(404, None))
        got = smoke._mm_normalized(cp, "trace-abc", "request", deadline_s=0)
        self.assertIsNotNone(
            got,
            "the row was found, so a 404 on its payload is a real defect — "
            "returning None reports it as 'the writer was slow'",
        )
        self.assertTrue(got.get("__empty__"))

    def test_no_row_at_all_stays_a_timeout(self):
        cp = self._StubCP(rows=[], normalized=(200, {}))
        self.assertIsNone(smoke._mm_normalized(cp, "trace-abc", "request", deadline_s=0))


class EveryTargetTemplateNamesTheCredentialTheSmokeNeeds(unittest.TestCase):
    """A gate whose credentials are undiscoverable is a gate that gets skipped.

    The smoke is binding on every ai-gateway change and exits at argument
    parsing without a virtual key. The local and stg templates named the
    variable; dev and prod documented seventeen others and not that one, so an
    operator who filled in the prod template by the book still could not start
    the mandatory gate — and nothing told them which variable was missing.
    """

    def test_all_four_templates_declare_the_virtual_key(self):
        for target in ("local", "dev", "stg", "prod"):
            path = _SRC.parents[1] / f".env.{target}.example"
            with self.subTest(target=target):
                self.assertRegex(
                    path.read_text(), r"(?m)^#?\s*NEXUS_(TEST_)?VK=",
                    f"{path.name} does not name the variable the smoke reads for "
                    "--vk, so a file copied from it cannot run the gate",
                )

    def test_the_prod_template_hands_the_loader_no_key_at_all(self):
        """loadenv reads the .example as DEFAULTS, so an uncommented placeholder
        there does not document the variable — it ARMS the run. Against prod the
        suite then authenticates with nvk_REPLACE_ME, 401s through every arm, and
        still disables and restores the live routing rules on its way past."""
        path = _SRC.parents[1] / ".env.prod.example"
        self.assertNotRegex(
            path.read_text(), r"(?m)^NEXUS_(TEST_)?VK=",
            f"{path.name} assigns the key uncommented; loadenv will supply it and "
            "the prod smoke will start instead of refusing",
        )

    def test_a_placeholder_key_is_refused(self):
        for value in ("nvk_REPLACE_ME", "REPLACE_WITH_PROD_VK", "", "   "):
            with self.subTest(value=value):
                self.assertFalse(
                    smoke._vk_is_usable(value),
                    f"{value!r} would start a live run that cannot authenticate",
                )
        self.assertTrue(smoke._vk_is_usable("nvk_0123456789abcdef"))


class ResultLabelsAreUnique(unittest.TestCase):
    """Two verdicts under one label make the run unreadable, and worse.

    Three media arms shared a base label: a dedicated comprehension case pinned
    to a model chosen for the capability, and an ingress arm's derived
    comprehension half running on whatever the general chat model was. They ask
    different questions, and under one name one arm's pass sat beside the
    other's failure with nothing to tell them apart.

    The consequence is not only cosmetic. The P8 deprecation pass builds a dict
    keyed by result name, so a duplicate silently keeps only the last — and that
    map decides which models are removed from the catalog.
    """

    def setUp(self):
        self._saved = list(smoke._results)
        smoke._results.clear()

    def tearDown(self):
        smoke._results.clear()
        smoke._results.extend(self._saved)

    def test_a_duplicate_is_reported_with_its_count(self):
        smoke.rec("PMM", "media/x").passed()
        smoke.rec("PMM", "media/x").failed()
        smoke.rec("PMM", "media/y").passed()
        dupes = smoke._duplicate_labels()
        self.assertEqual(len(dupes), 1, f"expected exactly the one duplicate, got {dupes}")
        self.assertIn("media/x", dupes[0])
        self.assertIn("2", dupes[0], "the count belongs in the message — it says how many verdicts were merged")

    def test_the_same_name_in_two_phases_is_not_a_duplicate(self):
        smoke.rec("PMM", "content").passed()
        smoke.rec("PVG", "content").passed()
        self.assertEqual(smoke._duplicate_labels(), [])

    def test_a_clean_run_reports_nothing(self):
        smoke.rec("PMM", "a").passed()
        smoke.rec("PMM", "b").passed()
        self.assertEqual(smoke._duplicate_labels(), [])

    def test_no_ingress_arm_shares_a_base_label_with_a_dedicated_comprehension_case(self):
        """The static half: the collision must not be reintroduced in source.

        The runtime check above only sees labels a RUN produced, and the media
        arms are gated on which models the catalog happens to offer — so an arm
        that did not run cannot collide, and the collision would return
        unnoticed the day it does.
        """
        src = _SRC.read_text()
        ingress = set(re.findall(r'_mm_ingress_case\(\s*gw,\s*cp,\s*"([^"]+)"', src))
        dedicated = set(re.findall(r'_mm_comprehension_case\(\s*gw,\s*"([^"]+)"', src))
        self.assertTrue(ingress and dedicated, "the label scrape found nothing; it has drifted from the source")

        # Read the derived suffix out of the source instead of restating it.
        # Written the other way, this test kept passing when the suffix was put
        # back to the colliding one: it compared against the suffix it assumed
        # rather than the one the code emits, so it was green for a reason that
        # had nothing to do with the property it names.
        m = re.search(r'_mm_assert_comprehension\(f"\{label\}(:[a-z-]+)"', src)
        self.assertIsNotNone(m, "could not find the derived comprehension label; this gate guards nothing")
        suffix = m.group(1)
        collisions = sorted({i + suffix for i in ingress} & dedicated)
        self.assertEqual(
            collisions, [],
            f"these labels are produced twice: {collisions}. A dedicated comprehension case "
            f"and an ingress arm's derived half must not share a name — they run against "
            f"different models and answer different questions")
