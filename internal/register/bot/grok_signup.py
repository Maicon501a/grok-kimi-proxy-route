"""Single-account xAI registration worker for the Go/Wails host.

Adapted from xinxinshuhao-create/grok-register@36f379ab. The upstream direct
HTTP registration flow is kept, while unbounded worker loops and plaintext key
files are replaced by a cancellable one-shot stdout protocol.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import email as email_parser
import hashlib
import hmac
import imaplib
import json
import os
import random
import re
import secrets
import shutil
import socket
import string
import struct
import sys
import tempfile
import time
from dataclasses import dataclass
from typing import Any
from urllib.parse import quote, urlencode, urljoin, urlparse

import requests as std_requests
from curl_cffi import requests
from dotenv import load_dotenv

load_dotenv()

UPSTREAM_COMMIT = "36f379ab2307ca1f718fd6c4502f4c0239317ce0"
SITE_URL = "https://accounts.x.ai"
USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
)
DEFAULT_SITE_KEY = "0x4AAAAAAAhr9JGVDZbrZOo0"
DEFAULT_STATE_TREE = "%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D"
OTP_RE = re.compile(r"\b([A-Z0-9]{3})-([A-Z0-9]{3})\b", re.I)
ACTION_PATTERNS = (
    re.compile(r"release[:\s]*[\"']([a-fA-F0-9]{40})[\"']"),
    re.compile(r"(7f[a-fA-F0-9]{40})"),
)


class SignupError(RuntimeError):
    def __init__(self, step: str, message: str):
        super().__init__(message)
        self.step = step


def emit(line: str) -> None:
    print(line, flush=True)


def step(name: str, message: str = "") -> None:
    emit(f"__STEP__ {name}" + (f" {message}" if message else ""))


def finish(status: str, *, step_name: str = "", reason: str = "") -> None:
    payload = {"status": status, "upstream_commit": UPSTREAM_COMMIT}
    if step_name:
        payload["step"] = step_name
    if reason:
        payload["reason"] = reason
    emit("__RESULT__ " + json.dumps(payload, ensure_ascii=False))


def proxy_url() -> str:
    return os.getenv("GROK_PROXY", "").strip()


def proxy_map() -> dict[str, str] | None:
    value = proxy_url()
    return {"http": value, "https": value} if value else None


def curl_session() -> requests.Session:
    kwargs: dict[str, Any] = {"impersonate": "chrome120"}
    if proxy_map():
        kwargs["proxies"] = proxy_map()
    return requests.Session(**kwargs)


def random_name() -> str:
    n = random.randint(4, 7)
    return random.choice(string.ascii_uppercase) + "".join(random.choices(string.ascii_lowercase, k=n - 1))


def random_password() -> str:
    return "Gk!" + "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(16))


def protobuf_varint(value: int) -> bytes:
    out = bytearray()
    while True:
        current = value & 0x7F
        value >>= 7
        out.append(current | (0x80 if value else 0))
        if not value:
            return bytes(out)


def grpc_string(field_id: int, value: str) -> bytes:
    raw = value.encode("utf-8")
    payload = protobuf_varint((field_id << 3) | 2) + protobuf_varint(len(raw)) + raw
    return b"\x00" + struct.pack(">I", len(payload)) + payload


def extract_otp(text: str) -> str:
    match = OTP_RE.search(text or "")
    return (match.group(1) + match.group(2)).upper() if match else ""


class Inbox:
    provider = ""
    address = ""

    def wait_code(self, timeout: int = 150) -> str:
        deadline = time.time() + timeout
        while time.time() < deadline:
            code = extract_otp(self.read_latest())
            if code:
                return code
            time.sleep(5)
        raise SignupError("otp", f"verification code did not arrive within {timeout}s")

    def read_latest(self) -> str:
        raise NotImplementedError


class MailTMInbox(Inbox):
    provider = "mailtm"
    base = "https://api.mail.tm"

    def __init__(self) -> None:
        self.http = requests.Session(impersonate="chrome120")
        self.http.headers.update({"User-Agent": USER_AGENT, "Accept": "application/json"})
        if proxy_map():
            self.http.proxies = proxy_map() or {}
        domains_response = self.http.get(f"{self.base}/domains", timeout=20)
        domains_response.raise_for_status()
        body = domains_response.json()
        domains = body if isinstance(body, list) else body.get("hydra:member", [])
        enabled = [d.get("domain") for d in domains if d.get("domain") and d.get("isActive", True)]
        if not enabled:
            raise SignupError("email", "mail.tm returned no active domain")
        self.address = f"grok{secrets.token_hex(6)}@{random.choice(enabled)}"
        self.password = random_password()
        created = self.http.post(
            f"{self.base}/accounts",
            json={"address": self.address, "password": self.password},
            timeout=20,
        )
        if created.status_code != 201:
            raise SignupError("email", f"mail.tm create HTTP {created.status_code}: {created.text[:160]}")
        auth = self.http.post(
            f"{self.base}/token",
            json={"address": self.address, "password": self.password},
            timeout=20,
        )
        auth.raise_for_status()
        self.token = auth.json()["token"]

    def read_latest(self) -> str:
        headers = {"Authorization": f"Bearer {self.token}"}
        listing = self.http.get(f"{self.base}/messages", headers=headers, timeout=15)
        if listing.status_code != 200:
            return ""
        body = listing.json()
        messages = body if isinstance(body, list) else body.get("hydra:member", [])
        if not messages:
            return ""
        detail = self.http.get(f"{self.base}/messages/{messages[0]['id']}", headers=headers, timeout=15)
        if detail.status_code != 200:
            return ""
        msg = detail.json()
        html = msg.get("html", [])
        if isinstance(html, list):
            html = "\n".join(html)
        return "\n".join(str(x or "") for x in (msg.get("subject"), msg.get("text"), html, msg.get("intro")))


class LuckMailInbox(Inbox):
    provider = "luckmail"

    def __init__(self) -> None:
        self.base = os.getenv("LUCKMAIL_BASE_URL", "https://mails.luckyous.com").strip().rstrip("/")
        self.api_key = os.getenv("LUCKMAIL_API_KEY", "").strip()
        self.api_secret = os.getenv("LUCKMAIL_API_SECRET", "").strip()
        self.use_hmac = os.getenv("LUCKMAIL_USE_HMAC", "").strip().lower() in {"1", "true", "yes"}
        if not self.api_key:
            raise SignupError("config", "LUCKMAIL_API_KEY is required for EMAIL_PROVIDER=luckmail")
        self.http = std_requests.Session()
        if proxy_map():
            self.http.proxies.update(proxy_map() or {})
        data = self.request(
            "POST",
            "/api/v1/openapi/email/purchase",
            {
                "project_code": os.getenv("LUCKMAIL_PROJECT_CODE", "grok"),
                "quantity": 1,
                "email_type": os.getenv("LUCKMAIL_EMAIL_TYPE", "ms_imap"),
                "domain": os.getenv("LUCKMAIL_DOMAIN", "outlook.com"),
            },
        )
        purchases = (data or {}).get("purchases", [])
        if not purchases:
            raise SignupError("email", "LuckMail purchase returned no inbox")
        self.address = str(purchases[0].get("email_address", "")).strip()
        self.token = str(purchases[0].get("token", "")).strip()
        if not self.address or not self.token:
            raise SignupError("email", "LuckMail purchase omitted email_address or token")

    def headers(self) -> dict[str, str]:
        headers = {"X-API-Key": self.api_key, "Accept": "application/json", "Content-Type": "application/json"}
        if self.use_hmac and self.api_secret:
            timestamp = str(int(time.time()))
            nonce = secrets.token_hex(16)
            message = f"{self.api_key}{timestamp}{nonce}".encode()
            headers.update({
                "X-Timestamp": timestamp,
                "X-Nonce": nonce,
                "X-Signature": hmac.new(self.api_secret.encode(), message, hashlib.sha256).hexdigest(),
            })
        return headers

    def request(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        response = self.http.request(method, self.base + path, headers=self.headers(), json=body, timeout=30)
        response.raise_for_status()
        payload = response.json()
        if isinstance(payload, dict) and payload.get("code", 0) not in (0, "0"):
            raise SignupError("email", f"LuckMail: {payload.get('message', 'API error')}")
        return payload.get("data") if isinstance(payload, dict) else payload

    def read_latest(self) -> str:
        try:
            code = self.request("GET", f"/api/v1/openapi/email/token/{quote(self.token, safe='')}/code") or {}
            chunks = [str(code.get("verification_code", "")), json.dumps(code.get("mail", {}), ensure_ascii=False)]
            mails = self.request("GET", f"/api/v1/openapi/email/token/{quote(self.token, safe='')}/mails") or {}
            rows = mails.get("mails", [])
            if rows:
                row = rows[0]
                chunks.extend(str(row.get(k, "")) for k in ("subject", "body", "html_body"))
                message_id = str(row.get("message_id", ""))
                if message_id:
                    detail = self.request(
                        "GET",
                        f"/api/v1/openapi/email/token/{quote(self.token, safe='')}/mails/{quote(message_id, safe='')}",
                    ) or {}
                    chunks.extend(str(detail.get(k, "")) for k in ("subject", "body_text", "body_html", "verification_code"))
            return "\n".join(chunks)
        except Exception:
            return ""


class MailNestInbox(Inbox):
    provider = "mailnest"

    def __init__(self) -> None:
        self.api_key = os.getenv("MAILNEST_API_KEY", "").strip()
        if not self.api_key:
            raise SignupError("config", "MAILNEST_API_KEY is required for EMAIL_PROVIDER=mailnest")
        self.http = std_requests.Session()
        self.http.headers.update({"Authorization": f"Bearer {self.api_key}"})
        if proxy_map():
            self.http.proxies.update(proxy_map() or {})
        rows = self.request("POST", "/api/v1/email/temporary/buy", {
            "project_code": os.getenv("MAILNEST_PROJECT_CODE", "x-ai001"), "count": 1,
        })
        if not rows:
            rows = self.request("POST", "/api/v1/email/exclusive/buy", {"count": 1})
        self.address = str((rows or [{}])[0].get("email", "")).strip()
        if not self.address:
            raise SignupError("email", "MailNest purchase returned no inbox")

    def request(self, method: str, path: str, body: dict[str, Any]) -> Any:
        response = self.http.request(method, "https://mailnest.top" + path, json=body, timeout=30)
        response.raise_for_status()
        payload = response.json()
        if payload.get("code") != "00000":
            raise SignupError("email", f"MailNest: {payload}")
        return payload.get("data")

    def read_latest(self) -> str:
        try:
            rows = self.request("POST", "/api/v1/email/receive", {"email": self.address}) or []
            if not rows:
                return ""
            return "\n".join(str(rows[0].get(k, "")) for k in ("subject", "body_preview", "body"))
        except Exception:
            return ""


class GmailInbox(Inbox):
    provider = "gmail"

    def __init__(self) -> None:
        self.base_email = os.getenv("GMAIL_BASE_EMAIL", "").strip()
        self.password = os.getenv("GMAIL_APP_PASSWORD", "").strip()
        if not self.base_email or not self.password or "@" not in self.base_email:
            raise SignupError("config", "GMAIL_BASE_EMAIL and GMAIL_APP_PASSWORD are required")
        local, domain = self.base_email.split("@", 1)
        self.address = f"{local}+grok{secrets.token_hex(5)}@{domain}"
        self.started = time.time()

    def read_latest(self) -> str:
        client = imaplib.IMAP4_SSL("imap.gmail.com", 993)
        try:
            client.login(self.base_email, self.password)
            client.select("INBOX")
            status, ids = client.search(None, "ALL")
            if status != "OK":
                return ""
            for msg_id in ids[0].split()[-10:][::-1]:
                status, data = client.fetch(msg_id, "(RFC822)")
                if status != "OK" or not data or not isinstance(data[0], tuple):
                    continue
                msg = email_parser.message_from_bytes(data[0][1])
                delivered = str(msg.get("Delivered-To", "")) + str(msg.get("To", ""))
                if self.address.lower() not in delivered.lower():
                    continue
                chunks = [str(msg.get("Subject", ""))]
                for part in msg.walk():
                    if part.get_content_type() in ("text/plain", "text/html"):
                        raw = part.get_payload(decode=True) or b""
                        chunks.append(raw.decode(part.get_content_charset() or "utf-8", errors="replace"))
                return "\n".join(chunks)
            return ""
        finally:
            try:
                client.logout()
            except Exception:
                pass


def create_inbox(provider: str) -> Inbox:
    provider = provider.strip().lower()
    factories = {
        "luckmail": LuckMailInbox,
        "mailtm": MailTMInbox,
        "mailnest": MailNestInbox,
        "gmail": GmailInbox,
    }
    if provider not in factories:
        raise SignupError("config", f"unsupported EMAIL_PROVIDER={provider!r}; use luckmail, mailnest, gmail, or mailtm")
    return factories[provider]()


class YesCaptcha:
    api = "https://api.yescaptcha.com"

    def __init__(self) -> None:
        self.key = os.getenv("YESCAPTCHA_KEY", "").strip()
        if not self.key:
            raise SignupError("config", "YESCAPTCHA_KEY is required")

    def solve(self, site_key: str) -> str:
        created = std_requests.post(
            self.api + "/createTask",
            json={
                "clientKey": self.key,
                "task": {"type": "TurnstileTaskProxyless", "websiteURL": SITE_URL, "websiteKey": site_key},
                "softID": 102154,
            },
            timeout=30,
        )
        created.raise_for_status()
        payload = created.json()
        if payload.get("errorId") != 0:
            raise SignupError("turnstile", f"YesCaptcha createTask: {payload.get('errorDescription')}")
        task_id = payload["taskId"]
        time.sleep(5)
        for _ in range(30):
            response = std_requests.post(
                self.api + "/getTaskResult",
                json={"clientKey": self.key, "taskId": task_id},
                timeout=30,
            )
            response.raise_for_status()
            data = response.json()
            if data.get("errorId") != 0:
                raise SignupError("turnstile", f"YesCaptcha result: {data.get('errorDescription')}")
            if data.get("status") == "ready":
                token = str((data.get("solution") or {}).get("token", ""))
                if token:
                    return token
                break
            time.sleep(2)
        raise SignupError("turnstile", "YesCaptcha did not return a token")


class DrissionTurnstile:
    """Solve Turnstile in installed Chrome through a fresh local CDP profile."""

    def __init__(self, headless: bool) -> None:
        self.headless = headless

    @staticmethod
    def chrome_path() -> str:
        configured = os.getenv("GROK_CHROME_PATH", "").strip()
        candidates = [configured] if configured else []
        if os.name == "nt":
            candidates.extend([
                os.path.join(os.getenv("PROGRAMFILES", ""), "Google", "Chrome", "Application", "chrome.exe"),
                os.path.join(os.getenv("PROGRAMFILES(X86)", ""), "Google", "Chrome", "Application", "chrome.exe"),
                os.path.join(os.getenv("LOCALAPPDATA", ""), "Google", "Chrome", "Application", "chrome.exe"),
            ])
        for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
            found = shutil.which(name)
            if found:
                candidates.append(found)
        for candidate in candidates:
            if candidate and os.path.isfile(candidate):
                return os.path.abspath(candidate)
        raise SignupError("turnstile", "installed Google Chrome was not found; set GROK_CHROME_PATH")

    @staticmethod
    def free_cdp_port() -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            return int(sock.getsockname()[1])

    def solve(self, site_key: str) -> str:
        from DrissionPage import ChromiumOptions, ChromiumPage

        profile_dir = tempfile.mkdtemp(prefix="grok-chrome-profile-")
        cdp_port = self.free_cdp_port()
        options = ChromiumOptions(read_file=False)
        options.set_browser_path(self.chrome_path())
        options.set_local_port(cdp_port)
        options.set_user_data_path(profile_dir)
        options.set_argument(f"--window-size={random.randint(1200, 1400)},{random.randint(800, 1000)}")
        options.set_argument("--lang=en-US")
        options.set_argument("--no-first-run")
        options.set_argument("--no-default-browser-check")
        if proxy_url():
            options.set_argument("--proxy-server=" + proxy_url())
        if self.headless:
            options.set_argument("--headless=new")
        emit(f"turnstile launching installed Chrome with fresh profile via CDP port {cdp_port}")
        page = None
        try:
            page = ChromiumPage(options)
            page.get(SITE_URL + "/sign-up?redirect=grok-com")
            time.sleep(4)
            page.run_js("""
                const nodes = document.querySelectorAll('button,[role=button],a');
                for (const node of nodes) {
                    if (!node.offsetParent) continue;
                    const text = (node.innerText || '').trim().toLowerCase();
                    if (text.includes('email') || text.includes('e-mail') || text.includes('邮箱')) {
                        node.click(); break;
                    }
                }
            """)
            # Fast path: give the page's native widget only a few seconds. The
            # old 120-second wait dominated the whole signup even though the
            # explicit render below is the path that consistently succeeds.
            try:
                native_wait = int(os.getenv("GROK_TURNSTILE_NATIVE_WAIT", "8"))
            except ValueError:
                native_wait = 8
            native_wait = max(2, min(native_wait, 30))
            deadline = time.time() + native_wait
            while time.time() < deadline:
                token = page.run_js('return document.querySelector("[name=cf-turnstile-response]")?.value || ""') or ""
                if len(token) > 50:
                    return token
                # Drission can pierce the challenge frame without WebDriver.
                try:
                    frame = page.get_frame('@src():challenges.cloudflare.com')
                    checkbox = frame.ele('tag:input@type=checkbox', timeout=1) if frame else None
                    if checkbox:
                        checkbox.click()
                except Exception:
                    pass
                time.sleep(1)
            emit("turnstile native widget did not finish quickly; using explicit fast render")
            # Explicitly render the widget in the same real Chrome page and
            # wait for its callback. If the signup bundle has not loaded the
            # Turnstile API yet, load the official explicit-render script.
            token = page.run_js("""
                const sitekey = arguments[0];
                return new Promise(resolve => {
                    const timer = setTimeout(() => resolve('timeout'), 70000);
                    const render = () => {
                        try {
                            document.getElementById('_grok_drission_turnstile')?.remove();
                            const mount = document.createElement('div');
                            mount.id = '_grok_drission_turnstile';
                            mount.style.cssText = 'position:fixed;top:10px;right:10px;z-index:99999';
                            document.body.appendChild(mount);
                            window.turnstile.render('#' + mount.id, {
                                sitekey,
                                theme: 'light',
                                callback: value => { clearTimeout(timer); resolve(value); },
                                'error-callback': error => { clearTimeout(timer); resolve('error:' + String(error)); }
                            });
                        } catch (error) {
                            clearTimeout(timer);
                            resolve('error:' + String(error));
                        }
                    };
                    if (window.turnstile?.render) {
                        render();
                        return;
                    }
                    const script = document.createElement('script');
                    script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit';
                    script.async = true;
                    script.onload = render;
                    script.onerror = () => { clearTimeout(timer); resolve('error:script-load'); };
                    document.head.appendChild(script);
                });
            """, site_key, timeout=75) or ""
            if isinstance(token, str) and len(token) > 50:
                return token
            raise SignupError("turnstile", f"DrissionPage did not obtain a Turnstile token: {token}")
        finally:
            if page is not None:
                page.quit()
            for _ in range(5):
                try:
                    shutil.rmtree(profile_dir)
                    break
                except FileNotFoundError:
                    break
                except OSError:
                    time.sleep(0.5)


@dataclass
class SignupConfig:
    site_key: str
    action_id: str
    state_tree: str


def action_from_javascript(source: str) -> str:
    for pattern in ACTION_PATTERNS:
        match = pattern.search(source or "")
        if match:
            return match.group(1)
    return ""


def discover_config(session: requests.Session) -> SignupConfig:
    response = session.get(SITE_URL + "/sign-up", timeout=30)
    response.raise_for_status()
    html = response.text
    site_key_match = re.search(r'sitekey\"?:\"(0x4[a-zA-Z0-9_-]+)\"', html)
    tree_match = re.search(r'next-router-state-tree\"?:\"([^\"]+)\"', html)
    js_urls = sorted(set(urljoin(SITE_URL + "/sign-up", path) for path in re.findall(r"/_next/static/chunks/[^\"'\s>]+\.js", html)))
    proxies = proxy_map()

    def fetch(url: str) -> str:
        try:
            kwargs: dict[str, Any] = {"impersonate": "chrome120"}
            if proxies:
                kwargs["proxies"] = proxies
            with requests.Session(**kwargs) as chunk_session:
                body = chunk_session.get(url, timeout=15, headers={"User-Agent": USER_AGENT}).text
                return action_from_javascript(body)
        except Exception:
            return ""

    action_id = ""
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(10, max(1, len(js_urls)))) as pool:
        for found in pool.map(fetch, js_urls):
            if found:
                action_id = found
                break
    cache = os.path.join(os.path.dirname(__file__), ".action_id.cache")
    if action_id:
        try:
            with open(cache, "w", encoding="ascii") as handle:
                handle.write(action_id)
        except OSError:
            pass
    else:
        try:
            cached = open(cache, encoding="ascii").read().strip()
            if action_from_javascript(f'release:"{cached}"') or re.fullmatch(r"7f[a-fA-F0-9]{40}", cached):
                action_id = cached
        except OSError:
            pass
    if not action_id:
        raise SignupError("discovery", f"Next.js action id not found ({len(js_urls)} chunks scanned)")
    return SignupConfig(
        site_key=site_key_match.group(1) if site_key_match else DEFAULT_SITE_KEY,
        action_id=action_id,
        state_tree=tree_match.group(1) if tree_match else DEFAULT_STATE_TREE,
    )


def send_email_code(session: requests.Session, address: str) -> tuple[bytes, str]:
    response = session.post(
        SITE_URL + "/auth_mgmt.AuthManagement/CreateEmailValidationCode",
        data=grpc_string(1, address),
        headers={
            "content-type": "application/grpc-web+proto",
            "x-grpc-web": "1",
            "x-user-agent": "connect-es/2.1.1",
            "origin": SITE_URL,
            "referer": SITE_URL + "/sign-up?redirect=grok-com",
        },
        timeout=20,
    )
    if response.status_code != 200:
        raise SignupError("otp", f"CreateEmailValidationCode HTTP {response.status_code}: {response.text[:160]}")
    return response.content, response.headers.get("content-type", "application/grpc-web+proto")


def safe_cookie_redirects(text: str) -> list[str]:
    normalized = (text or "").replace("\\u0026", "&").replace("\\/", "/")
    found: list[str] = []
    # Next.js RSC text chunks use `<row>:T<hex-length>,<text>`. Respect the
    # declared length; a regex-only extraction consumes the first character of
    # the following row and corrupts the encrypted `q` value (HTTP 400).
    for match in re.finditer(r"\d+:T([0-9a-fA-F]+),(https://)", normalized):
        length = int(match.group(1), 16)
        start = match.start(2)
        value = normalized[start:start + length]
        if "set-cookie" in value:
            found.append(value)
    if not found:
        found = re.findall(r"https://[^\"'\s]+set-cookie[^\"'\s]*", normalized)
    safe: list[str] = []
    for value in found:
        value = value.rstrip(":")
        host = (urlparse(value).hostname or "").lower()
        if (
            host == "x.ai" or host.endswith(".x.ai") or
            host == "grok.com" or host.endswith(".grok.com") or
            host == "auth.grokipedia.com"
        ):
            safe.append(value)
    return safe


def register_account(provider: str, max_attempts: int, captcha=None) -> tuple[dict[str, str], str]:
    step("discovery", "scanning xAI signup metadata")
    with curl_session() as discovery:
        config = discover_config(discovery)
    captcha = captcha or YesCaptcha()
    last_error: Exception | None = None
    for attempt in range(1, max_attempts + 1):
        try:
            step("email", f"{provider} attempt={attempt}")
            inbox = create_inbox(provider)
            given, family, password = random_name(), random_name(), random_password()
            with curl_session() as session:
                try:
                    session.get(SITE_URL, timeout=15)
                except Exception:
                    pass
                step("otp", "requesting verification code")
                send_email_code(session, inbox.address)
                code = inbox.wait_code()
                payload = [{
                    "emailValidationCode": code,
                    "createUserAndSessionRequest": {
                        "email": inbox.address,
                        "givenName": given,
                        "familyName": family,
                        "clearTextPassword": password,
                        "tosAcceptedVersion": "$undefined",
                    },
                    "turnstileToken": "",
                    "promptOnDuplicateEmail": True,
                }]
                step("turnstile", "solving challenge")
                turnstile = captcha.solve(config.site_key)
                payload[0]["turnstileToken"] = turnstile
                headers = {
                    "user-agent": USER_AGENT,
                    "accept": "text/x-component",
                    "content-type": "text/plain;charset=UTF-8",
                    "origin": SITE_URL,
                    "referer": SITE_URL + "/sign-up",
                    "next-router-state-tree": config.state_tree,
                    "next-action": config.action_id,
                }
                step("signup", "submitting account")
                response = session.post(SITE_URL + "/sign-up", json=payload, headers=headers, timeout=45)
                if response.status_code != 200:
                    raise SignupError("signup", f"signup HTTP {response.status_code}: {response.text[:220]}")
                response_text = response.text
                redirects = safe_cookie_redirects(response.text)
                for redirect in redirects:
                    try:
                        cookie_response = session.get(redirect, allow_redirects=True, timeout=20)
                        emit(
                            f"signup set-cookie follow: status={cookie_response.status_code} "
                            f"host={urlparse(cookie_response.url).hostname}"
                        )
                    except Exception as exc:
                        emit(f"signup set-cookie follow failed: {type(exc).__name__}: {exc}")
                    if session.cookies.get("sso"):
                        break
                sso = session.cookies.get("sso") or response.cookies.get("sso")
                cookie_header = response.headers.get("set-cookie", "")
                if not sso and cookie_header:
                    match = re.search(r"(?:^|[,;]\s*)sso=([^;\s,]+)", cookie_header)
                    sso = match.group(1) if match else ""
                if not sso:
                    errors = re.findall(r'\"error\"\s*:\s*\"([^\"]+)', response_text, re.I)
                    if not errors:
                        errors = re.findall(r'error[^A-Za-z0-9]{1,8}([^\r\n\"]{1,240})', response_text, re.I)
                    marker = f" errors={errors[:3]}" if errors else ""
                    marker += f" set_cookie_markers={response_text.lower().count('set-cookie')}"
                    raise SignupError("signup", f"xAI returned 200 without SSO;{marker}; body_start={response_text[:220]}")
                creds = {
                    "email": inbox.address,
                    "name": f"{given} {family}",
                    "password": password,
                    "provider": inbox.provider,
                }
                return creds, str(sso)
        except SignupError as exc:
            last_error = exc
            emit(f"attempt {attempt}/{max_attempts} failed at {exc.step}: {exc}")
            if exc.step == "config":
                raise
        except Exception as exc:
            last_error = exc
            emit(f"attempt {attempt}/{max_attempts} failed: {type(exc).__name__}: {exc}")
        if attempt < max_attempts:
            time.sleep(min(5 * attempt, 15))
    if isinstance(last_error, SignupError):
        raise last_error
    raise SignupError("signup", f"registration failed after {max_attempts} attempts: {last_error}")


def authorize_device(verification_url: str, user_code: str, sso: str, headless: bool) -> None:
    step("authorize", "opening OAuth device grant with the new SSO session")
    from patchright.sync_api import sync_playwright

    proxy = {"server": proxy_url()} if proxy_url() else None
    with sync_playwright() as playwright:
        launch: dict[str, Any] = {
            "headless": headless,
            "channel": "chrome",
            "args": ["--lang=en-US"],
        }
        if proxy:
            launch["proxy"] = proxy
        browser = playwright.chromium.launch(**launch)
        try:
            context = browser.new_context(viewport={"width": 1280, "height": 900}, locale="en-US")
            context.add_cookies([{"name": "sso", "value": sso, "domain": ".x.ai", "path": "/"}])
            page = context.new_page()
            page.goto(verification_url, timeout=60_000, wait_until="domcontentloaded")
            deadline = time.time() + 120
            clicked_allow = False
            while time.time() < deadline:
                current = page.url.lower()
                text = (page.locator("body").inner_text(timeout=5_000) or "").lower()
                if "/device/done" in current or "authorization complete" in text or "device authorized" in text:
                    return
                if user_code:
                    code_input = page.locator('input[name="user_code"], input[autocomplete="one-time-code"], input[type="text"]')
                    if code_input.count() and code_input.first.is_visible():
                        value = code_input.first.input_value()
                        if not value:
                            code_input.first.fill(user_code)
                button = page.locator("button, [role=button], input[type=submit]")
                for index in range(min(button.count(), 30)):
                    candidate = button.nth(index)
                    if not candidate.is_visible():
                        continue
                    label = (candidate.inner_text() or candidate.get_attribute("value") or "").strip().lower()
                    if label in {"allow", "permitir", "autorizar"} or label.startswith(("allow ", "permitir ", "autorizar ")):
                        candidate.click()
                        clicked_allow = True
                        time.sleep(2)
                        break
                    if label in {"continue", "continuar", "next", "avançar", "avancar"}:
                        candidate.click()
                        time.sleep(2)
                        break
                if clicked_allow:
                    time.sleep(3)
                    # The Go host's concurrent token poll is authoritative.
                    # xAI does not consistently navigate to /device/done after
                    # accepting consent, so a successful Allow click is enough
                    # for this browser worker to hand control back to Go.
                    return
                time.sleep(1)
            raise SignupError("authorize", f"device grant was not confirmed; last URL={page.url}")
        finally:
            browser.close()


def validate_config(provider: str) -> None:
    captcha = os.getenv("CAPTCHA_PROVIDER", "").strip().lower()
    if not captcha:
        captcha = "yescaptcha" if os.getenv("YESCAPTCHA_KEY", "").strip() else "browser"
    if captcha not in {"browser", "yescaptcha"}:
        raise SignupError("config", f"unsupported CAPTCHA_PROVIDER={captcha!r}")
    if captcha == "yescaptcha" and not os.getenv("YESCAPTCHA_KEY", "").strip():
        raise SignupError("config", "missing YESCAPTCHA_KEY for CAPTCHA_PROVIDER=yescaptcha")
    required = {
        "luckmail": ("LUCKMAIL_API_KEY",),
        "mailnest": ("MAILNEST_API_KEY",),
        "gmail": ("GMAIL_BASE_EMAIL", "GMAIL_APP_PASSWORD"),
        "mailtm": (),
    }
    if provider not in required:
        raise SignupError("config", f"unsupported EMAIL_PROVIDER={provider!r}")
    missing = [name for name in required[provider] if not os.getenv(name, "").strip()]
    if missing:
        raise SignupError("config", "missing " + ", ".join(missing))


def main() -> None:
    parser = argparse.ArgumentParser(description="xAI one-shot signup and device authorization")
    parser.add_argument("--verification-url", required=True)
    parser.add_argument("--user-code", default="")
    parser.add_argument("--headless", default="false")
    parser.add_argument("--email-provider", default=os.getenv("EMAIL_PROVIDER", "mailtm"))
    parser.add_argument("--max-attempts", type=int, default=2)
    parser.add_argument("--check-config", action="store_true")
    args = parser.parse_args()
    provider = args.email_provider.strip().lower()
    validate_config(provider)
    if args.check_config:
        finish("success")
        return
    headless = args.headless.strip().lower() in {"1", "true", "yes"}
    captcha = os.getenv("CAPTCHA_PROVIDER", "").strip().lower()
    if not captcha:
        captcha = "yescaptcha" if os.getenv("YESCAPTCHA_KEY", "").strip() else "browser"
    if captcha == "browser":
        creds, sso = register_account(
            provider,
            max(1, min(args.max_attempts, 5)),
            captcha=DrissionTurnstile(headless),
        )
    else:
        creds, sso = register_account(provider, max(1, min(args.max_attempts, 5)))
    step("web_ok", creds["email"])
    emit("__CREDS__ " + json.dumps(creds, ensure_ascii=False))
    authorize_device(
        args.verification_url,
        args.user_code,
        sso,
        headless,
    )
    step("done")
    finish("success")


if __name__ == "__main__":
    try:
        main()
    except SignupError as exc:
        finish("error", step_name=exc.step, reason=str(exc))
        sys.exit(1)
    except Exception as exc:
        finish("error", step_name="runtime", reason=f"{type(exc).__name__}: {exc}")
        sys.exit(1)

