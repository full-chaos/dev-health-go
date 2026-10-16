"""Python oracle driver for the dev-health-go llm package (CHAOS-8719).

Runs the REAL frozen Python providers of dev-health-ops
(src/dev_health_ops/llm/providers/{anthropic,local,gemini,qwen}.py, with their
SDKs) over a fixed case table and prints, per case, the exact HTTP requests
each provider sent and the result or error its parser returned, as JSON with
type-tagged leaves ({"t": type, "v": string}). No network: each provider's
hardened HTTP client factory is replaced by one on an in-memory transport that
records the request and serves the case's recorded answer.

Usage (see ../README.md):
    <ops-worktree>/.venv/bin/python -I driver.py <ops-worktree>/src <anthropic|compatible> <ops-sha> > out.json
"""

import asyncio
import hashlib
import json
import pathlib
import sys

SRC = pathlib.Path(sys.argv[1]).resolve()
SUITE = sys.argv[2]
OPS_SHA = sys.argv[3]
sys.path.insert(0, str(SRC))

import httpx  # noqa: E402
import httpx2  # noqa: E402

from dev_health_ops.llm.providers import anthropic as anthropic_mod  # noqa: E402
from dev_health_ops.llm.providers import local as local_mod  # noqa: E402
from dev_health_ops.llm.providers.gemini import GeminiProvider  # noqa: E402
from dev_health_ops.llm.providers.qwen import QwenProvider  # noqa: E402
from dev_health_ops.work_graph.investment.categorization_prompts import build_prompt  # noqa: E402

PROVIDER_FILES = [
    "dev_health_ops/llm/providers/anthropic.py",
    "dev_health_ops/llm/providers/local.py",
    "dev_health_ops/llm/providers/gemini.py",
    "dev_health_ops/llm/providers/qwen.py",
    "dev_health_ops/llm/providers/openai.py",
    "dev_health_ops/llm/providers/base.py",
    "dev_health_ops/llm/providers/_http.py",
    "dev_health_ops/llm/errors.py",
    "dev_health_ops/llm/json_utils.py",
]

ERROR_CLASSES = {
    "LLMAuthError": "auth",
    "LLMModelNotFoundError": "model_not_found",
    "LLMInvalidRequestError": "invalid_request",
    "LLMRateLimitError": "rate_limit",
    "LLMContextLengthError": "context_length",
    "LLMServerError": "server",
    "LLMTimeoutError": "timeout",
    "LLMTransportError": "transport",
    "LLMOutputError": "output",
    "LLMError": "generic",
}


def tag(v):
    if v is None:
        return {"t": "null", "v": ""}
    if isinstance(v, bool):
        return {"t": "bool", "v": "true" if v else "false"}
    if isinstance(v, int):
        return {"t": "number", "v": str(v)}
    if isinstance(v, float):
        return {"t": "number", "v": repr(v)}
    if isinstance(v, str):
        return {"t": "str", "v": v}
    if isinstance(v, list):
        return {"t": "array", "v": [tag(x) for x in v]}
    if isinstance(v, dict):
        return {"t": "object", "v": {str(k): tag(x) for k, x in v.items()}}
    raise TypeError(type(v).__name__)


CATEGORIZATION_PROMPT = build_prompt("[pr:1] Fix the login timeout\nRetries now back off. é中 <a> & b")
PLAIN_PROMPT = "Explain this work unit.\nReturn JSON."
MESSAGE = {"id": "msg_01", "type": "message", "role": "assistant", "model": "m", "stop_sequence": None}


def anthropic_answer(content, stop_reason="end_turn", usage=None):
    body = dict(MESSAGE)
    body["content"] = content
    body["stop_reason"] = stop_reason
    body["usage"] = usage or {"input_tokens": 11, "output_tokens": 7}
    return {"status": 200, "body": json.dumps(body)}


def text_block(text):
    return {"type": "text", "text": text}


def error_answer(status, kind, message):
    return {"status": status, "body": json.dumps({"type": "error", "error": {"type": kind, "message": message}})}


ANTHROPIC_CASES = [
    {"name": "plain_prompt_text", "prompt": PLAIN_PROMPT, "answers": [anthropic_answer([text_block('{"summary": "x"}')])]},
    {"name": "categorization_prompt_json", "prompt": CATEGORIZATION_PROMPT,
     "answers": [anthropic_answer([text_block('{"subcategories": {"feature_delivery.roadmap": 1.0}}')])]},
    {"name": "plain_text_kept_as_written", "prompt": PLAIN_PROMPT, "answers": [anthropic_answer([text_block("not json at all")])]},
    {"name": "refusal_empty_content", "prompt": PLAIN_PROMPT,
     "answers": [anthropic_answer([], stop_reason="refusal", usage={"input_tokens": 5, "output_tokens": 0})]},
    {"name": "max_tokens_partial_text", "prompt": PLAIN_PROMPT, "answers": [anthropic_answer([text_block('{"cut": ')], stop_reason="max_tokens")]},
    {"name": "model_override", "prompt": PLAIN_PROMPT, "model": "claude-sonnet-5-5", "answers": [anthropic_answer([text_block("{}")])]},
    {"name": "unauthorized", "prompt": PLAIN_PROMPT, "answers": [error_answer(401, "authentication_error", "invalid x-api-key")]},
    {"name": "bad_request", "prompt": PLAIN_PROMPT, "answers": [error_answer(400, "invalid_request_error", "messages: bad")]},
    {"name": "not_found", "prompt": PLAIN_PROMPT, "answers": [error_answer(404, "not_found_error", "model: nope")]},
    {"name": "server_error_then_success", "prompt": PLAIN_PROMPT,
     "answers": [error_answer(500, "api_error", "boom"), anthropic_answer([text_block("{}")])]},
    # Named divergences (the Go test declares each one):
    {"name": "divergence_cache_tokens", "prompt": PLAIN_PROMPT, "answers": [anthropic_answer([text_block("{}")], usage={
        "input_tokens": 100, "output_tokens": 9, "cache_creation_input_tokens": 20, "cache_read_input_tokens": 30})]},
    {"name": "divergence_two_text_blocks", "prompt": PLAIN_PROMPT, "answers": [anthropic_answer([text_block('{"a":'), text_block("1}")])]},
    {"name": "divergence_thinking_first", "prompt": PLAIN_PROMPT,
     "answers": [anthropic_answer([{"type": "thinking", "thinking": "", "signature": "sig"}, text_block("{}")])]},
    {"name": "divergence_overloaded_retries", "prompt": PLAIN_PROMPT, "answers": [error_answer(529, "overloaded_error", "Overloaded")]},
    {"name": "divergence_rate_limit_retries", "prompt": PLAIN_PROMPT,
     "answers": [dict(error_answer(429, "rate_limit_error", "slow"), headers={"retry-after": "0"})]},
]


def chat_answer(content, usage=None, finish="stop"):
    return {"status": 200, "body": json.dumps({
        "id": "c1", "object": "chat.completion", "created": 0, "model": "m",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": finish}],
        "usage": usage or {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
    })}


COMPATIBLE_CASES = []
for kind in ("local", "gemini", "qwen"):
    COMPATIBLE_CASES += [
        {"name": kind + "/categorization_schema", "kind": kind, "prompt": CATEGORIZATION_PROMPT,
         "answers": [chat_answer('{"b": 1, "a": [1, 2]}')]},
        {"name": kind + "/plain_prompt", "kind": kind, "prompt": PLAIN_PROMPT, "answers": [chat_answer("plain words")]},
        {"name": kind + "/unauthorized", "kind": kind, "prompt": PLAIN_PROMPT,
         "answers": [{"status": 401, "body": json.dumps({"error": {"message": "bad key", "code": "invalid_api_key"}})}]},
    ]
COMPATIBLE_CASES += [
    {"name": "local/schema_invalid_json_is_empty", "kind": "local", "prompt": CATEGORIZATION_PROMPT, "answers": [chat_answer("not json")]},
    {"name": "local/schema_400_then_text", "kind": "local", "prompt": CATEGORIZATION_PROMPT,
     "answers": [{"status": 400, "body": json.dumps({"error": {"message": "response_format not supported"}})}, chat_answer('{"a": 2}')]},
    {"name": "local/server_error_then_success", "kind": "local", "prompt": PLAIN_PROMPT,
     "answers": [{"status": 503, "body": "{}"}, chat_answer("ok")]},
    {"name": "local/server_error_twice", "kind": "local", "prompt": PLAIN_PROMPT, "answers": [{"status": 500, "body": "{}"}]},
    {"name": "local/rate_limited_twice", "kind": "local", "prompt": PLAIN_PROMPT,
     "answers": [{"status": 429, "body": "{}", "headers": {"retry-after": "0"}}]},
    {"name": "local/model_override", "kind": "local", "prompt": PLAIN_PROMPT, "model": "qwen2.5:7b", "answers": [chat_answer("x")]},
    {"name": "local/no_usage", "kind": "local", "prompt": PLAIN_PROMPT, "answers": [{"status": 200, "body": json.dumps({
        "id": "c", "object": "chat.completion", "created": 0, "model": "m",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": "x"}, "finish_reason": "stop"}]})}]},
    {"name": "local/no_choices", "kind": "local", "prompt": PLAIN_PROMPT, "answers": [{"status": 200, "body": json.dumps({
        "id": "c", "object": "chat.completion", "created": 0, "model": "m", "choices": []})}]},
]


class Recorder:
    def __init__(self, answers):
        self.answers = answers
        self.requests = []

    def respond(self, request, response_cls):
        body = request.content.decode() if request.content else ""
        self.requests.append({
            "method": request.method,
            "path": request.url.path,
            "headers": {k: request.headers.get(k) for k in ("x-api-key", "anthropic-version", "authorization", "content-type")
                        if request.headers.get(k) is not None},
            "body": json.loads(body) if body else None,
        })
        answer = self.answers[min(len(self.requests) - 1, len(self.answers) - 1)]
        headers = {"content-type": "application/json"}
        headers.update(answer.get("headers", {}))
        return response_cls(answer["status"], headers=headers, content=answer["body"].encode())


def outcome(result=None, exc=None):
    if exc is not None:
        name = type(exc).__name__
        return {"error_class": ERROR_CLASSES.get(name, "uncaught:" + name)}
    return {"error_class": "", "text": result.text, "model": result.model,
            "input_tokens": result.input_tokens, "output_tokens": result.output_tokens}


async def run_anthropic(case):
    recorder = Recorder(case["answers"])
    transport = httpx.MockTransport(lambda request: recorder.respond(request, httpx.Response))
    anthropic_mod.make_hardened_async_httpx_client = lambda: httpx.AsyncClient(transport=transport)
    provider = anthropic_mod.AnthropicProvider(api_key="oracle-placeholder", base_url="http://oracle.invalid",
                                               model=case.get("model", "claude-haiku-5-5"))
    try:
        result = await provider.complete(case["prompt"])
        out = outcome(result=result)
    except Exception as exc:  # the provider's own classification is the answer
        out = outcome(exc=exc)
    await provider.aclose()
    return {"name": case["name"], "prompt": case["prompt"], "answers": case["answers"], "requests": recorder.requests, "outcome": out}


async def run_compatible(case):
    recorder = Recorder(case["answers"])
    transport = httpx2.MockTransport(lambda request: recorder.respond(request, httpx2.Response))
    local_mod.make_hardened_async_httpx2_client = lambda: httpx2.AsyncClient(transport=transport)
    base = "http://oracle.invalid/v1"
    model = case.get("model", "m-" + case["kind"])
    if case["kind"] == "gemini":
        provider = GeminiProvider(api_key="oracle-placeholder", base_url=base, model=model)
    elif case["kind"] == "qwen":
        provider = QwenProvider(api_key="oracle-placeholder", base_url=base, model=model)
    else:
        provider = local_mod.LocalProvider(base_url=base, model=model, api_key="oracle-placeholder")
    try:
        result = await provider.complete(case["prompt"])
        out = outcome(result=result)
    except Exception as exc:
        out = outcome(exc=exc)
    await provider.aclose()
    return {"name": case["name"], "kind": case["kind"], "prompt": case["prompt"], "answers": case["answers"],
            "requests": recorder.requests, "outcome": out}


async def main():
    runner, cases = (run_anthropic, ANTHROPIC_CASES) if SUITE == "anthropic" else (run_compatible, COMPATIBLE_CASES)
    results = [await runner(case) for case in cases]
    import anthropic
    import openai
    header = {
        "producer": "dev-health-ops frozen Python llm providers, executed",
        "ops_sha": OPS_SHA,
        "suite": SUITE,
        "python": sys.version.split()[0],
        "sdk": {"anthropic": anthropic.__version__, "openai": openai.__version__, "httpx": httpx.__version__,
                "httpx2": httpx2.__version__},
        "source_sha256": {f: hashlib.sha256((SRC / f).read_bytes()).hexdigest() for f in PROVIDER_FILES},
    }
    json.dump({"header": header, "cases": [tag(r) for r in results]}, sys.stdout, sort_keys=True, ensure_ascii=True, indent=1)
    sys.stdout.write("\n")


asyncio.run(main())
