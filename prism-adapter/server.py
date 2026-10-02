"""Loopback-only Prism browser adapter for CLIProxyAPI P0/P1 text requests."""

import hashlib
import hmac
import json
import os
import re
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

BASE_URL = "https://prism.openai.com"
START_PATH = "/api/llm/response_with_tools_start"
STATUS_PATH = "/api/llm/response_with_tools_status"
MODEL = "gpt-5.6-sol"
MAX_BODY_BYTES = 1 << 20
MAX_PROMPT_CHARS = 32_000
PROJECT_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f-]{27,}$")
ACCOUNT_RE = re.compile(r"^[1-9][0-9]{0,18}$")


class AdapterError(Exception):
    def __init__(self, status, code, message):
        super().__init__(message)
        self.status = status
        self.code = code


def parse_request(payload):
    if not isinstance(payload, dict):
        raise AdapterError(400, "invalid_request", "request body must be an object")
    if payload.get("model") != MODEL:
        raise AdapterError(422, "unsupported_model", "Prism P1 supports gpt-5.6-sol only")
    if any(payload.get(key) is not None for key in ("tools", "additional_tools", "previous_response_id", "conversation")):
        raise AdapterError(422, "unsupported_request", "Prism P1 does not support tools or server-side conversation state")
    if any(payload.get(key) is not None for key in ("max_output_tokens", "temperature", "top_p", "background", "store", "include", "service_tier", "response_format")):
        raise AdapterError(422, "unsupported_request", "Prism P1 supports plain text Responses options only")
    reasoning = payload.get("reasoning") or {}
    if not isinstance(reasoning, dict) or reasoning.get("effort", "medium") != "medium":
        raise AdapterError(422, "unsupported_reasoning", "Prism P1 supports medium reasoning only")
    if reasoning.get("summary") not in (None, "none"):
        raise AdapterError(422, "unsupported_reasoning", "Prism P1 does not support reasoning summaries")
    if not isinstance(payload.get("stream", False), bool):
        raise AdapterError(400, "invalid_request", "stream must be a boolean")

    items = payload.get("input")
    if isinstance(items, str):
        items = [{"role": "user", "content": items}]
    if not isinstance(items, list) or not items:
        raise AdapterError(400, "invalid_request", "input must contain text")
    parts = []
    instructions = payload.get("instructions", "")
    if instructions:
        if not isinstance(instructions, str):
            raise AdapterError(400, "invalid_request", "instructions must be text")
        parts.append("[instructions]\n" + instructions)
    for item in items:
        if not isinstance(item, dict) or item.get("type", "message") != "message":
            raise AdapterError(422, "unsupported_input", "Prism P1 accepts text messages only")
        role = item.get("role", "user")
        if role not in ("system", "developer", "user", "assistant"):
            raise AdapterError(400, "invalid_request", "invalid message role")
        content = item.get("content")
        if isinstance(content, str):
            text = content
        elif isinstance(content, list) and content and all(
            isinstance(part, dict) and part.get("type") in ("input_text", "output_text", "text") and isinstance(part.get("text"), str)
            for part in content
        ):
            text = "\n".join(part["text"] for part in content)
        else:
            raise AdapterError(422, "unsupported_input", "Prism P1 accepts text messages only")
        if not text.strip():
            raise AdapterError(400, "invalid_request", "message content must not be empty")
        parts.append("[" + role + "]\n" + text)
    prompt = "\n\n".join(parts)
    if not prompt.strip() or len(prompt) > MAX_PROMPT_CHARS:
        raise AdapterError(400, "invalid_request", "text input is empty or too long")
    return prompt, payload.get("stream", False)


def terminal_text(data):
    if not isinstance(data, dict):
        return None
    response = data.get("response") or {}
    if data.get("status") in ("failed", "error") or response.get("status") in ("failed", "error"):
        raise AdapterError(502, "prism_failed", "Prism turn failed")
    if data.get("status") not in ("completed", "success"):
        return None
    output = (response.get("payload") or {}).get("output") or []
    texts = [part.get("text", "") for item in output if isinstance(item, dict) and item.get("type") == "message"
             for part in item.get("content", []) if isinstance(part, dict) and isinstance(part.get("text"), str)]
    if not texts:
        raise AdapterError(502, "unsupported_output", "Prism returned no text message")
    return "".join(texts)


class Journal:
    def __init__(self, root):
        self.root = Path(root)
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.pending = self.root / "pending"
        self.pending.mkdir(mode=0o700, exist_ok=True)
        self.receipts = self.root / "receipts"
        self.receipts.mkdir(mode=0o700, exist_ok=True)

    @staticmethod
    def atomic(path, value):
        tmp = path.with_name(path.name + ".tmp")
        with open(tmp, "w", encoding="utf-8") as handle:
            json.dump(value, handle, ensure_ascii=False, separators=(",", ":"))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)

    def begin(self, account_id, project_id):
        path = self.pending / account_id
        if path.exists():
            raise AdapterError(409, "pending_turn", "previous Prism turn outcome is unknown")
        self.atomic(path, {"stage": "submitting", "project_id": project_id, "started_at": int(time.time())})
        return path

    def update(self, path, **fields):
        value = json.loads(path.read_text(encoding="utf-8"))
        value.update(fields)
        self.atomic(path, value)

    def finish(self, path):
        try:
            path.unlink()
        except FileNotFoundError:
            pass

    def receipt(self, account_id, request_id, status, answer=""):
        digest = hashlib.sha256(request_id.encode()).hexdigest()
        value = {"account_id": account_id, "request_id": request_id, "model": MODEL,
                 "status": status, "usage_source": "unavailable", "completed_at": int(time.time())}
        if answer:
            value["answer_sha256"] = hashlib.sha256(answer.encode()).hexdigest()
            value["answer_chars"] = len(answer)
        self.atomic(self.receipts / (digest + ".json"), value)


class StartGate:
    def __init__(self):
        self.armed = False
        self.sent = False
        self.error = ""

    def accept(self, body):
        metadata = body.get("metadata") if isinstance(body, dict) else None
        if not self.armed or self.sent or not isinstance(metadata, dict) or metadata.get("model") != MODEL or metadata.get("reasoning_effort") != "medium":
            self.error = "Prism attempted an unarmed, repeated or mismatched start"
            return False
        self.sent = True
        return True


class BrowserTurn:
    def __init__(self, journal, chrome):
        self.journal = journal
        self.chrome = chrome

    def run(self, account_id, token, prompt):
        from playwright.sync_api import sync_playwright

        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(executable_path=self.chrome, headless=True, chromium_sandbox=True)
            gate = StartGate()
            pending = None
            request_id = ""
            terminal = None
            try:
                context = browser.new_context(service_workers="block")
                context.add_cookies([{"name": "prism_oai_access_token", "value": token, "domain": "prism.openai.com", "path": "/", "secure": True}])
                page = context.new_page()
                page.set_default_timeout(90_000)

                def gate_route(route):
                    nonlocal request_id
                    try:
                        request = route.request
                        if request.url == BASE_URL + START_PATH:
                            if gate.accept(request.post_data_json):
                                route.continue_()
                            else:
                                route.abort()
                            return
                        if request.url == BASE_URL + STATUS_PATH:
                            body = request.post_data_json
                            if gate.sent and request_id and isinstance(body, dict) and body.get("request_id") == request_id:
                                route.continue_()
                            else:
                                route.abort()
                            return
                        route.continue_()
                    except Exception:
                        gate.error = "Prism request could not be validated"
                        route.abort()

                page.route("**/api/llm/response_with_tools_*", gate_route)
                page.goto(BASE_URL, wait_until="domcontentloaded")
                page.get_by_role("button", name="New", exact=True).click()
                page.get_by_role("menuitem", name="Blank project").click()
                page.wait_for_function("new URL(location.href).searchParams.has('u')")
                project_id = parse_qs(urlparse(page.url).query).get("u", [""])[0]
                if not PROJECT_RE.fullmatch(project_id):
                    raise AdapterError(502, "invalid_project", "Prism returned an invalid project identifier")
                page.goto(BASE_URL + "/?u=" + project_id + "&pg=1", wait_until="domcontentloaded")
                textarea = page.locator('textarea[placeholder="Ask anything"]')
                textarea.wait_for(state="visible")
                pending = self.journal.begin(account_id, project_id)
                starts = []
                polls = []

                def on_request(request):
                    if request.url == BASE_URL + START_PATH:
                        starts.append(True)
                    elif request.url == BASE_URL + STATUS_PATH:
                        polls.append(True)

                def on_response(response):
                    nonlocal request_id, terminal
                    if response.url not in (BASE_URL + START_PATH, BASE_URL + STATUS_PATH):
                        return
                    try:
                        data = response.json()
                    except Exception:
                        return
                    returned_id = data.get("request_id")
                    if returned_id:
                        if request_id and returned_id != request_id:
                            gate.error = "Prism returned a foreign request identifier"
                            return
                        request_id = returned_id
                        self.journal.update(pending, stage="polling", request_id=request_id, turn_state=data.get("turn_state", ""))
                    result = terminal_text(data)
                    if result is not None:
                        terminal = result

                page.on("request", on_request)
                page.on("response", on_response)
                textarea.fill(prompt)
                gate.armed = True
                textarea.press("Enter")
                deadline = time.monotonic() + 240
                while time.monotonic() < deadline and terminal is None and not gate.error:
                    page.wait_for_timeout(500)
                if not gate.sent:
                    self.journal.finish(pending)
                    raise AdapterError(502, "start_not_sent", "Prism did not submit the turn")
                if gate.error or len(starts) != 1:
                    raise AdapterError(502, "unexpected_start", "Prism did not start exactly one turn")
                if terminal is None or not request_id:
                    raise AdapterError(504, "unknown_outcome", "Prism turn has no terminal result")
                self.journal.receipt(account_id, request_id, "completed", terminal)
                self.journal.finish(pending)
                return request_id, terminal, len(polls)
            finally:
                browser.close()


def response_payload(request_id, text):
    safe = re.sub(r"[^A-Za-z0-9_-]", "_", request_id)[:100]
    return {"id": "resp_prism_" + safe, "object": "response", "created_at": int(time.time()), "model": MODEL,
            "status": "completed", "usage": None, "output": [{"id": "msg_prism_" + safe, "type": "message",
            "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": text, "annotations": []}]}]}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    api_key = ""
    turn = None
    lock = None

    def log_message(self, *_args):
        return

    def send_json(self, status, value):
        body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.send_json(200 if self.path == "/health" else 404, {"status": "ok"} if self.path == "/health" else {"error": {"type": "not_found"}})

    def do_POST(self):
        if self.path != "/v1/responses":
            self.send_json(404, {"error": {"type": "not_found"}})
            return
        if not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + self.api_key):
            self.send_json(401, {"error": {"type": "unauthorized"}})
            return
        account_id = self.headers.get("X-Prism-Account-ID", "")
        token = self.headers.get("X-Prism-OAuth-Token", "")
        if not ACCOUNT_RE.fullmatch(account_id) or not token or "\n" in token or "\r" in token:
            self.send_json(400, {"error": {"type": "invalid_request", "message": "account identity is required"}})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length < 1 or length > MAX_BODY_BYTES or self.headers.get("Transfer-Encoding"):
                raise AdapterError(413, "request_too_large", "request body is empty or too large")
            payload = json.loads(self.rfile.read(length))
            prompt, stream = parse_request(payload)
            if not self.lock.acquire(blocking=False):
                raise AdapterError(429, "prism_busy", "Prism browser is busy; request was not submitted")
            try:
                request_id, answer, _polls = self.turn.run(account_id, token, prompt)
            finally:
                self.lock.release()
            response = response_payload(request_id, answer)
            if stream:
                created = dict(response, status="in_progress", output=[])
                body = ("event: response.created\ndata: " + json.dumps({"type": "response.created", "response": created}, separators=(",", ":")) + "\n\n" +
                        "event: response.completed\ndata: " + json.dumps({"type": "response.completed", "response": response}, separators=(",", ":")) + "\n\n").encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(body)
            else:
                self.send_json(200, response)
        except AdapterError as error:
            self.send_json(error.status, {"error": {"type": error.code, "message": str(error)}})
        except (ValueError, TypeError):
            self.send_json(400, {"error": {"type": "invalid_request", "message": "invalid JSON request"}})
        except Exception:
            print(json.dumps({"event": "prism_adapter_error", "class": "internal"}), file=sys.stderr, flush=True)
            self.send_json(502, {"error": {"type": "prism_unavailable", "message": "Prism browser request failed; inspect pending state before retrying"}})


def main():
    if os.geteuid() == 0:
        raise SystemExit("Prism adapter must run as a non-root user")
    api_key = os.environ.get("PRISM_ADAPTER_API_KEY", "")
    chrome = os.environ.get("PRISM_ADAPTER_CHROME", "")
    sandbox = os.environ.get("CHROME_DEVEL_SANDBOX", "")
    if len(api_key) < 32 or not Path(chrome).is_file() or not sandbox:
        raise SystemExit("adapter key, Chromium binary, and Chromium sandbox are required")
    Handler.api_key = api_key
    Handler.lock = __import__("threading").Lock()
    Handler.turn = BrowserTurn(Journal(os.environ.get("PRISM_ADAPTER_STATE_DIR", "/var/lib/cliproxy-prism")), chrome)
    server = ThreadingHTTPServer(("127.0.0.1", int(os.environ.get("PRISM_ADAPTER_PORT", "8319"))), Handler)
    server.daemon_threads = True
    server.serve_forever()


if __name__ == "__main__":
    main()
