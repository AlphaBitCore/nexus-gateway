"""Unit tests for engine.hooks_control — the valid CP-API hooks toggle + the
runtime-convergence proof that replaces the invalid June-16 DB-edit approach.
Mocks the runtime reader and the bash subprocess; no live CP, no AWS."""
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from engine import hooks_control as hc  # noqa: E402
from engine.hooks_control import RuntimeState  # noqa: E402


def _state(**kw) -> RuntimeState:
    base = dict(source_ok=True, hook_count=0, response_hook_count=0, enabled_names=[],
                desired_ver=5, reported_ver=5, detail="")
    base.update(kw)
    return RuntimeState(**base)


class TestPollUntilConverged(unittest.TestCase):
    def test_off_converges_when_no_response_hooks_and_versions_equal(self):
        reader = lambda cp, nid=None: _state(hook_count=0, response_hook_count=0, enabled_names=[])
        ok, ms, st = hc.poll_until_converged("off", "http://cp", timeout_s=1, interval_s=0.01, reader=reader)
        self.assertTrue(ok)
        self.assertEqual(st.response_hook_count, 0)

    def test_off_blocks_when_compliance_hook_still_enabled(self):
        # runtime says versions converged but pii-scanner is still enabled → NOT off
        reader = lambda cp, nid=None: _state(hook_count=1, response_hook_count=0, enabled_names=["pii-scanner"])
        ok, ms, st = hc.poll_until_converged("off", "http://cp", timeout_s=0.2, interval_s=0.01, reader=reader)
        self.assertFalse(ok)

    def test_off_blocks_when_versions_not_converged(self):
        reader = lambda cp, nid=None: _state(desired_ver=6, reported_ver=5, response_hook_count=0)
        ok, ms, st = hc.poll_until_converged("off", "http://cp", timeout_s=0.2, interval_s=0.01, reader=reader)
        self.assertFalse(ok)  # desired_ver != reported_ver → gateway hasn't applied the push

    def test_on_converges_when_hooks_present(self):
        reader = lambda cp, nid=None: _state(hook_count=4, response_hook_count=1, enabled_names=["pii-scanner", "keyword-blocker", "a", "b"])
        ok, ms, st = hc.poll_until_converged("on", "http://cp", timeout_s=1, interval_s=0.01, reader=reader)
        self.assertTrue(ok)

    def test_unreadable_runtime_never_reports_converged(self):
        reader = lambda cp, nid=None: RuntimeState(source_ok=False, hook_count=None, response_hook_count=None)
        ok, ms, st = hc.poll_until_converged("off", "http://cp", timeout_s=0.1, interval_s=0.01, reader=reader)
        self.assertFalse(ok)
        self.assertFalse(st.source_ok)


class TestReadRuntimeState(unittest.TestCase):
    def _fake_client(self, runtime_json, status=200):
        client = mock.MagicMock()
        resp = mock.MagicMock(status_code=status)
        resp.json.return_value = runtime_json
        client.get.return_value = resp
        return client

    def test_parses_enabled_and_response_counts(self):
        runtime = {
            "meta": {"desired_ver": 7, "reported_ver": 7},
            "snapshot": {"sources": {"config.hooks": {"ok": True, "value": [
                {"name": "pii-scanner", "stage": "request", "enabled": True},
                {"name": "keyword-blocker", "stage": "request", "enabled": False},
                {"name": "response-quality-signals", "stage": "response", "enabled": True},
            ]}}},
        }
        with mock.patch.dict("os.environ", {"NEXUS_ADMIN_API_KEY": "k", "NEXUS_GW_NODE_ID": "gw-x-3050"}):
            st = hc.read_runtime_state("http://cp", node_id="gw-x-3050", client=self._fake_client(runtime))
        self.assertTrue(st.source_ok)
        self.assertEqual(st.hook_count, 2)               # 2 enabled
        self.assertEqual(st.response_hook_count, 1)       # 1 response-stage enabled
        self.assertEqual(st.enabled_names, ["pii-scanner", "response-quality-signals"])
        self.assertTrue(st.versions_converged)

    def test_no_admin_key_returns_source_unavailable_not_fabricated(self):
        with mock.patch.dict("os.environ", {}, clear=True):
            st = hc.read_runtime_state("http://cp")
        self.assertFalse(st.source_ok)
        self.assertIsNone(st.hook_count)  # never a fabricated 0

    def test_source_not_ok_in_snapshot(self):
        runtime = {"meta": {"desired_ver": 1, "reported_ver": 1},
                   "snapshot": {"sources": {"config.hooks": {"ok": False, "error": "boom"}}}}
        with mock.patch.dict("os.environ", {"NEXUS_ADMIN_API_KEY": "k"}):
            st = hc.read_runtime_state("http://cp", node_id="n", client=self._fake_client(runtime))
        self.assertFalse(st.source_ok)


class TestSetHooksDelegatesToBash(unittest.TestCase):
    def test_set_hooks_runs_the_script_and_raises_on_failure(self):
        import subprocess
        with mock.patch("engine.hooks_control.subprocess.run") as run, \
             mock.patch.object(hc, "_SCRIPT", Path("/does/not/matter")), \
             mock.patch("pathlib.Path.exists", return_value=True):
            run.return_value = mock.MagicMock(returncode=0, stdout="converged")
            hc.set_hooks("off")
            args = run.call_args[0][0]
            self.assertEqual(args[0], "bash")
            self.assertEqual(args[-1], "off")
            # non-zero exit → CalledProcessError propagates (caller must not run)
            run.side_effect = subprocess.CalledProcessError(1, args, output="401")
            with self.assertRaises(subprocess.CalledProcessError):
                hc.set_hooks("off")

    def test_rejects_bad_mode(self):
        with self.assertRaises(ValueError):
            hc.set_hooks("maybe")


if __name__ == "__main__":
    unittest.main()
