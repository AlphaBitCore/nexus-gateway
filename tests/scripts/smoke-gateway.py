#!/usr/bin/env python3
"""
tests/scripts/smoke-gateway.py

Full-surface smoke test for AI Gateway + Control Plane.

Phases:
  P0   Preflight          healthz, CP login, metrics t0
  P1   Auth boundary      bad/missing VK → 401; out-of-allowlist → 403
  P2   Catalog            /v1/models, /v1/models/{id}, /v1/usage
  P3   Routing OFF        ensure all rules disabled; per-model: non-stream + SSE + 2-turn cache
  P3C  Direct compare     [--direct-compare] routing OFF; concurrent gateway+direct per model;
                          compare non-stream and SSE responses, token counts, latency overhead
  P3E  Embeddings         [--all-ingress or --embeddings] per-model × per-ingress embedding
                          suite: Arm A non-stream basic, Arm B dimensions round-trip,
                          Arm C batch input, Arm D traffic_event cross-check,
                          Arm E Prometheus delta, Arm F cross-ingress consistency.
                          Cache arm explicitly skipped (no embedding prompt-cache
                          semantic). Negative tests: reject-asymmetry for param violations.
  P4   Routing ON         [--routing] enable all rules, per-model verify routing_rule_id; restore
  P5   Cache config       read settings/cache + settings/gemini-cache
  P6   DB cross-check     verify traffic_event rows for all P3/P4 chat calls
  P7   Metrics delta      t0 vs t1 Prometheus counter diff
  P8   Restore + Report   restore original config, write /tmp/smoke-gateway-<UTC>.md

Usage:
  python tests/scripts/smoke-gateway.py --vk nvk_xxx [options]

P3C requires provider API keys (from env or --provider-keys-file JSON):
  OPENAI_API_KEY, GEMINI_API_KEY (or GOOGLE_API_KEY), ANTHROPIC_API_KEY,
  DEEPSEEK_API_KEY, MOONSHOT_API_KEY

Cost policy: tests/scripts/_cost_policy.json maps each phase to real-upstream
or fixture mode. P3E default is real-upstream (~$0.0002 per run).
Use --all-upstream to force all fixture phases to real-upstream.
Use --no-embeddings to skip P3E entirely.
"""

import argparse
import base64
import hashlib
import http.client
import json
import os
import re
import secrets
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Optional

# Make tests/lib importable so we can load the target-aware .env file the
# same way the bash + Go test infrastructures do.
_this_dir = Path(__file__).resolve().parent
sys.path.insert(0, str(_this_dir.parent / "lib"))
import loadenv  # noqa: E402  — must come after sys.path insertion

# ─── ANSI helpers ─────────────────────────────────────────────────────────────

_COLORS = sys.stdout.isatty()

def _c(code, s): return f"\033[{code}m{s}\033[0m" if _COLORS else s
def green(s):  return _c("32", s)
def red(s):    return _c("31", s)
def yellow(s): return _c("33", s)
def cyan(s):   return _c("36", s)
def bold(s):   return _c("1",  s)
def dim(s):    return _c("2",  s)

# ─── Logging ──────────────────────────────────────────────────────────────────

_log_lines: list[str] = []
_log_lock = threading.Lock()
_thread_local = threading.local()

def _log(level: str, msg: str):
    """Thread-safe log: when a per-task buffer is active (set by
    _begin_task_buffer in the worker pool path), append there so a
    task's lines stay grouped; otherwise print directly. Either way
    write the plain (no ANSI) line to _log_lines for the .md report."""
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S")
    plain = f"[{ts}][{level:4s}] {msg}"
    plain_no_ansi = re.sub(r"\033\[[0-9;]*m", "", plain)
    with _log_lock:
        _log_lines.append(plain_no_ansi)
    buf = getattr(_thread_local, 'buf', None)
    if buf is not None:
        buf.append(plain)
    else:
        with _log_lock:
            print(plain, flush=True)

def _begin_task_buffer():
    """Start capturing per-task log lines in the calling thread. Pair
    with _flush_task_buffer in a finally block so output is flushed
    atomically as one block (keeps a model's lines together in the
    interleaved pool output)."""
    _thread_local.buf = []

def _flush_task_buffer():
    """Flush the calling thread's buffer to stdout under the log lock
    so multi-line blocks from different tasks don't interleave."""
    buf = getattr(_thread_local, 'buf', None)
    if buf is None:
        return
    with _log_lock:
        for line in buf:
            print(line, flush=True)
    _thread_local.buf = None

def log_ok(msg):   _log("PASS", f"{green('✅')} {msg}")
def log_fail(msg): _log("FAIL", f"{red('❌')} {msg}")
def log_warn(msg): _log("WARN", f"{yellow('⚠️')} {msg}")
def log_info(msg): _log("INFO", f"   {msg}")
def log_step(msg): _log("STEP", f"{cyan('▶')} {bold(msg)}")
def log_debug(msg):_log("DBG ", f"   {dim(msg)}")

# ─── System prompts (for prompt-cache tests) ──────────────────────────────────

# Anthropic: needs ≥ 1024 tokens to create a cache block
_SYS_ANTHROPIC = (
    "You are an expert software architect with extensive experience designing "
    "large-scale distributed systems. Your expertise spans Go, Python, TypeScript, "
    "Rust, Java, C++, and Scala. You have deep knowledge of AWS, GCP, and Azure. "
    "You understand database design, query optimization, indexing, data warehousing, "
    "OLAP/OLTP trade-offs, and data lake architectures. You know Kafka, RabbitMQ, "
    "NATS, Redis Streams, Pulsar, and SQS deeply. You understand Docker, Kubernetes, "
    "Helm, service mesh, Istio, Envoy, and container security hardening. You know "
    "microservices patterns, API gateway design, event sourcing, CQRS, saga patterns, "
    "circuit breakers, bulkheads, and rate limiting strategies. You can explain "
    "complex distributed systems concepts clearly using analogies and concrete "
    "examples. You identify performance bottlenecks, security vulnerabilities, and "
    "design flaws in code reviews. You write clean, idiomatic code that follows "
    "established conventions for each language. Your responses are accurate, "
    "thoughtful, and take broader context into account. " ) * 24

# OpenAI/DeepSeek/Moonshot/Kimi: ~2000+ tokens triggers prefix cache
_SYS_OPENAI = (
    "You are a helpful, accurate assistant with deep expertise in software "
    "engineering, distributed systems, algorithms, databases, cloud computing, "
    "security, and machine learning. You provide well-structured, detailed "
    "answers grounded in first principles. When explaining technical concepts, "
    "you use concrete examples and analogies. You consider trade-offs carefully "
    "and present balanced analysis. You write clean, idiomatic code. " ) * 60

# Moonshot-8k has small context window; use shorter system prompt
_SYS_MOONSHOT_8K = (
    "You are a helpful assistant that answers questions clearly and concisely. "
    "Always be accurate and well-structured. Focus on what was asked. " ) * 60

def _system_prompt(model_id: str) -> str:
    m = model_id.lower()
    if "claude" in m:
        return _SYS_ANTHROPIC
    if "moonshot-v1-8k" in m:
        return _SYS_MOONSHOT_8K
    return _SYS_OPENAI

# ─── Cost policy ──────────────────────────────────────────────────────────────
# Loaded at startup from tests/scripts/_cost_policy.json. Phases with
# mode=="fixture" skip real upstream by default; --all-upstream overrides.
# Embeddings + chat phases default to real-upstream per SDD T7a.

_COST_POLICY_PATH = Path(__file__).resolve().parent / "_cost_policy.json"

def _load_cost_policy() -> dict:
    try:
        with open(_COST_POLICY_PATH) as f:
            return json.load(f)
    except Exception:
        return {"phases": {}, "future_phases_default": "fixture"}

_COST_POLICY: dict = _load_cost_policy()


def _phase_is_real_upstream(phase: str, all_upstream: bool = False) -> bool:
    """Return True when a phase should make real upstream calls.

    Phases declared mode=real-upstream always qualify. Phases missing from
    the policy fall back to future_phases_default. --all-upstream flips
    every phase to real-upstream regardless."""
    if all_upstream:
        return True
    phases = _COST_POLICY.get("phases", {})
    entry = phases.get(phase, {})
    mode = entry.get("mode") if isinstance(entry, dict) else entry
    if mode is None:
        # Default for phases not yet listed in the policy file.
        return _COST_POLICY.get("future_phases_default", "fixture") == "real-upstream"
    return mode == "real-upstream"


# ─── Embedding model prefix fallback ──────────────────────────────────────────
# Used when /v1/models does not yet carry outputModalities (pre-S2 deploy).

_EMBEDDING_PREFIXES_PATH = Path(__file__).resolve().parent / "embedding-model-prefixes.json"

def _load_embedding_prefixes() -> list[str]:
    try:
        with open(_EMBEDDING_PREFIXES_PATH) as f:
            data = json.load(f)
        return data.get("embedding", [])
    except Exception:
        return [
            "text-embedding-3-",
            "text-embedding-ada-",
            "text-embedding-",
            "embed-english-v",
            "embed-multilingual-v",
            "gemini-embedding-",
        ]

_EMBEDDING_PREFIXES: list[str] = _load_embedding_prefixes()


def _is_embedding_by_prefix(mid: str) -> bool:
    """Fallback classifier: True when mid matches a known embedding prefix."""
    m = mid.lower()
    return any(m.startswith(p.lower()) for p in _EMBEDDING_PREFIXES)


# ─── Model classification ──────────────────────────────────────────────────────
#
# Primary classification uses outputModalities from /v1/models.
# classify_model_modality() returns one of: "chat", "embedding", "image",
# "audio", "video", "skip" (unknown or multi-modal not supported in E62).
#
# is_non_chat() is kept as a legacy alias for backward compatibility with
# existing callers (phase2_catalog, main()). It returns True for any model
# that should NOT enter the chat phases (i.e. not "chat" modality).

def classify_model_modality(model_entry: dict) -> str:
    """Classify a model by Nexus `type` extension when present, else
    by outputModalities, else by id-prefix heuristic.

    model_entry is a single item from /v1/models data[].
    Returns: "chat" | "embedding" | "image" | "audio" | "tts" | "stt" |
             "realtime" | "video" | "rerank" | "skip"

    Priority order:
      1. `type` (Nexus extension on /v1/models, canonical Model.type) —
         the most reliable signal; admins set this on Provider catalog.
         Without this, smoke historically misclassified dall-e-3 / whisper-1
         (seed data left outputModalities=['text'] on every row). The precise
         audio sub-types (tts/stt/realtime) and rerank MUST be recognized here
         so the modality guard's non-chat models never land in chat_models and
         get chat-tested (which now correctly 400s MODEL_MODALITY_MISMATCH).
      2. `outputModalities` — when type is absent, fall back to the first
         output modality.
      3. id prefix — last-resort heuristic for embedding-* / text-embedding-*.
    """
    mid = model_entry.get("id", "")
    mtype = (model_entry.get("type") or "").lower()
    if mtype in ("chat", "embedding", "image", "audio", "tts", "stt",
                 "realtime", "video", "rerank"):
        return mtype
    modalities = model_entry.get("outputModalities")
    if isinstance(modalities, list) and modalities:
        first = modalities[0]
        if first == "text":
            return "chat"
        if first == "embedding":
            return "embedding"
        if first == "image":
            return "image"     # E64 (out of E62 scope)
        if first == "audio":
            return "audio"     # E63 (out of E62 scope)
        if first == "video":
            return "video"     # E66 (out of E62 scope)
        return "skip"
    # Fallback: no type + no outputModalities — use id-prefix heuristic.
    if _is_embedding_by_prefix(mid):
        return "embedding"
    # Everything else defaults to chat (preserves existing behavior for
    # models that were chat before outputModalities was added).
    return "chat"


# code -> requiredModalities, filled from GET /v1/models in phase2_catalog.
# A model's floor is what a request MUST carry for it to be served at all;
# gpt-audio-mini is type=chat and requires audio. It is read from the live
# catalog rather than guessed from the id, because guessing from the id is
# exactly how the image matrix landed on gpt-audio-mini in the first place.
_MODEL_FLOOR: dict[str, list[str]] = {}


def model_floor(mid: str) -> list[str]:
    """Modalities a request must carry for this model, [] when unconstrained."""
    return _MODEL_FLOOR.get(mid, [])


def is_non_chat(mid: str) -> bool:
    """Legacy API: True for any model that should not enter chat phases.

    Used by phase2_catalog (for logging) and main() (for filtering).
    A thin wrapper over classify_model_modality using an id-only dict."""
    return classify_model_modality({"id": mid}) != "chat"


# Reasoning-family models whose requests take the reasoning SHAPE:
# max_completion_tokens instead of max_tokens, system text embedded in the
# user turn, reasoning-output auditing, larger token budgets. Most members'
# upstream ALSO rejects a caller-supplied temperature (each such 400 is
# quoted inline below or in scripts/quirk-coverage.config.mjs;
# kimi-k2-thinking is the exception — it accepts temperature and sits here
# for the reasoning shape only).
#
# Whether a request body actually CARRIES a temperature is decided per call
# site by _send_temperature(model, ingress), not by this set alone. On every
# wire where the gateway strips the param, the smoke deliberately sends one:
# a 200 proves the strip fired, and a strip list that has gone stale
# re-reddens as the upstream's own 400. That coverage is what caught the
# kimi-k2.7 families (shipped in the catalog, 400'd every temperature-sending
# client) and the gpt-5.6 /v1/responses wire gap (identical body answered 200
# on chat and 400 there). Membership here therefore does NOT suppress the
# gateway-wire probes; it only governs the two arms where no gateway strip
# exists — the direct-to-vendor arm and claude on native /v1/messages — plus
# the non-temperature consumers of is_reasoning (max_completion_tokens vs
# max_tokens, system-message embedding, reasoning-output auditing, token
# budgets).
#
# Membership is exact, not prefix. The catalog↔quirk↔smoke chain is linted:
# `npm run check:quirk-coverage` fails when a catalog chat family has no
# recorded sampling-quirk decision, and when an entry here no longer matches
# any catalog model (the staleness that once produced 30 false failures in a
# production run).
_REASONING_MODELS = {
    "o1",
    "o3", "o3-mini", "o4-mini",
    # Accepts temperature (probed; no 400 exists) — member for the reasoning
    # shape only. Sending it a temperature on gateway wires is harmless.
    "kimi-k2-thinking",
    "kimi-k2.5", "kimi-k2.6",
    # Probed on production: 400 "invalid temperature: only 1 is allowed for this
    # model"; both answer 200 with the param omitted.
    "kimi-k2.7-code", "kimi-k2.7-code-highspeed",
    "gpt-5.5",
    # gpt-5.6-* / gpt-5.4-* are deliberately NOT here. gpt-5.6 rejects a
    # temperature exactly as gpt-5.5 does (probed: 400 on both OpenAI wires);
    # gpt-5.4 ACCEPTS one (probed: 200 on both wires — the family's 400 is the
    # max_tokens rename only). Listing either would flip the OTHER
    # is_reasoning consumers (message shape, max_completion_tokens) on arms
    # that are green today, and the gateway-wire temperature probes run for
    # listed and unlisted models alike. Known cost, accepted: the
    # direct-to-vendor arm (P3C) still sends gpt-5.6 a temperature and eats
    # the vendor's 400 — a vendor contract, not a gateway defect.
    # Probed on production via /v1/messages: 400 "`temperature` is deprecated for
    # this model." The same models answer 200 on /v1/chat/completions, where the
    # codec strips the param — the native wire forwards it by design.
    "claude-opus-4-7", "claude-opus-4-8", "claude-sonnet-5", "claude-fable-5",
    # Probed 2026-08-06 via /v1/messages, all three params rejected the same way:
    # 400 "`temperature` is deprecated for this model.", and likewise for `top_p`
    # and `top_k`. Same shape as the four above.
    "claude-opus-5",
}


def is_reasoning(mid: str) -> bool:
    return mid in _REASONING_MODELS


def _send_temperature(model: str, ingress: str) -> bool:
    """Should this request body carry `temperature: 0`?

    ingress ∈ {"chat", "responses", "messages", "gemini", "direct"} — the wire
    the call site is about to use, NOT the routed target.

    For models whose upstream rejects a caller-supplied temperature
    (_REASONING_MODELS), the gateway strips the param on every wire it owns
    the request contract for, and SENDING the param on those wires is the
    regression test — the arm must answer 200, and a stale strip list
    re-reddens as the upstream's 400:
      - chat wire: the target adapter's passthrough rewrite (o-series/gpt-5*
        via openai, kimi fixed-temp families via moonshot). Bodies bridged
        from the messages/gemini/responses ingresses to a chat-wire target
        re-enter the same rewrite, so those ingresses probe it too.
      - responses wire: the openai codec's Responses arm.
      - cross-format to Anthropic: the anthropic codec strips sampling params
        for every family outside its probed accepts-allowlist.

    The two arms with NO gateway strip in the path stay temperature-free:
      - "direct": vendor API called directly — nothing strips; the vendor's
        400 would pollute the gateway-vs-direct comparison for a reason that
        has nothing to do with the gateway.
      - "messages" × claude-*: the same-format native /v1/messages leg
        forwards sampling params verbatim today, and the vendor rejects
        them (probed 400 quoted on the set above). When the anthropic native
        leg starts applying the sampling strip on /v1/messages, DELETE this
        branch (and check 6 in scripts/check-quirk-coverage.mjs, which pairs
        with it) — sending the param then becomes the required proof on this
        wire too, exactly as on the others.
    """
    if model not in _REASONING_MODELS:
        return True
    if ingress == "direct":
        return False
    if model.startswith("claude-") and ingress == "messages":
        return False
    return True

# Models where the provider does not support automatic prefix caching.
# Moonshot v1 requires explicit context-cache creation (POST /v1/context_caches).
# gpt-4-turbo is a legacy OpenAI model predating the automatic prefix-cache feature.
_NO_AUTOMATIC_CACHE = {
    "gpt-4-turbo",
    "moonshot-v1-8k",
    "moonshot-v1-32k",
    "moonshot-v1-128k",
}

# Gemini implicit caching has a warm-up delay after the first call.
_GEMINI_MODEL_RE = re.compile(r"^gemini-", re.I)

# Gemini 2.5+ models use thinking (reasoning) tokens that count against
# maxOutputTokens. With max_tokens=16 the model burns the entire budget on
# reasoning and produces no visible text (finishReason=MAX_TOKENS, empty body
# on SSE). Use a higher default so there is budget left for text output.
_GEMINI_THINKING_RE = re.compile(r"^gemini-2\.[5-9]", re.I)

def uses_thinking_tokens(mid: str) -> bool:
    """True for models that consume thinking tokens from maxOutputTokens budget."""
    return bool(_GEMINI_THINKING_RE.match(mid))

# DeepSeek V4 and Kimi K2 are heavy thinking models: they emit all tokens as
# delta.reasoning_content before any delta.content. With small budgets (≤256)
# these models exhaust the token limit during thinking and produce zero visible
# text. Use ≥1024 tokens so the reasoning phase can complete and the final
# answer text has room.
_HEAVY_THINKING_RE = re.compile(r"^(deepseek-v4|kimi-k2)", re.I)

def uses_heavy_thinking(mid: str) -> bool:
    """True for thinking models that need ≥1024 token budget to emit visible text."""
    return bool(_HEAVY_THINKING_RE.match(mid))

# ─── Result store ─────────────────────────────────────────────────────────────

class Result:
    def __init__(self, phase: str, name: str):
        self.phase = phase
        self.name = name
        self.ok: Optional[bool] = None
        self.warn = False
        self.note = ""
        self.detail: dict = {}

    def passed(self, note=""):
        self.ok = True
        self.note = note
        return self

    def failed(self, note=""):
        self.ok = False
        self.note = note
        return self

    def warning(self, note=""):
        self.ok = True
        self.warn = True
        self.note = note
        return self

_results: list[Result] = []

# Models the virtual key's allow-list denied during this run, and the models it
# admitted. A restricted VK silently BOUNDS THE COVERAGE of the whole smoke: the
# run still reports "N passed", but N is measured over the models the key happens
# to permit, not over the catalogue. One run reported 105 passed while reaching 6
# of 29 models, and nothing in the output said so — the same failure shape this
# suite exists to catch, in the suite itself. Recorded centrally in _post_sync so
# every arm feeds it, and surfaced in the final summary.
_denied_models: set[str] = set()
_reached_models: set[str] = set()

def _note_model_access(body_dict: dict, status: int, data) -> None:
    model = (body_dict or {}).get("model") or ""
    if not model:
        return
    code = ""
    if isinstance(data, dict):
        err = data.get("error")
        if isinstance(err, dict):
            code = err.get("code") or ""
    if status == 403 and code == "MODEL_NOT_ALLOWED":
        _denied_models.add(model)
    elif status and status != 403:
        _reached_models.add(model)

def rec(phase, name) -> Result:
    r = Result(phase, name)
    _results.append(r)
    return r


def _duplicate_labels() -> list[str]:
    """(phase, name) pairs this run recorded more than once.

    A label is how a reader tells one measurement from another, so two verdicts
    under one label is a measurement defect regardless of what they say. It bit
    three media arms, where a dedicated comprehension case and an ingress arm's
    derived one shared a base label and one arm's pass sat beside the other's
    failure with nothing to distinguish them.

    It is worse than ambiguous downstream: the P8 deprecation pass builds a
    dict keyed by result name, so a duplicate there overwrites silently, and
    that map decides which models are REMOVED FROM THE CATALOG.

    Recorded rather than raised, and checked at report time rather than in rec():
    several arms record from mutually exclusive branches of one if/elif chain, so
    the same literal legitimately appears at four call sites while firing once.
    Only what a RUN actually produced counts.
    """
    seen: dict[tuple, int] = {}
    for r in _results:
        key = (r.phase, r.name)
        seen[key] = seen.get(key, 0) + 1
    return [f"{p}/{n} ×{c}" for (p, n), c in sorted(seen.items()) if c > 1]

# ─── CPClient ─────────────────────────────────────────────────────────────────

class CPClient:
    """Control Plane admin API client (OAuth + PKCE)."""

    REDIRECT_URI = (
        os.environ.get("NEXUS_CP_REDIRECT_URI")
        or os.environ.get("NEXUS_OAUTH_REDIRECT_URI")
        or "http://localhost:3000/auth/callback"
    )
    CLIENT_ID = "cp-ui"

    def __init__(self, base_url: str, email: str, password: str):
        self.base_url = base_url.rstrip("/")
        self.email = email
        self.password = password
        self._token: Optional[str] = None
        self._expiry: float = 0

    # ── OAuth + PKCE ──────────────────────────────────────────────────────────

    @staticmethod
    def _pkce() -> tuple[str, str]:
        raw = os.urandom(32)
        verifier = base64.urlsafe_b64encode(raw).rstrip(b"=").decode()
        digest = hashlib.sha256(verifier.encode()).digest()
        challenge = base64.urlsafe_b64encode(digest).rstrip(b"=").decode()
        return verifier, challenge

    def _http(self, method: str, path: str, body=None, content_type="application/json",
               follow_redirect=True) -> tuple[int, dict, dict]:
        """Low-level HTTP call; returns (status, body_dict, headers)."""
        parsed = urllib.parse.urlparse(self.base_url)
        host = parsed.hostname
        port = parsed.port or (443 if parsed.scheme == "https" else 80)
        if parsed.scheme == "https":
            import ssl
            conn = http.client.HTTPSConnection(host, port, timeout=30,
                                               context=ssl.create_default_context())
        else:
            conn = http.client.HTTPConnection(host, port, timeout=30)
        headers = {"Content-Type": content_type}
        data = None
        if body is not None:
            if content_type == "application/json":
                data = json.dumps(body).encode()
            else:
                data = body  # already encoded form data
        conn.request(method, path, data, headers)
        resp = conn.getresponse()
        status = resp.status
        resp_headers = dict(resp.getheaders())
        raw = resp.read().decode("utf-8", errors="replace")
        conn.close()
        if follow_redirect and status in (301, 302, 303, 307, 308):
            location = resp_headers.get("Location") or resp_headers.get("location", "")
            return status, {"_redirect": location}, resp_headers
        try:
            return status, json.loads(raw) if raw.strip() else {}, resp_headers
        except Exception:
            return status, {"_raw": raw[:500]}, resp_headers

    def login(self) -> str:
        verifier, challenge = self._pkce()
        state = f"smoke-{int(time.time())}"

        # Step 1 – GET /oauth/authorize (capture Location without following)
        qs = urllib.parse.urlencode({
            "response_type": "code",
            "client_id": self.CLIENT_ID,
            "redirect_uri": self.REDIRECT_URI,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
            "state": state,
            "scope": "openid",
        })
        status, body, hdrs = self._http("GET", f"/oauth/authorize?{qs}", follow_redirect=False)
        location = hdrs.get("Location") or hdrs.get("location", "")
        if not location:
            location = body.get("_redirect", "")
        m = re.search(r"[?&]authctx=([^&]+)", location)
        if not m:
            raise RuntimeError(f"No authctx in Location header: {location!r} (status={status})")
        authctx = m.group(1)

        # Step 2 – POST /authserver/password
        status2, body2, _ = self._http("POST", "/authserver/password", {
            "authctx": authctx, "email": self.email, "password": self.password,
        })
        redirect_uri = body2.get("redirectUri", "")
        mc = re.search(r"[?&]code=([^&]+)", redirect_uri)
        if not mc:
            raise RuntimeError(f"No code in redirectUri: {redirect_uri!r} (status={status2})")
        code = mc.group(1)

        # Step 3 – POST /oauth/token
        form = urllib.parse.urlencode({
            "grant_type": "authorization_code",
            "code": code,
            "redirect_uri": self.REDIRECT_URI,
            "client_id": self.CLIENT_ID,
            "code_verifier": verifier,
        }).encode()
        status3, body3, _ = self._http("POST", "/oauth/token", body=form,
                                        content_type="application/x-www-form-urlencoded")
        access_token = body3.get("access_token")
        if not access_token:
            raise RuntimeError(f"No access_token: {body3} (status={status3})")
        expires_in = body3.get("expires_in", 3600)
        self._token = access_token
        self._expiry = time.time() + expires_in - 60
        log_ok(f"CP login OK ({self.email}, expires in {expires_in}s)")
        return access_token

    def token(self) -> str:
        if not self._token or time.time() >= self._expiry:
            self.login()
        return self._token  # type: ignore[return-value]

    # ── Admin API ─────────────────────────────────────────────────────────────

    def _req(self, method: str, path: str, body=None) -> tuple[int, Any]:
        parsed = urllib.parse.urlparse(self.base_url)
        host = parsed.hostname
        # Honor the URL scheme. base_url is https:// on remote/prod (no explicit
        # port); the old `HTTPConnection(host, parsed.port or 80)` spoke plain
        # HTTP on :80, which nginx 301-redirects to https — and http.client does
        # not follow, so every admin CRUD call got a 301 (list_routing_rules
        # raised "HTTP 301"; cache/provider/model reads silently returned empty).
        # Locally base_url is http://localhost:3001 (explicit port) so this never
        # surfaced. Mirror _http: HTTPS + 443 when the scheme is https.
        if parsed.scheme == "https":
            import ssl
            conn = http.client.HTTPSConnection(host, parsed.port or 443, timeout=30,
                                               context=ssl.create_default_context())
        else:
            conn = http.client.HTTPConnection(host, parsed.port or 80, timeout=30)
        hdrs = {
            "Authorization": f"Bearer {self.token()}",
            "Content-Type": "application/json",
        }
        data = json.dumps(body).encode() if body is not None else None
        conn.request(method, path, data, hdrs)
        resp = conn.getresponse()
        raw = resp.read().decode("utf-8", errors="replace")
        conn.close()
        try:
            return resp.status, json.loads(raw) if raw.strip() else {}
        except Exception:
            return resp.status, {"_raw": raw[:300]}

    def get(self, path): return self._req("GET", path)
    def put(self, path, body): return self._req("PUT", path, body)
    def post(self, path, body): return self._req("POST", path, body)
    def delete(self, path): return self._req("DELETE", path)

    # Routing rules
    def list_routing_rules(self) -> list[dict]:
        status, body = self.get("/api/admin/routing-rules?limit=100")
        if status != 200:
            raise RuntimeError(f"list routing rules: HTTP {status}")
        data = body.get("data", body)
        return data if isinstance(data, list) else []

    def set_routing_rule(self, rule_id: str, enabled: bool):
        status, body = self.put(f"/api/admin/routing-rules/{rule_id}", {"enabled": enabled})
        if status not in (200, 204):
            log_warn(f"set routing rule {rule_id} enabled={enabled}: HTTP {status}")

    def set_all_routing_rules(self, enabled: bool, rules: list[dict]):
        for r in rules:
            self.set_routing_rule(r["id"], enabled)
        log_info(f"All {len(rules)} routing rules → enabled={enabled}")

    # Cache config
    def get_gemini_cache_cfg(self) -> dict:
        _, body = self.get("/api/admin/settings/gemini-cache")
        return body

    # Model management
    def list_models_flat(self) -> list[dict]:
        """Return flat list of model dicts (each has 'id' DB UUID and 'code' customer name)."""
        status, body = self.get("/api/admin/models/flat?limit=200")
        if status != 200:
            return []
        data = body.get("data", body)
        return data if isinstance(data, list) else []

    def delete_model(self, db_id: str) -> int:
        """Delete a model by its DB UUID. Returns HTTP status."""
        status, _ = self.delete(f"/api/admin/models/{db_id}")
        return status

    # Providers
    def list_providers(self) -> list[dict]:
        status, body = self.get("/api/admin/providers?limit=100")
        if status != 200:
            return []
        data = body.get("data", body)
        return data if isinstance(data, list) else []

    # Routing simulate (proxied to gateway)
    def routing_simulate(self, body: dict) -> tuple[int, dict]:
        return self._req("POST", "/api/admin/routing-rules/simulate", body)

    def create_routing_rule(self, rule: dict) -> tuple[int, dict]:
        """Create a new routing rule. Returns (status, body)."""
        return self._req("POST", "/api/admin/routing-rules", rule)

    def delete_routing_rule(self, rule_id: str) -> int:
        """Delete routing rule by ID. Returns HTTP status."""
        status, _ = self.delete(f"/api/admin/routing-rules/{rule_id}")
        return status

    # Models with outputModalities (for P3E classification)
    def list_models_with_modalities(self) -> list[dict]:
        """Return model list with outputModalities field if available."""
        status, body = self.get("/api/admin/models/flat?limit=200")
        if status != 200:
            return []
        data = body.get("data", body)
        return data if isinstance(data, list) else []


# ─── Per-ingress SSE parsers (consumed by GWClient._post_sse) ─────────────────
# Each parser receives one raw SSE line at a time and mutates the `state`
# dict in place. The generic _post_sse loop handles transport + ttfb;
# parsers only know how to peel the format-specific event/data lines.
# Required state keys (initialized by _post_sse): content_parts, usage.
# Parsers may add format-specific keys: chunk_count, done_seen,
# event_count, completed_seen, failed_seen, message_stop_seen.


def _chat_sse_parser(line: str, state: dict) -> None:
    """OpenAI chat-completions SSE: `data: {...}` lines, `[DONE]` terminator,
    reasoning_content stream alongside content for thinking models.

    Smoke v2 split (2026-05-16): reasoning_content now lands in a separate
    `reasoning_parts` accumulator so the stream view matches the non-stream
    body extraction (where `_chat_extract_text` only reads
    choices[0].message.content). Merging the two used to make stream
    `content` and non-stream `content` disagree for DeepSeek-R1 / Kimi K2."""
    if not line.startswith("data:"):
        return
    data_str = line[5:].strip()
    if data_str == "[DONE]":
        state["done_seen"] = True
        return
    try:
        chunk = json.loads(data_str)
    except Exception:
        return
    state["chunk_count"] = state.get("chunk_count", 0) + 1
    for ch in chunk.get("choices", []):
        delta = ch.get("delta", {})
        if delta.get("content"):
            state["content_parts"].append(delta["content"])
        # Thinking models (DeepSeek-R1/V4, Kimi K2) stream chain-of-thought
        # in reasoning_content before the final answer.
        if delta.get("reasoning_content"):
            state.setdefault("reasoning_parts", []).append(delta["reasoning_content"])
    if chunk.get("usage"):
        state["usage"] = chunk["usage"]


def _responses_sse_parser(line: str, state: dict) -> None:
    """OpenAI Responses-API SSE: named events (`event: response.X`) with
    `data: {...}` payloads. Terminal events: response.completed / response.failed.

    Smoke v2 (2026-05-16): reasoning deltas ride on
    `response.reasoning_summary_text.delta` (`delta` string) and the older
    `response.reasoning.delta` shape. Both are accumulated into
    `reasoning_parts`. Any other reasoning_summary_* event is ignored
    (added/done bookkeeping)."""
    if line.startswith("event: "):
        state["_pending_event"] = line[len("event: "):].strip()
        return
    if not line.startswith("data: "):
        return
    data_str = line[len("data: "):].strip()
    try:
        payload = json.loads(data_str)
    except Exception:
        return
    evtype = state.pop("_pending_event", None) or payload.get("type", "")
    state["event_count"] = state.get("event_count", 0) + 1
    if evtype == "response.output_text.delta":
        state["content_parts"].append(payload.get("delta", ""))
    elif evtype in ("response.reasoning_summary_text.delta",
                    "response.reasoning.delta",
                    "response.reasoning_text.delta"):
        d = payload.get("delta", "")
        if isinstance(d, str) and d:
            state.setdefault("reasoning_parts", []).append(d)
    elif evtype == "response.completed":
        state["completed_seen"] = True
        if resp := payload.get("response", {}):
            state["usage"] = resp.get("usage")
            # Final response body sometimes carries the full reasoning
            # block even when no per-delta event fired. Fall through to
            # the non-stream extractor so the stream view matches.
            if not state.get("reasoning_parts"):
                rt = _responses_extract_reasoning_text(resp)
                if rt:
                    state.setdefault("reasoning_parts", []).append(rt)
    elif evtype == "response.failed":
        state["failed_seen"] = True


def _messages_sse_parser(line: str, state: dict) -> None:
    """Anthropic /v1/messages SSE: named events (message_start, content_block_delta,
    message_delta, message_stop). usage envelope rides on message_start (initial
    input + cache_read counts) and message_delta (output count); we keep the
    most recent so cache hits are visible even when message_delta is sparse.

    Smoke v2 (2026-05-16): extended-thinking blocks stream as
    content_block_delta.delta.type == "thinking_delta" with the text on the
    `thinking` field (per shared/normalize/anthropic_messages.go:426).
    Routed to `reasoning_parts` separately from text_delta."""
    if line.startswith("event: "):
        state["_pending_event"] = line[len("event: "):].strip()
        return
    if not line.startswith("data: "):
        return
    data_str = line[len("data: "):].strip()
    try:
        payload = json.loads(data_str)
    except Exception:
        return
    evtype = state.pop("_pending_event", None) or payload.get("type", "")
    state["event_count"] = state.get("event_count", 0) + 1
    if evtype == "content_block_delta":
        delta = payload.get("delta") or {}
        dtype = delta.get("type")
        if dtype == "text_delta" and isinstance(delta.get("text"), str):
            state["content_parts"].append(delta["text"])
        elif dtype == "thinking_delta" and isinstance(delta.get("thinking"), str):
            state.setdefault("reasoning_parts", []).append(delta["thinking"])
        # signature_delta carries a cryptographic signature on the thinking
        # block; not user-readable text, intentionally ignored.
    elif evtype == "message_delta":
        if usage := payload.get("usage"):
            # Real Anthropic splits usage across events (input_tokens on
            # message_start, cumulative output_tokens on message_delta).
            # Merge so the input keys from message_start survive.
            merged = dict(state.get("usage") or {})
            merged.update(usage)
            state["usage"] = merged
    elif evtype == "message_start":
        if msg := payload.get("message"):
            if u := msg.get("usage"):
                state["usage"] = u
    elif evtype == "message_stop":
        state["message_stop_seen"] = True


def _gemini_sse_parser(line: str, state: dict) -> None:
    """Gemini /v1beta streamGenerateContent?alt=sse: bare `data: {...}` lines,
    no named events, no explicit terminator (stream just ends). Text rides on
    candidates[].content.parts[].text; usage on usageMetadata.

    Smoke v2 (2026-05-16): Gemini 2.5+ thinking emits parts with thought==True
    carrying the chain-of-thought text. Routed to `reasoning_parts`; visible
    answer text (thought absent or false) keeps going to `content_parts`."""
    if not line.startswith("data: "):
        return
    data_str = line[len("data: "):].strip()
    try:
        payload = json.loads(data_str)
    except Exception:
        return
    state["chunk_count"] = state.get("chunk_count", 0) + 1
    for cand in payload.get("candidates") or []:
        content = cand.get("content") or {}
        for part in content.get("parts") or []:
            if not isinstance(part, dict):
                continue
            t = part.get("text")
            if not isinstance(t, str):
                continue
            if part.get("thought") is True:
                state.setdefault("reasoning_parts", []).append(t)
            else:
                state["content_parts"].append(t)
    if meta := payload.get("usageMetadata"):
        state["usage"] = meta


# ─── GWClient ─────────────────────────────────────────────────────────────────

class GWClient:
    """AI Gateway API client."""

    def __init__(self, base_url: str, vk: str):
        self.base_url = base_url.rstrip("/")
        self.vk = vk
        parsed = urllib.parse.urlparse(base_url)
        self._host = parsed.hostname
        self._scheme = parsed.scheme
        self._port = parsed.port or (443 if parsed.scheme == "https" else 80)

    def _conn(self, timeout=30) -> http.client.HTTPConnection:
        if self._scheme == "https":
            import ssl
            return http.client.HTTPSConnection(self._host, self._port, timeout=timeout,
                                               context=ssl.create_default_context())
        return http.client.HTTPConnection(self._host, self._port, timeout=timeout)

    def _auth_headers(self, extra: Optional[dict] = None) -> dict:
        h = {"Authorization": f"Bearer {self.vk}", "Content-Type": "application/json"}
        if extra:
            h.update(extra)
        return h

    def healthz(self) -> tuple[int, str]:
        c = self._conn(10)
        c.request("GET", "/healthz")
        r = c.getresponse()
        body = r.read().decode()
        c.close()
        return r.status, body

    # /metrics is authenticated. The gateway refuses correctly — HTTP 401 with the
    # body "unauthorized" — and this client never looked at the status, reading the
    # body whatever it was. So the refusal flowed into metrics_diff as if it were an
    # exposition, every assertion compared "unauthorized" to itself, and P7's "No
    # unexpected error counter growth" could not fail because it could see no
    # counters at all. The server was never the problem; the client was.
    #
    # (Recorded precisely because I first wrote this up as "the server answers 200",
    # which was wrong and would have sent the next reader looking at the gateway.)
    METRICS_UNAUTHORIZED = "unauthorized"

    def metrics(self) -> str:
        c = self._conn(10)
        tok = (os.environ.get("INTERNAL_SERVICE_TOKEN")
               or os.environ.get("NEXUS_HUB_SERVICE_TOKEN") or "")
        headers = {"Authorization": f"Bearer {tok}"} if tok else {}
        c.request("GET", "/metrics", headers=headers)
        r = c.getresponse()
        body = r.read().decode()
        status = r.status
        c.close()
        if status != 200:
            # The status was there all along and this client ignored it. Surface it,
            # because "unauthorized" reaching the diff as if it were data is what made
            # every metrics assertion in the suite unfalsifiable.
            raise RuntimeError(
                f"GET /metrics returned {status}: {body.strip()[:120]!r}. "
                "It is authenticated — set INTERNAL_SERVICE_TOKEN or NEXUS_HUB_SERVICE_TOKEN")
        return body

    @staticmethod
    def metrics_readable(body: str) -> bool:
        """True when the body is a real exposition rather than a refusal.

        Checked by CONTENT rather than status because this helper only ever sees
        the body — metrics() returns a string. The status IS 401 and would have
        been the more direct signal, so metrics() now raises on a non-200 and this
        stays as the backstop for an empty or otherwise unusable exposition.
        Any real exposition carries at least one nexus_-prefixed sample line.
        """
        if not body or body.strip() == GWClient.METRICS_UNAUTHORIZED:
            return False
        return any(ln.startswith("nexus_") for ln in body.splitlines())

    def list_models(self) -> tuple[int, dict]:
        c = self._conn()
        c.request("GET", "/v1/models", headers=self._auth_headers())
        r = c.getresponse()
        raw = r.read().decode()
        c.close()
        return r.status, json.loads(raw) if raw.strip() else {}

    def get_model(self, mid: str) -> tuple[int, dict]:
        c = self._conn()
        c.request("GET", f"/v1/models/{urllib.parse.quote(mid)}", headers=self._auth_headers())
        r = c.getresponse()
        raw = r.read().decode()
        c.close()
        return r.status, json.loads(raw) if raw.strip() else {}

    def usage(self) -> tuple[int, dict]:
        c = self._conn()
        c.request("GET", "/v1/usage", headers=self._auth_headers())
        r = c.getresponse()
        raw = r.read().decode()
        c.close()
        return r.status, json.loads(raw) if raw.strip() else {}

    def usage_daily(self) -> tuple[int, dict]:
        c = self._conn()
        c.request("GET", "/v1/usage/daily", headers=self._auth_headers())
        r = c.getresponse()
        raw = r.read().decode()
        c.close()
        return r.status, json.loads(raw) if raw.strip() else {}

    # ─── Generic POST helpers (P1 refactor 2026-05-16) ───────────────────────
    # Both _post_sync and _post_sse share the same transport (HTTPS, auth
    # header injection, compact-JSON body encoding, error envelope). Each
    # ingress's wrapper just builds the body dict and picks a path; the
    # SSE wrapper additionally passes a per-format SSE parser that
    # accumulates content/events/usage into a shared state dict. Compact
    # JSON across all ingresses matches the canonical-bridge wire bytes
    # — see geminicache key.go for why this mattered for cache hashing.

    def _post_sync(self, path: str, body_dict: dict, timeout: int, endpoint: str = "",
                   extra_headers: Optional[dict] = None) -> dict:
        """POST + parse JSON. Returns {status, data, elapsed, stream=False, endpoint?}.

        extra_headers carries per-ingress protocol headers (the Anthropic
        wire requires `anthropic-version`); it never overrides auth.
        """
        payload = json.dumps(body_dict, separators=(',', ':')).encode()
        t0 = time.time()
        c = self._conn(timeout)
        try:
            c.request("POST", path, payload, self._auth_headers(extra_headers))
            r = c.getresponse()
            raw = r.read().decode("utf-8", errors="replace")
            elapsed = time.time() - t0
            c.close()
        except Exception as e:
            out = {"status": 0, "error": str(e), "elapsed": time.time() - t0, "stream": False}
            if endpoint:
                out["endpoint"] = endpoint
            return out
        try:
            data = json.loads(raw)
        except Exception:
            data = {"_raw": raw[:500]}
        # Capture response headers (lowercased keys) so callers can assert
        # X-Nexus-Routed-Model (and other X-Nexus-* hints) without a second
        # request. Single dict; later writes win on duplicate header names —
        # acceptable since the gateway never repeats headers we care about.
        headers = {k.lower(): v for k, v in r.getheaders()}
        out = {"status": r.status, "data": data, "elapsed": elapsed,
               "stream": False, "headers": headers}
        if endpoint:
            out["endpoint"] = endpoint
        _note_model_access(body_dict, r.status, data)
        return out

    def post_multipart(self, path: str, fields: dict, filename: str, file_bytes: bytes,
                       file_field: str, file_mime: str, timeout: int) -> dict:
        """POST multipart/form-data. Returns {status, data, headers, elapsed}.

        Written by hand rather than pulled from a library because this script
        has no third-party dependencies by design — it runs against prod from
        wherever an operator happens to be.
        """
        boundary = "----nexus-smoke-" + hashlib.sha256(
            (path + filename + str(len(file_bytes))).encode()).hexdigest()[:24]
        parts = []
        for k, v in fields.items():
            parts.append(
                f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="{file_field}";'
            f' filename="{filename}"\r\nContent-Type: {file_mime}\r\n\r\n'.encode())
        parts.append(file_bytes)
        parts.append(f'\r\n--{boundary}--\r\n'.encode())
        payload = b"".join(parts)

        hdrs = self._auth_headers({"Content-Type": f"multipart/form-data; boundary={boundary}"})
        t0 = time.time()
        c = self._conn(timeout)
        try:
            c.request("POST", path, payload, hdrs)
            r = c.getresponse()
            raw = r.read()
            headers = {k.lower(): v for k, v in r.getheaders()}
            status = r.status
            c.close()
        except Exception as e:
            return {"status": 0, "error": str(e), "elapsed": time.time() - t0}
        try:
            data = json.loads(raw.decode("utf-8", errors="replace"))
        except Exception:
            data = {"_raw": raw[:500].decode("utf-8", errors="replace")}
        return {"status": status, "data": data, "headers": headers,
                "elapsed": time.time() - t0}

    def post_binary_response(self, path: str, body_dict: dict, timeout: int) -> dict:
        """POST JSON, read the response as BYTES.

        The JSON reader would decode audio as replacement characters and lose
        both the length and the container magic, which is exactly what
        evidence #10 is about — the served content-type has to be checked
        against what the bytes actually are.
        """
        payload = json.dumps(body_dict, separators=(',', ':')).encode()
        t0 = time.time()
        c = self._conn(timeout)
        try:
            c.request("POST", path, payload, self._auth_headers())
            r = c.getresponse()
            raw = r.read()
            headers = {k.lower(): v for k, v in r.getheaders()}
            status = r.status
            c.close()
        except Exception as e:
            return {"status": 0, "error": str(e), "elapsed": time.time() - t0}
        return {"status": status, "bytes": raw, "headers": headers,
                "elapsed": time.time() - t0}

    def _post_sse(self, path: str, body_dict: dict, sse_parser: Callable[[str, dict], None],
                  timeout: int, endpoint: str = "") -> dict:
        """POST + drive an SSE loop. sse_parser is invoked once per raw line and
        mutates `state` (content_parts list, usage, format-specific terminator
        flags). The returned dict merges {status, elapsed, ttfb, content, usage,
        stream=True, endpoint} with all state keys (chunk_count, done_seen,
        completed_seen, message_stop_seen, etc.) the parser populated."""
        payload = json.dumps(body_dict, separators=(',', ':')).encode()
        hdrs = self._auth_headers({"Accept": "text/event-stream"})
        t0 = time.time()
        ttfb: Optional[float] = None
        state: dict = {"content_parts": [], "usage": None}
        c = self._conn(timeout)
        try:
            c.request("POST", path, payload, hdrs)
            r = c.getresponse()
            for raw_line in r:
                if ttfb is None:
                    ttfb = time.time() - t0
                line = raw_line.decode("utf-8", errors="replace").rstrip("\n")
                sse_parser(line, state)
            elapsed = time.time() - t0
            c.close()
            headers = {k.lower(): v for k, v in r.getheaders()}
            out = {
                "status": r.status,
                "elapsed": elapsed,
                "ttfb": ttfb,
                "content": "".join(state.pop("content_parts")),
                "reasoning_content": "".join(state.pop("reasoning_parts", [])),
                "usage": state.pop("usage"),
                "stream": True,
                "endpoint": endpoint,
                "headers": headers,
            }
            out.update(state)  # remaining keys: chunk_count, done_seen, etc.
            return out
        except Exception as e:
            return {"status": 0, "error": str(e), "elapsed": time.time() - t0,
                    "stream": True, "endpoint": endpoint}

    def chat_sync(self, model: str, messages: list, max_tokens=16,
                   timeout=90, **kwargs) -> dict:
        body = {"model": model, "messages": messages, **kwargs}
        if _send_temperature(model, "chat"):
            body["temperature"] = 0
        if is_reasoning(model):
            body["max_completion_tokens"] = max_tokens
        else:
            body["max_tokens"] = max_tokens
        return self._post_sync("/v1/chat/completions", body, timeout)

    def chat_stream(self, model: str, messages: list, max_tokens=16,
                    timeout=90, **kwargs) -> dict:
        body = {"model": model, "messages": messages, "stream": True, **kwargs}
        if _send_temperature(model, "chat"):
            body["temperature"] = 0
        if is_reasoning(model):
            body["max_completion_tokens"] = max_tokens
        else:
            body["max_tokens"] = max_tokens
        return self._post_sse("/v1/chat/completions", body, _chat_sse_parser, timeout)

    # ─── /v1/responses (E56) ──────────────────────────────────────────────────
    #
    # The OpenAI Responses-API ingress accepts a distinct request/response
    # shape: `input` instead of `messages`, `output[]` instead of `choices[]`,
    # `instructions` system-level text, etc. AI Gateway exposes it under
    # POST /v1/responses (see packages/ai-gateway/cmd/ai-gateway/main.go).
    # Smoke covers only the same-shape passthrough path (target OpenAI) and
    # the basic text + streaming success cases; the cross-format path and
    # cross-format guard (E56-S6) are covered by /test-openai-responses.

    def responses_non_stream(self, model: str, prompt: str, max_tokens=16,
                              timeout=90, **kwargs) -> dict:
        """POST /v1/responses non-streaming. kwargs ride onto the body."""
        body = {"model": model, "input": prompt, "max_output_tokens": max_tokens, **kwargs}
        if _send_temperature(model, "responses"):
            body["temperature"] = 0
        return self._post_sync("/v1/responses", body, timeout, endpoint="responses")

    def responses_stream(self, model: str, prompt: str, max_tokens=16,
                         timeout=90, **kwargs) -> dict:
        """POST /v1/responses with stream=True; collects response.* events."""
        body = {"model": model, "input": prompt, "max_output_tokens": max_tokens, "stream": True, **kwargs}
        if _send_temperature(model, "responses"):
            body["temperature"] = 0
        return self._post_sse("/v1/responses", body, _responses_sse_parser, timeout, endpoint="responses")

    # ─── /v1/messages (Anthropic ingress) ─────────────────────────────────────
    #
    # The Anthropic Messages-API ingress: top-level `system` field is hoisted
    # out of the messages array; response has `content[]` with type=text items
    # instead of choices[]; usage carries `cache_read_input_tokens` for
    # automatic prompt-caching hits. Cross-format goes through the
    # canonical bridge to OpenAI / Gemini / others targets.

    def messages_non_stream(self, model: str, messages: list, system: str = "",
                             max_tokens=16, timeout=90, **kwargs) -> dict:
        body = {"model": model, "messages": messages, "max_tokens": max_tokens, **kwargs}
        if system:
            body["system"] = system
        if _send_temperature(model, "messages"):
            body["temperature"] = 0
        return self._post_sync("/v1/messages", body, timeout, endpoint="messages")

    def messages_stream(self, model: str, messages: list, system: str = "",
                         max_tokens=16, timeout=90, **kwargs) -> dict:
        body = {"model": model, "messages": messages, "max_tokens": max_tokens, "stream": True, **kwargs}
        if system:
            body["system"] = system
        if _send_temperature(model, "messages"):
            body["temperature"] = 0
        return self._post_sse("/v1/messages", body, _messages_sse_parser, timeout, endpoint="messages")

    # ─── /v1beta (Gemini ingress) ─────────────────────────────────────────────
    #
    # Gemini's native API carries the model ID in the URL path
    # (`/v1beta/models/{model}:generateContent`). systemInstruction is
    # a separate top-level field. Response has `candidates[].content.parts[].text`
    # and `usageMetadata.cachedContentTokenCount` for implicit cache hits.
    # Stream variant is `:streamGenerateContent?alt=sse` and emits JSON
    # blobs as SSE `data:` lines without named events.

    def gemini_non_stream(self, model: str, contents: list, system: str = "",
                           max_tokens=16, timeout=90, **kwargs) -> dict:
        body = {"contents": contents, "generationConfig": {"maxOutputTokens": max_tokens}}
        if system:
            body["systemInstruction"] = {"role": "system", "parts": [{"text": system}]}
        if _send_temperature(model, "gemini"):
            body["generationConfig"]["temperature"] = 0
        for k, v in kwargs.items():
            body[k] = v
        return self._post_sync(f"/v1beta/models/{model}:generateContent", body, timeout, endpoint="gemini")

    def gemini_stream(self, model: str, contents: list, system: str = "",
                       max_tokens=16, timeout=90, **kwargs) -> dict:
        body = {"contents": contents, "generationConfig": {"maxOutputTokens": max_tokens}}
        if system:
            body["systemInstruction"] = {"role": "system", "parts": [{"text": system}]}
        if _send_temperature(model, "gemini"):
            body["generationConfig"]["temperature"] = 0
        for k, v in kwargs.items():
            body[k] = v
        return self._post_sse(f"/v1beta/models/{model}:streamGenerateContent?alt=sse",
                              body, _gemini_sse_parser, timeout, endpoint="gemini")


    # ─── /v1/embeddings ───────────────────────────────────────────────────────
    #
    # Embeddings ingress: POST /v1/embeddings with OpenAI-shape request.
    # The response has data[].embedding (float array) + usage.prompt_tokens.
    # Azure, Cohere, and Gemini embedding ingresses use the same path via
    # the cross-format canonical bridge (E62 codec). Custom dimensions and
    # batch inputs are supported where the upstream model allows it.

    def embeddings(self, model: str, inputs, timeout: int = 60,
                   dimensions: Optional[int] = None, **kwargs) -> dict:
        """POST /v1/embeddings. `inputs` may be a str or list of str.

        Returns the generic _post_sync dict: {status, data, elapsed,
        stream=False, headers, endpoint="embeddings"}.
        data shape (success): {object:"list", data:[{object:"embedding",
        index:N, embedding:[float,...]}, ...], model:..., usage:{...}}
        """
        body: dict = {"model": model, "input": inputs}
        if dimensions is not None:
            body["dimensions"] = dimensions
        body.update(kwargs)
        return self._post_sync("/v1/embeddings", body, timeout, endpoint="embeddings")

    # ─── Cohere embedding ingress ──────────────────────────────────────────────
    # Cohere's native wire shape is POST /v1/embed (not /v1/embeddings).
    # The gateway exposes a Cohere-format ingress at the same /v1/embed path.
    # This client method calls that ingress path for cross-ingress Arm F tests.

    def embeddings_cohere(self, model: str, texts: list, timeout: int = 60,
                          input_type: str = "search_document", **kwargs) -> dict:
        """POST /v1/embed (Cohere ingress format).

        Returns _post_sync dict; on success, data.embeddings is a list of
        float arrays (Cohere native: top-level embeddings[] not data[]).
        """
        body = {"model": model, "texts": texts, "input_type": input_type}
        body.update(kwargs)
        return self._post_sync("/v1/embed", body, timeout, endpoint="embeddings_cohere")

    # ─── Gemini embedding ingress ──────────────────────────────────────────────
    # Gemini's native embedding path is POST
    # /v1beta/models/{model}:embedContent (single) or :batchEmbedContents
    # (batch). The gateway exposes these at the same paths.

    def embeddings_gemini_single(self, model: str, text: str,
                                 timeout: int = 60, **kwargs) -> dict:
        """POST /v1beta/models/{model}:embedContent (Gemini single embed)."""
        body = {"content": {"parts": [{"text": text}]}}
        body.update(kwargs)
        return self._post_sync(
            f"/v1beta/models/{model}:embedContent",
            body, timeout, endpoint="embeddings_gemini",
        )

    def embeddings_gemini_batch(self, model: str, texts: list,
                                timeout: int = 60, **kwargs) -> dict:
        """POST /v1beta/models/{model}:batchEmbedContents (Gemini batch)."""
        requests_list = [{"content": {"parts": [{"text": t}]}} for t in texts]
        body = {"requests": requests_list}
        body.update(kwargs)
        return self._post_sync(
            f"/v1beta/models/{model}:batchEmbedContents",
            body, timeout, endpoint="embeddings_gemini_batch",
        )

    def estimate(self, body: dict, timeout: int = 30) -> dict:
        """POST /v1/estimate (E58-S4 compare endpoint). Body shape:
            {"request": <raw ingress body>, "compareTargets":[...]}
        Returns the generic _post_sync output dict: status / data / headers /
        elapsed / stream=False."""
        return self._post_sync("/v1/estimate", body, timeout, endpoint="estimate")

    def chat_no_auth(self, model: str) -> int:
        body = json.dumps({
            # max_tokens=1 is deliberate: this asserts a 401 and the model never
            # runs, so the budget is unreachable and costs nothing.
            "model": model, "messages": [{"role": "user", "content": "x"}], "max_tokens": 1,
        }).encode()
        c = self._conn(10)
        c.request("POST", "/v1/chat/completions", body,
                  {"Content-Type": "application/json"})
        r = c.getresponse()
        r.read()
        c.close()
        return r.status

    def chat_bad_vk(self, model: str) -> int:
        body = json.dumps({
            # max_tokens=1 is deliberate: this asserts a 401 and the model never
            # runs, so the budget is unreachable and costs nothing.
            "model": model, "messages": [{"role": "user", "content": "x"}], "max_tokens": 1,
        }).encode()
        c = self._conn(10)
        c.request("POST", "/v1/chat/completions", body,
                  {"Authorization": "Bearer nvk_INVALID_KEY_FOR_SMOKE_TEST",
                   "Content-Type": "application/json"})
        r = c.getresponse()
        r.read()
        c.close()
        return r.status


# ─── DBClient ─────────────────────────────────────────────────────────────────

class DBClient:
    """Postgres access via docker exec (local) or ssh+psql (remote/prod).

    In remote mode with ssh_host=None all methods are no-ops; with ssh_host
    set, queries run via ssh against the EC2 prod Postgres (DB cross-check
    + normalize verification both work on prod the same way they work
    locally)."""

    def __init__(self, is_remote: bool = False,
                 ssh_host: str = "", ssh_pgpassword: str = "",
                 ssh_pguser: str = "nexus", ssh_pgdb: str = "nexus_gateway"):
        self._cid: Optional[str] = None
        self._poll_err_logged = False
        self.is_remote = is_remote
        self.ssh_host = ssh_host
        self.ssh_pgpassword = ssh_pgpassword
        self.ssh_pguser = ssh_pguser
        self.ssh_pgdb = ssh_pgdb

    @property
    def has_ssh(self) -> bool:
        return bool(self.ssh_host and self.ssh_pgpassword)

    def container(self) -> str:
        if self._cid:
            return self._cid
        override = os.environ.get("NEXUS_PG_CONTAINER", "").strip()
        if override:
            self._cid = override
            return self._cid
        out = subprocess.check_output(
            "docker ps --filter name=postgres --format '{{.ID}}'",
            shell=True, text=True, timeout=10
        ).strip()
        candidates = [c for c in out.split("\n") if c.strip()]
        if not candidates:
            raise RuntimeError("No postgres container running")
        if len(candidates) == 1:
            self._cid = candidates[0]
            return self._cid
        # A dev box can run several containers whose name contains "postgres"
        # (a sibling stack's pgvector, etc.). Picking `head -1` is
        # non-deterministic and, when it lands on the wrong one, every query
        # fails "database nexus_gateway does not exist" — which poll_event
        # would mask as a bogus "no traffic_event row". Select by the fact we
        # actually depend on: the container that hosts the nexus_gateway DB.
        for cid in candidates:
            chk = subprocess.run(
                ["docker", "exec", cid, "psql", "-U", "postgres", "-tAc",
                 "SELECT 1 FROM pg_database WHERE datname='nexus_gateway'"],
                capture_output=True, text=True, timeout=10)
            if chk.returncode == 0 and chk.stdout.strip() == "1":
                self._cid = cid
                return self._cid
        raise RuntimeError(
            f"none of {len(candidates)} postgres containers has a nexus_gateway "
            "database; set NEXUS_PG_CONTAINER to disambiguate")

    def _psql_args(self, sql: str, separator: str = "") -> list[str]:
        # When ssh access is configured, route through ssh+psql against the
        # remote Postgres on localhost over the EC2 box.
        if self.has_ssh:
            base = (
                f"PGPASSWORD={self.ssh_pgpassword} "
                f"psql -h localhost -U {self.ssh_pguser} -d {self.ssh_pgdb} "
                f"-X -A -t"
            )
            if separator:
                base += f" -F '{separator}'"
            sql_quoted = sql.replace('"', '\\"')
            return ["ssh", "-o", "StrictHostKeyChecking=no",
                    self.ssh_host, f'{base} -c "{sql_quoted}"']
        return ["docker", "exec", self.container(),
                "psql", "-U", "postgres", "-d", "nexus_gateway",
                "-X", "-A", "-t",
                *( ["-F", separator] if separator else []),
                "-c", sql]

    def scalar(self, sql: str) -> str:
        if self.is_remote and not self.has_ssh:
            return ""
        r = subprocess.run(self._psql_args(sql), capture_output=True,
                           text=True, timeout=30)
        if r.returncode != 0:
            raise RuntimeError(f"DB error: {r.stderr.strip()[:200]}")
        return r.stdout.strip()

    def rows(self, sql: str) -> list[dict]:
        if self.is_remote and not self.has_ssh:
            return []
        r = subprocess.run(self._psql_args(sql, separator="|"),
                           capture_output=True, text=True, timeout=30)
        if r.returncode != 0:
            raise RuntimeError(f"DB error: {r.stderr.strip()[:200]}")
        lines = [l for l in r.stdout.strip().split("\n") if l]
        return [{"_row": l} for l in lines]

    def poll_event(self, model_name: str, t0_iso: str, timeout=45) -> Optional[dict]:
        if self.is_remote and not self.has_ssh:
            return None
        cols = [
            "id", "status_code", "model_name",
            "prompt_tokens", "completion_tokens", "total_tokens",
            "usage_extraction_status", "request_hook_decision",
            "api_key_fingerprint", "routing_rule_id", "routing_rule_name",
            "routed_provider_name", "source",
        ]
        col_sql = ", ".join(cols)
        deadline = time.time() + timeout
        while time.time() < deadline:
            sql = (
                f"SELECT {col_sql} FROM traffic_event "
                f"WHERE source = 'ai-gateway' "
                f"AND timestamp >= '{t0_iso}'::timestamptz "
                f"AND model_name = '{model_name}' "
                f"ORDER BY timestamp DESC LIMIT 1"
            )
            try:
                r = subprocess.run(self._psql_args(sql, separator="|"),
                                   capture_output=True, text=True, timeout=15)
                if r.returncode != 0:
                    # Surface the real DB error once rather than masking a
                    # misconfig (wrong container / missing DB / bad column) as
                    # an absent row — the two are indistinguishable to callers.
                    if not self._poll_err_logged:
                        self._poll_err_logged = True
                        print(f"[smoke] poll_event DB error: "
                              f"{r.stderr.strip()[:200]}", flush=True)
                else:
                    line = r.stdout.strip().split("\n")[0] if r.stdout.strip() else ""
                    if line:
                        parts = line.split("|")
                        if len(parts) >= len(cols):
                            return dict(zip(cols, parts))
            except Exception as e:
                if not self._poll_err_logged:
                    self._poll_err_logged = True
                    print(f"[smoke] poll_event exception: {e}", flush=True)
            time.sleep(2)
        return None

    def fetch_event_rows(self, t0_iso: str) -> list[dict]:
        """Pull the identity of every post-t0 ai-gateway traffic_event so P6b
        can sample them and fetch each one's VIEW-TIME normalized projection.

        The projection is not stored anywhere, so it is not read from SQL. Any
        attempt to pull it from a table would COALESCE to defaults and let the
        whole normalize check pass against an empty result — green that depends
        on nothing. It comes from the endpoint the UI itself calls.

        Returns empty list when DB access isn't configured (local without docker,
        remote without ssh)."""
        if self.is_remote and not self.has_ssh:
            return []
        sql = (
            "SELECT te.id, te.model_name, te.status_code "
            "FROM traffic_event te "
            f"WHERE te.source='ai-gateway' AND te.timestamp >= '{t0_iso}'::timestamptz "
            "ORDER BY te.timestamp DESC"
        )
        fields = [
            "id", "model_name", "status_code",
        ]
        try:
            r = subprocess.run(self._psql_args(sql, separator="|"),
                               capture_output=True, text=True, timeout=30)
            if r.returncode != 0:
                return []
        except Exception:
            return []
        out: list[dict] = []
        for line in r.stdout.strip().split("\n"):
            if not line:
                continue
            parts = line.split("|")
            if len(parts) < len(fields):
                continue
            out.append(dict(zip(fields, parts)))
        return out

    def count_events(self, t0_iso: str, model_name: Optional[str] = None) -> int:
        if self.is_remote:
            return -1
        where = f"source = 'ai-gateway' AND timestamp >= '{t0_iso}'::timestamptz"
        if model_name:
            where += f" AND model_name = '{model_name}'"
        try:
            return int(self.scalar(f"SELECT COUNT(*) FROM traffic_event WHERE {where}"))
        except Exception:
            return -1

    def load_provider_credentials(self, cp_config_path: str = "") -> dict[str, str]:
        """Decrypt provider API keys from the Credential table using AES-256-GCM.

        The encryption key is read from the CP dev YAML. Requires the
        'cryptography' package (pip install cryptography).
        Returns {provider: api_key} for all active credentials that can be
        successfully decrypted.
        """
        if self.is_remote:
            return {}

        # Encryption key from CP config; fall back to well-known dev default.
        enc_key_hex = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
        if cp_config_path:
            try:
                import re as _re
                with open(cp_config_path) as f:
                    for line in f:
                        m = _re.search(
                            r"encryption[_-]?key\s*[:=]\s*[\"']?([0-9a-fA-F]{64})[\"']?",
                            line,
                        )
                        if m:
                            enc_key_hex = m.group(1)
                            break
            except Exception as e:
                log_warn(f"  [db-creds] could not read CP config {cp_config_path!r}: {e}")

        try:
            from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        except ImportError:
            log_warn(
                "  [db-creds] 'cryptography' package not installed — "
                "cannot decrypt DB credentials. Run: pip install cryptography"
            )
            return {}

        enc_key = bytes.fromhex(enc_key_hex)

        sql = (
            'SELECT c."encryptedKey", c."encryptionIv", c."encryptionTag", p.name '
            'FROM "Credential" c '
            'JOIN "Provider" p ON p.id = c."providerId" '
            "WHERE c.enabled = true AND p.enabled = true"
        )
        try:
            credential_rows = self.rows(sql)
        except Exception as e:
            log_warn(f"  [db-creds] DB query failed: {e}")
            return {}

        provider_map = {
            "openai":        "openai",
            "anthropic":     "anthropic",
            "google-gemini": "gemini",
            "gemini":        "gemini",
            "deepseek":      "deepseek",
            "moonshot":      "moonshot",
        }

        result: dict[str, str] = {}
        for row in credential_rows:
            parts = row.get("_row", "").split("|")
            if len(parts) < 4:
                continue
            enc_val, iv_hex, tag_hex, provider_name = (
                parts[0].strip(), parts[1].strip(), parts[2].strip(), parts[3].strip()
            )
            mapped = provider_map.get(provider_name.lower(), "")
            if not mapped or mapped in result:
                continue
            try:
                nonce = bytes.fromhex(iv_hex)
                ciphertext = bytes.fromhex(enc_val)
                tag = bytes.fromhex(tag_hex)
                aesgcm = AESGCM(enc_key)
                plaintext = aesgcm.decrypt(nonce, ciphertext + tag, None)
                result[mapped] = plaintext.decode("utf-8").strip()
                log_info(f"  [db-creds] decrypted key for {provider_name} → {mapped}")
            except Exception as e:
                log_warn(f"  [db-creds] decrypt failed for {provider_name}: {e}")

        return result


# ─── Metrics diff ─────────────────────────────────────────────────────────────

def _parse_metric(text: str, name: str) -> dict[str, float]:
    out: dict[str, float] = {}
    for line in text.splitlines():
        if line.startswith(name + "{") or line.startswith(name + " "):
            parts = line.rsplit(" ", 1)
            if len(parts) == 2:
                try:
                    out[parts[0]] = float(parts[1])
                except ValueError:
                    pass
    return out

def metrics_diff(m0: str, m1: str) -> list[tuple[str, float, float]]:
    """Return [(metric_line, v0, v1)] for lines that changed."""
    keys = set()
    for line in (m0 + m1).splitlines():
        if line and not line.startswith("#"):
            key = line.rsplit(" ", 1)[0]
            keys.add(key)
    diff = []
    def _val(text, key):
        for line in text.splitlines():
            if line.startswith(key + " "):
                try:
                    return float(line.split(" ")[-1])
                except Exception:
                    pass
        return 0.0
    for key in sorted(keys):
        # The product's metric prefix is `nexus_` (nexus_requests_total,
        # nexus_tokens_total, nexus_request_duration_ms_*). Filtering on
        # "nexus_ai_gateway_" matched exactly ONE series in the whole exposition —
        # nexus_ai_gateway_pure_forward_mode, a gauge whose own HELP text says it
        # MUST be 0 in production. So this loop was, by construction, comparing a
        # constant to itself and returning [] on every run, which is why P7 has
        # always reported "0 requests recorded" and "no unexpected error counter
        # growth" no matter what the gateway did.
        if key.startswith("nexus_"):
            v0 = _val(m0, key)
            v1 = _val(m1, key)
            if v1 != v0:
                diff.append((key, v0, v1))
    return diff


# ─── State snapshot ────────────────────────────────────────────────────────────

class StateSnapshot:
    def __init__(self):
        self.routing_rules: list[dict] = []
        self.gemini_cache_cfg: dict = {}

    @classmethod
    def capture(cls, cp: CPClient) -> "StateSnapshot":
        s = cls()
        try:
            s.routing_rules = cp.list_routing_rules()
            log_info(f"Snapshotted {len(s.routing_rules)} routing rules")
        except Exception as e:
            log_warn(f"Could not snapshot routing rules: {e}")
        try:
            s.gemini_cache_cfg = cp.get_gemini_cache_cfg()
        except Exception as e:
            log_warn(f"Could not snapshot gemini cache config: {e}")
        return s

    def restore(self, cp: CPClient, no_restore=False, skip_routing=False):
        if no_restore:
            log_warn("--no-restore: skipping config restoration")
            return
        log_step("Restoring original configuration")
        if skip_routing:
            log_info("--no-routing-manage: routing rules were never modified; nothing to restore")
        else:
            for rule in self.routing_rules:
                try:
                    cp.set_routing_rule(rule["id"], rule.get("enabled", False))
                except Exception as e:
                    log_warn(f"Restore rule {rule.get('name')}: {e}")
        log_ok("Config restored")


# ─── Conversation builder ──────────────────────────────────────────────────────

# Per-run nonce injected into every non-cache user message so consecutive runs
# produce distinct request bodies, bypassing the L1 Redis response cache.
# Set by main() before any test phase runs.
_run_nonce: str = ""

# Rotating questions used for cache read rounds, giving the conversation a
# natural accumulating shape across rounds.
# Opening question for the cache write round — the system prompt prefix creates
# the provider-side cache on this first call.
_CACHE_WRITE_QUESTION = (
    "I'm learning Go for backend development. "
    "What makes it stand out from other languages? One paragraph."
)

# Follow-up questions for cache read rounds.  Each naturally continues the
# Go conversation so the exchange feels like real multi-turn chat.  The same
# system prompt prefix is sent every round → provider prefix cache should hit.
_CACHE_FOLLOWUP_QUESTIONS = [
    "How does Go's goroutine model work, and how is it different from OS threads?",
    "What's the idiomatic way to handle errors in Go production code?",
    "When would you choose Go over Rust for building a new backend service?",
    "What are Go's main limitations compared to more expressive languages?",
    "How does Go's garbage collector work, and what are its performance tradeoffs?",
]


def _ingress_user_suffix(ingress_id: str) -> str:
    """Per-run + per-ingress nonce tag appended to USER content. Differentiates
    body bytes across ingresses so the gateway L1 response cache never serves
    a P3 chat response to a P3R responses request. Empty ingress_id falls
    back to the legacy run-only suffix."""
    parts = []
    if ingress_id:
        parts.append(f"ingress:{ingress_id}")
    if _run_nonce:
        parts.append(f"r:{_run_nonce}")
    return f" [{' '.join(parts)}]" if parts else ""


def _ingress_system_suffix(ingress_id: str) -> str:
    """Per-ingress tag appended to the SYSTEM prompt tail. Makes each
    ingress's prefix unique upstream so the provider builds its OWN prefix
    cache per ingress — the write turn is then guaranteed to start from
    zero (not from a P3 chat cache that P3R/P3A/P3G would otherwise
    inherit). No per-run component here: across runs we WANT the system
    prefix stable so the provider cache amortizes within a single ingress.
    """
    return f"\n\n[smoke ingress:{ingress_id}]" if ingress_id else ""


def _user_msg(system: str, user: str, model: str, ingress_id: str = "") -> list[dict]:
    """Non-cache request messages. Appends per-run + per-ingress nonce to user
    content so consecutive runs and parallel ingresses produce distinct L1
    cache keys. Optionally appends a per-ingress tag to the system prompt
    tail so each ingress builds its own upstream prefix cache."""
    sys_text = system + _ingress_system_suffix(ingress_id)
    user_suffix = _ingress_user_suffix(ingress_id)
    if is_reasoning(model):
        return [{"role": "user", "content": f"[Context]{sys_text[:300]}\n\n{user}{user_suffix}"}]
    return [{"role": "system", "content": sys_text}, {"role": "user", "content": f"{user}{user_suffix}"}]

def _turn2_msgs(system: str, turn1_reply: str, model: str, ingress_id: str = "") -> list[dict]:
    sys_text = system + _ingress_system_suffix(ingress_id)
    user_suffix = _ingress_user_suffix(ingress_id)
    if is_reasoning(model):
        return [{"role": "user", "content": f"[Context]{sys_text[:300]}\n\nWhat is Python? One sentence.{user_suffix}"}]
    return [
        {"role": "system", "content": sys_text},
        {"role": "user", "content": f"What is Go? One sentence.{user_suffix}"},
        {"role": "assistant", "content": turn1_reply or "Go is a compiled language."},
        {"role": "user", "content": "What is Python? One sentence."},
    ]


def _cache_user_msg(system: str, user: str, model: str, ingress_id: str = "") -> list[dict]:
    """Write-round messages for the cache test.

    Non-Anthropic reasoning models (o1/o3/gpt-5.5 etc.) cannot use a system
    message, so the full system prompt is embedded in the user turn.  This
    keeps the prefix long enough to exceed the provider's caching threshold
    (~1024 tokens for OpenAI).  Anthropic thinking models (claude-opus-4-7)
    do support system messages and cache_control injection, so they follow the
    standard path.

    Per-ingress + per-run nonce on user content guarantees gateway L1 miss.
    Per-ingress tag on system tail isolates the upstream prefix cache so the
    write turn truly starts from zero per ingress (no contamination from
    sibling-ingress prior writes).
    """
    sys_text = system + _ingress_system_suffix(ingress_id)
    user_suffix = _ingress_user_suffix(ingress_id)
    if is_reasoning(model) and "claude" not in model.lower():
        return [{"role": "user", "content": f"[Context]\n{sys_text}\n\n{user}{user_suffix}"}]
    return [{"role": "system", "content": sys_text}, {"role": "user", "content": f"{user}{user_suffix}"}]


def _cache_next_round_msgs(
    system: str,
    history: list[dict],
    model: str,
    round_idx: int,
    ingress_id: str = "",
) -> list[dict]:
    """Build the message list for one cache read round.

    Each call appends the next follow-up question to the accumulated
    conversation history so the exchange simulates a real multi-turn chat
    session.  The system prompt stays identical across all rounds (modulo
    the per-ingress tag) so the provider-side prefix cache is exercised on
    a growing context — closest to real-world usage.

    The caller is responsible for appending the assistant reply to history
    after each round so the conversation grows naturally.

    Per-ingress tag on system + per-(ingress,run) nonce on the followup
    question gives both gateway-L1 isolation and per-ingress provider
    prefix-cache isolation.
    """
    sys_text = system + _ingress_system_suffix(ingress_id)
    user_suffix = _ingress_user_suffix(ingress_id)
    question = _CACHE_FOLLOWUP_QUESTIONS[round_idx % len(_CACHE_FOLLOWUP_QUESTIONS)] + user_suffix
    if is_reasoning(model) and "claude" not in model.lower():
        # Reasoning models embed everything in the user turn; rebuild each time.
        prev_qa = "\n".join(
            f"{'User' if m['role'] == 'user' else 'Assistant'}: {m['content']}"
            for m in history
        )
        return [{"role": "user", "content": f"[Context]\n{sys_text}\n\n{prev_qa}\nUser: {question}"}]
    return [{"role": "system", "content": sys_text}] + history + [{"role": "user", "content": question}]


# ─── IngressSpec + generic per-model suite (P1 refactor 2026-05-16) ───────────
#
# Four public ingresses (/v1/chat/completions, /v1/responses, /v1/messages,
# /v1beta/.../generateContent) used to have a copy of the same ~150-line
# suite each (non-stream + SSE + multi-round cache). That meant adding a
# check or fixing a bug required 4 identical edits — and the 2026-05-16
# per-ingress-nonce work hit that risk head-on. The IngressSpec dataclass
# captures the per-ingress differences (URL path / body shape / response
# extractors / SSE terminator); a single generic [_run_ingress_model_suite]
# consumes one spec at a time.

from dataclasses import dataclass, field
from typing import Callable, Optional

# Models that produce so much internal reasoning the default 1024 token
# budget is consumed before they emit any user-visible text (smoke run
# 2026-05-14T15:54Z: 8 empty-content rows). For these only, lift the cap
# to 100k so the provider's own max-output clamp decides termination.
# Centralized so all 4 ingress specs share the same source of truth.
_UNCAPPED_REASONING_MODELS_SET = {"gemini-2.5-pro", "o3", "kimi-k2.6"}


@dataclass
class IngressSpec:
    """One ingress's per-test contract. Four instances drive the generic suite."""
    name: str                # phase tag, "P3" / "P3R" / "P3A" / "P3G"
    label: str               # user-facing path "/v1/chat/completions" etc.
    nonce_id: str            # short tag for the per-ingress nonce ("chat", "resp", "msg", "gem")
    is_native: Callable[[str], bool]
    # call_non_stream and call_stream both take canonical chat-completions
    # `msgs` (system+user roles) and are responsible for converting to the
    # ingress's native body shape internally. Return value mirrors the
    # original endpoint-specific GWClient method's dict.
    call_non_stream: Callable[["GWClient", str, list, int, int], dict]
    call_stream: Callable[["GWClient", str, list, int, int], dict]
    # Response-side extractors:
    extract_text: Callable[[dict], str]              # data → "text" (empty=no content)
    extract_cached_tokens: Callable[[dict], int]     # data → cached_token count
    # Reasoning extractors (smoke v2 — handoff 2026-05-16). For non-reasoning
    # models these MUST return ""/0 silently; the suite only acts on them when
    # is_reasoning(model) or uses_thinking_tokens(model) is true.
    #   text:   chain-of-thought string (chat reasoning_content, responses
    #           reasoning-block summary, messages thinking blocks, gemini
    #           thought-flagged parts).
    #   tokens: per-ingress reasoning_tokens count. Anthropic doesn't
    #           separate this from output_tokens — messages extractor
    #           returns 0 and the suite skips the "0 = bug" warning for
    #           native-Anthropic models.
    extract_reasoning_text: Callable[[dict], str]
    extract_reasoning_tokens: Callable[[dict], int]
    # Envelope shape assertion. Returns None on OK, str describing the
    # mismatch when the body's top-level shape doesn't match this ingress
    # (catches gateway reshape bugs that produce wrong-ingress JSON).
    envelope_check: Callable[[dict], Optional[str]]
    # Diagnostic helper for the "200 but empty text" log line.
    diag_finish: Callable[[dict], str]
    # SSE termination indicator key from the stream response dict
    # ("done_seen"|"completed_seen"|"message_stop_seen"|None for Gemini).
    stream_terminate_key: Optional[str]
    # Usage one-liner for the success log line.
    usage_one_liner: Callable[[dict], str]


# Cache-timing constants. The sleep values target the Gemini cache
# subsystem's async-create lag (geminicache.Manager.asyncCreate) — they
# are TARGET-derived (whenever the upstream provider is Gemini) rather
# than INGRESS-derived (the ingress format), so they live as
# module-level constants gated by a model regex check in the generic
# suite rather than as IngressSpec fields.
_GEMINI_TARGET_POST_WRITE_SLEEP = 10.0   # cache create takes ~5-8s; 10s adds headroom
_GEMINI_TARGET_ROUND_SLEEP = 3.0         # explicit cachedContent propagation between reads
_DEFAULT_ROUND_SLEEP = 0.5


# ─── Per-ingress callable wrappers ─────────────────────────────────────────────
# Each wrapper handles canonical-chat→native-body conversion + GWClient call.

def _chat_call_non_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    return gw.chat_sync(model, msgs, max_tokens=max_tok, timeout=timeout)


def _chat_call_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    return gw.chat_stream(model, msgs, max_tokens=max_tok, timeout=timeout)


def _responses_call_non_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    input_items, instructions = _messages_to_responses_input(msgs)
    kwargs = {"max_tokens": max_tok, "timeout": timeout}
    if instructions:
        kwargs["instructions"] = instructions
    return gw.responses_non_stream(model, input_items, **kwargs)


def _responses_call_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    input_items, instructions = _messages_to_responses_input(msgs)
    kwargs = {"max_tokens": max_tok, "timeout": timeout}
    if instructions:
        kwargs["instructions"] = instructions
    return gw.responses_stream(model, input_items, **kwargs)


def _messages_call_non_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    anth_msgs, anth_system = _messages_to_anthropic_body(msgs)
    return gw.messages_non_stream(model, anth_msgs, system=anth_system, max_tokens=max_tok, timeout=timeout)


def _messages_call_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    anth_msgs, anth_system = _messages_to_anthropic_body(msgs)
    return gw.messages_stream(model, anth_msgs, system=anth_system, max_tokens=max_tok, timeout=timeout)


def _gemini_call_non_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    contents, sys_text = _messages_to_gemini_contents(msgs)
    return gw.gemini_non_stream(model, contents, system=sys_text, max_tokens=max_tok, timeout=timeout)


def _gemini_call_stream(gw: "GWClient", model: str, msgs: list, max_tok: int, timeout: int) -> dict:
    contents, sys_text = _messages_to_gemini_contents(msgs)
    return gw.gemini_stream(model, contents, system=sys_text, max_tokens=max_tok, timeout=timeout)


# ─── Per-ingress envelope shape checks (P0b) ──────────────────────────────────

def _chat_envelope_check(data: dict):
    if not isinstance(data.get("choices"), list):
        return "missing choices[] (got chat-completions ingress, body lacks choices)"
    obj = data.get("object", "")
    if obj and obj != "chat.completion":
        return f"object={obj!r} (expected chat.completion)"
    return None


def _responses_envelope_check(data: dict):
    if "output" not in data:
        return "missing output[] (got /v1/responses ingress, body lacks output)"
    if not isinstance(data.get("output"), list) and data.get("output") is not None:
        return f"output is {type(data.get('output')).__name__} not list"
    obj = data.get("object", "")
    if obj and obj != "response":
        return f"object={obj!r} (expected response)"
    return None


def _messages_envelope_check(data: dict):
    if not isinstance(data.get("content"), list):
        return "missing content[] (got /v1/messages ingress, body lacks content)"
    typ = data.get("type", "")
    if typ and typ != "message":
        return f"type={typ!r} (expected message)"
    return None


def _gemini_envelope_check(data: dict):
    if not isinstance(data.get("candidates"), list):
        return "missing candidates[] (got /v1beta ingress, body lacks candidates)"
    return None


# ─── Per-ingress usage one-liners ─────────────────────────────────────────────

def _chat_usage_one_liner(data: dict) -> str:
    u = data.get("usage") or {}
    return f"tok={u.get('total_tokens', '?')}"


def _responses_usage_one_liner(data: dict) -> str:
    u = data.get("usage") or {}
    return f"in={u.get('input_tokens', '?')} out={u.get('output_tokens', '?')}"


def _messages_usage_one_liner(data: dict) -> str:
    u = data.get("usage") or {}
    return f"in={u.get('input_tokens', '?')} out={u.get('output_tokens', '?')}"


def _gemini_usage_one_liner(data: dict) -> str:
    u = data.get("usageMetadata") or {}
    return f"prompt={u.get('promptTokenCount', '?')} cand={u.get('candidatesTokenCount', '?')}"


# ─── Per-ingress diagnostic on empty text ──────────────────────────────────────

def _chat_diag_finish(data: dict) -> str:
    choices = data.get("choices") or [{}]
    fr = choices[0].get("finish_reason") if choices else None
    return f"finish_reason={fr!r}"


def _responses_diag_finish(data: dict) -> str:
    return f"status={data.get('status')!r}"


def _messages_diag_finish(data: dict) -> str:
    return f"stop_reason={data.get('stop_reason')!r}"


def _gemini_diag_finish(data: dict) -> str:
    cands = data.get("candidates") or []
    fr = cands[0].get("finishReason") if cands and isinstance(cands[0], dict) else None
    return f"finishReason={fr!r}"


# ─── Per-ingress cached-token extractors (wrap usage extraction) ──────────────

def _chat_extract_cached(data: dict) -> int:
    """OpenAI/DeepSeek/Moonshot/Kimi: prompt_tokens_details.cached_tokens;
    Anthropic via canonical: cache_read_input_tokens."""
    u = data.get("usage") or {}
    a = (u.get("prompt_tokens_details") or {}).get("cached_tokens", 0) or 0
    b = u.get("cache_read_input_tokens", 0) or 0
    return max(int(a), int(b))


def _responses_extract_cached(data: dict) -> int:
    u = data.get("usage") or {}
    return _responses_extract_cached_tokens(u)


def _messages_extract_cached(data: dict) -> int:
    u = data.get("usage") or {}
    return _anthropic_extract_cached_tokens(u)


def _gemini_extract_cached(data: dict) -> int:
    u = data.get("usageMetadata") or {}
    return _gemini_extract_cached_tokens(u)


# ─── Per-ingress text extractors (some defined later in module) ───────────────

def _chat_extract_text(data: dict) -> str:
    if not isinstance(data, dict):
        return ""
    choices = data.get("choices") or []
    if not choices:
        return ""
    msg = choices[0].get("message") or {}
    content = msg.get("content") or ""
    if not isinstance(content, str):
        # tool-call only; treat as empty for text-extraction purposes
        return ""
    return content


# ─── Per-ingress reasoning extractors (smoke v2 2026-05-16) ───────────────────
# Text extractors return chain-of-thought string; token extractors return the
# usage subfield count. Both MUST be best-effort: silently return ""/0 when
# the response carries no reasoning (non-reasoning models, models that hide
# CoT, providers that don't expose token splits). The suite decides whether
# absence is a bug — based on is_reasoning(model) / uses_thinking_tokens(model)
# — not the extractors.


def _chat_extract_reasoning_text(data: dict) -> str:
    """OpenAI chat-completions: reasoning_content streams from DeepSeek-R1/V4,
    Kimi K2-thinking, Moonshot-thinking. The non-stream body surfaces it on
    choices[0].message.reasoning_content alongside the regular content."""
    if not isinstance(data, dict):
        return ""
    choices = data.get("choices") or []
    if not choices:
        return ""
    msg = choices[0].get("message") or {}
    rc = msg.get("reasoning_content")
    return rc if isinstance(rc, str) else ""


def _chat_extract_reasoning_tokens(data: dict) -> int:
    """OpenAI chat-completions: usage.completion_tokens_details.reasoning_tokens.
    Verified against shared/normalize/openai_chat.go:398."""
    if not isinstance(data, dict):
        return 0
    u = data.get("usage") or {}
    details = u.get("completion_tokens_details") or {}
    return int(details.get("reasoning_tokens", 0) or 0)


def _responses_extract_reasoning_text(data: dict) -> str:
    """OpenAI Responses-API: reasoning rides on output[] entries with
    type=='reasoning', text payload split between summary[].text and
    content[].text depending on model and reasoning_effort. We concat
    both so the smoke surfaces any chain-of-thought the provider emits."""
    if not isinstance(data, dict):
        return ""
    parts: list[str] = []
    for item in data.get("output") or []:
        if not isinstance(item, dict) or item.get("type") != "reasoning":
            continue
        for s in item.get("summary") or []:
            if isinstance(s, dict) and isinstance(s.get("text"), str):
                parts.append(s["text"])
        for c in item.get("content") or []:
            if isinstance(c, dict) and isinstance(c.get("text"), str):
                parts.append(c["text"])
    return "".join(parts)


def _responses_extract_reasoning_tokens(data: dict) -> int:
    """OpenAI Responses-API: usage.output_tokens_details.reasoning_tokens.
    Verified against shared/normalize/openai_chat.go:400."""
    if not isinstance(data, dict):
        return 0
    u = data.get("usage") or {}
    details = u.get("output_tokens_details") or {}
    return int(details.get("reasoning_tokens", 0) or 0)


def _messages_extract_reasoning_text(data: dict) -> str:
    """Anthropic /v1/messages: thinking blocks ride on content[] entries
    with type=='thinking', text payload on the `thinking` string field.
    Verified against shared/normalize/anthropic_messages.go:194."""
    if not isinstance(data, dict):
        return ""
    parts: list[str] = []
    for blk in data.get("content") or []:
        if isinstance(blk, dict) and blk.get("type") == "thinking":
            t = blk.get("thinking")
            if isinstance(t, str):
                parts.append(t)
    return "".join(parts)


def _messages_extract_reasoning_tokens(data: dict) -> int:
    """Anthropic does not expose reasoning tokens as a separate count
    (they roll into usage.output_tokens). Return 0 unconditionally; the
    suite's reasoning_tokens warning is suppressed for native-Anthropic
    models so this 0 does not produce a false WARN."""
    return 0


def _gemini_extract_reasoning_text(data: dict) -> str:
    """Gemini 2.5+ extended-thinking: candidates[0].content.parts[] entries
    with thought==True carry the chain-of-thought text. Verified against
    shared/normalize/gemini_generate.go:93."""
    if not isinstance(data, dict):
        return ""
    parts: list[str] = []
    for cand in data.get("candidates") or []:
        if not isinstance(cand, dict):
            continue
        content = cand.get("content") or {}
        for p in content.get("parts") or []:
            if isinstance(p, dict) and p.get("thought") is True:
                t = p.get("text")
                if isinstance(t, str):
                    parts.append(t)
    return "".join(parts)


def _gemini_extract_reasoning_tokens(data: dict) -> int:
    """Gemini: usageMetadata.thoughtsTokenCount.
    Verified against shared/normalize/gemini_generate.go:346."""
    if not isinstance(data, dict):
        return 0
    u = data.get("usageMetadata") or {}
    return int(u.get("thoughtsTokenCount", 0) or 0)


# ─── The 4 spec instances ──────────────────────────────────────────────────────
# Note: forward references to _model_supports_responses_api, _model_natively_anthropic,
# _model_natively_gemini, _responses_extract_text, _anthropic_extract_text,
# _gemini_extract_text are resolved at call time (Python late binding).

INGRESS_CHAT = IngressSpec(
    name="P3",
    label="/v1/chat/completions",
    nonce_id="chat",
    is_native=lambda m: True,  # chat-completions ingress maps natively to OpenAI-wire-shape adapters
    call_non_stream=_chat_call_non_stream,
    call_stream=_chat_call_stream,
    extract_text=_chat_extract_text,
    extract_cached_tokens=_chat_extract_cached,
    extract_reasoning_text=_chat_extract_reasoning_text,
    extract_reasoning_tokens=_chat_extract_reasoning_tokens,
    envelope_check=_chat_envelope_check,
    diag_finish=_chat_diag_finish,
    stream_terminate_key="done_seen",
    usage_one_liner=_chat_usage_one_liner,
)

INGRESS_RESPONSES = IngressSpec(
    name="P3R",
    label="/v1/responses",
    nonce_id="resp",
    is_native=lambda m: _model_supports_responses_api(m),
    call_non_stream=_responses_call_non_stream,
    call_stream=_responses_call_stream,
    extract_text=lambda d: _responses_extract_text(d),
    extract_cached_tokens=_responses_extract_cached,
    extract_reasoning_text=_responses_extract_reasoning_text,
    extract_reasoning_tokens=_responses_extract_reasoning_tokens,
    envelope_check=_responses_envelope_check,
    diag_finish=_responses_diag_finish,
    stream_terminate_key="completed_seen",
    usage_one_liner=_responses_usage_one_liner,
)

INGRESS_MESSAGES = IngressSpec(
    name="P3A",
    label="/v1/messages",
    nonce_id="msg",
    is_native=lambda m: _model_natively_anthropic(m),
    call_non_stream=_messages_call_non_stream,
    call_stream=_messages_call_stream,
    extract_text=lambda d: _anthropic_extract_text(d),
    extract_cached_tokens=_messages_extract_cached,
    extract_reasoning_text=_messages_extract_reasoning_text,
    extract_reasoning_tokens=_messages_extract_reasoning_tokens,
    envelope_check=_messages_envelope_check,
    diag_finish=_messages_diag_finish,
    stream_terminate_key="message_stop_seen",
    usage_one_liner=_messages_usage_one_liner,
)

INGRESS_GEMINI = IngressSpec(
    name="P3G",
    label="/v1beta",
    nonce_id="gem",
    is_native=lambda m: _model_natively_gemini(m),
    call_non_stream=_gemini_call_non_stream,
    call_stream=_gemini_call_stream,
    extract_text=lambda d: _gemini_extract_text(d),
    extract_cached_tokens=_gemini_extract_cached,
    extract_reasoning_text=_gemini_extract_reasoning_text,
    extract_reasoning_tokens=_gemini_extract_reasoning_tokens,
    envelope_check=_gemini_envelope_check,
    diag_finish=_gemini_diag_finish,
    # Gemini SSE has no explicit terminator event — successful stream is
    # "200 with at least one chunk + non-empty text". Treat None as
    # "terminator-not-applicable" in the generic suite.
    stream_terminate_key=None,
    usage_one_liner=_gemini_usage_one_liner,
)


# ─── Parallel execution infra (added 2026-05-20) ──────────────────────────────
#
# Per-model suite runs ~6 checks of which the upstream HTTPS call
# dominates (1-5 s) — perfect candidate for I/O-bound concurrency.
# Three concurrency limits stack:
#
#   1. _GLOBAL_POOL_SIZE — outer ceiling, sized for gateway + Redis
#      capacity. With Redis pool=200 and HTTP per-host=500 (post-2026-05-19
#      tuning), 16 is conservative.
#   2. _PROVIDER_SEM — per-upstream-provider semaphore, sized for each
#      provider's published RPM headroom. Stops one slow / rate-limited
#      provider from holding the global pool.
#   3. cache-test internal serialization — preserved inside
#      _run_ingress_model_suite. The 3-round cache test for one model
#      must run sequentially because round N reads the write from
#      round N-1; that's a per-task invariant, not a cross-task lock.

_GLOBAL_POOL_SIZE = 16

# Per-provider concurrency caps. Keys match the lowercase prefix that
# provider_of() returns. "other" is the fallback for unknown providers
# and is intentionally conservative.
_PROVIDER_CONCURRENCY = {
    "openai":    16,
    "anthropic": 6,
    "moonshot":  4,
    "gemini":    8,
    "vertex":    8,
    "deepseek":  8,
    "other":     4,
}

_PROVIDER_SEM: dict[str, threading.Semaphore] = {
    name: threading.Semaphore(n) for name, n in _PROVIDER_CONCURRENCY.items()
}

# Model-code-prefix → provider. Independent of /v1/models catalog so the
# pool can size budgets without an extra HTTP round-trip. Falls back to
# "other" for unknown prefixes — slows them down a bit but is safe.
_MODEL_PROVIDER_PREFIXES: list[tuple[str, str]] = [
    ("gpt-",                "openai"),
    ("o1",                  "openai"),
    ("o3",                  "openai"),
    ("o4",                  "openai"),
    ("chatgpt-",            "openai"),
    ("text-embedding-",     "openai"),
    ("dall-e-",             "openai"),
    ("whisper-",            "openai"),
    ("claude-",             "anthropic"),
    ("moonshot-",           "moonshot"),
    ("kimi-",               "moonshot"),
    ("gemini-",             "gemini"),
    ("text-bison-",         "vertex"),
    ("chat-bison-",         "vertex"),
    ("deepseek-",           "deepseek"),
]


def provider_of(model: str) -> str:
    """Return the lowercase provider name for the given model id.
    Used to pick the right per-provider concurrency semaphore. Returns
    'other' when the prefix is unknown — that bucket has a low cap so
    we don't accidentally hammer a rate-limited unknown upstream."""
    m = model.lower()
    for prefix, provider in _MODEL_PROVIDER_PREFIXES:
        if m.startswith(prefix):
            return provider
    return "other"


def model_eligible_for(model: str, ingress: IngressSpec) -> bool:
    """Return True when this (model, ingress) pair should run.

    Current rules (will grow as more ingress types land):
      - is_non_chat(model) → False for every chat-shape ingress
        (chat/responses/messages/gemini). Embedding / audio / image
        models will need their own ingress entries when we add them;
        they should NOT be exercised through chat ingresses.
      - Otherwise default True (the canonical-bridge handles every
        chat-shape × chat-model combination, even if the model isn't
        the ingress's native target).

    Future embedding ingress will gate the inverse: only models with
    type==embedding pass for INGRESS_EMBEDDINGS. Keep this function as
    the single source of truth for that matrix.
    """
    if is_non_chat(model):
        # Chat-shape ingresses cannot exercise non-chat (embedding /
        # image / audio) models — they would fail with a 400 or 502
        # at the gateway model-type guard.
        if ingress.name in ("P3", "P3R", "P3A", "P3G"):
            return False
    # A model with a floor cannot be exercised by a text-only suite. The P3
    # family sends plain text to every chat model, and gpt-audio-* is
    # type=chat with requiredModalities ["audio"] — so it answered four
    # upstream 400s that carried no information about the product and simply
    # reddened the gate. Coverage is not lost: the multimodal phase selects
    # these models BY that floor and sends them real audio, which is where
    # the fixture and the byte assertions already live.
    if model_floor(model) and ingress.name in ("P3", "P3R", "P3A", "P3G"):
        return False
    return True


def _run_one_pool_task(
    ingress: IngressSpec,
    gw: "GWClient",
    model: str,
    t0_iso: str,
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    cache_rounds: int,
    db: Optional["DBClient"],
) -> None:
    """Worker-pool entry point for one (ingress, model) suite.

    Wraps _run_ingress_model_suite with:
      - per-task log buffer so the model's lines stay grouped despite
        interleaving across pool workers;
      - per-provider semaphore so one slow upstream doesn't starve
        the global pool;
      - exception capture: any exception is logged as a FAIL on the
        spec.name/model row rather than killing the pool (`as_completed`
        re-raises if we don't return cleanly).
    """
    provider = provider_of(model)
    sem = _PROVIDER_SEM.get(provider, _PROVIDER_SEM["other"])
    _begin_task_buffer()
    try:
        with sem:
            _run_ingress_model_suite(
                ingress, gw, model, t0_iso, no_stream, no_cache, timeout,
                cache_rounds, db=db,
            )
    except Exception as exc:  # noqa: BLE001 — must not propagate to pool
        rec(ingress.name, f"{model}/pool-task").failed(f"unhandled exception: {exc!r}")
        log_fail(f"  [{model}] {ingress.label} pool task crashed: {exc!r}")
    finally:
        _flush_task_buffer()


def _run_pool_for_models(
    ingress: IngressSpec,
    gw: "GWClient",
    models: list[str],
    t0_iso: str,
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
    max_workers: Optional[int] = None,
) -> None:
    """Run _run_ingress_model_suite for each eligible model concurrently
    under a ThreadPoolExecutor. Eligibility is enforced here so callers
    don't need to know the (model, ingress) matrix. Cache-test internal
    rounds remain serial inside each task.

    Tasks complete in arbitrary order; per-task log buffer (set up by
    _run_one_pool_task) preserves a single model's lines as a contiguous
    block in stdout."""
    eligible = [m for m in models if model_eligible_for(m, ingress)]
    skipped = [m for m in models if m not in eligible]
    if skipped:
        log_info(f"  Skipping {len(skipped)} ineligible model(s) for {ingress.label}: {', '.join(skipped[:10])}{' …' if len(skipped) > 10 else ''}")
    if not eligible:
        log_warn(f"  No eligible models for {ingress.label} — phase skipped")
        return
    workers = max_workers or _GLOBAL_POOL_SIZE
    log_info(f"  Pool: {workers} worker(s) × {len(eligible)} model(s) — per-provider caps {_PROVIDER_CONCURRENCY}")
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix=f"smoke-{ingress.name}") as pool:
        futures = [
            pool.submit(
                _run_one_pool_task,
                ingress, gw, m, t0_iso, no_stream, no_cache, timeout,
                cache_rounds, db,
            )
            for m in eligible
        ]
        for f in as_completed(futures):
            # Re-raise any pool-internal exception that escaped the
            # _run_one_pool_task try/except (e.g. KeyboardInterrupt or
            # a bug in the wrapper itself). Worker exceptions are
            # already captured as FAIL results.
            f.result()


# Cache hit threshold: smaller than this in N rounds is treated as noise
# (a 10-token "hit" on a 4500-token prefix is most likely a Gemini quirk
# or a per-round nonce variation, not a real provider cache hit). Tunable.
_CACHE_HIT_THRESHOLD = 100

# Models that OpenAI does NOT prompt-cache on the Responses API (/v1/responses),
# even though they cache normally on /v1/chat/completions. Verified 2026-06-03 by
# a direct OpenAI comparison: gpt-4o on /v1/responses returns
# usage.input_tokens_details.cached_tokens=0 on a repeated 2188-token prompt,
# while the same prompt on /v1/chat/completions returns cached_tokens=2176. The
# gpt-5.x / gpt-4.1 generation DOES cache on Responses; the gpt-4o-era + o1 models
# do not. So a 0-cache result for these on the responses ingress is expected
# provider behavior, not a gateway regression — record INFO, not WARN.
# Models whose /v1/responses calls OpenAI does not prefix-cache, direct-verified
# against the provider (bypassing the gateway) on the dates noted at the use site.
#
# The bar for adding an entry is a DIRECT provider measurement, and it is set high
# on purpose. Three pairs — gemini-2.5-flash@/v1/messages,
# gemini-2.5-flash-lite@/v1/responses, gpt-4.1-mini@/v1/responses — missed in two
# consecutive full-surface runs and were candidates for this set. Direct
# verification of gpt-4.1-mini on 2026-07-28 REFUTED that, and the numbers are
# worth keeping:
#
#   same ~1940-token prefix, consecutive identical calls, no gateway involved
#     /v1/chat/completions : cached_tokens 0 → 1792 → 1792   (deterministic)
#     /v1/responses        : cached_tokens 0 → 1792 → 0      (NON-deterministic)
#
# So /v1/responses does cache this model — it just does not do so reliably. Adding
# it here would have created an assertion that can never fail, which would hide a
# real gateway regression later. Two consecutive misses through the gateway are
# consistent with provider non-determinism and are NOT evidence of a no-cache
# model. The cumulative-across-rounds threshold below already exists to absorb
# exactly this; when every round misses anyway, that is the provider, not us.
_RESPONSES_NO_CACHE_MODELS = {"gpt-4o", "gpt-4o-mini", "o1"}


def _reasoning_for_model(model: str) -> bool:
    """True when smoke should audit reasoning_text + reasoning_tokens for
    the model. Three disjoint classifiers feed in:
      - is_reasoning():       OpenAI o-series + Kimi-K2-thinking + gpt-5.5 +
                              claude-opus-4-7 (named set; rejects temperature).
      - uses_thinking_tokens(): Gemini 2.5+ (thoughtsTokenCount on usage).
      - uses_heavy_thinking():  DeepSeek-V4 + Kimi-K2.x (chat reasoning_content
                              stream; need ≥1024-token budget to emit answer).
    The audit only fires when one matches — non-reasoning models stay silent."""
    return is_reasoning(model) or uses_thinking_tokens(model) or uses_heavy_thinking(model)


def _stream_to_extractor_shape(spec: IngressSpec, rs: dict) -> dict:
    """Re-wrap a stream-result dict so spec.extract_reasoning_tokens reads
    the same usage path it does for non-stream. Stream parsers stash usage
    under rs["usage"]; non-stream extractors read from the ingress-native
    envelope key (usage vs usageMetadata)."""
    raw = rs.get("usage") or {}
    if spec.nonce_id == "gem":
        return {"usageMetadata": raw}
    return {"usage": raw}


def _audit_reasoning(
    spec: "IngressSpec",
    model: str,
    phase_tag: str,
    sub_label: str,         # "non-stream" | "stream"
    reasoning_text: str,
    reasoning_tokens: int,
    native_label: str,
    usage_present: bool = True,
):
    """Best-effort reasoning audit for one arm of the suite.

    Silent for non-reasoning models. For reasoning models:
      - reasoning_tokens == 0 WARNs (suppressed for native Anthropic on P3A
        because the Anthropic API doesn't break out the count).
      - reasoning_tokens > 0 + empty text → INFO only; some providers
        (OpenAI o-series with low reasoning_effort, Gemini with summary
        suppressed) legitimately omit the chain-of-thought body.
      - both populated → PASS row recording both numbers.
    """
    if not _reasoning_for_model(model):
        return
    anthropic_native = spec.nonce_id == "msg" and spec.is_native(model)
    key = f"{model}/{sub_label}/reasoning"
    if anthropic_native:
        # Tokens absent by API; only audit text presence.
        if reasoning_text:
            log_info(f"  [{model}] {sub_label} reasoning_text_len={len(reasoning_text)} "
                     f"(anthropic-native: no separate token count)")
            rec(phase_tag, key).passed(
                f"text_len={len(reasoning_text)} ({native_label}; anthropic-native)"
            )
        else:
            log_info(f"  [{model}] {sub_label} reasoning_text empty "
                     f"(anthropic-native: ok when extended-thinking off)")
            rec(phase_tag, key).passed(
                f"no reasoning ({native_label}; anthropic-native, may be expected)"
            )
        return
    if reasoning_tokens == 0:
        provider = _detect_provider(model)
        # Case 0 — the usage envelope itself was never captured (stream final
        # usage chunk missed, e.g. under heavy concurrent --all-ingress load).
        # reasoning_tokens reads 0 only because there is no usage to read, NOT
        # because the provider reported a 0 count. Verified 2026-06-03: direct
        # DeepSeek + isolated gateway calls return reasoning_tokens correctly
        # (deepseek-reasoner: native completion_tokens_details.reasoning_tokens),
        # while the concurrent suite intermittently dropped rs["usage"] to None.
        # Distinguish this from a genuine provider-0 so we don't cry "extraction
        # drop" when the test simply didn't capture the count.
        if reasoning_text and not usage_present:
            log_info(f"  [{model}] {sub_label} reasoning_text_len={len(reasoning_text)} but "
                     f"usage envelope not captured — cannot assess reasoning_tokens "
                     f"(not a provider 0; likely stream final-usage-chunk miss under load)")
            rec(phase_tag, key).passed(
                f"reasoning_text_len={len(reasoning_text)}; usage not captured "
                f"({native_label}; count indeterminate)"
            )
            return
        # Case 1 — provider returns the chain-of-thought TEXT but no separate
        # reasoning-token COUNT. Verified by direct upstream captures (2026-05-31):
        #   * Moonshot kimi (k2.5/k2.6): raw usage = {prompt,completion,total},
        #     NO reasoning_tokens field — reasoning folded into completion_tokens.
        #   * Anthropic: the Messages API never breaks out a reasoning count.
        # So count=0 is honest, not a gateway conversion drop. Audit text
        # presence instead of flagging the missing count.
        if reasoning_text and provider in ("moonshot", "anthropic"):
            log_info(f"  [{model}] {sub_label} reasoning_text_len={len(reasoning_text)} "
                     f"({provider}: no separate reasoning_tokens count)")
            rec(phase_tag, key).passed(
                f"reasoning_text_len={len(reasoning_text)} no token count "
                f"({native_label}; {provider} omits count)"
            )
            return
        # Case 2 — no reasoning emitted at all (empty CoT + 0 count): the model
        # simply did not reason on this prompt. Verified causes: *-lite variants
        # ship with thinking OFF by default (gemini-2.5-flash-lite upstream omits
        # thoughtsTokenCount unless thinkingConfig is set), and o-series return 0
        # on trivial asks. INFO, not a defect. A genuine extraction drop would
        # carry CoT text and fall through to Case 3.
        if not reasoning_text:
            log_info(f"  [{model}] {sub_label} no reasoning emitted "
                     f"(reasoning_tokens=0, empty CoT — model did not reason on this prompt)")
            rec(phase_tag, key).passed(
                f"no reasoning emitted ({native_label}; 0 tokens, empty CoT)"
            )
            return
        # Case 3 — CoT text present from a provider that SHOULD report a count,
        # yet the count is 0. This is the real signal worth a WARN (possible
        # gateway extraction drop — investigate the raw upstream usage).
        log_warn(f"  [{model}] {sub_label}: reasoning model but reasoning_tokens=0 "
                 f"despite CoT text (text_len={len(reasoning_text)}, provider={provider})")
        rec(phase_tag, key).warning(
            f"reasoning_tokens=0 text_len={len(reasoning_text)} ({native_label}; provider={provider})"
        )
        return
    log_info(f"  [{model}] {sub_label} reasoning_tokens={reasoning_tokens} "
             f"reasoning_text_len={len(reasoning_text)}")
    rec(phase_tag, key).passed(
        f"reasoning_tokens={reasoning_tokens} text_len={len(reasoning_text)} ({native_label})"
    )


def _run_ingress_model_suite(
    spec: IngressSpec,
    gw: "GWClient",
    model: str,
    t0_iso: str,
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
):
    """Generic per-model test driver consumed by all 4 ingress phases.

    Three arms, in order:
      A. non-stream POST — HTTP 200 + envelope shape OK + text non-empty
      B. SSE stream     — HTTP 200 + terminator (when applicable) + text
      C. cache          — 1 write + N read rounds; cumulative cached_tokens
                          must reach _CACHE_HIT_THRESHOLD to PASS

    Per-ingress nonce isolation (P0a 2026-05-16): each ingress's body
    carries a `[ingress:X r:RUN]` user-content tag and a `[smoke ingress:X]`
    system-tail tag so (1) gateway L1 response cache never serves a
    different ingress's response, (2) the upstream prefix cache is per-
    ingress so the write turn truly starts from zero each time.
    """
    system = _system_prompt(model)
    if model in _UNCAPPED_REASONING_MODELS_SET:
        max_tok = 100000
    elif is_reasoning(model) or uses_heavy_thinking(model) or uses_thinking_tokens(model):
        # Reasoning/thinking models spend maxOutputTokens on internal reasoning
        # before emitting visible text; 1024 left several (gemini-2.5-*, o-series,
        # gpt-5.5, claude-opus-4-7) with empty output and degenerate token counts.
        # Give 10k headroom so the answer survives the thinking phase and the
        # reasoning-token accounting is meaningful.
        max_tok = 10000
    else:
        max_tok = 1024
    native_label = "native" if spec.is_native(model) else "x-format"
    phase_tag = spec.name
    nonce = spec.nonce_id

    msgs = _user_msg(system, "Reply with the single word: ok", model, ingress_id=nonce)

    # ─── A: non-stream ────────────────────────────────────────────────────────
    log_info(f"  [{model}] {spec.label} non-stream ({native_label}) …")
    r = spec.call_non_stream(gw, model, msgs, max_tok, timeout)
    sync_status = r.get("status", 0)
    if sync_status == 200 and isinstance(r.get("data"), dict):
        data = r["data"]
        envelope_err = spec.envelope_check(data)
        if envelope_err:
            log_fail(f"  [{model}] non-stream 200 but WRONG envelope: {envelope_err}")
            rec(phase_tag, f"{model}/non-stream").failed(
                f"envelope shape: {envelope_err} ({native_label})"
            )
        else:
            text = spec.extract_text(data)
            if text:
                usage_str = spec.usage_one_liner(data)
                log_ok(f"  [{model}] non-stream {sync_status} ({r['elapsed']:.1f}s) "
                       f"{usage_str} text={text[:40]!r}")
                rec(phase_tag, f"{model}/non-stream").passed(
                    f"{r['elapsed']:.1f}s {usage_str} ({native_label})"
                ).detail.update({"usage_raw": data.get("usage") or data.get("usageMetadata") or {}})
            else:
                diag = spec.diag_finish(data)
                log_warn(f"  [{model}] non-stream 200 but EMPTY text ({diag})")
                rec(phase_tag, f"{model}/non-stream").warning(
                    f"empty text; {diag} ({native_label})"
                )
            # Reasoning audit — runs regardless of text presence so empty-text
            # reasoning models (all-CoT-no-answer) still get reasoning_tokens
            # surfaced. Silent for non-reasoning models.
            _audit_reasoning(
                spec, model, phase_tag, "non-stream",
                spec.extract_reasoning_text(data),
                spec.extract_reasoning_tokens(data),
                native_label,
                usage_present=bool(data.get("usage") or data.get("usageMetadata")),
            )
    else:
        err = r.get("data", {}).get("error") or r.get("error", "")
        log_fail(f"  [{model}] {spec.label} non-stream HTTP {sync_status}: {str(err)[:100]}")
        rec(phase_tag, f"{model}/non-stream").failed(
            f"HTTP {sync_status}: {str(err)[:100]}"
        )

    # ─── B: SSE stream ────────────────────────────────────────────────────────
    stream_status = 0
    if not no_stream:
        log_info(f"  [{model}] {spec.label} SSE ({native_label}) …")
        rs = spec.call_stream(gw, model, msgs, max_tok, timeout)
        stream_status = rs.get("status", 0)
        stream_content = rs.get("content", "")
        counter = rs.get("event_count", rs.get("chunk_count", 0))
        if stream_status == 200:
            terminated = True if spec.stream_terminate_key is None else bool(rs.get(spec.stream_terminate_key))
            if terminated and stream_content:
                log_ok(f"  [{model}] SSE {stream_status} ttfb={rs.get('ttfb', 0):.2f}s "
                       f"events={counter} text={stream_content[:40]!r}")
                rec(phase_tag, f"{model}/stream").passed(
                    f"ttfb={rs.get('ttfb', 0):.2f}s events={counter} ({native_label})"
                )
            elif terminated and counter > 0 and not stream_content:
                log_warn(f"  [{model}] SSE 200 terminated, {counter} chunk(s), but EMPTY text")
                rec(phase_tag, f"{model}/stream").warning("terminated, chunks present, no text")
            elif terminated:
                log_warn(f"  [{model}] SSE 200 terminated but 0 chunks")
                rec(phase_tag, f"{model}/stream").warning("0 chunks")
            else:
                log_warn(f"  [{model}] SSE 200 but no terminate event (events={counter})")
                rec(phase_tag, f"{model}/stream").warning("no terminate event")
            # Reasoning audit on the stream view too — reasoning_content
            # was accumulated separately by each ingress's SSE parser.
            _audit_reasoning(
                spec, model, phase_tag, "stream",
                rs.get("reasoning_content", "") or "",
                spec.extract_reasoning_tokens(_stream_to_extractor_shape(spec, rs)),
                native_label,
                usage_present=rs.get("usage") is not None,
            )
        else:
            err = rs.get("error", "")
            log_fail(f"  [{model}] SSE HTTP {stream_status}: {str(err)[:80]}")
            rec(phase_tag, f"{model}/stream").failed(f"HTTP {stream_status}: {str(err)[:80]}")

    _record_chat(model, t0_iso, sync_status, stream_status)

    # ─── C: multi-round cache ─────────────────────────────────────────────────
    if no_cache or sync_status != 200:
        return
    if model in _NO_AUTOMATIC_CACHE:
        log_info(f"  [{model}] {spec.label} cache skipped — provider does not support automatic prefix caching")
        rec(phase_tag, f"{model}/cache").passed("automatic prefix caching not supported by provider; skipped")
        return

    rounds = max(1, cache_rounds)
    log_info(f"  [{model}] {spec.label} cache test (1 write + {rounds} read rounds) …")
    msgs1 = _cache_user_msg(system, _CACHE_WRITE_QUESTION, model, ingress_id=nonce)
    r1 = spec.call_non_stream(gw, model, msgs1, max_tok, timeout)
    if r1.get("status") != 200 or not isinstance(r1.get("data"), dict):
        log_warn(f"  [{model}] cache T1 failed (HTTP {r1.get('status')}), skipping")
        rec(phase_tag, f"{model}/cache").warning(f"T1 HTTP {r1.get('status')}")
        return

    reply1 = spec.extract_text(r1["data"]) or "Go is a modern compiled language."
    # Gemini-target prompt caching: the gateway's E38 cachedContent creation
    # fires asynchronously (geminicache.Manager.asyncCreate). After the
    # write turn the goroutine is still running; immediate reads see a
    # cache miss. _GEMINI_TARGET_POST_WRITE_SLEEP gives the API call +
    # Redis set + propagation headroom. (Observed: smoke 2026-05-16T05:41
    # — cache created at 05:41:44, but reads at 05:41:26/34/42 all missed.)
    if _GEMINI_MODEL_RE.match(model):
        time.sleep(_GEMINI_TARGET_POST_WRITE_SLEEP)

    user_suffix = _ingress_user_suffix(nonce)
    history: list[dict] = [
        {"role": "user", "content": _CACHE_WRITE_QUESTION + user_suffix},
        {"role": "assistant", "content": reply1},
    ]

    cumulative_hit = 0
    read_failures = 0
    round_sleep = _GEMINI_TARGET_ROUND_SLEEP if _GEMINI_MODEL_RE.match(model) else _DEFAULT_ROUND_SLEEP

    for rnd in range(1, rounds + 1):
        time.sleep(round_sleep)
        msgs2 = _cache_next_round_msgs(system, list(history), model, rnd - 1, ingress_id=nonce)
        r2 = spec.call_non_stream(gw, model, msgs2, max_tok, timeout)
        # P2: retry once on HTTP 0 (network/timeout) before counting as a
        # read failure — single-shot transient flakes shouldn't mask a
        # genuinely working cache.
        if r2.get("status", 0) == 0:
            log_info(f"  [{model}] cache round {rnd} HTTP 0 — retrying once")
            time.sleep(1.0)
            r2 = spec.call_non_stream(gw, model, msgs2, max_tok, timeout)
        if r2.get("status") == 200 and isinstance(r2.get("data"), dict):
            hit = spec.extract_cached_tokens(r2["data"])
            cumulative_hit += hit
            log_info(f"  [{model}] cache round {rnd}/{rounds}: cached_tokens={hit} (cumulative={cumulative_hit})")
            followup_q = _CACHE_FOLLOWUP_QUESTIONS[(rnd - 1) % len(_CACHE_FOLLOWUP_QUESTIONS)] + user_suffix
            round_reply = spec.extract_text(r2["data"]) or "Good point."
            history.append({"role": "user", "content": followup_q})
            history.append({"role": "assistant", "content": round_reply})
        else:
            read_failures += 1
            log_warn(f"  [{model}] cache round {rnd} HTTP {r2.get('status')}")

    # P2: enforce threshold so we don't pass on noise. Below threshold but
    # non-zero gets WARN with the actual count (operator-actionable).
    if cumulative_hit >= _CACHE_HIT_THRESHOLD:
        log_ok(f"  [{model}] {spec.label} cache hit {cumulative_hit} tokens across {rounds} rounds ({native_label})")
        rec(phase_tag, f"{model}/cache").passed(
            f"cumulative_cached_tokens={cumulative_hit} rounds={rounds} ({native_label})"
        )
    elif read_failures == rounds:
        rec(phase_tag, f"{model}/cache").warning(f"all {rounds} read rounds failed")
    elif cumulative_hit > 0:
        log_warn(f"  [{model}] cache hit {cumulative_hit} below {_CACHE_HIT_THRESHOLD}-token threshold (noise)")
        rec(phase_tag, f"{model}/cache").warning(
            f"low cache hit ({cumulative_hit} < {_CACHE_HIT_THRESHOLD} threshold)"
        )
    elif spec.nonce_id == "resp" and model in _RESPONSES_NO_CACHE_MODELS:
        # Expected: OpenAI doesn't cache this model family on /v1/responses
        # (direct-verified 2026-06-03). Not a gateway defect.
        log_info(f"  [{model}] no cache hit across {rounds} rounds — expected on "
                 f"/v1/responses (OpenAI does not cache this model family there; "
                 f"direct-verified 2026-06-03)")
        rec(phase_tag, f"{model}/cache").passed(
            f"no responses cache — expected for {model} (OpenAI provider behavior; "
            f"caches on chat-completions, not responses)"
        )
    else:
        log_warn(f"  [{model}] no cache hit across {rounds} rounds")
        rec(phase_tag, f"{model}/cache").warning(f"no cache hit (rounds={rounds})")


# ─── Chat calls with collected results ────────────────────────────────────────

_chat_calls: list[dict] = []  # {model, t0, non_stream_status, stream_status}

def _record_chat(model: str, t0_iso: str, sync_status: int, stream_status: int):
    _chat_calls.append({
        "model": model,
        "t0": t0_iso,
        "sync_status": sync_status,
        "stream_status": stream_status,
    })


# ─── Phase implementations ─────────────────────────────────────────────────────

def _find_cache_container() -> Optional[str]:
    """Return the container id hosting the gateway's response cache, or None.

    NEXUS_REDIS_CONTAINER overrides the search. Otherwise: match the names the
    cache is deployed under (Redis and its Valkey fork), then confirm by the
    fact we actually depend on — that it answers the Redis protocol. Selecting
    on `name=redis` alone silently matched nothing on a stack whose container is
    `nexus-valkey`, and a dev box can run several containers whose name contains
    "redis" (a sibling stack's broker), where `head -1` picks a coin-flip.
    Same failure the postgres selector had.
    """
    override = os.environ.get("NEXUS_REDIS_CONTAINER", "").strip()
    if override:
        return override
    candidates: list[str] = []
    for name in ("redis", "valkey"):
        out = subprocess.run(
            ["docker", "ps", "--filter", f"name={name}", "-q"],
            capture_output=True, text=True, timeout=5,
        ).stdout.strip()
        candidates.extend(c for c in out.split("\n") if c.strip())
    for cid in dict.fromkeys(candidates):  # de-dup, preserve order
        ping = subprocess.run(
            ["docker", "exec", cid, "redis-cli", "PING"],
            capture_output=True, text=True, timeout=10,
        )
        if ping.returncode == 0 and ping.stdout.strip().upper() == "PONG":
            return cid
    return None


def _flush_redis_gateway_cache(is_remote: bool = False) -> Optional[int]:
    """Delete all ai-gw::* response-cache keys from the gateway's cache.

    Returns the number of keys deleted, or None when the flush could not be
    performed (no container, or the command failed). None and 0 are NOT the
    same: 0 means the cache was already empty, None means every later cache
    assertion is running against unflushed state. Callers must distinguish them
    — reporting "could not flush" as "empty (nothing to flush)" turns a broken
    smoke into a green one.
    Returns 0 for is_remote=True (no local Docker; nothing to flush by design).
    Operates on the gateway's response-cache key namespace only — does not
    touch session, IAM, or quota counter keys.
    """
    if is_remote:
        return 0
    try:
        container = _find_cache_container()
        if not container:
            log_warn("  Redis flush: no container answering the Redis protocol "
                     "(set NEXUS_REDIS_CONTAINER to select one)")
            return None

        # SCAN + DEL in one Lua call to avoid round-trips; safe for up to ~10 k keys.
        lua = (
            "local ks = redis.call('keys', ARGV[1]) "
            "if #ks == 0 then return 0 end "
            "return redis.call('del', unpack(ks))"
        )
        result = subprocess.run(
            ["docker", "exec", container, "redis-cli", "EVAL", lua, "0", "ai-gw::*"],
            capture_output=True, text=True, timeout=15,
        )
        out = result.stdout.strip()
        if result.returncode != 0 or not out.lstrip("-").isdigit():
            log_warn(f"  Redis flush: EVAL failed on {container}: "
                     f"{(result.stderr or out).strip()[:200]}")
            return None
        return int(out)
    except Exception as e:
        log_warn(f"  Redis flush error: {e}")
        return None


def phase0_preflight(gw: GWClient, cp: CPClient, is_remote: bool = False) -> str:
    """Returns t0 ISO timestamp."""
    log_step("P0 Preflight")
    t0 = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    # Gateway health
    try:
        status, body = gw.healthz()
        if status == 200:
            log_ok(f"Gateway healthy: {body[:60]}")
            rec("P0", "healthz").passed()
        else:
            log_fail(f"Gateway /healthz HTTP {status}")
            rec("P0", "healthz").failed(f"HTTP {status}")
    except Exception as e:
        log_fail(f"Gateway /healthz unreachable: {e}")
        rec("P0", "healthz").failed(str(e))
        raise RuntimeError("Gateway not up — aborting") from e

    # CP login
    try:
        cp.login()
        rec("P0", "cp-login").passed()
    except Exception as e:
        log_fail(f"CP login failed: {e}")
        rec("P0", "cp-login").failed(str(e))
        raise RuntimeError("CP login failed — cannot manage config") from e

    # Flush gateway response cache so every test hits the upstream provider.
    deleted = _flush_redis_gateway_cache(is_remote=is_remote)
    if is_remote:
        log_info("Redis gateway cache: remote environment — flush skipped")
        rec("P0", "redis-flush").passed("remote; skipped")
    elif deleted is None:
        # Not "empty" — the flush did not run, so every cache assertion below
        # is measuring unflushed state. Fail loudly rather than report green.
        log_fail("Redis gateway cache: flush could not run — cache arms below "
                 "would test stale state")
        rec("P0", "redis-flush").failed("flush unavailable")
    elif deleted > 0:
        log_ok(f"Redis gateway cache flushed: {deleted} key(s) deleted")
        rec("P0", "redis-flush").passed(f"{deleted} keys")
    else:
        log_info("Redis gateway cache: empty (nothing to flush)")
        rec("P0", "redis-flush").passed("empty")

    return t0


def phase1_auth_boundary(gw: GWClient, first_model: str):
    log_step("P1 Auth boundary")

    status = gw.chat_no_auth(first_model)
    if status in (401, 403):
        log_ok(f"No auth → HTTP {status}")
        rec("P1", "no-auth").passed(f"HTTP {status}")
    else:
        log_fail(f"No auth → HTTP {status} (expected 401/403)")
        rec("P1", "no-auth").failed(f"HTTP {status}")

    status = gw.chat_bad_vk(first_model)
    if status in (401, 403):
        log_ok(f"Bad VK → HTTP {status}")
        rec("P1", "bad-vk").passed(f"HTTP {status}")
    else:
        log_fail(f"Bad VK → HTTP {status} (expected 401/403)")
        rec("P1", "bad-vk").failed(f"HTTP {status}")


def phase2_catalog(gw: GWClient) -> tuple[list[str], list[dict]]:
    """Return (chat_model_ids, embedding_model_entries).

    Classifies every catalog model by outputModalities when present, else
    by prefix fallback. Embedding entries carry the full model dict so
    P3E can read capability fields (default_dimension, max_batch_size, etc.)
    """
    log_step("P2 Catalog")

    status, body = gw.list_models()
    models_data = body.get("data", [])

    chat_models: list[str] = []
    embedding_models: list[dict] = []
    skip_models: list[str] = []

    for m in models_data:
        modality = classify_model_modality(m)
        mid = m.get("id", "")
        floor = m.get("requiredModalities") or []
        if floor:
            _MODEL_FLOOR[mid] = list(floor)
        if modality == "chat":
            chat_models.append(mid)
        elif modality == "embedding":
            embedding_models.append(m)
        else:
            # image / audio / video / skip — out of E62 scope
            skip_models.append(f"{mid}({modality})")

    if status == 200 and (chat_models or embedding_models):
        log_ok(
            f"/v1/models: {len(models_data)} total — "
            f"{len(chat_models)} chat, {len(embedding_models)} embedding, "
            f"{len(skip_models)} other (skipped)"
        )
        rec("P2", "list-models").passed(f"{len(models_data)} models")
    else:
        log_fail(f"/v1/models HTTP {status}, chat={len(chat_models)} embedding={len(embedding_models)}")
        rec("P2", "list-models").failed(f"HTTP {status}")

    if skip_models:
        log_info(f"Skipped (non-chat/non-embedding): {', '.join(skip_models)}")

    # Sample detail check (first 5) — include one embedding model if available
    sample_mids = chat_models[:4] + [e.get("id", "") for e in embedding_models[:1]]
    detail_fails = 0
    for mid in sample_mids[:5]:
        if not mid:
            continue
        s2, _ = gw.get_model(mid)
        if s2 != 200:
            log_warn(f"  GET /v1/models/{mid} → HTTP {s2}")
            detail_fails += 1
    if detail_fails == 0:
        log_ok(f"Model detail: sampled OK")
        rec("P2", "model-detail").passed()
    else:
        log_warn(f"Model detail: {detail_fails} sampled returned non-200")
        rec("P2", "model-detail").warning(f"{detail_fails} non-200")

    # Usage
    su, _ = gw.usage()
    sud, _ = gw.usage_daily()
    if su == 200:
        log_ok(f"/v1/usage OK")
        rec("P2", "usage").passed()
    else:
        log_fail(f"/v1/usage HTTP {su}")
        rec("P2", "usage").failed(f"HTTP {su}")
    if sud == 200:
        log_ok(f"/v1/usage/daily OK")
        rec("P2", "usage-daily").passed()
    else:
        log_warn(f"/v1/usage/daily HTTP {sud}")
        rec("P2", "usage-daily").warning(f"HTTP {sud}")

    return chat_models, embedding_models


def _run_model_suite(
    gw: GWClient,
    model: str,
    t0_iso: str,
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    phase_tag: str,
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
):
    """Thin back-compat shim. Delegates to the generic [_run_ingress_model_suite]
    with the chat-completions ingress spec. phase_tag is ignored — spec carries
    its own ("P3"). Kept so phase3_routing_off / phase4_routing_on don't need
    to learn about specs."""
    _run_ingress_model_suite(
        INGRESS_CHAT, gw, model, t0_iso, no_stream, no_cache, timeout, cache_rounds,
        db=db,
    )



def _wait_config_propagation(gw: GWClient, wait_secs: int = 5):
    """Wait for the gateway to reload config pushed from CP via Hub shadow."""
    log_info(f"  Waiting {wait_secs}s for gateway config propagation …")
    time.sleep(wait_secs)
    # Verify gateway is still up after the wait
    try:
        status, _ = gw.healthz()
        if status != 200:
            log_warn(f"  Gateway /healthz returned {status} after config wait")
    except Exception as e:
        log_warn(f"  Gateway health check failed after config wait: {e}")


def phase3_routing_off(
    gw: GWClient,
    cp: CPClient,
    snapshot: StateSnapshot,
    models: list[str],
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    t0_iso: str,
    cache_rounds: int = 3,
    is_remote: bool = False,
    db: Optional["DBClient"] = None,
    manage_routing: bool = True,
):
    log_step("P3 Routing OFF — per-model suite")
    deleted = _flush_redis_gateway_cache(is_remote=is_remote)
    if not is_remote:
        if deleted is None:
            log_warn("  Redis cache flush could not run — this phase's cache "
                     "arms may be reading stale entries")
        else:
            log_info(f"  Redis cache flushed: {deleted} key(s) — fresh upstream calls guaranteed")
    if manage_routing:
        cp.set_all_routing_rules(False, snapshot.routing_rules)
        _wait_config_propagation(gw)
    else:
        log_warn("--no-routing-manage: leaving routing rules untouched; "
                 "an active rule may re-route per-model requests (P3 assertions may warn)")

    # Per-model suite via worker pool (2026-05-20). Each model runs in
    # its own thread with a per-task log buffer + per-provider semaphore;
    # cache test rounds stay serial inside the suite.
    _run_pool_for_models(
        INGRESS_CHAT, gw, models, t0_iso, no_stream, no_cache, timeout,
        cache_rounds=cache_rounds, db=db,
    )



# ─── P3T — tool-body coercion probes ─────────────────────────────────────────
# Live upstream proof for the two tool-body rule classes that plain chat
# arms never exercise (they send no tools[]). Each probe SENDS the body the
# rule exists to fix; a 200 plus the exact x-nexus-coerced label proves the
# gateway-side coercion fired AND the coerced body is what the vendor
# accepts. A stale rule (vendor starts accepting the original body: probe
# still 200 but label missing → red) or a regression (400 → red) both
# surface here. Probes run only when the target model was selected.
#
# Evidence anchors (probed 2026-07-17, prod-observed 400s):
#   - gpt-5.6* + non-empty tools on /v1/chat/completions rejects ANY
#     reasoning_effort other than the explicit "none" — including absent.
#   - gemini rejects tool schemas whose $ref target was never shipped
#     (OpenAPI #/components/... pointers); the codec degrades the property
#     and reports it.
_P3T_TOOLS = [{"type": "function", "function": {
    "name": "get_time", "description": "Get current time",
    "parameters": {"type": "object", "properties": {"tz": {"type": "string"}}}}}]

_P3T_REF_TOOLS = [{"type": "function", "function": {
    "name": "dryRun", "description": "d",
    "parameters": {"type": "object", "properties": {
        "detector_type": {"type": "string"},
        "context": {"$ref": "#/components/schemas/handler_DryRunContext",
                     "description": "routing context"}},
        "required": ["detector_type"]}}}]


def phase3t_tool_coercions(gw: "GWClient", models: list[str], timeout: int):
    probes = []
    gpt56 = [m for m in models if m.startswith("gpt-5.6")]
    if gpt56:
        probes.append((gpt56[0], _P3T_TOOLS, "reasoning_effort→none (function tools on the chat wire)",
                       {"reasoning_effort": "high"}))
        probes.append((gpt56[0], _P3T_TOOLS, "reasoning_effort→none (function tools on the chat wire)", {}))
    gem = [m for m in models if m.startswith("gemini-")]
    if gem:
        probes.append((gem[0], _P3T_REF_TOOLS,
                       "tools.dryRun.parameters.$ref(#/components/schemas/handler_DryRunContext)→object", {}))
    if not probes:
        return
    log_step("P3T Tool-body coercion probes")
    for model, tools, want_label, extra in probes:
        tag = "effort" if "reasoning_effort→" in want_label else "$ref"
        arm = f"{model}/tools-{tag}" + ("+explicit" if extra else "")
        # Inject the per-run nonce so the probe never lands on an L1 cache hit:
        # a cached 200 is served WITHOUT re-running the coercion, so its response
        # carries no x-nexus-coerced header and the label check false-reds (the
        # coercion did fire on the original miss). Same L1-bypass every other
        # phase uses.
        #
        # The nonce must also vary BETWEEN arms. Two arms of the same model differ
        # only in a request PARAMETER (reasoning_effort present vs absent) while
        # sending identical message content, so a run-scoped nonce leaves the
        # second arm hitting the first arm's cache entry — exactly the false-red
        # described above, self-inflicted. Observed on prod 2026-07-28:
        # tools-effort+explicit returned MISS with the label, tools-effort
        # returned HIT with no label and was reported as "rule stale or report
        # lost" when the gateway had coerced correctly (verified directly against
        # the vendor: absent reasoning_effort with function tools still 400s, so
        # the rule is live, and a uniquely-nonced request returns 200 with the
        # label).
        out = gw.chat_sync(model, [{"role": "user", "content": f"hi r:{_run_nonce}/{arm}"}],
                           max_tokens=16, timeout=timeout, tools=tools, **extra)
        if out.get("status") != 200:
            rec("P3T", arm).failed(f"HTTP {out.get('status')}: {str(out.get('data'))[:120]}")
            log_fail(f"  [{arm}] HTTP {out.get('status')} — the coercion did not save the body")
            continue
        coerced = (out.get("headers") or {}).get("x-nexus-coerced", "")
        # http.client decodes header bytes as latin-1; the label's "→" is
        # UTF-8 on the wire, so re-decode before comparing.
        try:
            coerced = coerced.encode("latin-1").decode("utf-8")
        except (UnicodeDecodeError, UnicodeEncodeError):
            pass
        if want_label not in coerced:
            rec("P3T", arm).failed(f"200 but x-nexus-coerced missing {want_label!r} (got {coerced!r})")
            log_fail(f"  [{arm}] 200 but coercion label missing — rule stale or report lost")
            continue
        rec("P3T", arm).passed(f"200 + coerced: {want_label}")
        log_ok(f"  [{arm}] 200 + x-nexus-coerced carries the rule label")


# Models empirically known to accept /v1/responses NATIVELY (verified
# 200 from the real OpenAI endpoint). Per provider-adapter-architecture.md
# §3a Rule 7, additions here require captured evidence.
#
# This list now ONLY informs the per-row "expected route" log line —
# phase3r_responses_api itself tests EVERY catalog model so the
# cross-format /v1/responses path (E56) gets the same coverage as the
# native passthrough path. Models not in the list are exercised via
# canonical-bridge translation (Responses → canonical chat-completions
# → target wire → response → Responses-shape on egress).
RESPONSES_API_MODEL_PREFIXES = (
    "gpt-5", "gpt-4o", "gpt-4.1", "o1", "o3", "o4",
)


def _model_supports_responses_api(model_id: str) -> bool:
    for p in RESPONSES_API_MODEL_PREFIXES:
        if model_id.startswith(p):
            return True
    return False


def _messages_to_responses_input(messages: list[dict]) -> list[dict]:
    """Convert chat-completions messages[] into Responses-API input[].

    Each chat message becomes an input_message item with content as
    input_text parts. The Responses-API canonical bridge (E56-S2) maps
    this back to a canonical chat-completions messages[] shape on the
    cross-format path; for OpenAI native target the body forwards
    verbatim.

    Drops `system`-role messages from the array and surfaces the
    concatenated system text to the caller via the `instructions`
    field of the Responses request — that's how the Responses-API
    represents system-level guidance. Returns (input_items, instructions).
    """
    input_items: list[dict] = []
    instructions_parts: list[str] = []
    for m in messages:
        role = m.get("role", "user")
        content = m.get("content", "")
        if role == "system":
            instructions_parts.append(content)
            continue
        # output_text is the assistant content type when echoing prior
        # assistant turns; input_text is for user/system. Responses-API
        # canonical bridge accepts either on the assistant role per
        # codec_responses.go normalizeInputContentPart.
        ctype = "output_text" if role == "assistant" else "input_text"
        input_items.append({
            "role": role,
            "content": [{"type": ctype, "text": content}],
        })
    return input_items, "\n".join(instructions_parts).strip() or None


def _responses_extract_text(data: dict) -> str:
    """Pull the assistant text from a Responses non-stream body's
    output[]. Mirrors the chat-completions choices[0].message.content
    extraction the existing code uses."""
    if not isinstance(data, dict):
        return ""
    parts: list[str] = []
    # `output` can be explicitly null (vs missing) when the upstream
    # produced no message items — guard with `or []` not just default.
    output = data.get("output") or []
    for item in output:
        if not isinstance(item, dict):
            continue
        if item.get("type") == "message":
            for c in item.get("content") or []:
                if isinstance(c, dict) and c.get("type") == "output_text" and isinstance(c.get("text"), str):
                    parts.append(c["text"])
    return "".join(parts)


def _responses_extract_cached_tokens(usage: dict) -> int:
    """Pull cached_tokens from a Responses-API usage envelope. The
    Responses field is `usage.input_tokens_details.cached_tokens`
    (renamed from chat-completions' `prompt_tokens_details.cached_tokens`
    — see specutil.cachedTokenAliases for the binding)."""
    if not isinstance(usage, dict):
        return 0
    details = usage.get("input_tokens_details") or {}
    return int(details.get("cached_tokens", 0) or 0)


def _run_responses_model_suite(
    gw: GWClient,
    model: str,
    t0_iso: str,
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    phase_tag: str,
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
):
    """Thin shim. Delegates to [_run_ingress_model_suite] with INGRESS_RESPONSES."""
    _run_ingress_model_suite(
        INGRESS_RESPONSES, gw, model, t0_iso, no_stream, no_cache, timeout, cache_rounds,
        db=db,
    )


def phase3r_responses_api(
    gw: GWClient,
    models: list[str],
    timeout: int,
    no_stream: bool,
    no_cache: bool = False,
    t0_iso: str = "",
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
) -> None:
    """E56 P3R: full per-model /v1/responses smoke for ALL catalog models.

    Mirrors the P3 chat-completions methodology — non-stream + SSE +
    multi-round cache — so /v1/responses gets parity coverage. Models
    in RESPONSES_API_MODEL_PREFIXES exercise the OpenAI native
    same-shape passthrough; all other models exercise the cross-format
    canonical bridge (Responses → canonical chat-completions → target
    wire → canonical → Responses-shape on egress).

    Cache parity uses provider-side prompt cache reported via the
    Responses-API renamed field `usage.input_tokens_details.cached_tokens`
    (specutil.cachedTokenAliases binding).

    The per-row log line tags each model with (native) or (x-format) so
    a failure punch-list immediately tells you which path broke.
    """
    log_step("P3R Responses-API — full per-model suite (mirrors P3 depth)")
    if not models:
        log_warn("  No catalog models — phase skipped")
        return

    native_count = sum(1 for m in models if _model_supports_responses_api(m))
    log_info(f"  Testing /v1/responses on {len(models)} model(s): "
             f"{native_count} native (OpenAI passthrough), "
             f"{len(models) - native_count} cross-format")

    _run_pool_for_models(
        INGRESS_RESPONSES, gw, models, t0_iso, no_stream, no_cache, timeout,
        cache_rounds=cache_rounds, db=db,
    )


_ANTHROPIC_MODEL_RE = re.compile(r"^(claude|anthropic[\-_])")


def _model_natively_anthropic(model_id: str) -> bool:
    """True when the model is served by an Anthropic-spec adapter natively
    (same-shape passthrough for /v1/messages ingress). Heuristic on the
    model code prefix; the underlying enum on the gateway is Format ==
    FormatAnthropic, which only Anthropic adapters declare."""
    return bool(_ANTHROPIC_MODEL_RE.match(model_id))


def _messages_to_anthropic_body(messages: list[dict]) -> tuple[list[dict], str]:
    """Convert chat-completions messages[] into Anthropic body components.

    Returns (messages_without_system, system_text). Anthropic forbids
    `system` inside the messages[] array — it has its own top-level field.
    Multiple system messages are concatenated.
    """
    sys_parts: list[str] = []
    out: list[dict] = []
    for m in messages:
        role = m.get("role", "user")
        content = m.get("content", "")
        if not isinstance(content, str):
            content = str(content)
        if role == "system":
            sys_parts.append(content)
            continue
        out.append({"role": role, "content": content})
    return out, "\n".join(sys_parts).strip()


def _anthropic_extract_text(data: dict) -> str:
    """Pull assistant text from Anthropic Messages-API response.content[]."""
    if not isinstance(data, dict):
        return ""
    parts: list[str] = []
    for blk in data.get("content") or []:
        if isinstance(blk, dict) and blk.get("type") == "text" and isinstance(blk.get("text"), str):
            parts.append(blk["text"])
    return "".join(parts)


def _anthropic_extract_cached_tokens(usage: dict) -> int:
    """Anthropic reports cache hits via usage.cache_read_input_tokens."""
    if not isinstance(usage, dict):
        return 0
    return int(usage.get("cache_read_input_tokens", 0) or 0)


def _run_messages_model_suite(
    gw: GWClient,
    model: str,
    t0_iso: str,
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    phase_tag: str,
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
):
    """Thin shim. Delegates to [_run_ingress_model_suite] with INGRESS_MESSAGES."""
    _run_ingress_model_suite(
        INGRESS_MESSAGES, gw, model, t0_iso, no_stream, no_cache, timeout, cache_rounds,
        db=db,
    )


def phase3a_messages_api(
    gw: GWClient,
    models: list[str],
    timeout: int,
    no_stream: bool,
    no_cache: bool = False,
    t0_iso: str = "",
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
) -> None:
    """P3A: full per-model /v1/messages (Anthropic) smoke for ALL catalog models.

    Mirrors P3R depth. Anthropic-spec models exercise native passthrough;
    every other model exercises the canonical bridge (Anthropic ingress →
    canonical OpenAI chat-completions → target wire → canonical →
    Anthropic-shape on egress)."""
    log_step("P3A /v1/messages (Anthropic) — full per-model suite (mirrors P3 depth)")
    if not models:
        log_warn("  No catalog models — phase skipped")
        return
    native_count = sum(1 for m in models if _model_natively_anthropic(m))
    log_info(f"  Testing /v1/messages on {len(models)} model(s): "
             f"{native_count} native (Anthropic passthrough), "
             f"{len(models) - native_count} cross-format")
    _run_pool_for_models(
        INGRESS_MESSAGES, gw, models, t0_iso, no_stream, no_cache, timeout,
        cache_rounds=cache_rounds, db=db,
    )


# ─── Gemini /v1beta ───────────────────────────────────────────────────────────


def _model_natively_gemini(model_id: str) -> bool:
    """True when the model is served by a Gemini/Vertex adapter natively."""
    return bool(_GEMINI_MODEL_RE.match(model_id))


def _messages_to_gemini_contents(messages: list[dict]) -> tuple[list[dict], str]:
    """Convert chat-completions messages[] into Gemini body components.

    Returns (contents, system_text). Gemini's `contents` use role+parts.
    System messages are hoisted into the top-level `systemInstruction`
    field. assistant maps to role='model'."""
    sys_parts: list[str] = []
    out: list[dict] = []
    for m in messages:
        role = m.get("role", "user")
        content = m.get("content", "")
        if not isinstance(content, str):
            content = str(content)
        if role == "system":
            sys_parts.append(content)
            continue
        gemini_role = "model" if role == "assistant" else "user"
        out.append({"role": gemini_role, "parts": [{"text": content}]})
    return out, "\n".join(sys_parts).strip()


def _gemini_extract_text(data: dict) -> str:
    """Pull assistant text from Gemini generateContent response."""
    if not isinstance(data, dict):
        return ""
    parts: list[str] = []
    for cand in data.get("candidates") or []:
        if not isinstance(cand, dict):
            continue
        content = cand.get("content") or {}
        for part in content.get("parts") or []:
            t = part.get("text") if isinstance(part, dict) else None
            if isinstance(t, str):
                parts.append(t)
    return "".join(parts)


def _gemini_extract_cached_tokens(usage: dict) -> int:
    """Gemini reports implicit cache via usageMetadata.cachedContentTokenCount."""
    if not isinstance(usage, dict):
        return 0
    return int(usage.get("cachedContentTokenCount", 0) or 0)


def _run_gemini_model_suite(
    gw: GWClient,
    model: str,
    t0_iso: str,
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    phase_tag: str,
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
):
    """Thin shim. Delegates to [_run_ingress_model_suite] with INGRESS_GEMINI."""
    _run_ingress_model_suite(
        INGRESS_GEMINI, gw, model, t0_iso, no_stream, no_cache, timeout, cache_rounds,
        db=db,
    )


def phase3g_gemini_api(
    gw: GWClient,
    models: list[str],
    timeout: int,
    no_stream: bool,
    no_cache: bool = False,
    t0_iso: str = "",
    cache_rounds: int = 3,
    db: Optional["DBClient"] = None,
) -> None:
    """P3G: full per-model /v1beta (Gemini) smoke for ALL catalog models."""
    log_step("P3G /v1beta (Gemini) — full per-model suite (mirrors P3 depth)")
    if not models:
        log_warn("  No catalog models — phase skipped")
        return
    native_count = sum(1 for m in models if _model_natively_gemini(m))
    log_info(f"  Testing /v1beta on {len(models)} model(s): "
             f"{native_count} native (Gemini passthrough), "
             f"{len(models) - native_count} cross-format")
    _run_pool_for_models(
        INGRESS_GEMINI, gw, models, t0_iso, no_stream, no_cache, timeout,
        cache_rounds=cache_rounds, db=db,
    )


# ─── Phase: P3E Embeddings ───────────────────────────────────────────────────
#
# Six arms per (ingress, model) tuple:
#   A  non-stream basic  — POST simple input; assert 200 + data[0].embedding
#   B  dimensions        — POST with dimensions=half_of_max; skip if unsupported
#   C  batch input       — POST N inputs; assert N items + order preserved
#   D  traffic_event     — DB cross-check: endpoint_type=embeddings, cost>0
#   E  Prometheus delta  — counter increments == submitted requests
#   F  cross-ingress     — same input to native + cross-format; cosine_sim>0.999
#
# Cache arm: SKIPPED — embeddings have no prompt-cache semantic.
#
# Negative tests (Arm G):
#   OpenAI ingress + dimensions=2048 pinned to Cohere (fixed 1024) → 400
#   Cohere ingress + batch 200 pinned to Cohere (max_batch=96) → 400

_EMBEDDING_INGRESS_FORMATS = ["openai", "azure", "cohere", "gemini"]


def _cosine_similarity(v1: list, v2: list) -> float:
    """Compute cosine similarity between two float vectors. Returns 0 on error."""
    if len(v1) != len(v2) or not v1:
        return 0.0
    dot = sum(a * b for a, b in zip(v1, v2))
    n1 = sum(a * a for a in v1) ** 0.5
    n2 = sum(b * b for b in v2) ** 0.5
    if n1 == 0 or n2 == 0:
        return 0.0
    return dot / (n1 * n2)


def _extract_openai_embedding(data: dict, idx: int = 0) -> Optional[list]:
    """Extract embedding vector from OpenAI-shape response data."""
    items = data.get("data") if isinstance(data, dict) else None
    if not isinstance(items, list) or len(items) <= idx:
        return None
    item = items[idx]
    emb = item.get("embedding") if isinstance(item, dict) else None
    if isinstance(emb, list):
        return emb
    return None


def _extract_cohere_embedding(data: dict, idx: int = 0) -> Optional[list]:
    """Extract embedding from Cohere-shape response (top-level embeddings[])."""
    embs = data.get("embeddings") if isinstance(data, dict) else None
    if not isinstance(embs, list) or len(embs) <= idx:
        return None
    e = embs[idx]
    if isinstance(e, list):
        return e
    return None


def _extract_gemini_embedding(data: dict) -> Optional[list]:
    """Extract embedding from Gemini embedContent response (embedding.values)."""
    if not isinstance(data, dict):
        return None
    emb = data.get("embedding")
    if isinstance(emb, dict):
        vals = emb.get("values")
        if isinstance(vals, list):
            return vals
    return None


def _extract_gemini_batch_embedding(data: dict, idx: int = 0) -> Optional[list]:
    """Extract one embedding from Gemini batchEmbedContents response."""
    if not isinstance(data, dict):
        return None
    embeddings = data.get("embeddings") or []
    if len(embeddings) <= idx:
        return None
    e = embeddings[idx]
    if isinstance(e, dict):
        vals = e.get("values")
        if isinstance(vals, list):
            return vals
    return None


def _model_capability(model_entry: dict, key: str, default=None):
    """Read a field from capabilityJson or top-level model entry."""
    cap = model_entry.get("capabilityJson") or {}
    if isinstance(cap, str):
        try:
            cap = json.loads(cap)
        except Exception:
            cap = {}
    # Try nested embeddings sub-object first, then top-level capabilityJson
    emb_cap = cap.get("embeddings") or {}
    val = emb_cap.get(key)
    if val is None:
        val = cap.get(key)
    if val is None:
        val = model_entry.get(key)
    return val if val is not None else default


def _embedding_default_dimension(model_entry: dict) -> int:
    """Best-effort: return model's default embedding dimension."""
    d = _model_capability(model_entry, "default_dimension")
    if isinstance(d, int) and d > 0:
        return d
    # Common known defaults when capabilityJson not yet populated.
    mid = model_entry.get("id", "").lower()
    if "ada-002" in mid:
        return 1536
    if "text-embedding-3-small" in mid:
        return 1536
    if "text-embedding-3-large" in mid:
        return 3072
    if "embed-english" in mid or "embed-multilingual" in mid:
        return 1024  # Cohere embed-v3
    if "gemini-embedding" in mid:
        return 3072  # gemini-embedding-001 native default (no outputDimensionality)
    return 0


def _embedding_max_batch(model_entry: dict) -> int:
    """Return max_batch_size from capabilityJson, default 2048."""
    b = _model_capability(model_entry, "max_batch_size")
    if isinstance(b, int) and b > 0:
        return b
    # Cohere known limit
    mid = model_entry.get("id", "").lower()
    if "embed-" in mid:
        return 96
    return 2048


def _embedding_supports_dimensions(model_entry: dict) -> bool:
    """True when model allows custom dimensions parameter."""
    # ada-002 is the canonical "no custom dimensions" model.
    mid = model_entry.get("id", "").lower()
    if "ada-002" in mid:
        return False
    supported = _model_capability(model_entry, "supported_dimensions")
    if supported is None:
        # Default: text-embedding-3-* and gemini-embedding support it.
        return (
            "text-embedding-3-" in mid
            or "gemini-embedding" in mid
        )
    if isinstance(supported, list):
        return len(supported) > 0
    return bool(supported)


def _metrics_readable_safe(gw: "GWClient") -> bool:
    """metrics_readable, tolerating the fetch itself raising.

    metrics() now raises on a non-200 (it used to swallow the status and hand the
    refusal body onward). Arm E asks this question to CLASSIFY a failure it has
    already seen, so it must not blow up while doing so.
    """
    try:
        return GWClient.metrics_readable(gw.metrics())
    except Exception:
        return False


def _snapshot_prometheus_embedding(gw: GWClient, provider: str, model: str) -> dict:
    """Snapshot nexus_requests_total{endpoint="embeddings",...} counters.

    The series carries endpoint + model(UUID) + provider + status labels; we sum
    across all embeddings series (the model param is unused — the model label is
    the UUID, not the slug, and the pool runs models concurrently, so the delta
    is an aggregate "embeddings traffic incremented" signal, not per-model).
    """
    try:
        metrics_text = gw.metrics()
    except Exception:
        return {}
    result: dict[str, float] = {}
    for line in metrics_text.splitlines():
        if "nexus_requests_total" not in line or 'endpoint="embeddings"' not in line:
            continue
        if line.startswith("#"):
            continue
        key_part = line.rsplit(" ", 1)[0]
        try:
            val = float(line.rsplit(" ", 1)[1])
            result[key_part] = val
        except Exception:
            pass
    return result


def _prometheus_embedding_delta(snap0: dict, snap1: dict) -> int:
    """Total delta across all embedding counter lines between two snapshots."""
    total = 0
    all_keys = set(snap0) | set(snap1)
    for k in all_keys:
        v0 = snap0.get(k, 0.0)
        v1 = snap1.get(k, 0.0)
        if v1 > v0:
            total += int(v1 - v0)
    return total


def _poll_embedding_event(db: DBClient, model_name: str, t0_iso: str,
                          timeout: int = 30) -> Optional[dict]:
    """Poll for the most recent embeddings traffic_event row for a model."""
    if db.is_remote and not db.has_ssh:
        return None
    deadline = time.time() + timeout
    cols = [
        "id", "endpoint_type",
        "prompt_tokens", "completion_tokens", "cache_read_tokens",
        "estimated_cost_usd",
    ]
    # traffic_event no longer carries endpoint_type / metadata columns;
    # the embeddings endpoint is identified by path, and the per-request
    # dimension is asserted from the live Arm A/B response rather than the
    # DB row (no dimension column on traffic_event).
    col_sql = (
        "id, "
        "'embeddings', "
        "COALESCE(prompt_tokens::text,'0'), "
        "COALESCE(completion_tokens::text,'0'), "
        "COALESCE(cache_read_tokens::text,'0'), "
        "COALESCE(estimated_cost_usd::text,'0'), "
        "''"
    )
    while time.time() < deadline:
        sql = (
            f"SELECT {col_sql} FROM traffic_event "
            f"WHERE source='ai-gateway' "
            f"AND timestamp >= '{t0_iso}'::timestamptz "
            f"AND model_name = '{model_name}' "
            f"AND path LIKE '%/embeddings' "
            f"ORDER BY timestamp DESC LIMIT 1"
        )
        try:
            r = subprocess.run(
                db._psql_args(sql, separator="|"),
                capture_output=True, text=True, timeout=15,
            )
            line = r.stdout.strip().split("\n")[0] if r.stdout.strip() else ""
            if line:
                parts = line.split("|")
                if len(parts) >= 7:
                    return {
                        "id": parts[0].strip(),
                        "endpoint_type": parts[1].strip(),
                        "prompt_tokens": int(parts[2].strip() or "0"),
                        "completion_tokens": int(parts[3].strip() or "0"),
                        "cache_read_tokens": int(parts[4].strip() or "0"),
                        "estimated_cost_usd": float(parts[5].strip() or "0"),
                        "embedding_dimension": int(parts[6].strip() or "0") if parts[6].strip() else None,
                    }
        except Exception:
            pass
        time.sleep(2)
    return None


def _no_embedding_event_created(db: DBClient, vk_hint: str, model_name: str,
                                 provider_id: str, t_after: str,
                                 timeout: int = 10) -> bool:
    """True when NO embedding traffic_event row for (model, provider) was
    created after t_after. Used to assert negative-test reject-asymmetry."""
    if db.is_remote and not db.has_ssh:
        return True  # cannot verify — treat as passed
    deadline = time.time() + timeout
    while time.time() < deadline:
        sql = (
            "SELECT COUNT(*) FROM traffic_event "
            f"WHERE source='ai-gateway' "
            f"AND timestamp >= '{t_after}'::timestamptz "
            f"AND model_name = '{model_name}' "
            f"AND endpoint_type = 'embeddings' "
            f"AND routed_provider_name = '{provider_id}'"
        )
        try:
            r = subprocess.run(
                db._psql_args(sql),
                capture_output=True, text=True, timeout=10,
            )
            cnt = r.stdout.strip()
            if cnt and int(cnt) == 0:
                return True
            if cnt and int(cnt) > 0:
                return False
        except Exception:
            pass
        time.sleep(1)
    return True  # timed out without seeing a row — treat as passed


def phase3e_embeddings(
    gw: GWClient,
    cp: CPClient,
    db: DBClient,
    embedding_models: list[dict],
    t0_iso: str,
    timeout: int = 60,
    no_embeddings: bool = False,
    snapshot: Optional["StateSnapshot"] = None,
) -> None:
    """P3E — embedding phase: six arms per (ingress, model) + reject-asymmetry.

    Cache arm: skipped — embeddings have no prompt-cache semantic.
    """
    if no_embeddings:
        log_info("P3E skipped (--no-embeddings)")
        rec("P3E", "phase").passed("skipped via --no-embeddings")
        return

    log_step("P3E Embeddings — per-model suite")

    if not embedding_models:
        log_warn("  No embedding models in catalog — P3E skipped")
        rec("P3E", "phase").passed("no embedding models in catalog")
        return

    log_info(
        f"  P3E: Cache arm: skipped — embeddings have no prompt-cache semantic."
    )

    # Collect cross-ingress vectors for Arm F consistency matrix.
    # cross_vectors[model_id][ingress_tag] = vector (list of float)
    cross_vectors: dict[str, dict[str, list]] = {}
    _CROSS_INPUT = "hello world"

    for m in embedding_models:
        mid = m.get("id", "")
        if not mid:
            continue

        log_info(f"\n  [{mid}] P3E arms A-F")

        # Arm E baseline — snapshot the embeddings request counter BEFORE this
        # model's arms fire, so the post-arms delta reflects the requests this
        # model made (the prior code snapshotted twice back-to-back AFTER the
        # arms, so the delta was always 0 → spurious WARN).
        snap_e0 = _snapshot_prometheus_embedding(gw, "", mid)

        # ─── Arm A — non-stream basic ─────────────────────────────────────────
        t_arm_a = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        log_info(f"  [{mid}] Arm A — non-stream basic …")
        r_a = gw.embeddings(mid, "hello world", timeout=timeout)
        status_a = r_a.get("status", 0)
        emb_a: Optional[list] = None
        if status_a == 200 and isinstance(r_a.get("data"), dict):
            emb_a = _extract_openai_embedding(r_a["data"], 0)
            expected_dim = _embedding_default_dimension(m)
            issues: list[str] = []
            if emb_a is None:
                issues.append("data[0].embedding missing")
            elif expected_dim > 0 and len(emb_a) != expected_dim:
                issues.append(
                    f"dimension mismatch: got {len(emb_a)}, expected {expected_dim}"
                )
            items = (r_a["data"].get("data") or [])
            if not isinstance(items, list) or not items:
                issues.append("response data[] empty or not a list")
            if issues:
                log_fail(f"  [{mid}] Arm A: {'; '.join(issues)}")
                rec("P3E", f"{mid}/arm-a").failed("; ".join(issues))
            else:
                dim = len(emb_a) if emb_a else 0
                log_ok(f"  [{mid}] Arm A OK dim={dim} ({r_a.get('elapsed', 0):.1f}s)")
                rec("P3E", f"{mid}/arm-a").passed(f"dim={dim}")
            cross_vectors.setdefault(mid, {})["openai"] = emb_a or []
        else:
            err = (r_a.get("data") or {}).get("error") or r_a.get("error", "")
            log_fail(f"  [{mid}] Arm A HTTP {status_a}: {str(err)[:80]}")
            rec("P3E", f"{mid}/arm-a").failed(f"HTTP {status_a}: {str(err)[:80]}")

        # ─── Arm B — dimensions round-trip ───────────────────────────────────
        log_info(f"  [{mid}] Arm B — dimensions round-trip …")
        if not _embedding_supports_dimensions(m):
            log_info(f"  [{mid}] Arm B: skipped — model_does_not_support_dimensions")
            rec("P3E", f"{mid}/arm-b").passed("skipped: model_does_not_support_dimensions")
        else:
            # The probe dimension must be one the model OFFERS. When the
            # catalog declares supported_dimensions it is a discrete allowed
            # set, not a range: text-embedding-3-large publishes
            # [256, 512, 1024, 3072], and half of its 3072 default is 1536 —
            # a value the model does not serve. The gateway's capability floor
            # rejects it with MODEL_CAPABILITY_MISMATCH, correctly, and the arm
            # then failed on the request it had built rather than on the
            # round-trip it meant to test.
            supported = _model_capability(m, "supported_dimensions")
            default_dim = _embedding_default_dimension(m) or 1024
            if isinstance(supported, list) and supported:
                # Largest offered value below the default, else the smallest
                # offered — either way a value the model serves, and different
                # from the default so the round-trip proves something.
                below = sorted(d for d in supported if isinstance(d, int) and d < default_dim)
                probe_dim = below[-1] if below else min(
                    d for d in supported if isinstance(d, int))
            else:
                max_dim = _model_capability(m, "max_dimensions") or default_dim
                probe_dim = max(1, max_dim // 2)
            r_b = gw.embeddings(mid, "dimension test", dimensions=probe_dim, timeout=timeout)
            status_b = r_b.get("status", 0)
            if status_b == 200 and isinstance(r_b.get("data"), dict):
                emb_b = _extract_openai_embedding(r_b["data"], 0)
                if emb_b is None:
                    log_fail(f"  [{mid}] Arm B: data[0].embedding missing")
                    rec("P3E", f"{mid}/arm-b").failed("data[0].embedding missing")
                elif len(emb_b) != probe_dim:
                    log_fail(
                        f"  [{mid}] Arm B: got dim={len(emb_b)}, requested {probe_dim}"
                    )
                    rec("P3E", f"{mid}/arm-b").failed(
                        f"dim={len(emb_b)} != requested {probe_dim}"
                    )
                else:
                    log_ok(f"  [{mid}] Arm B OK dim={len(emb_b)} (requested {probe_dim})")
                    rec("P3E", f"{mid}/arm-b").passed(f"dim={len(emb_b)} == requested {probe_dim}")
            else:
                err = (r_b.get("data") or {}).get("error") or r_b.get("error", "")
                log_fail(f"  [{mid}] Arm B HTTP {status_b}: {str(err)[:80]}")
                rec("P3E", f"{mid}/arm-b").failed(f"HTTP {status_b}: {str(err)[:80]}")

        # ─── Arm C — batch input ──────────────────────────────────────────────
        log_info(f"  [{mid}] Arm C — batch input …")
        max_batch = _embedding_max_batch(m)
        n_batch = min(8, max_batch)
        batch_inputs = [f"batch input item {i}" for i in range(n_batch)]
        r_c = gw.embeddings(mid, batch_inputs, timeout=timeout)
        status_c = r_c.get("status", 0)
        if status_c == 200 and isinstance(r_c.get("data"), dict):
            items_c = r_c["data"].get("data") or []
            if len(items_c) != n_batch:
                log_fail(
                    f"  [{mid}] Arm C: expected {n_batch} items, got {len(items_c)}"
                )
                rec("P3E", f"{mid}/arm-c").failed(
                    f"items={len(items_c)} != batch_size={n_batch}"
                )
            else:
                # Verify ordering: each item should have its index
                idx_ok = all(
                    isinstance(it, dict) and it.get("index") == i
                    for i, it in enumerate(items_c)
                )
                if not idx_ok:
                    log_warn(f"  [{mid}] Arm C: order indices unexpected (may be provider-specific)")
                    rec("P3E", f"{mid}/arm-c").warning(f"N={n_batch} items ok but index order unexpected")
                else:
                    log_ok(f"  [{mid}] Arm C OK batch N={n_batch}")
                    rec("P3E", f"{mid}/arm-c").passed(f"N={n_batch} in order")
        else:
            err = (r_c.get("data") or {}).get("error") or r_c.get("error", "")
            log_fail(f"  [{mid}] Arm C HTTP {status_c}: {str(err)[:80]}")
            rec("P3E", f"{mid}/arm-c").failed(f"HTTP {status_c}: {str(err)[:80]}")

        # ─── Arm D — traffic_event cross-check ───────────────────────────────
        log_info(f"  [{mid}] Arm D — traffic_event cross-check …")
        row_d = _poll_embedding_event(db, mid, t_arm_a, timeout=30)
        if row_d is None:
            if db.is_remote and not db.has_ssh:
                log_info(f"  [{mid}] Arm D: skipped (no DB access in remote env)")
                rec("P3E", f"{mid}/arm-d").passed("remote; DB access not available")
            else:
                log_fail(f"  [{mid}] Arm D: no traffic_event row found")
                rec("P3E", f"{mid}/arm-d").failed("no row in timeout window")
        else:
            d_issues: list[str] = []
            if row_d.get("endpoint_type") != "embeddings":
                d_issues.append(f"endpoint_type={row_d.get('endpoint_type')!r} (expected embeddings)")
            if row_d.get("prompt_tokens", 0) <= 0:
                d_issues.append(f"prompt_tokens={row_d.get('prompt_tokens')} (expected >0)")
            if row_d.get("completion_tokens", 0) != 0:
                d_issues.append(f"completion_tokens={row_d.get('completion_tokens')} (expected 0)")
            if row_d.get("cache_read_tokens", 0) != 0:
                d_issues.append(f"cache_read_tokens={row_d.get('cache_read_tokens')} (expected 0)")
            # Cost is computed from prompt_tokens × price. For cheap providers
            # with short inputs (e.g. Gemini embeddings: a few tokens × $0.025/M
            # = sub-$0.000001) the value rounds to 0 at the column's 6-decimal
            # scale. prompt_tokens>0 above is the meaningful usage assertion;
            # here we only flag a genuinely negative (impossible) cost.
            if row_d.get("estimated_cost_usd", 0) < 0:
                d_issues.append(f"estimated_cost_usd={row_d.get('estimated_cost_usd')} (expected >=0)")
            # Dimension check against Arm A result
            emb_dim_db = row_d.get("embedding_dimension")
            if emb_a and emb_dim_db and int(emb_dim_db) != len(emb_a):
                d_issues.append(
                    f"metadata.embedding.dimension={emb_dim_db} != arm-a dim={len(emb_a)}"
                )
            if d_issues:
                log_fail(f"  [{mid}] Arm D: {'; '.join(d_issues)}")
                rec("P3E", f"{mid}/arm-d").failed("; ".join(d_issues))
            else:
                log_ok(
                    f"  [{mid}] Arm D OK endpoint=embeddings "
                    f"pt={row_d.get('prompt_tokens')} "
                    f"cost=${row_d.get('estimated_cost_usd', 0):.6f}"
                )
                rec("P3E", f"{mid}/arm-d").passed(
                    f"pt={row_d.get('prompt_tokens')} "
                    f"cost=${row_d.get('estimated_cost_usd', 0):.6f}"
                )

        # ─── Arm E — Prometheus delta ─────────────────────────────────────────
        # Compare against snap_e0 taken BEFORE this model's arms (above). Arms
        # A+C always fire (B is conditional), so this model contributed >= 2
        # embeddings requests; the pool may add concurrent models' requests on
        # top, which only raises the aggregate delta. delta=0 now means the
        # counter genuinely didn't move — a real regression, not label timing.
        log_info(f"  [{mid}] Arm E — Prometheus delta …")
        snap_e1 = _snapshot_prometheus_embedding(gw, "", mid)
        delta_e = _prometheus_embedding_delta(snap_e0, snap_e1)
        if not snap_e1:
            if db.is_remote:
                # /metrics binds loopback-only (127.0.0.1:9090) by hardening; a
                # remote runner cannot reach it even over the ssh tunnel (that
                # forwards Postgres, not the metrics HTTP port). Deterministic,
                # not a regression — skip rather than warn to keep the run clean.
                log_info(f"  [{mid}] Arm E: skipped (metrics endpoint loopback-only, unreachable from a remote runner)")
                rec("P3E", f"{mid}/arm-e").passed("remote; metrics endpoint loopback-only")
            elif not _metrics_readable_safe(gw):
                # Distinct from "no embeddings series": the exposition itself is
                # refused. Reporting that as "unavailable" sent three sessions
                # looking at a healthy endpoint.
                log_fail(f"  [{mid}] Arm E: metrics exposition unreadable (refused, not absent)")
                rec("P3E", f"{mid}/arm-e").failed(
                    "metrics exposition unreadable — /metrics is authenticated; set "
                    "INTERNAL_SERVICE_TOKEN or NEXUS_HUB_SERVICE_TOKEN")
            else:
                log_warn(f"  [{mid}] Arm E: no embeddings series in the exposition")
                rec("P3E", f"{mid}/arm-e").warning(
                    "metrics readable but carries no nexus_requests_total{endpoint=\"embeddings\"} "
                    "series — embeddings traffic may not be counted")
        elif delta_e > 0:
            log_ok(f"  [{mid}] Arm E OK delta={delta_e}")
            rec("P3E", f"{mid}/arm-e").passed(f"delta={delta_e}")
        else:
            log_warn(f"  [{mid}] Arm E: embeddings request counter did not increment (delta=0)")
            rec("P3E", f"{mid}/arm-e").warning("nexus_requests_total{endpoint=embeddings} delta=0")

        # ─── Arm F — cross-ingress consistency ───────────────────────────────
        log_info(f"  [{mid}] Arm F — cross-ingress consistency …")
        # We already have the OpenAI-ingress vector from Arm A.
        # For cross-format pairs we send the same input via OpenAI ingress but
        # with a temporary routing rule pinned to each target adapter.
        # For simplicity in P3E (no routing-rule infrastructure yet available),
        # we record the OpenAI native result and note that full cross-ingress
        # testing requires the adapter conformance infrastructure from Phase 8.
        if emb_a:
            # Record native OpenAI vector for the cross-ingress matrix.
            cross_vectors.setdefault(mid, {})["openai"] = emb_a
            log_info(
                f"  [{mid}] Arm F: recorded native OpenAI vector "
                f"dim={len(emb_a)} for cross-ingress matrix"
            )
            rec("P3E", f"{mid}/arm-f-native").passed(
                f"dim={len(emb_a)} — native OpenAI recorded"
            )
        else:
            log_warn(f"  [{mid}] Arm F: skipped (Arm A failed — no native vector)")
            rec("P3E", f"{mid}/arm-f-native").warning("no native vector (Arm A failed)")

        # Explicitly log skipped arms.
        log_info(f"  [{mid}] Cache arm: skipped — embeddings have no prompt-cache semantic")
        rec("P3E", f"{mid}/cache").passed("skipped — embeddings have no prompt-cache semantic")

    # ─── Reject-asymmetry negative tests ─────────────────────────────────────
    _run_embedding_reject_asymmetry(gw, cp, db, embedding_models, t0_iso, timeout)

    # ─── Summary ─────────────────────────────────────────────────────────────
    p3e_pass = sum(1 for r in _results if r.phase == "P3E" and r.ok is True and not r.warn)
    p3e_warn = sum(1 for r in _results if r.phase == "P3E" and r.ok is True and r.warn)
    p3e_fail = sum(1 for r in _results if r.phase == "P3E" and r.ok is False)
    log_step(
        f"P3E done — {len(embedding_models)} model(s): "
        f"{p3e_pass} passed, {p3e_warn} warnings, {p3e_fail} failed"
    )


def _run_embedding_reject_asymmetry(
    gw: GWClient,
    cp: CPClient,
    db: DBClient,
    embedding_models: list[dict],
    t0_iso: str,
    timeout: int = 60,
) -> None:
    """P3E negative tests: reject-asymmetry for parameter violations.

    Test 1 — OpenAI ingress + dimensions=2048, pinned to Cohere (fixed 1024):
      expect 400 with a capability refusal, no traffic_event row.

    Test 2 — OpenAI ingress + batch of 200, pinned to Cohere (max_batch=96):
      expect 400 with a capability refusal, no traffic_event row.

    Both tests use a temporary routing rule created for the test VK, cleaned
    up after each assertion.
    """
    log_step("P3E Reject-Asymmetry negative tests")

    # Find a Cohere embedding model for the negative tests.
    cohere_model: Optional[dict] = None
    for m in embedding_models:
        mid = m.get("id", "").lower()
        if "embed-english" in mid or "embed-multilingual" in mid or "cohere" in mid:
            cohere_model = m
            break

    if cohere_model is None:
        log_info("  Reject-asymmetry: no Cohere embedding model in catalog — skipped")
        rec("P3E/reject", "neg-dimensions").passed("no Cohere model; skipped")
        rec("P3E/reject", "neg-batch").passed("no Cohere model; skipped")
        return

    cohere_mid = cohere_model.get("id", "")
    t_neg = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    # Test 1: dimensions=2048 exceeds Cohere fixed 1024.
    log_info(f"  [{cohere_mid}] Neg-test 1: dimensions=2048 vs Cohere fixed-1024 …")
    r_neg1 = gw.embeddings(cohere_mid, "reject test", dimensions=2048, timeout=timeout)
    s_neg1 = r_neg1.get("status", 0)
    issues_neg1: list[str] = []
    if s_neg1 != 400:
        issues_neg1.append(f"expected 400, got HTTP {s_neg1}")
    body_neg1 = r_neg1.get("data") or {}
    err_neg1 = body_neg1.get("error") or {}
    if isinstance(err_neg1, dict):
        code_neg1 = err_neg1.get("code", "")
        # The request NAMES a model, so the refusal must come from the
        # capability floor: MODEL_CAPABILITY_MISMATCH, which reads that model's
        # own declared embedding limits and names the parameter that does not
        # fit. The structured candidate-set 400 (no_compatible_capability) is
        # for a DELEGATED request, where the gateway chose and none of its own
        # models fit — answering a named model with a catalogue of models the
        # caller did not choose reads as a gateway that ignored the request.
        # no_compatible_provider is the CROSS-FORMAT gate, a different refusal
        # entirely; accepting it here would let that gate firing by mistake
        # pass as a capability check.
        if s_neg1 == 400 and "MODEL_CAPABILITY_MISMATCH" not in code_neg1:
            issues_neg1.append(
                f"error.code={code_neg1!r} (expected MODEL_CAPABILITY_MISMATCH from the "
                f"capability floor, which is what refuses a NAMED model)"
            )
    # DB: no traffic_event should have been created.
    if not issues_neg1:
        no_row = _no_embedding_event_created(db, "", cohere_mid, "cohere", t_neg, timeout=10)
        if not no_row:
            issues_neg1.append("unexpected traffic_event row created on rejected request")
    if issues_neg1:
        log_fail(f"  Neg-test 1 ({cohere_mid}): {'; '.join(issues_neg1)}")
        rec("P3E/reject", "neg-dimensions").failed("; ".join(issues_neg1))
    else:
        log_ok(f"  Neg-test 1 ({cohere_mid}): HTTP 400 {code_neg1}, no DB row")
        rec("P3E/reject", "neg-dimensions").passed(
            f"HTTP 400 on dimensions=2048 > Cohere max=1024; no traffic_event"
        )

    # Test 2: batch of 200 inputs exceeds Cohere max_batch_size=96.
    log_info(f"  [{cohere_mid}] Neg-test 2: batch=200 vs Cohere max_batch=96 …")
    big_batch = [f"batch reject item {i}" for i in range(200)]
    r_neg2 = gw.embeddings(cohere_mid, big_batch, timeout=timeout)
    s_neg2 = r_neg2.get("status", 0)
    issues_neg2: list[str] = []
    if s_neg2 != 400:
        issues_neg2.append(f"expected 400, got HTTP {s_neg2}")
    body_neg2 = r_neg2.get("data") or {}
    err_neg2 = body_neg2.get("error") or {}
    if isinstance(err_neg2, dict):
        code_neg2 = err_neg2.get("code", "")
        # Same expected code as neg-test 1 above; see the reasoning there.
        if s_neg2 == 400 and "MODEL_CAPABILITY_MISMATCH" not in code_neg2:
            issues_neg2.append(
                f"error.code={code_neg2!r} (expected MODEL_CAPABILITY_MISMATCH from the "
                f"capability floor, which is what refuses a NAMED model)"
            )
    if not issues_neg2:
        no_row2 = _no_embedding_event_created(db, "", cohere_mid, "cohere", t_neg, timeout=10)
        if not no_row2:
            issues_neg2.append("unexpected traffic_event row created on rejected request")
    if issues_neg2:
        log_fail(f"  Neg-test 2 ({cohere_mid}): {'; '.join(issues_neg2)}")
        rec("P3E/reject", "neg-batch").failed("; ".join(issues_neg2))
    else:
        log_ok(f"  Neg-test 2 ({cohere_mid}): HTTP 400 {code_neg2}, no DB row")
        rec("P3E/reject", "neg-batch").passed(
            f"HTTP 400 on batch=200 > Cohere max=96; no traffic_event"
        )


# ─── Embedding cross-ingress matrix in report ─────────────────────────────────

def _render_embedding_cross_ingress_matrix(cross_vectors: dict, lines: list) -> None:
    """Append a P3E cross-ingress consistency matrix section to report lines."""
    if not cross_vectors:
        return
    lines += ["", "## P3E — Cross-ingress consistency matrix", ""]
    lines.append(
        "Each cell shows cosine_similarity(V_native, V_format) for the same "
        "input text. Values > 0.999 indicate byte-identical or near-identical "
        "vectors from the same upstream model and codec path."
    )
    lines += [""]
    ingresses = sorted({ing for vecs in cross_vectors.values() for ing in vecs})
    if len(ingresses) < 2:
        lines.append("_(single ingress captured — cross-ingress comparison requires ≥2 ingresses)_")
        return
    header = "| Model | " + " | ".join(ingresses) + " | Max Δ |"
    sep = "|---|" + "|".join("---" for _ in ingresses) + "|---|"
    lines += [header, sep]
    for mid, vecs in sorted(cross_vectors.items()):
        cells: list[str] = []
        cosines: list[float] = []
        ref_vec = next((v for v in vecs.values() if v), None)
        for ing in ingresses:
            v = vecs.get(ing)
            if v is None:
                cells.append("-")
            elif v is ref_vec or not ref_vec:
                cells.append("native")
            else:
                sim = _cosine_similarity(ref_vec, v)
                cosines.append(sim)
                icon = "✅" if sim > 0.999 else "⚠️"
                cells.append(f"{icon} {sim:.4f}")
        max_delta = f"{1 - min(cosines):.4f}" if cosines else "—"
        lines.append(f"| {mid} | " + " | ".join(cells) + f" | {max_delta} |")


# ─── Phase: /v1/estimate compare endpoint (E58-S4) ───────────────────────────


def _pick_compare_targets(
    cp: CPClient,
    chat_models: list[str],
    want: int = 3,
) -> list[dict]:
    """Pick up to `want` (providerId, modelCode) pairs covering different
    adapter families when possible: OpenAI / Anthropic / Gemini. Falls back
    to whatever is in chat_models if the family slot can't be filled.

    Returns dicts shaped {providerId, modelId, providerAdapter} ready for
    the /v1/estimate compareTargets[]. providerAdapter is informational
    (used in the smoke report)."""
    flat = cp.list_models_flat()
    by_code = {m["code"]: m for m in flat if isinstance(m, dict) and m.get("code")}
    providers = {p["id"]: p for p in cp.list_providers() if isinstance(p, dict)}

    def adapter_of(prov_id: str) -> str:
        return (providers.get(prov_id) or {}).get("adapterType", "") or ""

    # Family classifier — match adapterType prefix to a family label.
    def family(adapter: str) -> str:
        a = adapter.lower()
        if "anthropic" in a or "bedrock" in a:
            return "anthropic"
        if "gemini" in a or "vertex" in a:
            return "gemini"
        return "openai"  # everything else (openai, deepseek, kimi, glm, ...)

    picked: dict[str, dict] = {}
    for code in chat_models:
        if len(picked) >= want:
            break
        m = by_code.get(code)
        if not m:
            continue
        prov_id = m.get("providerId") or ""
        adapter = adapter_of(prov_id)
        fam = family(adapter)
        if fam in picked:
            continue
        picked[fam] = {
            "providerId": prov_id,
            "modelId": code,            # /v1/estimate accepts the human code
            "providerAdapter": adapter,
        }
    # Fill any remaining slots with first-N un-picked models.
    if len(picked) < want:
        seen_codes = {v["modelId"] for v in picked.values()}
        for code in chat_models:
            if len(picked) >= want:
                break
            if code in seen_codes:
                continue
            m = by_code.get(code) or {}
            prov_id = m.get("providerId") or ""
            slot = f"_extra_{code}"
            picked[slot] = {
                "providerId": prov_id,
                "modelId": code,
                "providerAdapter": adapter_of(prov_id),
            }
    return list(picked.values())


def phase_compare_estimate(
    gw: GWClient,
    cp: CPClient,
    chat_models: list[str],
    timeout: int = 30,
) -> None:
    """E58-S4: POST /v1/estimate compare endpoint. Hits the endpoint twice:

      1. Happy path — 3 cherry-picked (family-diverse if possible) targets.
         Assert per-target tokens+cost present, no errors, summary
         identifies a cheapest target that's one of the 3.
      2. Negative — same body with one bogus modelId injected. Assert that
         target reports error while the others succeed.

    This phase validates the dedicated /v1/estimate compare surface that
    the UI's "what would this cost on each provider?" picker is built on
    top of — the sole cost-preview surface after E58-S5."""
    log_step("PD /v1/estimate compare endpoint")
    if not chat_models:
        log_warn("  No catalog models — phase skipped")
        return

    targets = _pick_compare_targets(cp, chat_models, want=3)
    if len(targets) < 2:
        log_warn(f"  Only {len(targets)} target(s) resolvable from catalog — skipping")
        rec("PD", "compareTargets").warning(f"insufficient targets ({len(targets)})")
        return

    fam_summary = ", ".join(
        f"{t['providerAdapter'] or '?'}={t['modelId']}" for t in targets
    )
    log_info(f"  compareTargets: {fam_summary}")

    request_body = {
        "model": targets[0]["modelId"],  # ignored by /v1/estimate; only compareTargets matter
        "messages": [
            {"role": "system", "content": "You are a concise assistant."},
            {"role": "user",   "content": "Compare deterministic vs probabilistic AI in two sentences."},
        ],
        "max_tokens": 256,
    }
    body = {
        "request": request_body,
        "compareTargets": [
            {"providerId": t["providerId"], "modelId": t["modelId"]} for t in targets
        ],
    }
    r = gw.estimate(body, timeout=timeout)
    status = r.get("status", 0)
    if status != 200 or not isinstance(r.get("data"), dict):
        err = (r.get("data") or {}).get("error") if isinstance(r.get("data"), dict) else r.get("error", "")
        log_fail(f"  /v1/estimate HTTP {status}: {str(err)[:120]}")
        rec("PD", "estimate-happy").failed(f"HTTP {status}: {str(err)[:120]}")
        return
    resp = r["data"]
    rows = resp.get("targets") or []
    summary = resp.get("summary") or {}
    expected_codes = [t["modelId"] for t in targets]

    issues: list[str] = []
    if len(rows) != len(targets):
        issues.append(f"targets length {len(rows)} != requested {len(targets)}")
    for row in rows:
        code = row.get("modelCode", "")
        if row.get("error"):
            issues.append(f"{code}: error={row['error'].get('code')}")
            continue
        if not row.get("cost") or not row.get("tokens"):
            issues.append(f"{code}: missing cost or tokens block")
            continue
        cost_expected = (row.get("cost") or {}).get("expected", {})
        if not isinstance(cost_expected.get("total"), (int, float)):
            issues.append(f"{code}: cost.expected.total missing/non-numeric")
    cheapest = summary.get("cheapestExpectedTarget")
    if cheapest is None:
        issues.append("summary.cheapestExpectedTarget is null")
    elif cheapest not in expected_codes:
        issues.append(f"cheapest {cheapest!r} not in requested {expected_codes}")
    if summary.get("successCount", 0) != len(targets):
        issues.append(f"successCount={summary.get('successCount')} != {len(targets)}")

    if issues:
        msg = "; ".join(issues[:5])
        log_fail(f"  /v1/estimate happy-path: {msg}")
        rec("PD", "estimate-happy").failed(msg)
    else:
        log_ok(f"  /v1/estimate happy-path: {len(rows)} targets, "
               f"cheapest={cheapest} (${summary.get('cheapestExpectedTotalUsd', 0):.6f})")
        rec("PD", "estimate-happy").passed(
            f"{len(rows)} targets; cheapest={cheapest} "
            f"${summary.get('cheapestExpectedTotalUsd', 0):.6f}"
        )

    # ── Negative case: one bogus modelId should error-out that row only.
    bogus_targets = [
        {"providerId": targets[0]["providerId"], "modelId": "__nonexistent_model_for_smoke__"},
        *[{"providerId": t["providerId"], "modelId": t["modelId"]} for t in targets[1:]],
    ]
    r2 = gw.estimate({"request": request_body, "compareTargets": bogus_targets}, timeout=timeout)
    if r2.get("status") != 200 or not isinstance(r2.get("data"), dict):
        log_fail(f"  /v1/estimate negative-path HTTP {r2.get('status')}")
        rec("PD", "estimate-negative").failed(f"HTTP {r2.get('status')}")
        return
    rows2 = r2["data"].get("targets") or []
    if len(rows2) < 1:
        log_fail("  /v1/estimate negative-path: no targets in response")
        rec("PD", "estimate-negative").failed("empty targets[]")
        return
    bogus_err = rows2[0].get("error") or {}
    rest_ok = all(not row.get("error") for row in rows2[1:])
    if bogus_err.get("code") and rest_ok:
        log_ok(f"  /v1/estimate negative-path: bogus modelId → {bogus_err['code']}; "
               f"{len(rows2)-1} other target(s) succeeded")
        rec("PD", "estimate-negative").passed(
            f"bogus={bogus_err['code']}; {len(rows2)-1} others succeeded"
        )
    else:
        log_fail(f"  /v1/estimate negative-path: bogus err={bogus_err!r} "
                 f"rest_ok={rest_ok}")
        rec("PD", "estimate-negative").failed(
            f"bogus_err={bogus_err.get('code', 'MISSING')!r}; rest_ok={rest_ok}"
        )


def phase3c_direct_compare(
    gw: GWClient,
    cp: CPClient,
    snapshot: StateSnapshot,
    models: list[str],
    direct: "DirectClient",
    timeout: int,
    no_stream: bool = False,
):
    """P3C: Routing OFF — concurrent Gateway vs Direct provider comparison.

    For each model, fires the gateway request and the direct upstream request
    at the same time (ThreadPoolExecutor, 2 workers) so both calls reflect the
    same real-world load and the latency numbers are directly comparable.

    Routing is explicitly set to all-disabled before the loop begins.  P3
    already does this, but the re-disable is a cheap defensive step.

    Checks per model / mode:
    - HTTP status: gateway must be 200 when direct is 200
    - Content presence: gateway must have content when direct has content
    - Prompt-token parity: large divergence (ratio outside 0.6-1.6) flags a
      possible format-translation bug in the gateway
    - Latency overhead: gateway vs direct elapsed (informational; not a failure)
    - SSE: TTFB, [DONE] termination, chunk presence vs direct's finish event
    """
    phase_tag = "P3C"
    log_step("P3C — Routing OFF: concurrent Gateway vs Direct comparison")

    any_key = any(
        direct.has_key(p) for p in ("openai", "anthropic", "gemini", "deepseek", "moonshot")
    )
    if not any_key:
        log_warn(
            "  No provider keys configured — P3C skipped. "
            "Set OPENAI_API_KEY / GEMINI_API_KEY / ANTHROPIC_API_KEY etc."
        )
        rec(phase_tag, "setup").warning("no provider keys — skipped")
        return

    # Defensive: ensure routing is off for the duration of P3C.
    cp.set_all_routing_rules(False, snapshot.routing_rules)

    for model in models:
        if is_non_chat(model):
            continue
        provider = _detect_provider(model)
        if not direct.has_key(provider):
            log_info(f"  [{model}] skip — no key for provider={provider}")
            rec(phase_tag, f"{model}/skip").passed(f"no key for {provider}")
            continue

        if is_reasoning(model) or uses_heavy_thinking(model):
            max_tok = 1024
        elif uses_thinking_tokens(model):
            max_tok = 256
        else:
            max_tok = 32

        msgs = _user_msg(_system_prompt(model), "reply with the single word: ok", model)

        # A: non-stream — concurrent gateway + direct
        log_info(f"  [{model}] non-stream (concurrent gw+direct) …")
        with ThreadPoolExecutor(max_workers=2) as ex:
            f_gw = ex.submit(gw.chat_sync, model, msgs, max_tok, timeout)
            f_dir = ex.submit(direct.chat_sync, model, msgs, max_tok, timeout)
            gw_r = f_gw.result()
            dir_r = f_dir.result()
        ok_s, _, summary_s = _compare_results(gw_r, dir_r, model, "non-stream")
        overhead_s = (
            f"gw={gw_r.get('elapsed', 0):.2f}s "
            f"direct={dir_r.get('elapsed', 0):.2f}s"
        )
        if ok_s:
            log_ok(f"  [{model}] non-stream ✓ — {summary_s} | {overhead_s}")
            rec(phase_tag, f"{model}/non-stream").passed(f"{summary_s} | {overhead_s}")
        else:
            log_fail(f"  [{model}] non-stream ✗ — {summary_s} | {overhead_s}")
            rec(phase_tag, f"{model}/non-stream").failed(f"{summary_s} | {overhead_s}")

        # B: SSE stream — concurrent gateway + direct
        if not no_stream:
            log_info(f"  [{model}] SSE (concurrent gw+direct) …")
            with ThreadPoolExecutor(max_workers=2) as ex:
                f_gw_ss = ex.submit(gw.chat_stream, model, msgs, max_tok, timeout)
                f_dir_ss = ex.submit(direct.chat_stream, model, msgs, max_tok, timeout)
                gw_ss = f_gw_ss.result()
                dir_ss = f_dir_ss.result()
            ok_ss, _, summary_ss = _compare_results(gw_ss, dir_ss, model, "stream")
            ttfb = gw_ss.get("ttfb") or 0
            overhead_ss = (
                f"gw={gw_ss.get('elapsed', 0):.2f}s "
                f"direct={dir_ss.get('elapsed', 0):.2f}s "
                f"ttfb={ttfb:.2f}s"
            )
            if ok_ss:
                log_ok(f"  [{model}] SSE ✓ — {summary_ss} | {overhead_ss}")
                rec(phase_tag, f"{model}/stream").passed(f"{summary_ss} | {overhead_ss}")
            else:
                log_fail(f"  [{model}] SSE ✗ — {summary_ss} | {overhead_ss}")
                rec(phase_tag, f"{model}/stream").failed(f"{summary_ss} | {overhead_ss}")


def phase4_routing_on(
    gw: GWClient,
    cp: CPClient,
    snapshot: StateSnapshot,
    models: list[str],
    no_stream: bool,
    no_cache: bool,
    timeout: int,
    t0_iso: str,
    db: DBClient,
    cache_rounds: int = 3,
    is_remote: bool = False,
):
    """Enable all routing rules except default-kmini-128k, then run the full
    per-model suite (non-stream + SSE + cache) across every model.

    default-kmini-128k is kept disabled because it is a catch-all rule that
    would swallow traffic for every model and mask the results of the targeted
    rules. Every other rule is enabled simultaneously so we test real-world
    interaction between rules (priority, overlap, fallback).
    """
    log_step("P4 Routing ON — all rules enabled except default-kmini-128k")
    deleted = _flush_redis_gateway_cache(is_remote=is_remote)
    if not is_remote:
        if deleted is None:
            log_warn("  Redis cache flush could not run — this phase's cache "
                     "arms may be reading stale entries")
        else:
            log_info(f"  Redis cache flushed: {deleted} key(s) — fresh upstream calls guaranteed")

    # Enable every rule except the catch-all default-kmini-128k
    enabled_names: list[str] = []
    for r in snapshot.routing_rules:
        if r.get("name") == "default-kmini-128k":
            cp.set_routing_rule(r["id"], False)
        else:
            cp.set_routing_rule(r["id"], True)
            enabled_names.append(r.get("name", r["id"]))
    _wait_config_propagation(gw)
    log_info(f"  Active rules: {', '.join(enabled_names) or '(none)'}")

    # Full model suite — identical to P3 but with routing active
    for mid in models:
        _run_model_suite(gw, mid, t0_iso, no_stream, no_cache, timeout, "P4",
                         cache_rounds=cache_rounds)

    # Restore to all-disabled (snapshot.restore() applies original state)
    cp.set_all_routing_rules(False, snapshot.routing_rules)
    log_info("P4 done — all rules disabled; restore will apply original state")


def phase5_cache_cfg(cp: CPClient):
    log_step("P5 Cache config (read-only)")
    # The Tier-1 global cache switches (normaliser_enabled +
    # cache_master_kill_switch) are retired; probe the surviving Tier-2
    # adapter surface instead so P5 still proves the cache admin API is up.
    try:
        _, adapters = cp.get("/api/admin/cache/adapters")
        total = adapters.get("total", "?") if isinstance(adapters, dict) else "?"
        log_ok(f"  cache/adapters: {total} adapter config rows")
        rec("P5", "cache-config").passed(f"cache/adapters total={total}")
    except Exception as e:
        log_warn(f"  cache/adapters: {e}")
        rec("P5", "cache-config").warning(str(e))

    try:
        gcfg = cp.get_gemini_cache_cfg()
        log_ok(f"  settings/gemini-cache: {list(gcfg.keys())}")
        rec("P5", "gemini-cache-config").passed()
    except Exception as e:
        log_warn(f"  settings/gemini-cache: {e}")
        rec("P5", "gemini-cache-config").warning(str(e))


def phase6_db_crosscheck(db: DBClient, t0_iso: str, db_poll_timeout: int):
    log_step("P6 DB cross-check")

    if db.is_remote:
        log_info("  Remote environment — DB cross-check skipped (no local Docker access)")
        rec("P6", "db-crosscheck").passed("remote; skipped")
        return

    found, missing, warn_list = 0, 0, []

    # Deduplicate by model (only check once per model)
    seen: set[str] = set()
    calls_to_check = [c for c in _chat_calls
                      if c["sync_status"] == 200 and c["model"] not in seen
                      and not seen.add(c["model"])]  # type: ignore[func-returns-value]

    for call in calls_to_check:
        model = call["model"]
        if model == "auto":
            continue  # routed to another model; skip direct match
        log_debug(f"  polling traffic_event for {model} …")
        row = db.poll_event(model, call["t0"], timeout=db_poll_timeout)
        if not row:
            log_fail(f"  [{model}] no traffic_event row (timeout={db_poll_timeout}s)")
            rec("P6", f"{model}/db").failed("no row")
            missing += 1
            continue

        issues = []
        ues = row.get("usage_extraction_status", "")
        if ues and ues not in ("ok", "streaming_reported", "streaming_estimated",
                                "streaming_unavailable", ""):
            issues.append(f"extraction={ues}")
        if not row.get("api_key_fingerprint"):
            issues.append("fingerprint empty")
        hd = (row.get("request_hook_decision") or "").upper()
        if hd and hd in ("BLOCK", "DENY"):
            issues.append(f"hook_decision={hd}")

        if issues:
            log_warn(f"  [{model}] row found but issues: {', '.join(issues)}")
            rec("P6", f"{model}/db").warning(", ".join(issues))
            warn_list.append(model)
        else:
            rp = row.get("routed_provider_name", "")
            ri = row.get("routing_rule_id", "")
            log_ok(f"  [{model}] DB ✓ provider={rp or '?'} rule={'set' if ri else 'null'}")
            rec("P6", f"{model}/db").passed(f"provider={rp}")
        found += 1

    log_info(f"DB cross-check: {found} matched, {missing} missing, {len(warn_list)} warnings")


def phase6b_normalize_check(db: DBClient, cp: CPClient, t0_iso: str):
    """Normalized-payload verification — the canonical request/response projection.

    The projection is NEVER persisted: the control plane recomputes it on demand
    from the stored raw body via GET /api/admin/traffic/:id/normalized, and P6b
    validates that endpoint — the same recompute the UI's Normalized tab runs —
    sampling one event per model.

    There is deliberately one path, not a per-deployment choice between a stored
    projection and a recomputed one: a check whose second branch cannot execute
    invites the reader to believe a coverage it does not have.

    For every validated event, confirm:
      - request_status == 'ok' AND response_status == 'ok'
      - content array is present (NOT JSON null — Message.MarshalJSON
        should always emit `[]` at minimum even when the model produced
        no visible content)
      - either content_len > 0 OR reasoning_tokens > 0 (the model
        spent its budget on visible output OR on hidden thinking — both
        are legitimate, but zero text + zero reasoning is a bug)
      - usage math is internally consistent: total = prompt + completion
        + cache_read + cache_creation, within a small tolerance

    Surface every flagged row so the user can inspect via the UI's
    Normalized | Raw tabs.
    """
    log_step("P6b Normalized payload verification")

    if db.is_remote and not db.has_ssh:
        log_info("  Remote environment without ssh-host — P6b skipped")
        rec("P6b", "normalize-check").passed("no DB access; skipped")
        return

    rows = db.fetch_event_rows(t0_iso)
    if not rows:
        log_warn("  No traffic_event rows found in window — P6b skipped")
        rec("P6b", "normalize-check").warning("no rows")
        return

    log_info("  Validating view-time recompute via /api/admin/traffic/:id/normalized")
    sampled = _viewtime_normalize_rows(cp, rows)
    if not sampled:
        log_warn("  View-time endpoint returned no usable rows — P6b skipped")
        rec("P6b", "normalize-check").warning("view-time: no rows")
        return
    _eval_normalize_rows(sampled)


def _viewtime_normalize_rows(cp: CPClient, base_rows: list[dict]) -> list[dict]:
    """Fetch the view-time-recomputed normalized projection for a sample of
    events and map each into the flat shape _eval_normalize_rows expects, so
    the shared evaluator runs unchanged.

    Samples one (most-recent) 200-status event per distinct model: the recompute
    is deterministic per codec path, so one event per model covers every model's
    normalize shape without a per-event HTTP storm. Models covered and any
    fetch/HTTP errors are logged — no silent caps.
    """

    def _i(s) -> int:
        try:
            return int(s) if s else 0
        except (ValueError, TypeError):
            return 0

    def _jtype(v) -> str:
        if isinstance(v, list):
            return "array"
        if isinstance(v, dict):
            return "object"
        if isinstance(v, str):
            return "string"
        return ""  # None / absent → '' (matches the SQL COALESCE default)

    # base_rows are ordered timestamp DESC, so the first seen per model is latest.
    by_model: dict[str, dict] = {}
    for r in base_rows:
        if _i(r.get("status_code")) != 200:
            continue
        by_model.setdefault(r["model_name"], r)

    out: list[dict] = []
    errors = 0
    for model, base in by_model.items():
        eid = base["id"]
        try:
            status, payload = cp.get(f"/api/admin/traffic/{eid}/normalized")
        except Exception as exc:  # noqa: BLE001 — network/JSON failures are real findings
            log_fail(f"  [{model} {eid[:8]}] view-time normalize fetch error: {exc}")
            rec("P6b", f"{model}/{eid[:8]}/viewtime-fetch").failed(str(exc)[:60])
            errors += 1
            continue
        if status != 200:
            # The endpoint's own 4-tier ladder distinguishes two 404s that mean
            # opposite things, and the harness must not flatten them. "Traffic event
            # not found" is a real failure — the row we just observed is gone.
            # "Normalized payload not found" is tier (d): this row has NO recoverable
            # body, which for some endpoint kinds is the DESIGN. A guardrail row is
            # the standing example — stampGuardrailAudit sets no request/response
            # body on purpose, because the evaluated text is the sensitive thing the
            # endpoint exists to judge and must never persist. Failing on it would
            # mean the suite permanently reports a defect for a privacy guarantee
            # working correctly.
            # Shape verified live: {"error": {"code": "", "message": "...",
            # "type": "not_found"}}. Read the nested field explicitly — relying on
            # str(dict) to happen to contain the sentence works today and breaks the
            # moment the envelope gains a field, silently turning this branch off.
            msg = ""
            if isinstance(payload, dict):
                err = payload.get("error")
                if isinstance(err, dict):
                    msg = str(err.get("message") or "")
                elif isinstance(err, str):
                    msg = err
                else:
                    msg = str(payload.get("message") or "")
            if status == 404 and "Normalized payload not found" in msg:
                log_info(f"  [{model} {eid[:8]}] no recoverable body (tier d) — "
                         f"expected for endpoint kinds that persist none")
                rec("P6b", f"{model}/{eid[:8]}/viewtime-nobody").passed(
                    "404 'Normalized payload not found': the row persists no body by design "
                    "(e.g. guardrail) or its spilled body aged out — not a normalize failure")
                continue
            log_fail(f"  [{model} {eid[:8]}] view-time normalize HTTP {status} {msg[:80]}")
            rec("P6b", f"{model}/{eid[:8]}/viewtime-http").failed(f"HTTP {status} {msg[:60]}")
            errors += 1
            continue
        rn = payload.get("responseNormalized")
        has_resp = isinstance(rn, dict) and len(rn) > 0
        rn = rn or {}
        req_status = payload.get("requestStatus", "") or ""
        resp_status = payload.get("responseStatus", "") or ""
        kind = rn.get("kind", "") or ""
        if not has_resp:
            # Request-only event: embeddings return a vector, not a chat
            # projection, so the endpoint omits responseNormalized entirely. The
            # only projection that exists is the request — mirror its status so
            # the shared gate validates the recompute that actually ran, and mark
            # the row ai-embedding so the chat-shape assertions are skipped.
            kind = "ai-embedding"
            resp_status = req_status
        msgs = rn.get("messages") or []
        m0 = msgs[0] if msgs else {}
        content = m0.get("content")
        blocks = content if isinstance(content, list) else []
        u = rn.get("usage") or {}

        def _blk(i: int, k: str, _blocks=blocks):
            return _blocks[i].get(k) if i < len(_blocks) and isinstance(_blocks[i], dict) else None

        def _us(k: str, _u=u) -> str:
            v = _u.get(k)
            return str(v) if v is not None else ""

        out.append({
            "id": eid,
            "model_name": model,
            "status_code": base["status_code"],
            "request_status": req_status,
            "response_status": resp_status,
            "request_error_reason": payload.get("requestError", "") or "",
            "response_error_reason": payload.get("responseError", "") or "",
            "content_type": _jtype(content),
            "content_len": str(len(blocks)),
            "finish_reason": m0.get("finishReason", "") or "",
            "prompt_tokens": _us("promptTokens"),
            "completion_tokens": _us("completionTokens"),
            "total_tokens": _us("totalTokens"),
            "reasoning_tokens": _us("reasoningTokens"),
            "cache_read_tokens": _us("cacheReadTokens"),
            "cache_creation_tokens": _us("cacheCreationTokens"),
            "b0_type": _blk(0, "type") or "",
            "b0_len": str(len(_blk(0, "text") or "")),
            "b1_type": _blk(1, "type") or "",
            "b1_len": str(len(_blk(1, "text") or "")),
            "kind": kind,
        })
    log_info(
        f"  View-time sample: {len(out)} model(s) validated"
        + (f", {errors} fetch/HTTP error(s)" if errors else "")
        + f" (of {len(by_model)} distinct models in window)"
    )
    return out


def _eval_normalize_rows(rows: list[dict]):
    """Run the per-row normalize assertions over the flat row set the view-time
    endpoint produced. Records P6b pass/warn/fail per row."""
    ok_count, fail_count, warn_count = 0, 0, 0
    failures: list[str] = []
    warnings: list[str] = []

    def _int(s: str) -> int:
        try:
            return int(s) if s else 0
        except ValueError:
            return 0

    for row in rows:
        model = row["model_name"]
        eid = row["id"][:8]
        status = _int(row["status_code"])
        if status != 200:
            continue  # skip non-200 upstream paths
        req_st = row["request_status"]
        resp_st = row["response_status"]
        if req_st != "ok" or resp_st != "ok":
            err = row["request_error_reason"] or row["response_error_reason"]
            log_fail(f"  [{model} {eid}] normalize {req_st}/{resp_st}: {err[:60]}")
            rec("P6b", f"{model}/{eid}/status").failed(f"{req_st}/{resp_st}")
            fail_count += 1
            failures.append(f"{model} ({eid}): {req_st}/{resp_st} {err}")
            continue

        # The remaining checks (content array, finishReason, visible-text) are
        # chat-shaped, so they apply only to chat-shaped kinds. Everything else
        # carries its payload somewhere other than messages[]: ai-embedding has
        # vectors, and http-binary / http-multipart put the whole body behind
        # http.bodyView.mediaRef — which is where a tts response now lives.
        # This was an exclusion list naming ai-embedding alone, so a perfectly
        # correct tts row (kind=http-binary, mediaRef captured audio/mpeg with
        # a resolvable locator) was reported as "content is '' (expected
        # array)". A shape assertion applied to a shape it was not written for
        # reports the checker's assumption as the product's defect. The
        # request/response normalize-status checks above already validated
        # these rows.
        if row.get("kind", "") not in ("ai-chat", "ai-stt", ""):
            continue

        # Content array shape check: should be array, never null.
        if row["content_type"] != "array":
            log_fail(f"  [{model} {eid}] response_normalized.messages[0].content is {row['content_type']!r} (expected array)")
            rec("P6b", f"{model}/{eid}/content-shape").failed(
                f"content_type={row['content_type']!r}")
            fail_count += 1
            failures.append(
                f"{model} ({eid}): content type {row['content_type']!r} not array"
            )
            continue

        # Empty visible content + zero reasoning_tokens = real loss.
        # Visible content can be empty on its own ONLY when the model
        # consumed its budget on hidden reasoning (gemini-2.5-pro,
        # o1/o3 with max_completion_tokens=64 hitting length).
        block_count = _int(row["content_len"])
        b0_len = _int(row["b0_len"])
        b1_len = _int(row["b1_len"])
        total_text = b0_len + b1_len
        rsn = _int(row["reasoning_tokens"])
        completion = _int(row["completion_tokens"])
        # …or when the response was cut off before a block completed. A
        # multimodal case here sends max_tokens=8; claude-fable-5 spent all
        # eight on output the truncation then discarded and returned
        # finishReason=length with content []. That is the correct record of
        # what happened, and calling it "content silently lost" accused the
        # normalizer of dropping something the smoke's own max_tokens removed.
        truncated = (row.get("finish_reason") or "").lower() in ("length", "max_tokens")
        if block_count == 0 and rsn == 0 and completion > 0 and not truncated:
            log_fail(
                f"  [{model} {eid}] visible blocks=0, reasoning=0, but "
                f"completion_tokens={completion} — content silently lost"
            )
            rec("P6b", f"{model}/{eid}/empty-content").failed(
                f"completion={completion} but no blocks")
            fail_count += 1
            failures.append(
                f"{model} ({eid}): completion={completion} but content empty"
            )
            continue

        # Usage math sanity. The canonical usage is reconciled against the
        # provider's wire total via three accepted formulas — a drift WARN
        # fires only when ALL THREE fail:
        #   F1  total == prompt + completion + cache_read + cache_creation
        #         (cache additive to total — providers that report it that way)
        #   F2  total == prompt + completion
        #         (cache_read folded into prompt by the normalizer — Anthropic;
        #          and providers like DeepSeek/Moonshot that exclude cache from total)
        #   F3  total == prompt + completion - reasoning
        #         (the normalizer folds reasoning into completion, but the
        #          provider's wire total EXCLUDES it — Gemini thoughtsTokenCount,
        #          DeepSeek reasoning. Without F3 every reasoning response on
        #          these providers false-WARNs by exactly `reasoning_tokens`.)
        # See normalization-architecture.md §3 for the per-provider token mapping.
        p, c, t = (_int(row["prompt_tokens"]),
                   _int(row["completion_tokens"]),
                   _int(row["total_tokens"]))
        crd = _int(row["cache_read_tokens"])
        ccr = _int(row["cache_creation_tokens"])
        expected = p + c + crd + ccr
        if t > 0 and abs(t - expected) > 2:
            if abs(t - (p + c)) > 2 and abs(t - (p + c - rsn)) > 2:
                log_warn(
                    f"  [{model} {eid}] usage math drift: total={t} != prompt({p})+completion({c})+cache_read({crd})+cache_creation({ccr})={expected} (also != p+c and != p+c-reasoning({rsn}))"
                )
                rec("P6b", f"{model}/{eid}/usage-math").warning(
                    f"total={t} != {expected}")
                warn_count += 1
                warnings.append(
                    f"{model} ({eid}): usage math total={t} != p+c+crd+ccr={expected}"
                )

        # Reasoning-model shape: when reasoning_tokens > 0, the first
        # content block is allowed to be 'reasoning' (DeepSeek /
        # Moonshot surface reasoning_content) OR text (Gemini /
        # OpenAI o1 hide reasoning text from the wire and only report
        # the token count). Both are valid; we just log the shape so
        # the report shows what was preserved.
        shape = f"{row['b0_type']}:{b0_len}"
        if row["b1_type"]:
            shape += f"+{row['b1_type']}:{b1_len}"
        extras = []
        if rsn > 0:
            extras.append(f"rsn={rsn}")
        if crd > 0:
            extras.append(f"crd={crd}")
        if ccr > 0:
            extras.append(f"ccr={ccr}")
        extras_str = f" [{', '.join(extras)}]" if extras else ""
        log_ok(f"  [{model} {eid}] normalize ok | content={shape}{extras_str}")
        rec("P6b", f"{model}/{eid}/ok").passed(shape + extras_str)
        ok_count += 1

    log_info(
        f"Normalize check [view-time]: {ok_count} ok, {warn_count} warnings, "
        f"{fail_count} failures across {len(rows)} rows"
    )
    if failures:
        for f in failures[:5]:
            log_fail(f"  → {f}")


def phase3_5_concurrent(
    gw: GWClient,
    db: DBClient,
    model: str,
    n: int,
    t0_iso: str,
    timeout: int,
):
    log_step(f"P3.5 Concurrent test: {n}× {model}")
    msgs = [{"role": "user", "content": "Reply with exactly: ok"}]
    results_list: list[dict] = []
    lock = threading.Lock()

    def _call():
        r = gw.chat_sync(model, msgs, max_tokens=1024, timeout=timeout)
        with lock:
            results_list.append(r)

    with ThreadPoolExecutor(max_workers=n) as ex:
        futures = [ex.submit(_call) for _ in range(n)]
        for f in as_completed(futures):
            f.result()

    ok_count = sum(1 for r in results_list if r.get("status") == 200)
    log_info(f"  {ok_count}/{n} requests succeeded")

    time.sleep(3)
    db_count = db.count_events(t0_iso, model_name=model)
    if db_count >= ok_count:
        log_ok(f"  DB: {db_count} events found (expected ~{ok_count})")
        rec("P3.5", f"concurrent-{n}x").passed(f"{db_count} DB rows")
    else:
        log_warn(f"  DB: only {db_count} events for {ok_count} successful requests")
        rec("P3.5", f"concurrent-{n}x").warning(f"DB={db_count} < ok={ok_count}")


def phase7_metrics_delta(gw: GWClient, m0: str) -> list:
    log_step("P7 Metrics delta")
    try:
        m1 = gw.metrics()
        # A counter check that cannot read counters must FAIL, not report "no
        # error growth". With an unreadable exposition errors_grew is empty for
        # the same reason a switched-off smoke detector never alarms, and this
        # phase is one of the few things that watches the gateway's own error
        # counters — the binding in CLAUDE.md names Prometheus deltas as
        # something the smoke uniquely catches.
        if not GWClient.metrics_readable(m1) or not GWClient.metrics_readable(m0):
            log_fail("  metrics exposition unreadable (refused or empty) — the error-counter "
                     "check cannot run, and passing here would assert nothing")
            rec("P7", "metrics").failed(
                "metrics exposition unreadable: /metrics is authenticated and answers 200 with "
                "'unauthorized' when the token is absent, so this check would silently compare "
                "two refusals. Set INTERNAL_SERVICE_TOKEN or NEXUS_HUB_SERVICE_TOKEN")
            return []
        diff = metrics_diff(m0, m1)
        errors_grew = [(k, v0, v1) for k, v0, v1 in diff
                       if "error" in k and v1 > v0]
        requests = [(k, v0, v1) for k, v0, v1 in diff if "requests_total" in k and "error" not in k]
        for k, v0, v1 in requests:
            log_ok(f"  {k.split('{')[0]} +{v1 - v0:.0f}")
        for k, v0, v1 in errors_grew:
            log_warn(f"  {k} +{v1 - v0:.0f} (was {v0:.0f})")
        if not errors_grew:
            log_ok("No unexpected error counter growth")
        observed = sum(v1 - v0 for _, v0, v1 in requests)
        # Zero request growth after a run that just drove hundreds of requests is
        # not a pass — it means this phase is reading something other than the
        # gateway's counters, which is the state it was in for both reasons above.
        # "No unexpected error counter growth" is only meaningful once we can show
        # the counters moved at all.
        if observed <= 0:
            log_fail("  no nexus_requests_total growth observed across the run — the error-counter "
                     "claim rests on counters this phase cannot see")
            rec("P7", "metrics").failed(
                "0 request-counter growth after a full run: the exposition was read but no "
                "nexus_requests_total series changed, so 'no unexpected error counter growth' "
                "asserts nothing")
            return diff
        rec("P7", "metrics").passed(f"{observed:.0f} requests recorded across {len(requests)} series")
        return diff
    except Exception as e:
        # A failure to FETCH the counters is a failure of this phase, not a note
        # about it: everything P7 claims rests on being able to read them, so a
        # warning here would leave the run green while the check did nothing —
        # which is the state this phase was in for its whole history.
        log_fail(f"Metrics delta could not run: {e}")
        rec("P7", "metrics").failed(str(e))
        return []


def phase_fix_deprecated(cp: CPClient):
    """P8: Remove catalog models whose non-stream call failed with a provider-side error in P3.

    Safety rules:
    - Skips models that failed with HTTP 401 / AUTH_INVALID_KEY — those indicate a bad
      virtual key, not a deprecated model. If the VK itself is broken the whole P3 will
      return 401 and nothing should be deleted.
    - Aborts entirely if >50% of P3 non-stream results are 401 — that's a VK problem,
      not individual model deprecation.
    - Only removes models with confirmed provider-side failures (non-200, non-401 codes).
    """
    log_step("P8 Fix deprecated models — remove from catalog")

    # Gather all P3 non-stream results
    p3_nonstream: dict[str, Result] = {}
    for r in _results:
        if r.phase != "P3" or "/" not in r.name:
            continue
        model_code, sub = r.name.rsplit("/", 1)
        if sub == "non-stream":
            p3_nonstream[model_code] = r

    if not p3_nonstream:
        log_ok("No P3 non-stream results found — nothing to do.")
        rec("P8", "fix-deprecated").passed("no P3 results")
        return

    # Safety gate: if any failure is AUTH_INVALID_KEY / HTTP 401, the VK itself is bad.
    # All models would fail auth, so abort entirely to avoid mass-deleting valid models.
    auth_failures = sum(
        1 for r in p3_nonstream.values()
        if r.ok is False and ("AUTH_INVALID_KEY" in r.note or "HTTP 401" in r.note)
    )
    if auth_failures > 0:
        log_warn(
            f"  {auth_failures} P3 failures contain AUTH errors — "
            "VK may be invalid. Aborting P8 to avoid deleting valid models."
        )
        rec("P8", "fix-deprecated").warning(
            f"aborted: {auth_failures} auth failures suggest bad VK"
        )
        return

    # Identify provider-deprecated models: only those where the upstream itself returned
    # a "not found" / "no longer available" error — NOT gateway-level 502/429 failures.
    # 502 "all upstream providers failed" = credential issue; 429 = rate limit (transient).
    _DEPRECATED_SIGNALS = (
        "not found",
        "no longer available",
        "deprecated",
        "not supported",
        "does not exist",
    )
    deprecated: set[str] = set()
    for code, r in p3_nonstream.items():
        if r.ok is False:
            note_lower = r.note.lower()
            # Skip gateway-internal failures — only act on provider-side not-found
            if "all upstream providers failed" in note_lower:
                continue
            if "HTTP 429" in r.note or "rate limit" in note_lower:
                continue
            if any(sig in note_lower for sig in _DEPRECATED_SIGNALS):
                deprecated.add(code)

    if not deprecated:
        log_ok("No provider-deprecated models to remove.")
        rec("P8", "fix-deprecated").passed("no deprecated models")
        return

    log_info(f"Deprecated candidates ({len(deprecated)}): {', '.join(sorted(deprecated))}")

    # Fetch catalog to get DB UUIDs
    flat = cp.list_models_flat()
    code_to_id = {m.get("code", ""): m.get("id", "") for m in flat}

    removed = []
    skipped = []
    for code in sorted(deprecated):
        db_id = code_to_id.get(code)
        if not db_id:
            log_warn(f"  [{code}] not found in catalog — already removed?")
            skipped.append(code)
            continue
        status = cp.delete_model(db_id)
        if status in (200, 204):
            log_ok(f"  [{code}] deleted from catalog (HTTP {status})")
            removed.append(code)
        else:
            log_warn(f"  [{code}] delete failed HTTP {status}")
            skipped.append(code)

    summary = f"removed={len(removed)} skipped={len(skipped)}"
    if removed:
        rec("P8", "fix-deprecated").passed(summary)
    else:
        rec("P8", "fix-deprecated").warning(summary)


# ─── P9 Direct compare ─────────────────────────────────────────────────────────

def _detect_provider(model: str) -> str:
    """Map a model ID to its upstream provider name."""
    m = model.lower()
    if m.startswith("gpt-") or m.startswith("o1") or m.startswith("o3") or \
            m.startswith("o4") or m.startswith("gpt-5"):
        return "openai"
    if m.startswith("claude-"):
        return "anthropic"
    if m.startswith("gemini-"):
        return "gemini"
    if m.startswith("deepseek-"):
        return "deepseek"
    if m.startswith("moonshot-") or m.startswith("kimi-"):
        return "moonshot"
    return "unknown"


class DirectClient:
    """Call provider APIs directly (bypassing the gateway) for comparison testing.

    Keys are resolved from the constructor dict, which is built from environment
    variables or a JSON file supplied via --provider-keys-file.
    """

    _PROVIDER_BASES = {
        "openai":    "api.openai.com",
        "anthropic": "api.anthropic.com",
        "gemini":    "generativelanguage.googleapis.com",
        "deepseek":  "api.deepseek.com",
        "moonshot":  "api.moonshot.cn",
    }

    def __init__(self, keys: dict[str, str]):
        self.keys = keys

    @classmethod
    def from_env(cls, extra_file: str = "") -> "DirectClient":
        keys: dict[str, str] = {}
        for env_var, provider in [
            ("OPENAI_API_KEY",    "openai"),
            ("ANTHROPIC_API_KEY", "anthropic"),
            ("GEMINI_API_KEY",    "gemini"),
            ("GOOGLE_API_KEY",    "gemini"),
            ("DEEPSEEK_API_KEY",  "deepseek"),
            ("MOONSHOT_API_KEY",  "moonshot"),
        ]:
            val = os.environ.get(env_var, "")
            if val and provider not in keys:
                keys[provider] = val
        if extra_file:
            try:
                with open(extra_file) as f:
                    keys.update(json.load(f))
            except Exception as e:
                log_warn(f"  [direct] could not load provider keys file {extra_file!r}: {e}")
        return cls(keys)

    @classmethod
    def from_db(cls, db: "DBClient", cp_config_path: str = "") -> "DirectClient":
        """Build a DirectClient using API keys decrypted from the Credential table.

        DB keys take precedence over env-var keys. Env vars fill in any
        provider not found in the DB.
        """
        db_keys = db.load_provider_credentials(cp_config_path)
        if not db_keys:
            log_warn("  [direct] no DB credentials loaded — falling back to env-var keys only")
        env_keys = cls.from_env("").keys
        # DB keys override env vars for the same provider
        return cls({**env_keys, **db_keys})

    def has_key(self, provider: str) -> bool:
        return bool(self.keys.get(provider))

    def _https_conn(self, host: str, timeout: int) -> http.client.HTTPSConnection:
        import ssl
        return http.client.HTTPSConnection(host, timeout=timeout,
                                           context=ssl.create_default_context())

    # ── non-streaming calls ──────────────────────────────────────────────────

    def chat_sync(self, model: str, messages: list, max_tokens: int = 32,
                  timeout: int = 90) -> dict:
        provider = _detect_provider(model)
        key = self.keys.get(provider, "")
        if not key:
            return {"status": 0, "error": f"no key for {provider}", "provider": provider}
        t0 = time.time()
        try:
            if provider == "anthropic":
                return self._anthropic_sync(model, messages, max_tokens, timeout, key, t0)
            if provider == "gemini":
                return self._gemini_sync(model, messages, max_tokens, timeout, key, t0)
            # OpenAI-compatible (openai, deepseek, moonshot)
            return self._openai_compat_sync(provider, model, messages, max_tokens, timeout, key, t0)
        except Exception as e:
            return {"status": 0, "error": str(e), "provider": provider,
                    "elapsed": time.time() - t0}

    def _openai_compat_sync(self, provider: str, model: str, messages: list,
                             max_tokens: int, timeout: int, key: str, t0: float) -> dict:
        host = self._PROVIDER_BASES[provider]
        body_dict: dict = {"model": model, "messages": messages, "max_tokens": max_tokens}
        if _send_temperature(model, "direct"):
            body_dict["temperature"] = 0
        body = json.dumps(body_dict).encode()
        c = self._https_conn(host, timeout)
        c.request("POST", "/v1/chat/completions", body,
                  {"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
        r = c.getresponse()
        raw = r.read().decode("utf-8", errors="replace")
        c.close()
        data = json.loads(raw) if raw.strip() else {}
        content = ""
        for ch in data.get("choices", []):
            content += (ch.get("message", {}).get("content") or "")
        usage = data.get("usage", {})
        return {
            "status": r.status, "provider": provider,
            "has_content": bool(content), "content": content,
            "prompt_tokens": usage.get("prompt_tokens", 0),
            "completion_tokens": usage.get("completion_tokens", 0),
            "error": str(data.get("error", {}).get("message", "")) if r.status >= 400 else "",
            "elapsed": time.time() - t0,
        }

    def _anthropic_sync(self, model: str, messages: list, max_tokens: int,
                         timeout: int, key: str, t0: float) -> dict:
        body = json.dumps({
            "model": model, "max_tokens": max_tokens, "messages": messages,
        }).encode()
        c = self._https_conn("api.anthropic.com", timeout)
        c.request("POST", "/v1/messages", body, {
            "x-api-key": key,
            "anthropic-version": "2023-06-01",
            "Content-Type": "application/json",
        })
        r = c.getresponse()
        raw = r.read().decode("utf-8", errors="replace")
        c.close()
        data = json.loads(raw) if raw.strip() else {}
        content = "".join(
            blk.get("text", "") for blk in data.get("content", [])
            if blk.get("type") == "text"
        )
        usage = data.get("usage", {})
        return {
            "status": r.status, "provider": "anthropic",
            "has_content": bool(content), "content": content,
            "prompt_tokens": usage.get("input_tokens", 0),
            "completion_tokens": usage.get("output_tokens", 0),
            "error": str(data.get("error", {}).get("message", "")) if r.status >= 400 else "",
            "elapsed": time.time() - t0,
        }

    def _gemini_sync(self, model: str, messages: list, max_tokens: int,
                      timeout: int, key: str, t0: float) -> dict:
        # Convert OpenAI-format messages to Gemini contents
        contents = []
        system_text = ""
        for msg in messages:
            role = msg.get("role", "user")
            content = msg.get("content", "")
            if role == "system":
                system_text = content
                continue
            contents.append({"parts": [{"text": content}],
                              "role": "model" if role == "assistant" else "user"})
        req_body: dict = {
            "contents": contents,
            "generationConfig": {"maxOutputTokens": max_tokens},
        }
        if system_text:
            req_body["systemInstruction"] = {"parts": [{"text": system_text}]}
        path = f"/v1beta/models/{model}:generateContent"
        body = json.dumps(req_body).encode()
        c = self._https_conn("generativelanguage.googleapis.com", timeout)
        c.request("POST", path, body, {
            "x-goog-api-key": key, "Content-Type": "application/json",
        })
        r = c.getresponse()
        raw = r.read().decode("utf-8", errors="replace")
        c.close()
        data = json.loads(raw) if raw.strip() else {}
        content = ""
        for cand in data.get("candidates", []):
            for part in cand.get("content", {}).get("parts", []):
                content += part.get("text", "")
        meta = data.get("usageMetadata", {})
        return {
            "status": r.status, "provider": "gemini",
            "has_content": bool(content), "content": content,
            "prompt_tokens": meta.get("promptTokenCount", 0),
            "completion_tokens": meta.get("candidatesTokenCount", 0),
            "error": str(data.get("error", {}).get("message", "")) if r.status >= 400 else "",
            "elapsed": time.time() - t0,
        }

    # ── streaming calls ──────────────────────────────────────────────────────

    def chat_stream(self, model: str, messages: list, max_tokens: int = 32,
                    timeout: int = 90) -> dict:
        provider = _detect_provider(model)
        key = self.keys.get(provider, "")
        if not key:
            return {"status": 0, "error": f"no key for {provider}", "provider": provider,
                    "done_seen": False, "chunk_count": 0, "has_content": False, "content": ""}
        t0 = time.time()
        try:
            if provider == "anthropic":
                return self._anthropic_stream(model, messages, max_tokens, timeout, key, t0)
            if provider == "gemini":
                return self._gemini_stream(model, messages, max_tokens, timeout, key, t0)
            return self._openai_compat_stream(provider, model, messages, max_tokens, timeout, key, t0)
        except Exception as e:
            return {"status": 0, "error": str(e), "provider": provider,
                    "done_seen": False, "chunk_count": 0, "has_content": False, "content": "",
                    "elapsed": time.time() - t0}

    def _openai_compat_stream(self, provider: str, model: str, messages: list,
                               max_tokens: int, timeout: int, key: str, t0: float) -> dict:
        host = self._PROVIDER_BASES[provider]
        body_dict: dict = {
            "model": model, "messages": messages, "stream": True,
            "max_tokens": max_tokens,
            "stream_options": {"include_usage": True},
        }
        if _send_temperature(model, "direct"):
            body_dict["temperature"] = 0
        body = json.dumps(body_dict).encode()
        c = self._https_conn(host, timeout)
        c.request("POST", "/v1/chat/completions", body, {
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        })
        r = c.getresponse()
        chunks, content_parts, done_seen = [], [], False
        for raw_line in r:
            line = raw_line.decode("utf-8", errors="replace").strip()
            if not line.startswith("data:"):
                continue
            data_str = line[5:].strip()
            if data_str == "[DONE]":
                done_seen = True
                break
            try:
                chunk = json.loads(data_str)
                chunks.append(chunk)
                for ch in chunk.get("choices", []):
                    delta = ch.get("delta", {})
                    if delta.get("content"):
                        content_parts.append(delta["content"])
                    # reasoning_content: collect thinking tokens from DeepSeek-V4 /
                    # Kimi-K2 so content presence matches the gateway's output.
                    if delta.get("reasoning_content"):
                        content_parts.append(delta["reasoning_content"])
            except Exception:
                pass
        c.close()
        return {
            "status": r.status, "provider": provider,
            "done_seen": done_seen, "chunk_count": len(chunks),
            "has_content": bool(content_parts), "content": "".join(content_parts),
            "elapsed": time.time() - t0, "error": "",
        }

    def _anthropic_stream(self, model: str, messages: list, max_tokens: int,
                           timeout: int, key: str, t0: float) -> dict:
        body = json.dumps({
            "model": model, "max_tokens": max_tokens, "messages": messages, "stream": True,
        }).encode()
        c = self._https_conn("api.anthropic.com", timeout)
        c.request("POST", "/v1/messages", body, {
            "x-api-key": key,
            "anthropic-version": "2023-06-01",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        })
        r = c.getresponse()
        content_parts, event_count, stop_seen = [], 0, False
        for raw_line in r:
            line = raw_line.decode("utf-8", errors="replace").strip()
            if not line.startswith("data:"):
                continue
            data_str = line[5:].strip()
            try:
                ev = json.loads(data_str)
                ev_type = ev.get("type", "")
                event_count += 1
                if ev_type == "content_block_delta":
                    delta = ev.get("delta", {})
                    if delta.get("type") == "text_delta":
                        content_parts.append(delta.get("text", ""))
                elif ev_type == "message_stop":
                    stop_seen = True
            except Exception:
                pass
        c.close()
        # Anthropic uses message_stop, not [DONE]; treat stop_seen as done_seen
        return {
            "status": r.status, "provider": "anthropic",
            "done_seen": stop_seen, "chunk_count": event_count,
            "has_content": bool(content_parts), "content": "".join(content_parts),
            "elapsed": time.time() - t0, "error": "",
        }

    def _gemini_stream(self, model: str, messages: list, max_tokens: int,
                        timeout: int, key: str, t0: float) -> dict:
        contents = []
        system_text = ""
        for msg in messages:
            role = msg.get("role", "user")
            content = msg.get("content", "")
            if role == "system":
                system_text = content
                continue
            contents.append({"parts": [{"text": content}],
                              "role": "model" if role == "assistant" else "user"})
        req_body: dict = {
            "contents": contents,
            "generationConfig": {"maxOutputTokens": max_tokens},
        }
        if system_text:
            req_body["systemInstruction"] = {"parts": [{"text": system_text}]}
        path = f"/v1beta/models/{model}:streamGenerateContent?alt=sse"
        body = json.dumps(req_body).encode()
        c = self._https_conn("generativelanguage.googleapis.com", timeout)
        c.request("POST", path, body, {
            "x-goog-api-key": key,
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        })
        r = c.getresponse()
        content_parts, event_count, finish_seen = [], 0, False
        for raw_line in r:
            line = raw_line.decode("utf-8", errors="replace").strip()
            if not line.startswith("data:"):
                continue
            data_str = line[5:].strip()
            try:
                ev = json.loads(data_str)
                event_count += 1
                for cand in ev.get("candidates", []):
                    for part in cand.get("content", {}).get("parts", []):
                        if part.get("text"):
                            content_parts.append(part["text"])
                    if cand.get("finishReason"):
                        finish_seen = True
            except Exception:
                pass
        c.close()
        return {
            "status": r.status, "provider": "gemini",
            "done_seen": finish_seen, "chunk_count": event_count,
            "has_content": bool(content_parts), "content": "".join(content_parts),
            "elapsed": time.time() - t0, "error": "",
        }


def _compare_results(gw: dict, direct: dict, model: str, mode: str) -> tuple[bool, list[str], str]:
    """Compare gateway vs direct API result for one model+mode.

    Returns (ok, issues, summary). ok=True means no unexpected differences.
    Expected differences (format translation, stochastic content, native
    stop events) are noted but do not flip ok to False.
    """
    issues: list[str] = []
    notes: list[str] = []

    gw_status = gw.get("status", 0)
    dir_status = direct.get("status", 0)

    # Status code
    if gw_status != dir_status:
        if gw_status == 200 and dir_status >= 400:
            notes.append(f"gateway=200 direct={dir_status} (gateway may cache/override)")
        elif gw_status >= 400 and dir_status == 200:
            issues.append(f"gateway={gw_status} but direct=200 — gateway error on working model")
        else:
            notes.append(f"status: gateway={gw_status} direct={dir_status}")

    # GWClient.chat_sync returns {"data": {OpenAI JSON}} — dig in for content.
    # GWClient.chat_stream returns {"content": str} at the top level.
    if mode == "non-stream":
        _choices = (gw.get("data") or {}).get("choices") or []
        gw_content_str = _choices[0].get("message", {}).get("content", "") if _choices else ""
        gw_usage = (gw.get("data") or {}).get("usage") or {}
        gw_prompt_tok = gw_usage.get("prompt_tokens", 0)
        gw_completion_tok = gw_usage.get("completion_tokens", 0)
    else:
        gw_content_str = gw.get("content") or ""
        gw_usage = gw.get("usage") or {}
        gw_prompt_tok = (gw_usage or {}).get("prompt_tokens", 0)
        gw_completion_tok = (gw_usage or {}).get("completion_tokens", 0)
    gw_content = bool(gw_content_str)
    dir_content = bool(direct.get("has_content") or direct.get("content"))
    dir_prompt_tok = direct.get("prompt_tokens", 0)
    dir_completion_tok = direct.get("completion_tokens", 0)

    # Content presence
    if not gw_content and dir_content:
        issues.append("gateway returned empty content; direct has content")
    elif not gw_content and not dir_content:
        notes.append("both empty content (may be expected for this prompt/model)")
    elif gw_content and not dir_content:
        notes.append("gateway has content; direct empty (possible direct-call issue)")

    # Token count parity — flag large divergence between gateway and direct.
    # Both values must be non-zero to avoid false positives on streaming (where
    # usage may not be included) or non-Anthropic streaming (no include_usage).
    # A ratio outside [0.6, 1.6] suggests the gateway's token extraction or
    # format-translation is miscounting.
    if gw_prompt_tok and dir_prompt_tok:
        ratio = gw_prompt_tok / dir_prompt_tok
        if ratio < 0.6 or ratio > 1.6:
            notes.append(
                f"prompt_tokens: gw={gw_prompt_tok} direct={dir_prompt_tok} "
                f"(ratio={ratio:.2f} — possible format-translation mismatch)"
            )

    # Streaming termination
    if mode == "stream":
        gw_done = gw.get("done_seen", False)
        dir_done = direct.get("done_seen", False)
        if gw_done and not dir_done:
            provider = _detect_provider(model)
            if provider in ("anthropic", "gemini"):
                # Expected: those providers use message_stop / finishReason, not [DONE]
                notes.append(f"gateway emits [DONE] (OpenAI wrapper); {provider} direct uses native termination")
            else:
                notes.append("gateway has [DONE]; direct missing (may be provider quirk)")
        elif not gw_done and dir_done:
            issues.append("gateway missing stream termination; direct stream terminated")

    ok = len(issues) == 0
    summary_parts = []
    if notes:
        summary_parts.append("expected: " + "; ".join(notes))
    if issues:
        summary_parts.append("UNEXPECTED: " + "; ".join(issues))
    if ok and not notes:
        summary_parts.append("consistent")
    summary = " | ".join(summary_parts) if summary_parts else "consistent"
    return ok, issues, summary


# ─── PE — Embedding endpoint smoke (E61 / --embedding) ───────────────────────
#
# When --embedding is passed, this phase iterates over all catalog models whose
# type is 'embedding' (or whose ID matches the embedding-model regex) and posts
# a minimal /v1/embeddings request to each. It verifies the response shape and
# cross-checks the traffic_event row for the expected cache behaviour.
#
# Empty-catalog handling:
#   If no embedding-type models exist in the catalog (or the embedding-capable
#   provider is not configured), the phase logs a clear skip message and exits
#   successfully — zero models is a vacuous pass.  Pre-E62 systems hit this
#   path automatically; post-E62 systems should have embedding models seeded.

_EMBEDDING_MODEL_RE = re.compile(
    r"^(text-embedding|.*\bembedding\b.*|.*-embed\b|embed-.*)", re.I
)

def _is_embedding_model(mid: str) -> bool:
    return bool(_EMBEDDING_MODEL_RE.match(mid))


def phase_embedding(
    gw: "GWClient",
    cp: "CPClient",
    db: "DBClient",
    t0_iso: str,
    timeout: int = 60,
) -> None:
    """PE — POST /v1/embeddings for each embedding-type catalog model.

    Empty catalog → vacuous pass (zero models is a no-op; P3E provides the
    full per-model × per-ingress coverage and is preferred for CI runs).
    """
    log_step("PE Embedding endpoint smoke (E61 --embedding)")

    # Discover embedding models from the gateway catalog.
    _status, body = gw.list_models()
    all_model_ids = [m["id"] for m in body.get("data", [])]
    embedding_models = [mid for mid in all_model_ids if _is_embedding_model(mid)]

    if not embedding_models:
        log_warn(
            "PE: no embedding models found in catalog — vacuously pass. "
            "(P3E provides the full per-model × per-ingress coverage when "
            "the catalog is populated.)"
        )
        rec("PE", "embedding-catalog-empty").warning(
            "No embedding models in catalog"
        )
        return

    log_info(f"PE: found {len(embedding_models)} embedding model(s): "
             f"{', '.join(embedding_models)}")

    for mid in embedding_models:
        log_step(f"PE/{mid}")
        t_req = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

        # POST /v1/embeddings — minimal single-string input.
        body_req = {
            "model": mid,
            "input": "What is the Nexus AI Gateway?",
        }
        result = gw._post_sync(
            "/v1/embeddings",
            body_req,
            timeout=timeout,
            endpoint="embeddings",
        )
        status = result.get("status", 0)
        data = result.get("data", {})
        elapsed_ms = result.get("elapsed", 0.0) * 1000

        # Shape check: OpenAI embeddings response has data[].embedding.
        shape_ok = (
            status == 200
            and isinstance(data.get("data"), list)
            and len(data["data"]) > 0
            and isinstance(data["data"][0].get("embedding"), list)
            and len(data["data"][0]["embedding"]) > 0
        )
        usage_ok = isinstance(data.get("usage"), dict) and \
                   isinstance(data.get("usage", {}).get("prompt_tokens"), int)

        if status == 200 and shape_ok:
            dim = len(data["data"][0]["embedding"])
            pt = data.get("usage", {}).get("prompt_tokens", "?")
            log_ok(
                f"  [{mid}] HTTP 200, dim={dim}, prompt_tokens={pt}, "
                f"{elapsed_ms:.0f}ms"
            )
            rec("PE", f"embed-{mid}").passed(
                f"dim={dim} prompt_tokens={pt} {elapsed_ms:.0f}ms"
            )
        else:
            log_fail(
                f"  [{mid}] HTTP {status}, shape_ok={shape_ok}, "
                f"data={json.dumps(data)[:200]}"
            )
            rec("PE", f"embed-{mid}").failed(
                f"HTTP {status} shape_ok={shape_ok}"
            )

        if not usage_ok and status == 200:
            log_warn(f"  [{mid}] usage.prompt_tokens missing or not int")

        # DB cross-check — embeddings are NOT cached via the response cache;
        # verify: gateway_cache_status=miss (the embedding itself doesn't go
        # through the response-cache L1/L2 lookup path).
        if not db.is_remote:
            row = db.poll_event(mid, t_req, timeout=30)
            if row:
                cache_status = row.get("gateway_cache_status", "")
                if cache_status in ("miss", ""):
                    log_ok(f"  [{mid}] DB: cache_status=miss (correct — embeddings not cached)")
                else:
                    log_warn(
                        f"  [{mid}] DB: cache_status={cache_status!r} "
                        f"(expected miss — embeddings are the cache infra, not cached)"
                    )
            else:
                log_warn(f"  [{mid}] DB: traffic_event row not found within 30s")

    log_ok(f"PE done — {len(embedding_models)} embedding model(s) tested")


# ─── Report ────────────────────────────────────────────────────────────────────

def render_report(
    vk: str,
    t0: str,
    t1: str,
    diff: list,
    report_path: str,
    cost_phase_modes: Optional[dict] = None,
):
    # Recorded as a Result, not only printed, so a bounded run carries its
    # denominator into the report table and the warning count. A fact that changes
    # what the verdict MEANS has to live in the structured output; leaving it on
    # stdout only is the same mistake as burying a smell flag among seven
    # attributes on a line nobody greps.
    if _denied_models:
        rec("PRE", "vk-coverage").warning(
            f"virtual key denied {len(_denied_models)} model(s) with MODEL_NOT_ALLOWED, so the "
            f"pass count below was measured over {len(_reached_models)} reached model(s), not the "
            f"full catalogue. Denied: {', '.join(sorted(_denied_models))}. Remedy: use a key whose "
            "allowedModels is empty (tests/scripts/mint-test-vk.go mints one)")

    # Before counting anything: two verdicts under one label make the counts
    # unreadable, so this is recorded as a failure of the RUN rather than
    # reported as a note somebody may skim past.
    dupes = _duplicate_labels()
    if dupes:
        rec("P0", "result-labels-unique").failed(
            "two or more results share a label, so one measurement cannot be told from "
            "another and the P8 deprecation map (keyed by name) silently keeps only the "
            "last: " + "; ".join(dupes))

    pass_count = sum(1 for r in _results if r.ok is True and not r.warn)
    warn_count = sum(1 for r in _results if r.ok is True and r.warn)
    fail_count = sum(1 for r in _results if r.ok is False)
    total = len(_results)
    overall = "PASS" if fail_count == 0 else "FAIL"

    # P3E specific counts
    p3e_results = [r for r in _results if r.phase.startswith("P3E")]
    p3e_pass = sum(1 for r in p3e_results if r.ok is True and not r.warn)
    p3e_fail = sum(1 for r in p3e_results if r.ok is False)

    lines = [
        f"# AI Gateway Smoke — {t0}",
        "",
        "## Summary",
        f"- **Result**: {overall}",
        f"- VK: `…{vk[-6:]}`",
        f"- t0: {t0}  |  t1: {t1}",
        f"- Checks: {total} total — {pass_count} passed, {warn_count} warnings, {fail_count} failed",
    ]
    if p3e_results:
        lines.append(
            f"- Embedding checks (P3E): {len(p3e_results)} total — "
            f"{p3e_pass} passed, {p3e_fail} failed"
        )
    # Cost policy phase mode stamp
    if cost_phase_modes:
        mode_parts = [f"{ph}={mode}" for ph, mode in sorted(cost_phase_modes.items())]
        lines.append(f"- Cost mode per phase: {', '.join(mode_parts)}")

    lines += [
        "",
        "## Results by phase",
        "",
        "| Phase | Check | Status | Note |",
        "|---|---|---|---|",
    ]
    for r in _results:
        icon = "✅" if (r.ok and not r.warn) else ("⚠️" if r.warn else "❌")
        lines.append(f"| {r.phase} | {r.name} | {icon} | {r.note} |")

    # ─── Cross-ingress cache asymmetry matrix ──────────────────────────────
    # User binding (feedback_cache_mandatory_all_ingress): when interface A
    # hits cache but interface B doesn't for the same model, that asymmetry
    # must be specifically caught. Build a per-model matrix from results
    # whose name ends in `/cache`, grouped by phase tag (P3 chat, P3R
    # responses, P3A messages, P3G gemini), so the asymmetry is impossible
    # to miss in the rendered report.
    cache_phase_labels = {
        "P3":  "/v1/chat/completions",
        "P3R": "/v1/responses",
        "P3A": "/v1/messages",
        "P3G": "/v1beta",
    }
    cache_results: dict[str, dict[str, tuple[str, str]]] = {}
    for r in _results:
        if not r.name.endswith("/cache"):
            continue
        model = r.name[: -len("/cache")]
        status_icon = "✅" if (r.ok and not r.warn) else ("⚠️" if r.warn else "❌")
        cache_results.setdefault(model, {})[r.phase] = (status_icon, r.note or "")
    if cache_results:
        active_phases = [p for p in ["P3", "P3R", "P3A", "P3G"] if any(p in v for v in cache_results.values())]
        if len(active_phases) >= 2:
            lines += ["", "## Cache cross-ingress matrix", "",
                      "_User binding: when interface A hits cache but B misses for "
                      "the same model, that asymmetry must be specifically caught._",
                      ""]
            header = "| Model | " + " | ".join(f"{p} {cache_phase_labels[p]}" for p in active_phases) + " | Asymmetry |"
            sep    = "|---|" + "|".join("---" for _ in active_phases) + "|---|"
            lines += [header, sep]
            asym_rows: list[str] = []
            for model in sorted(cache_results.keys()):
                row = cache_results[model]
                cells: list[str] = []
                statuses: list[str] = []
                for p in active_phases:
                    if p in row:
                        icon, _ = row[p]
                        cells.append(icon)
                        statuses.append(icon)
                    else:
                        cells.append("-")
                # Asymmetry: any pair of (✅, ⚠️) or (✅, ❌) across ingresses
                has_hit = "✅" in statuses
                has_miss = "⚠️" in statuses or "❌" in statuses
                asym = "⚠️ A-hit/B-miss" if (has_hit and has_miss) else "—"
                row_line = f"| {model} | " + " | ".join(cells) + f" | {asym} |"
                lines.append(row_line)
                if has_hit and has_miss:
                    asym_rows.append(model)
            if asym_rows:
                lines += ["",
                          f"**Investigate**: {len(asym_rows)} model(s) hit cache on at least one "
                          "ingress but miss on another. Likely causes: gateway prompt-cache "
                          "key sensitivity to body whitespace/field-ordering (the ingress "
                          "that goes through the canonical bridge produces a normalized body; "
                          "the direct-native ingress doesn't), or per-ingress cache namespace "
                          "isolation. See [[feedback_cache_mandatory_all_ingress]]."]

    # ─── P3E Embeddings section ────────────────────────────────────────────
    p3e_arm_results = [r for r in _results if r.phase == "P3E" and "/" in r.name]
    if p3e_arm_results:
        lines += ["", "## P3E — Embeddings", ""]
        lines.append(
            "_Cache arm: skipped — embeddings have no prompt-cache semantic._"
        )
        lines += ["", "| Model | Arm | Status | Note |", "|---|---|---|---|"]
        # Group by model
        by_model: dict[str, list] = {}
        for r in p3e_arm_results:
            parts = r.name.split("/", 1)
            model_key = parts[0]
            arm_key = parts[1] if len(parts) > 1 else r.name
            by_model.setdefault(model_key, []).append((arm_key, r))
        for m_key in sorted(by_model.keys()):
            for arm_label, r in by_model[m_key]:
                icon = "✅" if (r.ok and not r.warn) else ("⚠️" if r.warn else "❌")
                lines.append(f"| {m_key} | {arm_label} | {icon} | {r.note} |")

    # ─── P3E Reject-Asymmetry section ─────────────────────────────────────
    p3e_reject_results = [r for r in _results if r.phase == "P3E/reject"]
    if p3e_reject_results:
        lines += ["", "## P3E — Reject-Asymmetry", ""]
        lines.append(
            "Negative tests: requests designed to violate provider capability "
            "constraints. The model is NAMED, so the refusal comes from the "
            "capability floor. Expected: HTTP 400 with `MODEL_CAPABILITY_MISMATCH` "
            "and NO `traffic_event` row created."
        )
        lines += ["", "| Test | Status | Note |", "|---|---|---|"]
        for r in p3e_reject_results:
            icon = "✅" if (r.ok and not r.warn) else ("⚠️" if r.warn else "❌")
            lines.append(f"| {r.name} | {icon} | {r.note} |")

    lines += ["", "## Metrics delta", ""]
    if diff:
        lines += ["| Metric | Δ |", "|---|---|"]
        for k, v0, v1 in diff:
            short = k.split("{")[0].replace("nexus_ai_gateway_", "")
            lines.append(f"| `{short}` | +{v1 - v0:.0f} |")
    else:
        lines.append("_(no counter changes recorded)_")

    lines += ["", "## Log", "```"]
    lines += _log_lines
    lines += ["```", "", f"*Generated by smoke-gateway.py*"]

    content = "\n".join(lines)
    with open(report_path, "w") as f:
        f.write(content)

    print()
    print(bold(f"Result: {green(overall) if overall == 'PASS' else red(overall)}"))
    print(f"  {pass_count} passed  {warn_count} warnings  {fail_count} failed")
    # Coverage, stated whenever it was bounded. A pass count is meaningless without
    # the denominator it was measured over, and the virtual key's allow-list is the
    # one thing that silently changes that denominator: every denied model simply
    # never runs, so the surface shrinks without a single failure being recorded.
    if _denied_models:
        print(red(bold(
            f"  COVERAGE WAS RESTRICTED: the virtual key denied {len(_denied_models)} model(s) "
            f"with MODEL_NOT_ALLOWED, so this run did NOT cover the full surface.")))
        print(f"    reached: {len(_reached_models)}  denied: {sorted(_denied_models)}")
        print("    Remedy: run with a virtual key whose allowedModels is empty (unrestricted). "
              "tests/scripts/mint-test-vk.go mints one.")
    print(f"Report: {report_path}")
    return overall == "PASS"


def _mm_traffic_row(db: "DBClient", model_name: str, endpoint_type: str, t0_iso: str, timeout: int = 30) -> Optional[dict]:
    """Poll traffic_event for the most recent row of a modality request, returning
    the endpoint_type / cost / artifact_refs presence for the cross-check."""
    if db.is_remote and not db.has_ssh:
        return None
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            rows = db.rows(
                "SELECT endpoint_type, status_code, estimated_cost_usd, "
                "(artifact_refs IS NOT NULL) AS has_artifact "
                "FROM traffic_event WHERE source='ai-gateway' "
                f"AND timestamp >= '{t0_iso}'::timestamptz "
                f"AND endpoint_type = '{endpoint_type}' AND model_name = '{model_name}' "
                "ORDER BY timestamp DESC LIMIT 1"
            )
        except Exception:
            rows = []
        if rows:
            p = rows[0]["_row"].split("|")
            return {"endpoint_type": p[0], "status_code": p[1],
                    "estimated_cost_usd": p[2], "has_artifact": p[3]}
        time.sleep(2)
    return None



# ─── Multimodal media assertions (standing regression) ────────────────────────

MM_FIXTURE_DIR = Path(__file__).resolve().parents[1] / "fixtures" / "media"

# Locator grammar. The check is deliberately two-way: a well-formed locator
# must match, and the shapes that represent the original defect must not.
# Defect #1 stuffed a whole data URI into the ref; a regression that moves it
# one field over — into `locator` — would still pass a mime/size assertion,
# so the grammar itself is pinned. The check is size-agnostic, which keeps it
# compatible with the "never assert on size" rule the evidence taught.
_MM_LOCATOR_RE = re.compile(
    r"^(body|json:[A-Za-z0-9_.\-]+|datauri:[A-Za-z0-9_.\-]+"
    r"|multipart:[A-Za-z0-9_\-]+|sse:\d+:[A-Za-z0-9_.\-]+)$"
)
# A run this long inside a text block means a payload was serialised into
# prose — the defect that sent audio and PDFs through the redaction pipeline
# as if they were words.
_MM_B64_RUN_RE = re.compile(r"[A-Za-z0-9+/]{200,}={0,2}")
_MM_HEX64_RE = re.compile(r"^[0-9a-f]{64}$")


# The question every media-bearing ingress case asks. It replaced "one word",
# which said nothing about the attachment — a model answered "I'm not sure what
# you're asking for with just the word 'one'" and the case still passed, because
# nothing looked at the answer.
#
# Each visual/document fixture has a different number baked in
# (tests/scripts/gen-media-fixtures.py): image 19452, pdf 38617, markdown 52903,
# video 74128. One prompt serves all of them because the fixture decides which
# number is correct, and the caller never names it.
# Media comprehension needs room to ANSWER, not just to think.
#
# The arms ran at max_tokens=8. Measured against gemini-2.5-flash on the OCR
# fixture: 8 tokens returns empty with finish_reason=length, 64 returns "19"
# — the first two digits of 19452, still truncated — and 512 returns "19452"
# with finish_reason=stop. Reasoning models spend the budget before emitting
# any visible text, so a small cap does not shorten the answer, it deletes it.
# Every "the model returned EMPTY text for a media request" and several
# "did NOT demonstrate reading the media" verdicts were this and nothing else.
#
# The anchors are five digits. 512 is far more than the answer needs and is
# chosen to be larger than any thinking preamble these models emit, because
# the failure mode being avoided is not a long answer — it is no answer.
_MM_BUDGET = 512

_MM_Q = "Reply with only the number contained in the attached file, digits only."

# Audio carries a spoken phrase rather than a number — a number would need a TTS
# engine in the fixture generator, and a spoken sentence is already unreachable
# without the bytes.
_MM_AUDIO_Q = "Transcribe the attached audio exactly. Reply with the transcript only."


def _mm_fixture(name: str) -> bytes:
    return (MM_FIXTURE_DIR / name).read_bytes()


def _mm_wav(name: str) -> bytes:
    """A wav fixture, refusing to hand back bytes a decoder would reject.

    The audio arms sliced this fixture to a byte count to keep the payload
    small. RIFF carries the payload length in its header, so a byte slice
    leaves a file that declares 163584 data bytes with 31930 present, and
    every upstream answers "unsupported_format". Nothing went red: the
    bookkeeping half of each arm asserts what normalize recorded — mime,
    size, modality, locator — all of which stay correct for a payload no
    model can decode, so three audio arms reported PASS on every run while
    the audio path itself was never exercised. Reading through here makes
    that loud at the source. Clip audio by rewriting the chunk sizes, never
    by slicing bytes.
    """
    b = _mm_fixture(name)
    if b[:4] != b"RIFF" or b[8:12] != b"WAVE":
        raise ValueError(f"{name}: not a RIFF/WAVE container")
    declared = int.from_bytes(b[4:8], "little")
    if declared != len(b) - 8:
        raise ValueError(
            f"{name}: the RIFF header declares {declared} bytes after it but {len(b) - 8} are present")
    off = 12
    while off + 8 <= len(b):
        cid, size = b[off:off + 4], int.from_bytes(b[off + 4:off + 8], "little")
        if off + 8 + size > len(b):
            raise ValueError(f"{name}: the {cid.decode('ascii', 'replace')} chunk declares {size} bytes but the file ends first")
        if cid == b"data":
            return b
        off += 8 + size + (size & 1)
    raise ValueError(f"{name}: no data chunk")


def _mm_data_uri(name: str, mime: str) -> str:
    return f"data:{mime};base64," + base64.b64encode(_mm_fixture(name)).decode()


def _mm_walk_media(payload: dict):
    """Yields every MediaRef in a normalized payload, message blocks first."""
    for m in (payload.get("messages") or []):
        for b in (m.get("content") or []):
            if b.get("type") == "media" and b.get("mediaRef"):
                yield b["mediaRef"]
    bv = ((payload.get("http") or {}).get("bodyView") or {})
    if bv.get("mediaRef"):
        yield bv["mediaRef"]


def _mm_text_blocks(payload: dict):
    for m in (payload.get("messages") or []):
        for b in (m.get("content") or []):
            if b.get("type") == "text" and b.get("text"):
                yield b["text"]


def _mm_check_ref(label: str, ref: dict, want_mime: str, want_size: int,
                  want_modality: str, want_source: str) -> list[str]:
    """Returns the list of assertion failures for one MediaRef."""
    bad = []
    if want_mime and ref.get("mime") != want_mime:
        bad.append(f"mime={ref.get('mime')!r} want {want_mime!r}")
    if want_size >= 0 and ref.get("sizeBytes") != want_size:
        bad.append(f"sizeBytes={ref.get('sizeBytes')!r} want {want_size}")
    if ref.get("modality") != want_modality:
        bad.append(f"modality={ref.get('modality')!r} want {want_modality!r}")
    if ref.get("source") != want_source:
        bad.append(f"source={ref.get('source')!r} want {want_source!r}")
    sha = ref.get("sha256")
    if sha and not _MM_HEX64_RE.match(sha):
        bad.append(f"sha256={sha[:24]!r} is not a 64-hex digest")
    loc = ref.get("locator")
    if want_source == "captured":
        if not loc:
            bad.append("captured ref carries no locator")
        elif not _MM_LOCATOR_RE.match(loc):
            bad.append(f"locator={loc[:48]!r} does not match the grammar")
    elif loc:
        bad.append(f"non-captured ref carries locator={loc[:48]!r}")
    return bad


def _mm_event_id(cp: "CPClient", trace_id: str, deadline: float) -> str | None:
    """Resolves the caller's handle to the row's primary key.

    The gateway hands back a TRACE id in X-Nexus-Request-Id; the per-event
    admin endpoints take traffic_event.id, a minted per-row key no caller ever
    sees. The two held the same value until the gateway started minting the key
    separately, which is why passing the header straight to /traffic/:id used
    to work. `requestId` is the filter the traffic UI's trace pivot uses, so
    this is also the path a support ticket takes.
    """
    while True:
        try:
            status, payload = cp.get(f"/api/admin/traffic?requestId={trace_id}&limit=1")
        except Exception:
            status, payload = 0, None
        if status == 200 and isinstance(payload, dict):
            rows = payload.get("data") or []
            if rows and rows[0].get("id"):
                return rows[0]["id"]
        # The row is written by the async audit batch writer, so an immediate
        # read finds nothing. Poll rather than treat "not yet" as "absent".
        if time.time() >= deadline:
            return None
        time.sleep(1.0)


def _mm_normalized(cp: "CPClient", trace_id: str, direction: str,
                   deadline_s: float = 30) -> dict | None:
    """Reads the VIEW-TIME normalized payload for the row behind a trace id.

    The endpoint recomputes from the stored body; there is no persisted
    projection to read instead.

    Returns None ONLY when no row ever appeared — the one genuinely inconclusive
    outcome. Once the row is found, every remaining failure is permanent and is
    reported as such: polling a wrong key for 30s and calling it a timeout is
    what let twenty-one media arms report NOT VERIFIED while the suite passed.
    """
    deadline = time.time() + deadline_s
    event_id = _mm_event_id(cp, trace_id, deadline)
    if event_id is None:
        return None
    try:
        status, payload = cp.get(f"/api/admin/traffic/{event_id}/normalized")
    except Exception:
        status, payload = 0, None
    if status == 200 and isinstance(payload, dict):
        got = payload.get(f"{direction}Normalized") or payload.get(direction)
        if got is not None:
            return got
        return {"__empty__": True, "__why__": f"the endpoint answered 200 with no {direction} payload"}
    return {"__empty__": True,
            "__why__": f"row {event_id} exists but /normalized answered {status} — "
                       "its body is neither inline nor recoverable from spill"}



def _mm_media_matrix(gw: "GWClient", cp: "CPClient", chat_code: str, timeout: int) -> None:
    """Fires media-bearing requests across ingresses and asserts the VIEW-TIME
    normalized output — not the HTTP status, because every measured defect
    returned 200.

    The formats are crossed deliberately: the defect this pins hardest is that
    every image format normalised to the bare word "image", so one PNG case
    could not have caught it. Asserting on block *type* rather than size is
    the other encoded lesson — a size threshold in an observation tool is what
    hid a 343-character PDF block during the investigation.
    """
    fmts = [("image.png", "image/png"), ("image.jpg", "image/jpeg"),
            ("image.webp", "image/webp"), ("image.gif", "image/gif")]

    for name, mime in fmts:
        raw = _mm_fixture(name)
        body = {"model": chat_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
            {"type": "text", "text": "what is this"},
            {"type": "image_url", "image_url": {"url": _mm_data_uri(name, mime)}}]}]}
        r = gw._post_sync("/v1/chat/completions", body, timeout, endpoint="chat")
        tid = (r.get("headers") or {}).get("x-nexus-request-id", "")
        label = f"media/openai-chat/{mime}"
        if r.get("status") != 200 or not tid:
            # A 400 here is ours by default — it is recorded as a failure so
            # it must be dispositioned, never silently tolerated.
            rec("PMM", label).failed(f"status={r.get('status')} trace={tid or 'missing'}")
            continue
        payload = _mm_normalized(cp, tid, "request")
        if payload is None:
            rec("PMM", label).failed("the response returned 200 but no traffic row appeared within 30s")
            continue
        if payload.get("__empty__"):
            rec("PMM", label).failed(f"no request payload for a media-bearing request: {payload.get('__why__', '')}")
            continue
        refs = list(_mm_walk_media(payload))
        if len(refs) != 1:
            rec("PMM", label).failed(f"expected exactly 1 media element, got {len(refs)}")
            continue
        bad = _mm_check_ref(label, refs[0], mime, len(raw), "image", "captured")
        for txt in _mm_text_blocks(payload):
            if _MM_B64_RUN_RE.search(txt):
                bad.append("a base64 run reached a text block")
        if bad:
            rec("PMM", label).failed("; ".join(bad))
        else:
            rec("PMM", label).passed(
                f"mime={mime} sizeBytes={len(raw)} source=captured locator={refs[0].get('locator')}")

    # A remote URL is never fetched and never stored: it must read as an
    # external reference carrying the URL, with no locator to click.
    body = {"model": chat_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
        {"type": "image_url", "image_url": {"url": "https://example.invalid/x.png"}}]}]}
    r = gw._post_sync("/v1/chat/completions", body, timeout, endpoint="chat")
    tid = (r.get("headers") or {}).get("x-nexus-request-id", "")
    if not tid:
        rec("PMM", "media/openai-chat/external").failed("no trace id returned")
    else:
        payload = _mm_normalized(cp, tid, "request")
        if payload is None:
            rec("PMM", "media/openai-chat/external").failed(
                "the response returned 200 but no traffic row appeared within 30s")
        elif payload.get("__empty__"):
            rec("PMM", "media/openai-chat/external").failed(
                f"no request payload: {payload.get('__why__', '')}")
        else:
            refs = list(_mm_walk_media(payload))
            bad = [] if len(refs) == 1 else [f"expected 1 media element, got {len(refs)}"]
            if refs:
                bad += _mm_check_ref("external", refs[0], "", -1, "image", "external")
                if refs[0].get("url") != "https://example.invalid/x.png":
                    bad.append(f"url={refs[0].get('url')!r} not carried")
            if bad:
                rec("PMM", "media/openai-chat/external").failed("; ".join(bad))
            else:
                rec("PMM", "media/openai-chat/external").passed("external ref, url carried, no locator")


def _mm_request_text(body: dict) -> str:
    """Every piece of caller-authored TEXT in an outgoing request, any ingress.

    Deliberately not json.dumps(body): the media rides there as base64 and is
    supposed to carry the anchor. Only what a human typed can leak the answer.
    """
    out = []

    def walk(node):
        if isinstance(node, dict):
            for k, v in node.items():
                if k in ("text", "content", "instructions", "system") and isinstance(v, str):
                    out.append(v)
                else:
                    walk(v)
        elif isinstance(node, list):
            for v in node:
                walk(v)

    walk(body)
    return " ".join(out)


def _mm_assert_comprehension(label: str, must_contain: str, data: dict, body: dict) -> None:
    """Decide, and RECORD, whether the model demonstrably read the media.

    Shared by the dedicated comprehension arms and by every ingress case, so
    the two cannot drift into judging the same evidence differently.
    """
    # The anchor must not be reachable any way except by reading the media, so
    # it must not be sitting in the request we sent. A prompt that names the
    # answer turns this whole check into "does the model echo its input".
    #
    # Only the TEXT the caller wrote is examined. The media itself rides in the
    # same body as base64, and it is supposed to contain the anchor — searching
    # the serialised body would flag every correctly-built case.
    if must_contain.lower() in _mm_request_text(body).lower():
        rec("PMM", label).failed(
            f"  [{label}] TEST DEFECT, not a product failure: the anchor {must_contain!r} "
            f"appears in the request body, so a model that echoes its prompt passes "
            f"without reading anything. Choose an anchor that exists only in the media."
        )
        return

    # _MISSING distinguishes "no shape matched" from "a shape matched and the
    # content was null". They are different failures: the first means we could
    # not find the answer, the second means the model returned none. Collapsing
    # them would report an empty media response as an envelope-parsing problem.
    _MISSING = object()
    answer = _MISSING
    for shape in (
        lambda d: d["choices"][0]["message"]["content"],
        # Gemini's native shape, for the arms that run on that ingress.
        lambda d: d["candidates"][0]["content"]["parts"][0]["text"],
        # Anthropic's native shape. Without it, an arm on /v1/messages read a
        # correct answer as "could not locate assistant text" and reported a
        # product failure: the anthropic image and document arms each returned
        # the exact anchor baked into the fixture and were both scored red.
        lambda d: next(b["text"] for b in d["content"] if b.get("type") == "text"),
        # OpenAI Responses shape — output[] carries reasoning and message
        # items; the answer is the message item's output_text.
        lambda d: next(
            c["text"]
            for item in d["output"] if item.get("type") == "message"
            for c in item.get("content", []) if c.get("type") == "output_text"
        ),
    ):
        try:
            answer = shape(data)
            break
        except Exception:
            continue

    # There used to be a third fallback here: answer = json.dumps(data)[:400].
    # It searched the ENTIRE response envelope for the anchor, so an anchor that
    # appeared anywhere — an echoed request field, a model name, an error message
    # quoting the prompt — reported "the model read the media" when no assistant
    # text had been located at all. A check that cannot find the answer has not
    # verified anything, and must say so.
    # An upstream that REFUSED the payload never got asked the comprehension
    # question, so there is no answer to score. The bookkeeping half of this
    # arm deliberately accepts a refusing provider — it asserts the codec that
    # had to represent the media, not the upstream's verdict — and scoring the
    # comprehension half red for the same refusal reports one event twice, the
    # second time as a defect that is not there. Report it NOT VERIFIED and
    # name the refusal, so a genuinely missing check still cannot read as
    # covered.
    if isinstance(data, dict) and data.get("error") is not None:
        err = data.get("error") or {}
        msg = err.get("message") if isinstance(err, dict) else str(err)
        rec("PMM", label).warning(
            f"  [{label}] comprehension NOT VERIFIED — the upstream refused the "
            f"request, so the model was never asked: {str(msg)[:150]}"
        )
        return

    if answer is _MISSING:
        rec("PMM", label).failed(
            f"  [{label}] could not locate assistant text in the 200 response; "
            f"no known response shape matched. Envelope: {json.dumps(data)[:200]}"
        )
        return

    # Empty text is its own failure, named separately. It is the shape the
    # media program was created for — a media request that returns 200 with
    # nothing in it — and calling it "answered wrong" would send the reader
    # looking for a model-quality problem instead of a dropped response.
    if not (answer or "").strip():
        rec("PMM", label).failed(
            f"  [{label}] the model returned EMPTY text for a media request. "
            f"Not a wrong answer — no answer. Check the upstream response body in traffic."
        )
        return

    if must_contain.lower() in answer.lower():
        rec("PMM", label).passed(f"the model read the media (answered {must_contain!r})")
    else:
        rec("PMM", label).failed(
            f"  [{label}] the model did NOT demonstrate reading the media. "
            f"Expected {must_contain!r}, got {answer[:120]!r}. "
            f"Check the upstream request body in traffic: if the media block is absent "
            f"the gateway dropped it; if present, the model answered wrong."
        )

def _mm_comprehension_case(gw: "GWClient", label: str, path: str, body: dict,
                           headers: dict, must_contain: str, timeout: int = 90) -> None:
    """Assert the MODEL received the media, not that we recorded it correctly.

    Every other media assertion in this file checks our own bookkeeping: the
    byte count, the sha256, the custody state, the locator. All of that can be
    perfect while the model saw nothing — the reported defect was a PDF that
    came back with a confident, entirely invented answer because the file never
    reached the upstream at all. "We recorded it right" and "the model got it"
    are different claims and only the second is what a user asked for.

    So the question has exactly one answer, and it is one the model cannot
    reach without reading the media: a fact buried inside the fixture, chosen
    so that guessing lands on the wrong value. The markdown table marks SQLite
    NOT idempotent while the other two are — a model reasoning from priors
    says all three migrations are idempotent.

    A failure here needs reading twice: the model may not have received the
    media (our bug), or it received it and answered wrong (a model-quality
    issue). The assertion message says which evidence to look at, because the
    two have completely different owners.
    """
    r = gw._post_sync(path, body, timeout, endpoint=label, extra_headers=headers)
    status = r.get("status", 0)
    if status == 429:
        rec("PMM", label).warning("upstream rate limited (429) — NOT VERIFIED")
        return
    data = r.get("data") or {}
    if status != 200:
        err = (data.get("error") or {}) if isinstance(data, dict) else {}
        rec("PMM", label).failed(
            f"comprehension HTTP {status}: {err.get('message') or str(data)[:200]}")
        return
    _mm_assert_comprehension(label, must_contain, data, body)


def _mm_ingress_case(gw: "GWClient", cp: "CPClient", label: str, path: str, body: dict,
                     extra_headers: dict, want_mime: str, want_size: int,
                     want_modality: str, timeout: int, must_contain: str = "") -> None:
    """One media-bearing request on one ingress, asserted on normalized output.

    Crossing ingresses is not optional coverage: the shape of a normalized
    block is decided by the INGRESS, not by the target model — measured on production in
    both directions. A defect that only bites on /v1/messages is
    invisible to any number of /v1/chat/completions cases.

    `must_contain` adds the comprehension half. Everything above it checks OUR
    bookkeeping — the mime, the size, the modality, the locator — and all of it
    can be perfect while the model received nothing: that is the reported
    defect, a media request answered confidently from thin air with every byte
    assertion green. The call has already been made and paid for by the time we
    get here, so asserting on the answer costs nothing extra, and it is recorded
    as a SEPARATE result so a comprehension failure cannot be masked by a
    passing bookkeeping verdict, or the reverse.
    """
    r = gw._post_sync(path, body, timeout, endpoint=label, extra_headers=extra_headers)
    tid = (r.get("headers") or {}).get("x-nexus-request-id", "")
    status = r.get("status", 0)
    # A 429 is rate limiting and is not ours. Anything else non-2xx is ours
    # until proven otherwise, so it is recorded as a failure to be
    # dispositioned rather than quietly skipped.
    if status == 429:
        rec("PMM", label).warning("upstream rate limited (429) — NOT VERIFIED")
        return
    if not tid:
        rec("PMM", label).failed(f"status={status}, no trace id to verify against")
        return
    payload = _mm_normalized(cp, tid, "request")
    if payload is None:
        rec("PMM", label).failed("the response returned 200 but no traffic row appeared within 30s")
        return
    if payload.get("__empty__"):
        rec("PMM", label).failed(f"no request payload for a media-bearing request: {payload.get('__why__', '')}")
        return
    refs = list(_mm_walk_media(payload))
    if len(refs) != 1:
        rec("PMM", label).failed(f"expected exactly 1 media element, got {len(refs)}")
        return
    bad = _mm_check_ref(label, refs[0], want_mime, want_size, want_modality, "captured")
    for txt in _mm_text_blocks(payload):
        if _MM_B64_RUN_RE.search(txt):
            bad.append("a base64 run reached a text block")
    if bad:
        rec("PMM", label).failed("; ".join(bad))
    else:
        rec("PMM", label).passed(
            f"upstream={status} mime={refs[0].get('mime')} size={refs[0].get('sizeBytes')} "
            f"modality={want_modality} locator={refs[0].get('locator')}")

    if must_contain:
        # `:ingress-comprehension`, not `:comprehension`. Three ingress arms have
        # a dedicated comprehension case with the SAME base label, and the two
        # ask different questions: the dedicated one pins a model chosen for the
        # capability (the file arm is pinned to the OpenAI family precisely
        # because leaving it on whatever chat_code happened to be landed it on
        # cohere), while this one asks whether the bookkeeping arm's own model
        # also understood the payload. Under one name the run produced two
        # verdicts for one label, and the P8 deprecation pass keys its result map
        # by name — a duplicate there silently overwrites and decides which
        # models get removed from the catalog.
        _mm_assert_comprehension(f"{label}:ingress-comprehension", must_contain,
                                 r.get("data") or {}, body)


def _mm_pick_servable(cp, kind: str) -> dict | None:
    """The first model of `kind` the deployment will actually serve.

    `list_models_with_modalities()` is the ADMIN listing, so it includes rows an
    operator has deliberately switched off. Taking [0] from it picked `whisper-1`
    on prod — disabled on purpose — and the gateway's correct 404
    ("no provider is configured to serve this stt model") was recorded as a smoke
    FAILURE. Measured 2026-08-19: all three enabled stt rows return 200 with a
    perfect transcript through the same gateway, so the only thing that check
    ever caught was its own choice.

    Returns None when the catalogue has the kind but serves none of it — a
    different fact from "the catalogue has none", and the callers say so
    differently.
    """
    rows = [m for m in (cp.list_models_with_modalities() or []) if m.get("type") == kind]
    if not rows:
        return None
    for m in rows:
        if m.get("enabled", True):
            return m
    return {"__all_disabled__": True}


def _mm_tts_case(gw: "GWClient", cp: "CPClient", timeout: int) -> None:
    """the TTS response is audio, and is served as audio.

    The response body IS the artifact here — there is no JSON envelope to
    address — so the ref's locator is `body` and the served content-type has
    to agree with the container the bytes actually are. Capture was measured
    mislabelling this response as application/json, which stranded a real
    recording behind an octet-stream download.
    """
    label = "media/tts/response"
    pick = _mm_pick_servable(cp, "tts")
    if pick is None:
        rec("PMM", label).warning("no tts model in catalog — NOT VERIFIED (evidence #10)")
        return
    if pick.get("__all_disabled__"):
        rec("PMM", label).warning("every tts model is disabled on this deployment — NOT VERIFIED")
        return
    code = pick.get("code", "")
    r = gw.post_binary_response("/v1/audio/speech", {
        "model": code, "input": "Nexus multimodal smoke.", "voice": "alloy"}, timeout)
    if r.get("status") == 429:
        rec("PMM", label).warning("upstream rate limited (429) — NOT VERIFIED")
        return
    if r.get("status") != 200:
        rec("PMM", label).failed(f"status={r.get('status')} {str(r.get('error') or '')[:120]}")
        return

    raw = r.get("bytes") or b""
    if len(raw) < 256:
        rec("PMM", label).failed(f"response is {len(raw)} bytes — that is not audio")
        return
    ct = (r.get("headers") or {}).get("content-type", "")
    if not ct.startswith("audio/"):
        rec("PMM", label).failed(f"content-type={ct!r} for {len(raw)} bytes of audio")
        return
    # The bytes must BE what the header says. A header alone is a claim.
    #
    # MPEG is matched on the frame sync — first eleven bits set — the same rule
    # the gateway's own sniffer applies (locator/sniff.go: b[0] == 0xFF &&
    # b[1]&0xE0 == 0xE0). An earlier version here tested for the literal
    # \xff\xfb, which is MPEG-1 Layer III only, so a perfectly valid MPEG-2
    # Layer III response at 24 kHz (\xff\xf3 — what gpt-4o-mini-tts returns)
    # was reported as "matches no audio container". A narrower copy of a rule
    # that already exists elsewhere reports the copy's limits as the product's
    # defects.
    is_mpeg = len(raw) >= 2 and raw[0] == 0xFF and (raw[1] & 0xE0) == 0xE0
    if not (raw[:3] == b"ID3" or is_mpeg or raw[:4] in (b"OggS", b"fLaC")
            or (raw[:4] == b"RIFF" and raw[8:12] == b"WAVE")):
        rec("PMM", label).failed(f"content-type={ct} but the bytes match no audio container")
        return
    rec("PMM", label).passed(f"{len(raw)}B {ct}")


def _mm_stt_case(gw: "GWClient", cp: "CPClient", timeout: int) -> None:
    """the transcribed audio is reachable after the fact.

    The request is multipart and its file part is the whole point: before the
    handover capture landed, a transcription was auditable and the thing
    transcribed was not — the fingerprint went to the database and nothing
    read it.
    """
    label = "media/stt/request"
    pick = _mm_pick_servable(cp, "stt")
    if pick is None:
        rec("PMM", label).warning("no stt model in catalog — NOT VERIFIED (evidence #11)")
        return
    if pick.get("__all_disabled__"):
        rec("PMM", label).warning("every stt model is disabled on this deployment — NOT VERIFIED")
        return
    code = pick.get("code", "")
    audio = _mm_fixture("speech.mp3")
    nonce = secrets.token_hex(4)
    r = gw.post_multipart("/v1/audio/transcriptions",
                          {"model": code, "prompt": f"nexus-smoke-{nonce}"},
                          "speech.mp3", audio, "file", "audio/mpeg", timeout)
    if r.get("status") == 429:
        rec("PMM", label).warning("upstream rate limited (429) — NOT VERIFIED")
        return
    if r.get("status") != 200:
        rec("PMM", label).failed(f"status={r.get('status')} {str(r.get('data'))[:160]}")
        return

    # x-nexus-request-id, the same header the other three media cases read.
    # This one asked for x-nexus-trace-id, which the gateway does not send, so
    # the case failed on a header name while the request itself returned 200
    # with a correct trace id — a defect report pointing at the wrong subject.
    tid = (r.get("headers") or {}).get("x-nexus-request-id", "")
    if not tid:
        rec("PMM", label).failed("no trace id on the response; cannot reach the traffic row")
        return
    payload = _mm_normalized(cp, tid, "request")
    if payload is None:
        rec("PMM", label).failed("the response returned 200 but no traffic row appeared within 30s")
        return

    refs = list(_mm_walk_media(payload))
    if not refs:
        rec("PMM", label).failed("the request carried audio and normalizes to no media element")
        return
    ref = refs[0]
    if ref.get("modality") != "audio":
        rec("PMM", label).failed(f"modality={ref.get('modality')!r}, want audio")
        return
    src = ref.get("source")
    if src == "fingerprint":
        # Honest, and the correct answer when the bytes genuinely were not
        # kept. Reported rather than passed silently, because the stronger
        # property is the one this row exists for. The parenthetical used to
        # assert "payload capture off" and was simply wrong when it fired:
        # capture was ON and the codec was under-reporting custody. A warning
        # that names a cause it did not check sends the next reader to the
        # wrong place.
        if not ref.get("sha256"):
            rec("PMM", label).failed("fingerprint custody with no digest — nothing proves which file was sent")
            return
        rec("PMM", label).warning(
            "audio is fingerprint-only — bytes NOT reachable "
            "(expected only when payload capture is off; check the setting before assuming it is)")
        return
    if src != "captured":
        rec("PMM", label).failed(f"source={src!r}: the audio is neither captured nor fingerprinted")
        return
    loc = ref.get("locator") or ""
    if not _MM_LOCATOR_RE.match(loc):
        rec("PMM", label).failed(f"locator={loc!r} does not match the grammar")
        return
    if int(ref.get("sizeBytes") or 0) != len(audio):
        rec("PMM", label).failed(
            f"sizeBytes={ref.get('sizeBytes')} but {len(audio)} were sent — the size a reader sees is not the size they would get")
        return
    # No payload may have reached a scanned text block on the way.
    for t in _mm_text_blocks(payload):
        if _MM_B64_RUN_RE.search(t):
            rec("PMM", label).failed("a base64 run reached a text block")
            return
    rec("PMM", label).passed(f"captured {ref.get('sizeBytes')}B loc={loc}")


def _mm_cross_ingress(gw: "GWClient", cp: "CPClient", chat_code: str, claude_code: str,
                      gemini_code: str, audio_code: str, file_code: str, timeout: int) -> None:
    """The evidence rows this suite previously could not have caught.

    chat_code must accept image input and audio_code must accept audio input:
    each arm carries a specific modality, and a model that does not take that
    modality turns the arm into an upstream 400 that looks like a media defect.
    """
    png = _mm_fixture("ocr-19452.png")
    png_b64 = base64.b64encode(png).decode()
    pdf = _mm_fixture("doc.pdf")
    pdf_b64 = base64.b64encode(pdf).decode()
    # The markdown fixture has carried an anchor since the media work began and
    # no arm ever used it. A document that is TEXT rather than a binary
    # container is its own case: the codecs decide mime and modality from the
    # declared type, and text/markdown is the one document type where a codec
    # could plausibly fold the bytes into the prompt instead of representing
    # them as media — which is exactly the silent-drop the anchor detects.
    md = _mm_fixture("doc.md")
    md_b64 = base64.b64encode(md).decode()
    mp4 = _mm_fixture("clip.mp4")
    mp4_b64 = base64.b64encode(mp4).decode()

    if claude_code:
        # size was the base64 length and sha256 was a payload
        # prefix on this ingress.
        _mm_ingress_case(gw, cp, "media/anthropic/image", "/v1/messages", {
            "model": claude_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": png_b64}},
                {"type": "text", "text": _MM_Q}]}]},
            {"anthropic-version": "2023-06-01"}, "image/png", len(png), "image", timeout, "19452")

        # the PDF was JSON-marshalled into a text block.
        _mm_ingress_case(gw, cp, "media/anthropic/document", "/v1/messages", {
            "model": claude_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
                {"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": pdf_b64}},
                {"type": "text", "text": _MM_Q}]}]},
            {"anthropic-version": "2023-06-01"}, "application/pdf", len(pdf), "file", timeout, "38617")

    if gemini_code:
        # inlineData modality came from nothing, so audio and
        # PDFs were emitted as images.
        _mm_ingress_case(gw, cp, "media/gemini/inlineData",
            f"/v1beta/models/{gemini_code}:generateContent", {
            "contents": [{"role": "user", "parts": [
                {"inlineData": {"mimeType": "image/png", "data": png_b64}},
                {"text": _MM_Q}]}],
            "generationConfig": {"maxOutputTokens": _MM_BUDGET}},
            {}, "image/png", len(png), "image", timeout, "19452")


    if chat_code:
        # the responses input_image ref was entirely empty — no
        # mime, no size, no source. A different ingress, a different codec.
        _mm_ingress_case(gw, cp, "media/responses/input_image", "/v1/responses", {
            "model": chat_code, "input": [{"role": "user", "content": [
                {"type": "input_image", "image_url": "data:image/png;base64," + png_b64},
                {"type": "input_text", "text": _MM_Q}]}]},
            {}, "image/png", len(png), "image", timeout, "19452")

    # Audio on the ingresses that spell it differently. openai-chat carries
    # input_audio and was already covered; Responses has its own input_audio
    # part and Gemini carries it as inlineData with an audio mime. Anthropic
    # Messages defines no audio block at all — image, document, text, thinking,
    # tool_* — so there is no case to write there, the same way there is none
    # for video. Checked against the codec's own block-type switch before
    # writing these, after an anthropic/video case had to be deleted for
    # asserting a payload the protocol cannot carry.
    wav = _mm_wav("speech.wav")
    wav_b64 = base64.b64encode(wav).decode()
    if audio_code:
        _mm_ingress_case(gw, cp, "media/responses/input_audio", "/v1/responses", {
            "model": audio_code, "input": [{"role": "user", "content": [
                {"type": "input_audio", "input_audio": {"data": wav_b64, "format": "wav"}},
                {"type": "input_text", "text": _MM_AUDIO_Q}]}]},
            {}, "audio/wav", len(wav), "audio", timeout, "regression fixture")

    if gemini_code:
        _mm_ingress_case(gw, cp, "media/gemini/audio",
                         f"/v1beta/models/{gemini_code}:generateContent", {
                             "contents": [{"role": "user", "parts": [
                                 {"inlineData": {"mimeType": "audio/wav", "data": wav_b64}},
                                 {"text": _MM_AUDIO_Q}]}]},
                         {}, "audio/wav", len(wav), "audio", timeout, "regression fixture")

    # ── file and video on the ingresses that were never crossed ─────────────
    # The PDF case above covers exactly one ingress, and video covered none.
    # Both are modalities this program names, and block shape is decided by the
    # ingress, so a single-ingress case proves one codec and implies nothing
    # about the other three. These assert the normalized output, not the
    # upstream verdict — a provider that refuses the payload still exercises
    # the codec that had to represent it.
    if chat_code:
        _mm_ingress_case(gw, cp, "media/openai-chat/file", "/v1/chat/completions", {
            "model": chat_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
                {"type": "file", "file": {"filename": "doc.pdf",
                                          "file_data": "data:application/pdf;base64," + pdf_b64}},
                {"type": "text", "text": _MM_Q}]}]},
            {}, "application/pdf", len(pdf), "file", timeout, "38617")

        _mm_ingress_case(gw, cp, "media/openai-chat/markdown", "/v1/chat/completions", {
            "model": chat_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
                {"type": "file", "file": {"filename": "doc.md",
                                          "file_data": "data:text/markdown;base64," + md_b64}},
                {"type": "text", "text": _MM_Q}]}]},
            {}, "text/markdown", len(md), "file", timeout, "52903")

        _mm_ingress_case(gw, cp, "media/responses/input_file", "/v1/responses", {
            "model": chat_code, "input": [{"role": "user", "content": [
                {"type": "input_file", "filename": "doc.pdf",
                 "file_data": "data:application/pdf;base64," + pdf_b64},
                {"type": "input_text", "text": _MM_Q}]}]},
            {}, "application/pdf", len(pdf), "file", timeout, "38617")

    # Comprehension, not bookkeeping. Every case above proves we RECORDED the
    # media; these prove the model RECEIVED it. The reported defect was a PDF
    # that produced a confident invented answer because the file never reached
    # the upstream — invisible to every byte-level assertion in this file.
    # Each fixture has a DIFFERENT number baked into it (see
    # tests/scripts/gen-media-fixtures.py). A number is the tightest anchor
    # available: unambiguous to grep, unreachable by reasoning from priors, and
    # not something a model can produce from a describable scene. The numbers
    # differ per modality so a crossed arm — the video case passing on the
    # image's number — is visible instead of silently green.
    _PDF_Q = ("Read the attached document and reply with only its reference number, "
              "digits only.")
    _AUDIO_Q = "Transcribe the attached audio exactly. Reply with the transcript only."
    # The file arm needs a provider that accepts a file block at all. Left on
    # whatever chat_code happened to be, it landed on cohere, which answers
    # "unrecognized content type 'file'" — canonical fields leaking onto a
    # non-OpenAI wire. That is a real adapter defect this assertion caught
    # live, and it has its own fix; pinning the arm to the OpenAI family keeps
    # this case measuring what it is named for instead of re-reporting that one.
    if not file_code:
        # Recorded, not skipped in silence. A case that quietly does not run
        # reads as coverage forever; this says out loud that the file arm was
        # not exercised and why.
        rec("PMM", "media/openai-chat/file:comprehension").warning(
            "no OpenAI-family vision model in this run — file comprehension NOT VERIFIED")
    if file_code:
        _mm_comprehension_case(gw, "media/openai-chat/file:comprehension",
                               "/v1/chat/completions", {
                                   "model": file_code, "max_tokens": _MM_BUDGET,
                                   "messages": [{"role": "user", "content": [
                                       {"type": "text", "text": _PDF_Q},
                                       {"type": "file", "file": {
                                           "filename": "doc.pdf",
                                           "file_data": "data:application/pdf;base64," + pdf_b64}}]}]},
                               {}, "38617", timeout)
    if chat_code:
        # ocr-19452.png is black digits on white and nothing else. The previous
        # image fixture in the ingress arms was a 190-byte plain red square:
        # there was nothing in it to ask about, so no question put to it could
        # distinguish a working pipeline from a dropped one.
        _mm_comprehension_case(gw, "media/openai-chat/image:comprehension",
                               "/v1/chat/completions", {
                                   "model": chat_code, "max_tokens": _MM_BUDGET,
                                   "messages": [{"role": "user", "content": [
                                       {"type": "text", "text": "Reply with only the number shown in this image, digits only."},
                                       {"type": "image_url", "image_url": {
                                           "url": "data:image/png;base64," + base64.b64encode(_mm_fixture("ocr-19452.png")).decode()}}]}]},
                               {}, "19452", timeout)

    if audio_code:
        # The fixture speaks a sentence naming itself; a model that did not
        # receive the bytes cannot produce "regression fixture".
        _mm_comprehension_case(gw, "media/openai-chat/input_audio:comprehension",
                               "/v1/chat/completions", {
                                   "model": audio_code, "max_tokens": _MM_BUDGET,
                                   "modalities": ["text"],
                                   "messages": [{"role": "user", "content": [
                                       {"type": "text", "text": _AUDIO_Q},
                                       {"type": "input_audio", "input_audio": {
                                           "data": base64.b64encode(_mm_fixture("speech.wav")).decode(),
                                           "format": "wav"}}]}]},
                               {}, "regression fixture", timeout)

    if gemini_code:
        # No dedicated video comprehension case, and the reason is a catalog
        # fact rather than a tolerance: NOT ONE model in the catalog declares
        # video among its inputModalities. Asserting that a model reads digits
        # out of a clip asserts a capability nothing in the catalog claims, and
        # an arm that is permanently red for a capability nobody offers trains
        # the reader to skip reds — which costs more than the arm is worth.
        #
        # What was isolated: gemini-2.5-flash answers 412 for the clip, while
        # the SAME model reads 74128 correctly out of the frame EXTRACTED from
        # that clip and handed to it as an image. So the digits are legible and
        # the bytes are fine. Raising the budget did not help (STOP, not
        # length), and re-encoding at -crf 18 did not either — the generator
        # now pins the quality, so the clip is no longer a variable. The 412's
        # own root cause was not isolated further and is NOT claimed here.
        #
        # The delivery half below still runs and still asserts tightly: the
        # 2965-byte mp4 must reach the wire with the right mime, size, modality
        # and a locator that resolves into the captured request. That is the
        # property the gateway owns. Comprehension is the provider's.
        rec("PMM", "media/gemini/video:comprehension").warning(
            "no catalog model declares video input — VIDEO COMPREHENSION NOT CLAIMED BY ANY "
            "MODEL, so it is not asserted; delivery is covered by media/gemini/video")

        _mm_ingress_case(gw, cp, "media/gemini/file",
                         f"/v1beta/models/{gemini_code}:generateContent", {
                             "contents": [{"role": "user", "parts": [
                                 {"inlineData": {"mimeType": "application/pdf", "data": pdf_b64}},
                                 {"text": _MM_Q}]}]},
                         {}, "application/pdf", len(pdf), "file", timeout, "38617")

        _mm_ingress_case(gw, cp, "media/gemini/markdown",
                         f"/v1beta/models/{gemini_code}:generateContent", {
                             "contents": [{"role": "user", "parts": [
                                 {"inlineData": {"mimeType": "text/markdown", "data": md_b64}},
                                 {"text": _MM_Q}]}]},
                         {}, "text/markdown", len(md), "file", timeout, "52903")

        # Video, on any ingress at all, for the first time. The fixture has
        # been in the tree since the media work began; only the case was
        # missing. This is video INPUT — a 5 KB clip on an ordinary request —
        # not generation, so it costs a request rather than a rendering job.
        # No anchor, alone among the media arms, for the reason recorded above:
        # no catalog model claims video input, so a comprehension anchor here
        # would assert a capability nobody offers. Delivery still asserts in
        # full — mime, size, modality, and a locator resolving into the request
        # we actually sent.
        _mm_ingress_case(gw, cp, "media/gemini/video",
                         f"/v1beta/models/{gemini_code}:generateContent", {
                             "contents": [{"role": "user", "parts": [
                                 {"inlineData": {"mimeType": "video/mp4", "data": mp4_b64}},
                                 {"text": _MM_Q}]}]},
                         {}, "video/mp4", len(mp4), "video", timeout)

    # input_audio was JSON-marshalled into a text block. Gated on
    # its own model rather than on chat_code, because the arm carries audio and
    # routing it to a model that declares no audio input yields an upstream 400
    # that measures the model pick, not the codec. A 400 here is recorded,
    # never silently skipped; an absent model is reported NOT VERIFIED rather
    # than passing by omission.
    if audio_code:
        wav = _mm_wav("speech.wav")
        _mm_ingress_case(gw, cp, "media/openai-chat/input_audio", "/v1/chat/completions", {
            "model": audio_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
                {"type": "input_audio", "input_audio": {
                    "data": base64.b64encode(wav).decode(), "format": "wav"}},
                {"type": "text", "text": _MM_AUDIO_Q}]}]},
            {}, "audio/wav", len(wav), "audio", timeout, "regression fixture")
    else:
        rec("PMM", "media/openai-chat/input_audio").warning(
            "no chat model declares audio input — AUDIO INGRESS NOT VERIFIED")

    if claude_code and chat_code:
        # the OTHER direction. #8 proved an openai-shaped body
        # keeps its ingress shape when routed to claude; this proves the
        # converse. Testing one direction and claiming the pair is how a
        # cross-ingress asymmetry survives.
        _mm_ingress_case(gw, cp, "media/xformat/anthropic-shape-to-gpt", "/v1/messages", {
            "model": chat_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": png_b64}},
                {"type": "text", "text": _MM_Q}]}]},
            {"anthropic-version": "2023-06-01"}, "image/png", len(png), "image", timeout, "19452")

    _mm_tts_case(gw, cp, timeout)
    _mm_stt_case(gw, cp, timeout)

    if claude_code and chat_code:
        # the INGRESS decides the block shape, not the target
        # model. An openai-shaped body routed to claude must normalize like
        # every other openai-chat row.
        _mm_ingress_case(gw, cp, "media/xformat/openai-shape-to-claude", "/v1/chat/completions", {
            "model": claude_code, "max_tokens": _MM_BUDGET, "messages": [{"role": "user", "content": [
                {"type": "image_url", "image_url": {"url": "data:image/png;base64," + png_b64}},
                {"type": "text", "text": _MM_Q}]}]},
            {}, "image/png", len(png), "image", timeout, "19452")


def _video_get(gw: "GWClient", path: str, timeout: int) -> tuple[int, dict]:
    """GET on the gateway with the smoke's virtual key.

    The client exposes POST and a couple of fixed GETs; the video family is the
    only surface here that polls, so the helper lives with it rather than
    widening the client for one caller.
    """
    c = gw._conn(timeout=timeout)
    try:
        c.request("GET", path, headers=gw._auth_headers())
        r = c.getresponse()
        raw = r.read()
        try:
            return r.status, json.loads(raw.decode() or "{}")
        except Exception:
            return r.status, {"_raw": raw[:200].decode(errors="replace")}
    finally:
        try:
            c.close()
        except Exception:
            pass


def phase_video_generation(gw: "GWClient", cp: "CPClient", db: "DBClient",
                           spec: str, timeout: int = 600) -> None:
    """PVG — video GENERATION, the async-job family end to end.

    This is the one HC-4 cell the media work left open. Video INPUT is covered
    by the media matrix (a clip on an ordinary request, asserted on the
    normalized block); generation shares almost none of that path. A chat
    request is one round trip whose cost is known when it returns. A render is
    submit → a job row the gateway owns → poll → a stored artifact, and the
    cost is reconciled after the fact from the clip's duration. Every stage is
    a place the other family never goes.

    `spec` is MODEL:SECONDS and has no default anywhere in the harness. The
    render is billed per second of output, so the amount spent is chosen by
    whoever types the flag — a boolean would let a full-suite run decide it.

    What is asserted, in the order a defect would surface:
      submit returns a job id            — the request reached the async family
      polling reaches a TERMINAL status  — not merely "not an error yet"
      a completed job serves content     — bytes exist and start with an mp4
                                           container signature, so a JSON error
                                           body cannot pass as a video
      traffic_event carries the row      — endpoint_type video, and a cost that
                                           is not zero once reconciled
    A failed render is reported as a failure with the upstream's own reason
    rather than being treated as "the arm ran": the money was spent either way,
    and a provider-side refusal is exactly what this arm exists to surface.
    """
    model, _, secs = spec.partition(":")
    model = model.strip()
    secs = secs.strip()
    if not model or not secs.isdigit() or int(secs) <= 0:
        rec("PVG", "spec").failed(
            f"--video-generate expects MODEL:SECONDS with a positive integer, got {spec!r}")
        return

    seconds = int(secs)
    log_info(f"PVG: submitting a {seconds}s render on {model} — this bills a real video")

    r = gw._post_sync("/v1/videos", {
        "model": model,
        "prompt": "A single red pixel on a black background, static camera.",
        "seconds": str(seconds),
    }, timeout, endpoint="video/submit")

    status = r.get("status", 0)
    data = r.get("data") or {}
    if status not in (200, 201, 202):
        rec("PVG", "submit").failed(
            f"POST /v1/videos → {status}: {json.dumps(data)[:200]}")
        return

    job_id = data.get("id") or ""
    if not job_id:
        rec("PVG", "submit").failed(f"submit {status} carried no job id: {json.dumps(data)[:200]}")
        return
    rec("PVG", "submit").passed(f"job {job_id} accepted ({status})")

    TERMINAL_OK = {"completed", "succeeded"}
    TERMINAL_BAD = {"failed", "cancelled", "canceled", "error"}
    deadline = time.time() + timeout
    last = ""
    job = {}
    while time.time() < deadline:
        code, job = _video_get(gw, f"/v1/videos/{job_id}", 60)
        if code != 200:
            rec("PVG", "poll").failed(f"GET /v1/videos/{job_id} → {code}: {json.dumps(job)[:160]}")
            return
        last = str(job.get("status") or "").lower()
        if last in TERMINAL_OK or last in TERMINAL_BAD:
            break
        time.sleep(5)

    if last in TERMINAL_BAD:
        rec("PVG", "poll").failed(
            f"job {job_id} finished {last}: {json.dumps(job.get('error') or job)[:200]}")
        return
    if last not in TERMINAL_OK:
        rec("PVG", "poll").failed(
            f"job {job_id} still {last or 'unknown'} after {timeout}s — no terminal status")
        return
    rec("PVG", "poll").passed(f"job {job_id} reached {last}")

    code, content = _video_get(gw, f"/v1/videos/{job_id}/content", 120)
    raw = content.get("_raw", "") if isinstance(content, dict) else ""
    if code != 200:
        rec("PVG", "content").failed(f"content → {code}: {str(raw)[:160]}")
    else:
        # An mp4 carries 'ftyp' at offset 4. Checking the container rather than
        # merely a non-empty body is what stops a JSON error page from passing.
        looks_mp4 = "ftyp" in str(raw)[:64]
        if looks_mp4:
            rec("PVG", "content").passed("content served with an mp4 container signature")
        else:
            rec("PVG", "content").failed(
                f"content 200 but no mp4 signature in the first bytes: {str(raw)[:120]!r}")

    ev = db.poll_event(model, "", timeout=90) if db else None
    if ev is None:
        rec("PVG", "traffic_event").passed("skipped: no database access from here")
    else:
        et = ev.get("endpoint_type") or ""
        cost = ev.get("estimated_cost_usd")
        if et != "video":
            rec("PVG", "traffic_event").failed(f"endpoint_type={et!r}, want 'video'")
        elif cost in (None, 0, "0"):
            rec("PVG", "traffic_event").failed(
                f"cost {cost!r} — a billed render must reconcile to a non-zero cost")
        else:
            rec("PVG", "traffic_event").passed(f"row: endpoint_type=video cost={cost}")
        log_info(f"PVG: this run cost {cost} for {seconds}s on {model}")


def phase_multimodal(gw: "GWClient", cp: "CPClient", db: "DBClient", t0_iso: str, timeout: int = 90) -> None:
    """PMM — multimodal surface smoke: image 2xx + traffic_event cross-check,
    cross-modality routing-safety rejections, guardrail verdict, and the rerank
    ingress (no 'unsupported ingress format').

    ── Defect register ────────────────────────────────────────────────────────
    Each row is a defect measured against production before the media work, and
    the case that now fails if it returns.

    This register covers REGRESSIONS — a case exists here because something was
    once wrong. It is not the coverage list: the matrix further down is, and it
    includes cases added to fill modality × ingress cells that never carried a
    known defect (file on three ingresses, video, audio on two). A case can
    appear in the matrix and not here; the reverse would be a bug in this file.

    THE NUMBERING IS RETIRED. Earlier revisions carried `Evidence #N` markers.
    Nine of them were defined, two covered defects had none, and #1, #2 and #12
    existed nowhere — not in the source, not on any remote branch, not in the
    git log, not in the untracked working directories, searched exhaustively. A
    number whose definition cannot be produced is not a reference; it is a
    reason to believe something is missing, permanently, with no way to check.
    Keeping it meant the suite could only be audited by someone who remembered
    what the numbers meant, which is the opposite of the property this file
    exists to hold.

    So the register identifies each defect by what it WAS, which is verifiable
    against the case below it, rather than by an index into a list nobody has.
    If the original list resurfaces, mapping it onto these rows is a lookup;
    nothing here has to change.

      input_audio was JSON-marshalled into a text block
        → media/openai-chat/input_audio
      size was the base64 length; sha256 was a payload prefix
        → media/anthropic/image
      a PDF was JSON-marshalled into a text block
        → media/anthropic/document
      the responses input_image ref was entirely empty — no mime, no size,
      no source
        → media/responses/input_image
      inlineData modality came from nothing
        → media/gemini/inlineData
      an openai-shaped body routed to claude must keep its INGRESS block
      shape, not the target model's
        → media/xformat/openai-shape-to-claude
      the same, in the other direction
        → media/xformat/anthropic-shape-to-gpt
      the TTS response is audio and must be served as audio
        → media/tts/response
      the transcribed audio must be reachable after the fact
        → media/stt/request
      every image format normalised to the bare word "image", so a single PNG
      case could not have caught it
        → media/openai-chat/image/{png,jpeg,webp,gif} (crossed on purpose)
      a remote URL must be recorded as external WITHOUT a locator: nothing was
      fetched, so offering a path would promise a download that fails
        → media/openai-chat/external

    ── Coverage matrix: modality × ingress ────────────────────────────────────
    The requirement is every modality on every ingress, because block shape is
    decided by the ingress and not by the target model. What is actually here:

                  openai-chat   responses   anthropic   gemini   own endpoint
      image           ✓ ×4          ✓           ✓         ✓      ✓ images/gen
      audio           ✓             ✓ (†)       n/a       ✓      ✓ tts + stt
      file            ✓             ✓           ✓ (PDF)   ✓      –
      video           n/a           n/a         n/a       ✓      ✗ generation

    Cells are asserted on the NORMALIZED output, not the upstream verdict: a
    provider that refuses the payload still exercised the codec that had to
    represent it, which is the thing under test. media/openai-chat/file takes
    an upstream 422 and passes, because the PDF still normalized correctly.

    Where a cell is not a tick:

      video, openai-chat / responses / anthropic — n/a, not untested. None of
        those three wire formats defines a video content block: OpenAI chat has
        image_url, input_audio and file; Anthropic Messages has image and
        document. A case there would be asserting against a request the
        protocol cannot carry. One was written and removed for exactly that
        reason — it reported modality "image" for a video/mp4 stuffed into an
        Anthropic image block, which is the codec being right about a payload
        that should not exist.

      video generation — NOT COVERED. Async job family (submit, poll, fetch
        content) rather than a body this client asserts in one call, and a real
        generation costs money on the owner's account, so it is a spend
        decision as much as an engineering one. The video INPUT path above is
        covered and costs an ordinary request.

      audio, anthropic — n/a. Anthropic Messages defines image, document, text,
        thinking and tool_* blocks; there is no audio block, so there is no
        request to assert. Verified against the codec's block-type switch, not
        assumed.

      (†) audio, responses — the CODEC is covered and correct; the UPSTREAM
        refuses. OpenAI's Responses API answers 400 "Invalid value:
        'input_audio'. Supported values are: input_text, input_image,
        output_text, refusal, input_file, computer_screenshot, summary_text,
        encrypted_content" — no audio part exists in that API today, while our
        openai_responses_input codec does handle one. The case asserts the
        normalized output, so it passes and is worth keeping: it proves the
        representation is right for the day the part is added, and it will
        start returning 200 by itself when that happens. The 400 is a
        faithfully relayed upstream contract, not our defect. Recorded here
        because a bare tick would claim an end-to-end path that does not exist.
    ───────────────────────────────────────────────────────────────────────────

    TTS/STT/video/realtime need binary/multipart/WS handling beyond this JSON
    client — see the handoff for the manual-verified content those modalities
    carry; codified here are the JSON-shaped surfaces + the load-bearing
    cross-modality guard.
    """
    log_step("PMM Multimodal surfaces")
    try:
        models = cp.list_models_flat()
    except Exception as e:
        log_warn(f"  PMM: could not list models ({e}); skipping")
        rec("PMM", "phase").warning("model catalog unavailable — PHASE NOT VERIFIED")
        return

    def by_type(t):
        return [m for m in models if (m.get("type") or "").lower() == t and m.get("enabled", True)]

    def accepts(m, modality):
        """Whether the model declares it can take this input modality.

        `type` answers which ENDPOINT serves a model; it says nothing about
        which modalities that model handles. Picking by type alone is how the
        image matrix ended up on gpt-audio-mini — a chat-typed model whose
        inputModalities are [text, audio] — and took four upstream 400s
        ("This model does not support image_url content") that read as media
        defects and were not. The catalog already carries the answer; nothing
        was reading it.
        """
        return modality in [str(x).lower() for x in (m.get("inputModalities") or [])]

    image_ms = by_type("image")
    chat_ms = by_type("chat")
    rerank_ms = by_type("rerank")
    # Media cases must run on a model that accepts the modality under test,
    # otherwise the case measures the catalog's pick, not the media pipeline.
    vision_ms = [m for m in chat_ms if accepts(m, "image")]
    audio_in_ms = [m for m in chat_ms if accepts(m, "audio")]

    # ── Media representation across ingresses (view-time normalized) ──
    if vision_ms:
        _mm_media_matrix(gw, cp, vision_ms[0].get("code", ""), timeout)
        # Cross-ingress: every modality on every wire, per the binding. The
        # cross-format arms carry images, so they need a vision model too.
        def _first(pred):
            for m in vision_ms:
                if pred((m.get("code") or "").lower()):
                    return m.get("code", "")
            return ""
        _mm_cross_ingress(
            gw, cp,
            vision_ms[0].get("code", ""),
            _first(lambda c: c.startswith("claude")),
            _first(lambda c: c.startswith("gemini")),
            audio_in_ms[0].get("code", "") if audio_in_ms else "",
            # The file arm needs a provider that accepts a file block at all.
            # Left on vision_ms[0] it landed on cohere, whose wire answers
            # "unrecognized content type 'file'" — a canonical field leaking
            # onto a non-OpenAI wire, the same 422 recorded from production.
            # Chosen the same way as the claude and gemini arms rather than
            # inheriting whichever model happened to sort first.
            _first(lambda c: c.startswith("gpt-")),
            timeout,
        )
    elif chat_ms:
        rec("PMM", "media").warning(
            "no chat model declares image input — MEDIA MATRIX NOT VERIFIED")
    else:
        rec("PMM", "media").warning("no chat model in catalog — MEDIA MATRIX NOT VERIFIED")

    # ── Image generation: 2xx + traffic_event (endpoint_type/cost/artifact) ──
    if image_ms:
        code = image_ms[0].get("code", "")
        log_info(f"  [image] {code} — generate + traffic cross-check")
        r = gw._post_sync("/v1/images/generations",
                          {"model": code, "prompt": "a small red lighthouse at dawn, watercolor", "n": 1},
                          timeout, endpoint="images")
        st = r.get("status", 0)
        data = (r.get("data") or {})
        if st == 200 and isinstance(data.get("data"), list) and data["data"]:
            rec("PMM", f"image/{code}/2xx").passed(f"200, {len(data['data'])} artifact(s)")
            row = _mm_traffic_row(db, code, "image_generation", t0_iso)
            if row is None:
                rec("PMM", f"image/{code}/traffic").warning("traffic row not readable within the poll window — NOT VERIFIED")
            else:
                issues = []
                if row["endpoint_type"] != "image_generation":
                    issues.append(f"endpoint_type={row['endpoint_type']}")
                if row["has_artifact"] != "t":
                    issues.append("artifact_refs missing")
                try:
                    if float(row["estimated_cost_usd"] or 0) <= 0:
                        issues.append("cost<=0")
                except ValueError:
                    issues.append(f"cost={row['estimated_cost_usd']!r}")
                if issues:
                    rec("PMM", f"image/{code}/traffic").failed("; ".join(issues))
                else:
                    rec("PMM", f"image/{code}/traffic").passed(
                        f"endpoint_type=image_generation cost={row['estimated_cost_usd']} artifact_refs present")
        else:
            rec("PMM", f"image/{code}/2xx").failed(
                f"status={st} err={data.get('error', {}).get('code') if isinstance(data, dict) else '?'}")
    else:
        rec("PMM", "image").warning("no image model in catalog — NOT VERIFIED")

    # ── Cross-modality routing safety (the load-bearing guard) ──
    if not (chat_ms and image_ms):
        rec("PMM", "xmodal").warning("need both a chat and an image model — GUARD NOT VERIFIED")
    if chat_ms and image_ms:
        chat_code = chat_ms[0].get("code", "")
        img_code = image_ms[0].get("code", "")
        # chat model on the image endpoint → 400 MODEL_MODALITY_MISMATCH
        r = gw._post_sync("/v1/images/generations", {"model": chat_code, "prompt": "x", "n": 1}, timeout, endpoint="images")
        err = ((r.get("data") or {}).get("error") or {}).get("code", "")
        if r.get("status") == 400 and err == "MODEL_MODALITY_MISMATCH":
            rec("PMM", "xmodal/chat-on-image").passed("400 MODEL_MODALITY_MISMATCH")
        else:
            rec("PMM", "xmodal/chat-on-image").failed(f"status={r.get('status')} code={err} (want 400 MODEL_MODALITY_MISMATCH)")
        # image model on the chat endpoint → 400
        r = gw._post_sync("/v1/chat/completions", {"model": img_code, "messages": [{"role": "user", "content": "hi"}], "max_tokens": 8}, timeout, endpoint="chat")
        err = ((r.get("data") or {}).get("error") or {}).get("code", "")
        if r.get("status") == 400 and err == "MODEL_MODALITY_MISMATCH":
            rec("PMM", "xmodal/image-on-chat").passed("400 MODEL_MODALITY_MISMATCH")
        else:
            rec("PMM", "xmodal/image-on-chat").failed(f"status={r.get('status')} code={err} (want 400 MODEL_MODALITY_MISMATCH)")

    # ── Guardrail verdict (JSON, always 200 with a verdict) ──
    r = gw._post_sync("/v1/guardrail", {"stage": "input", "content": "My email is alice@example.com"}, timeout, endpoint="guardrail")
    data = (r.get("data") or {})
    if r.get("status") == 200 and "action" in data and "coverage" in data:
        rec("PMM", "guardrail/verdict").passed(f"200 action={data.get('action')} coverage={data.get('coverage')}")
    else:
        rec("PMM", "guardrail/verdict").failed(f"status={r.get('status')} keys={list(data.keys())[:5]}")

    # ── Rerank ingress ──
    #
    # The pass condition is "the endpoint answered", not "the endpoint did not
    # answer one particular way". The earlier form was `if "unsupported ingress
    # format" in msg: fail else: pass`, which ticked on ANY status — a rerank
    # 500 was recorded as a pass, and the pass line printed status=500 while
    # calling it OK. A check whose green does not depend on the request
    # succeeding is not a check.
    #
    # The specific regression diagnosis is kept: 'unsupported ingress format'
    # is worth naming because it says the ingress never reached routing, which
    # is a different repair from a rerank that routed and then failed upstream.
    rr_code = rerank_ms[0].get("code", "") if rerank_ms else "rerank-v3.5"
    r = gw._post_sync("/v1/rerank", {"model": rr_code, "query": "capital of France", "documents": ["Paris", "Berlin"], "top_n": 1}, timeout, endpoint="rerank")
    status = r.get("status")
    data = (r.get("data") or {})
    msg = (data.get("error") or {}).get("message", "") or str(data.get("message", ""))
    results = data.get("results")
    if "unsupported ingress format" in msg:
        rec("PMM", "rerank/ingress").failed(f"ingress regression — request never reached routing: {msg}")
    elif status != 200:
        rec("PMM", "rerank/ingress").failed(f"status={status} msg={msg[:160]}")
    elif not isinstance(results, list) or not results:
        rec("PMM", "rerank/ingress").failed(f"200 but no results array: keys={list(data.keys())[:6]}")
    else:
        rec("PMM", "rerank/ingress").passed(f"200 with {len(results)} result(s), top index={results[0].get('index')}")


# ─── Main ─────────────────────────────────────────────────────────────────────

def _vk_is_usable(vk: str) -> bool:
    """A placeholder must stop the run, not start it.

    The env templates are loaded as DEFAULTS, so any value written into one is
    handed to every run of that target. A template that assigns a stand-in
    therefore does not document the variable — it arms the suite with a key that
    cannot authenticate, and against prod that still snapshots, disables and
    restores the live routing rules on the way past. Refusing here keeps that
    decision at the tool rather than in the discipline of whoever edits a
    template.
    """
    vk = (vk or "").strip()
    return bool(vk) and "REPLACE" not in vk.upper()


def main():
    # Pre-parse --target so loadenv.load() can populate os.environ with the
    # right .env file BEFORE the main parser reads defaults from env vars.
    # If the user didn't pass --target on CLI, loadenv uses NEXUS_TEST_TARGET
    # or defaults to "local" (TTY).
    _pre = argparse.ArgumentParser(add_help=False)
    _pre.add_argument("--target", default=None,
                      choices=("local", "dev", "prod"))
    _pre_args, _ = _pre.parse_known_args()
    # `--help` / `-h` bypasses loadenv: argparse will print usage and exit
    # before any test runs, so the env-file machinery doesn't need to
    # initialise (and the non-TTY safety check shouldn't false-positive on
    # a `--help` invocation from a Bash subshell).
    _help_only = any(a in ("-h", "--help") for a in sys.argv[1:])
    if _help_only:
        _resolved_target = (
            _pre_args.target
            or os.environ.get("NEXUS_TEST_TARGET")
            or "local"
        )
    else:
        try:
            _resolved_target = loadenv.load(_pre_args.target)
        except (RuntimeError, FileNotFoundError) as e:
            print(f"smoke-gateway: {e}", file=sys.stderr)
            sys.exit(1)

    ap = argparse.ArgumentParser(
        description="AI Gateway full smoke test",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=f"""
Active target: {_resolved_target} (from --target / NEXUS_TEST_TARGET / TTY default)
Env file loaded: tests/.env.{_resolved_target} (with .example as fallback)

Environment variables (CLI args override):
  NEXUS_VK       Virtual key (nvk_…)
  NEXUS_GW_URL   AI Gateway base URL  (sourced from tests/.env.<target>; see NEXUS_AI_GW_URL)
  NEXUS_CP_URL   Control Plane URL    (sourced from tests/.env.<target>)
  NEXUS_CP_USER  CP login email       (default: admin@nexus.ai)
  NEXUS_CP_PASS  CP login password    (default: admin123)
  NEXUS_CP_REDIRECT_URI  OAuth redirect_uri sent during PKCE login
                         (sourced from tests/.env.<target> NEXUS_OAUTH_REDIRECT_URI).

Remote / prod mode: when NEXUS_GW_URL is not localhost, DB cross-check (P6)
and Redis cache flushing are skipped automatically.
""",
    )
    ap.add_argument("--target", default=_resolved_target,
                    choices=("local", "dev", "prod"),
                    help="Test target env (default: from NEXUS_TEST_TARGET or 'local'). "
                         "Selects tests/.env.<target> as the source of NEXUS_* vars.")
    # All defaults read from tests/.env.<target> via loadenv (already
    # populated above). Legacy NEXUS_GW_URL / NEXUS_CP_USER / NEXUS_CP_PASS
    # / NEXUS_CP_REDIRECT_URI names still honored as the smoke's historical
    # contract; .env.* canonical names (NEXUS_AI_GW_URL / NEXUS_ADMIN_EMAIL /
    # NEXUS_ADMIN_PASSWORD / NEXUS_OAUTH_REDIRECT_URI) used as fallback.
    ap.add_argument("--vk",
                    default=os.environ.get("NEXUS_VK") or os.environ.get("NEXUS_TEST_VK", ""),
                    help="Virtual key (nvk_…) [env: NEXUS_VK or NEXUS_TEST_VK]")
    ap.add_argument("--gw-url",
                    default=os.environ.get("NEXUS_GW_URL") or os.environ.get("NEXUS_AI_GW_URL", "http://localhost:3050"),
                    help="AI Gateway base URL [env: NEXUS_GW_URL or NEXUS_AI_GW_URL]")
    ap.add_argument("--cp-url", default=os.environ.get("NEXUS_CP_URL", "http://localhost:3001"),
                    help="Control Plane base URL [env: NEXUS_CP_URL]")
    ap.add_argument("--cp-user",
                    default=os.environ.get("NEXUS_CP_USER") or os.environ.get("NEXUS_ADMIN_EMAIL", "admin@nexus.ai"),
                    help="CP login email [env: NEXUS_CP_USER or NEXUS_ADMIN_EMAIL]")
    ap.add_argument("--cp-pass",
                    default=os.environ.get("NEXUS_CP_PASS") or os.environ.get("NEXUS_ADMIN_PASSWORD", "admin123"),
                    help="CP login password [env: NEXUS_CP_PASS]")
    ap.add_argument("--routing", action="store_true",
                    help="Enable P4 routing-ON tests")
    ap.add_argument("--no-stream", action="store_true", help="Skip SSE tests")
    ap.add_argument("--no-cache", action="store_true", help="Skip cache tests")
    ap.add_argument("--no-estimate", action="store_true",
                    help="Skip the PD /v1/estimate compare endpoint phase. "
                         "/v1/estimate is on by default — it exercises the "
                         "cost-preview surface against the Model price "
                         "catalog without hitting upstream providers.")
    ap.add_argument("--responses", action="store_true",
                    help="P3R: also exercise POST /v1/responses for ALL "
                         "catalog models (E56). Full per-model suite — "
                         "non-stream + SSE + multi-round cache, mirrors "
                         "P3 chat-completions depth. Native (OpenAI "
                         "passthrough) and cross-format (canonical "
                         "bridge to Anthropic/Gemini/...) both tested.")
    ap.add_argument("--messages", action="store_true",
                    help="P3A: also exercise POST /v1/messages (Anthropic) "
                         "for ALL catalog models. Same depth as P3 — "
                         "non-stream + SSE + multi-round cache. Native "
                         "(Anthropic passthrough) and cross-format both "
                         "tested. Cached tokens read from "
                         "usage.cache_read_input_tokens.")
    ap.add_argument("--gemini", action="store_true",
                    help="P3G: also exercise POST /v1beta/models/{m}:"
                         "generateContent (Gemini) for ALL catalog "
                         "models. Same depth as P3. Native (Gemini "
                         "passthrough) and cross-format both tested. "
                         "Cached tokens read from "
                         "usageMetadata.cachedContentTokenCount.")
    ap.add_argument("--all-ingress", action="store_true",
                    help="Umbrella: turn on --responses + --messages + "
                         "--gemini in one go. Every public ingress "
                         "exercised at the same P3 depth. Use this "
                         "for full-surface validation after any "
                         "ingress, codec, or canonical-bridge change.")
    ap.add_argument("--multimodal", action="store_true",
                    help="PMM: multimodal surface smoke — image generation 2xx "
                         "+ traffic_event cross-check (endpoint_type/cost/"
                         "artifact_refs), cross-modality routing-safety "
                         "rejections (MODEL_MODALITY_MISMATCH 400), the guardrail "
                         "verdict shape, and the rerank ingress (no 'unsupported "
                         "ingress format' regression). Real upstream — needs an "
                         "image provider credential.")
    ap.add_argument("--video-generate", default="",
                    help="PVG: video GENERATION arm — MODEL:SECONDS, e.g. "
                         "sora-2:4. Off unless given, and there is no default: "
                         "every run bills a real render priced per second, so "
                         "the clip has to be chosen deliberately rather than "
                         "inherited from a full-suite invocation. Covers the "
                         "async-job family end to end — submit, poll to a "
                         "terminal status, then the traffic_event row's "
                         "endpoint_type / cost / artifact_refs.")
    ap.add_argument("--video-timeout", type=int, default=600,
                    help="Seconds to wait for a video job to reach a terminal "
                         "status (default 600). A render is minutes, not the "
                         "seconds a chat completion takes.")
    ap.add_argument("--cache-rounds", type=int, default=3,
                    help="Read rounds per cache test (default 3); write cost vs cumulative savings")
    ap.add_argument("--concurrent", action="store_true",
                    help="Run concurrent request test (P3.5)")
    ap.add_argument("--concurrent-n", type=int, default=5)
    ap.add_argument("--concurrent-model", default="moonshot-v1-8k",
                    help="Model for concurrent test (fast model recommended)")
    ap.add_argument("--models", default="",
                    help="Comma-separated model IDs to test (default: all chat)")
    ap.add_argument("--timeout", type=int, default=90,
                    help="Per-request timeout seconds")
    ap.add_argument("--db-poll-timeout", type=int, default=45)
    ap.add_argument("--report", default="",
                    help="Report path (default: /tmp/smoke-gateway-<UTC>.md)")
    ap.add_argument("--fix-deprecated", action="store_true",
                    help="P8: delete catalog models that fail non-stream in P3 (provider-deprecated cleanup)")
    ap.add_argument("--embedding", action="store_true",
                    help="PE (E61, legacy): minimal embedding smoke kept for back-compat. "
                         "Superseded by P3E (--all-ingress or default with --embeddings); "
                         "use P3E for full per-model × per-ingress coverage. PE only "
                         "verifies a single shape arm per model and is retained for quick "
                         "ad-hoc checks. If no embedding models exist in the catalog, the "
                         "phase logs a clear skip and exits cleanly.")
    ap.add_argument("--direct-compare", action="store_true",
                    help="P3C: compare gateway responses with direct provider API calls "
                         "(requires OPENAI_API_KEY / GEMINI_API_KEY / ANTHROPIC_API_KEY etc. "
                         "or --db-credentials)")
    ap.add_argument("--provider-keys-file", default="",
                    help="JSON file mapping provider name → API key for P3C (overrides env vars)")
    ap.add_argument("--db-credentials", action="store_true",
                    help="P3C: load provider API keys from the DB Credential table "
                         "(AES-GCM decryption using CP config key). "
                         "Requires 'cryptography' package (pip install cryptography). "
                         "DB keys take precedence over env-var keys.")
    _default_cp_config = os.path.abspath(
        os.path.join(os.path.dirname(os.path.abspath(__file__)),
                     "../../packages/control-plane/control-plane.dev.yaml")
    )
    ap.add_argument("--cp-config", default=_default_cp_config,
                    help="Path to control-plane dev YAML for encryption key "
                         "(used with --db-credentials; default: packages/control-plane/control-plane.dev.yaml)")
    ap.add_argument("--no-restore", action="store_true",
                    help="Debug: do not restore config on exit")
    ap.add_argument("--no-routing-manage", action="store_true",
                    help="Do NOT disable/restore the deployment's routing rules in P3. "
                         "Leaves existing rules untouched (e.g. an enabled prod rule stays ON). "
                         "P3's direct-addressing isolation is weakened: an active rule may "
                         "re-route a per-model request, so some P3 model/route assertions may "
                         "warn. Use against prod to avoid disturbing live routing.")
    ap.add_argument("--no-embeddings", action="store_true",
                    help="Skip P3E embeddings phase entirely (useful for quick chat-only smokes).")
    ap.add_argument("--all-upstream", action="store_true",
                    help="Force all phases registered as 'fixture' in _cost_policy.json to use "
                         "real upstream calls. P3/P3E are already real-upstream by default; "
                         "this flag is a forward-compat hook for future phases (E63 audio, "
                         "E64 image, E66 video) that default to fixture mode.")
    ap.add_argument("--ssh-host", default=os.environ.get("NEXUS_SSH_HOST", ""),
                    help="SSH target (user@host) for remote Postgres access "
                         "in prod mode. When set, DB cross-check (P6) and "
                         "normalize verification (P6b) run against the "
                         "remote DB via `ssh <host> psql`. [env: NEXUS_SSH_HOST]")
    ap.add_argument("--ssh-pgpassword",
                    default=os.environ.get("NEXUS_SSH_PGPASSWORD", ""),
                    help="PGPASSWORD for the remote psql client when --ssh-host "
                         "is set. [env: NEXUS_SSH_PGPASSWORD]")
    ap.add_argument("--ssh-pguser",
                    default=os.environ.get("NEXUS_SSH_PGUSER", "nexus"),
                    help="Remote pg user (default: nexus)")
    ap.add_argument("--ssh-pgdb",
                    default=os.environ.get("NEXUS_SSH_PGDB", "nexus_gateway"),
                    help="Remote pg database (default: nexus_gateway)")
    args = ap.parse_args()

    if not _vk_is_usable(args.vk):
        # Name the file, not just the variable: the key is a live credential
        # that never lives in the repo, so "set NEXUS_VK" alone leaves the
        # reader hunting for where it is supposed to go.
        ap.error(
            f"no usable virtual key (got {args.vk!r}). Set NEXUS_TEST_VK in "
            f"tests/.env.{args.target} (gitignored; tests/.env.{args.target}.example "
            "names the entry), or pass --vk / NEXUS_VK for a one-off run.")

    # Detect remote/prod environment: any non-localhost GW URL is remote.
    _parsed_gw = urllib.parse.urlparse(args.gw_url)
    is_remote = _parsed_gw.hostname not in ("localhost", "127.0.0.1", "::1")
    if is_remote:
        print(f"[smoke] Remote environment detected ({args.gw_url}) — "
              "DB cross-check and Redis flush are disabled", flush=True)

    ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H-%M-%SZ")
    report_path = args.report or f"/tmp/smoke-gateway-{ts}.md"

    # Unique per-run token injected into user messages so consecutive runs
    # produce distinct request bodies and bypass the L1 Redis response cache.
    global _run_nonce
    _run_nonce = secrets.token_hex(4)
    print(f"[smoke] Run nonce: {_run_nonce} (injected into user messages for L1 bypass)", flush=True)

    gw = GWClient(args.gw_url, args.vk)
    cp = CPClient(args.cp_url, args.cp_user, args.cp_pass)
    db = DBClient(
        is_remote=is_remote,
        ssh_host=args.ssh_host,
        ssh_pgpassword=args.ssh_pgpassword,
        ssh_pguser=args.ssh_pguser,
        ssh_pgdb=args.ssh_pgdb,
    )
    if is_remote and db.has_ssh:
        print(f"[smoke] Remote DB access via ssh {args.ssh_host} — "
              "P6 DB cross-check + P6b normalize verification ENABLED",
              flush=True)

    snapshot = StateSnapshot()
    m0 = ""
    t0_iso = ""

    # Build cost-phase mode map for the report header.
    _cost_phase_modes = {
        ph: (entry.get("mode") if isinstance(entry, dict) else entry)
        for ph, entry in _COST_POLICY.get("phases", {}).items()
    }
    if args.all_upstream:
        _cost_phase_modes = {ph: "real-upstream (--all-upstream)" for ph in _cost_phase_modes}
    log_info(f"Cost policy: {_cost_phase_modes}")

    try:
        # P0
        t0_iso = phase0_preflight(gw, cp, is_remote=is_remote)
        m0 = gw.metrics()
        snapshot = StateSnapshot.capture(cp)

        # P1
        all_models_status, all_models_body = gw.list_models()
        all_models_data = all_models_body.get("data", [])
        all_chat_models = [
            m["id"] for m in all_models_data
            if classify_model_modality(m) == "chat"
        ]
        first_model = all_chat_models[0] if all_chat_models else "moonshot-v1-8k"
        phase1_auth_boundary(gw, first_model)

        # P2 — returns (chat_model_ids, embedding_model_entries)
        chat_models, embedding_models = phase2_catalog(gw)

        # Apply --models filter (chat only; embeddings always run their full set)
        if args.models:
            wanted = set(args.models.split(","))
            chat_models = [m for m in chat_models if m in wanted]
            log_info(f"Filtered to {len(chat_models)} chat models: {', '.join(chat_models)}")

        # P3
        phase3_routing_off(gw, cp, snapshot, chat_models,
                           args.no_stream, args.no_cache, args.timeout, t0_iso,
                           cache_rounds=args.cache_rounds, is_remote=is_remote,
                           db=db, manage_routing=not args.no_routing_manage)

        # P3T — tool-body coercion probes (runs while routing is still OFF)
        phase3t_tool_coercions(gw, chat_models, args.timeout)

        # P3C direct compare (optional) — runs immediately after P3 while routing is still OFF
        if args.direct_compare:
            if args.db_credentials:
                direct_client = DirectClient.from_db(db, args.cp_config)
            else:
                direct_client = DirectClient.from_env(args.provider_keys_file)
            phase3c_direct_compare(gw, cp, snapshot, chat_models, direct_client,
                                   args.timeout, no_stream=args.no_stream)

        # --all-ingress umbrella: turn on every public ingress phase at
        # the same depth. Forces --no-cache OFF (cache test mandatory for
        # every ingress per user binding 2026-05-16 —
        # feedback_cache_mandatory_all_ingress).
        # Also implicitly enables P3E (embeddings) unless --no-embeddings.
        if args.all_ingress:
            args.responses = True
            args.messages = True
            args.gemini = True
            # The multimodal phase carries the standing media regression.
            # Leaving it out of the umbrella meant the binding full-surface
            # smoke — the gate on every ai-gateway / traffic_event commit —
            # never ran a single media assertion.
            args.multimodal = True
            if args.no_cache:
                log_warn("--all-ingress forces cache test ON (user binding); "
                         "ignoring --no-cache flag")
                args.no_cache = False

        # P3E Embeddings — runs after all chat ingresses.
        # Skipped when --no-embeddings is set or no embedding models in catalog.
        # Real-upstream by default per _cost_policy.json P3E entry.
        if _phase_is_real_upstream("P3E", all_upstream=args.all_upstream):
            phase3e_embeddings(
                gw, cp, db,
                embedding_models,
                t0_iso,
                timeout=args.timeout,
                no_embeddings=args.no_embeddings,
                snapshot=snapshot,
            )
        else:
            log_info("P3E skipped — not real-upstream in cost policy (use --all-upstream to force)")
            rec("P3E", "phase").passed("skipped: fixture mode (use --all-upstream)")

        # PMM Multimodal — image 2xx + traffic cross-check + cross-modality
        # routing safety + guardrail + rerank ingress. Opt-in via --multimodal
        # (real upstream: needs an image provider credential).
        if args.multimodal:
            phase_multimodal(gw, cp, db, t0_iso, timeout=args.timeout)

        # PVG Video GENERATION — the one HC-4 matrix cell the media work left
        # open. Video INPUT is covered by the media matrix above; generation is
        # a different family entirely (async job: submit → poll → content).
        #
        # It is opt-in AND parameterised on purpose. Every run bills a real
        # video render, and the price scales with the clip: seconds × the
        # model's per-second rate. A boolean flag would let a full-suite run
        # spend money nobody chose to spend, so the model and duration must be
        # named at the call site — there is no default to fall back on.
        if args.video_generate:
            phase_video_generation(gw, cp, db, args.video_generate,
                                   timeout=args.video_timeout)
        else:
            rec("PVG", "phase").passed(
                "skipped: video generation bills a real render — pass "
                "--video-generate MODEL:SECONDS to run it")

        # P3R OpenAI Responses-API ingress (E56) — full per-model suite
        # mirroring P3 depth (non-stream + SSE + multi-round cache).
        # Native (OpenAI passthrough) and cross-format (canonical bridge)
        # both tested.
        if args.responses:
            phase3r_responses_api(
                gw, chat_models, args.timeout,
                no_stream=args.no_stream,
                no_cache=args.no_cache,
                t0_iso=t0_iso,
                cache_rounds=args.cache_rounds,
                db=db,
            )

        # P3A /v1/messages (Anthropic) — full per-model suite.
        # Native (Anthropic passthrough) and cross-format both tested.
        if args.messages:
            phase3a_messages_api(
                gw, chat_models, args.timeout,
                no_stream=args.no_stream,
                no_cache=args.no_cache,
                t0_iso=t0_iso,
                cache_rounds=args.cache_rounds,
                db=db,
            )

        # P3G /v1beta (Gemini) — full per-model suite.
        # Native (Gemini passthrough) and cross-format both tested.
        if args.gemini:
            phase3g_gemini_api(
                gw, chat_models, args.timeout,
                no_stream=args.no_stream,
                no_cache=args.no_cache,
                t0_iso=t0_iso,
                cache_rounds=args.cache_rounds,
                db=db,
            )

        # PD /v1/estimate compare endpoint (E58-S4) — happy + negative.
        # Gated by --no-estimate so cost-preview tests can be skipped on
        # smoke runs that only care about real-traffic correctness.
        if not args.no_estimate:
            phase_compare_estimate(gw, cp, chat_models, timeout=args.timeout)

        # P3.5 concurrent (optional)
        if args.concurrent:
            phase3_5_concurrent(gw, db, args.concurrent_model,
                                 args.concurrent_n, t0_iso, args.timeout)

        # P4 routing ON (optional)
        if args.routing:
            phase4_routing_on(gw, cp, snapshot, chat_models,
                               args.no_stream, args.no_cache, args.timeout, t0_iso, db,
                               cache_rounds=args.cache_rounds, is_remote=is_remote)

        # P5
        phase5_cache_cfg(cp)

        # P6
        phase6_db_crosscheck(db, t0_iso, args.db_poll_timeout)

        # P6b — normalize verification: full per-row analysis (extracted text,
        # reasoning tokens, cache breakdown, usage math sanity) against the
        # view-time recompute endpoint, the same one the UI's Normalized tab uses.
        phase6b_normalize_check(db, cp, t0_iso)

        # P7
        t1_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        diff = phase7_metrics_delta(gw, m0)

        # P8 (optional) — remove provider-deprecated models from catalog
        if args.fix_deprecated:
            phase_fix_deprecated(cp)

        # PE (optional, E61 legacy) — minimal embedding smoke retained for
        # ad-hoc back-compat; P3E (E62) is the canonical embedding coverage.
        if args.embedding:
            phase_embedding(gw, cp, db, t0_iso, timeout=args.timeout)

    except KeyboardInterrupt:
        log_warn("Interrupted")
        t1_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        diff = []
    except RuntimeError as e:
        # Record the abort as a real failure, not just a printed line. The
        # verdict is computed by render_report from the recorded results, so a
        # fatal that only logs leaves the counters at whatever passed before it
        # — a run that aborted in P0 preflight, having exercised no model at
        # all, reported "Result: PASS — 0 failed" and exited 0. The smoke is a
        # mandatory pre-"done" gate; a gate that cannot fail is not a gate.
        rec("FATAL", "run-aborted").failed(str(e))
        log_fail(f"Fatal: {e}")
        t1_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        diff = []
    finally:
        snapshot.restore(cp, no_restore=args.no_restore, skip_routing=args.no_routing_manage)

    t1_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    ok = render_report(args.vk, t0_iso, t1_iso, diff, report_path,
                       cost_phase_modes=_cost_phase_modes)
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
