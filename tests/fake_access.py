"""A fake Cloudflare Access for the test suite: the team's signing keys
(/cdn-cgi/access/certs), the identity endpoint (/cdn-cgi/access/get-identity),
and tokens signed as Access signs them (RS256, iss = the team domain, aud =
the application's AUD tag). luxd is pointed at it as its Access team, so its
real verification code runs."""

from __future__ import annotations

import base64
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric import padding, rsa


def _b64(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def _int(n: int) -> str:
    return _b64(n.to_bytes((n.bit_length() + 7) // 8, "big"))


class FakeAccess:
    AUD = "test-aud-0123456789"

    def __init__(self, host: str):
        self.key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        self.kid = "test-key-1"
        self.names: dict[str, str] = {}  # email → name, for get-identity
        self.pictures: dict[str, str] = {}  # email → photo URL (oidc_fields.picture)
        self.server = ThreadingHTTPServer((host, 0), self._handler())
        self.team = f"http://{host}:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()

    def token(self, email: str, name: str = "", *, picture: str = "", aud: str | None = None, iss: str | None = None,
              ttl: int = 300, key=None) -> str:
        """An Access application token for email (as Access puts in
        Cf-Access-Jwt-Assertion and the CF_Authorization cookie)."""
        if name:
            self.names[email] = name
        if picture:
            self.pictures[email] = picture
        now = int(time.time())
        header = {"alg": "RS256", "kid": self.kid, "typ": "JWT"}
        claims = {"aud": [aud or self.AUD], "email": email, "iss": iss or self.team, "sub": email,
                  "iat": now, "nbf": now, "exp": now + ttl, "type": "app"}
        signing = f"{_b64(json.dumps(header).encode())}.{_b64(json.dumps(claims).encode())}"
        sig = (key or self.key).sign(signing.encode(), padding.PKCS1v15(), hashes.SHA256())
        return f"{signing}.{_b64(sig)}"

    def _handler(self):
        access = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def _json(self, code: int, body: dict):
                b = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(b)))
                self.end_headers()
                self.wfile.write(b)

            def do_GET(self):
                if self.path == "/cdn-cgi/access/certs":
                    pub = access.key.public_key().public_numbers()
                    self._json(200, {"keys": [{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": access.kid,
                                               "n": _int(pub.n), "e": _int(pub.e)}]})
                elif self.path == "/cdn-cgi/access/get-identity":
                    cookie = self.headers.get("Cookie", "")
                    token = cookie.split("CF_Authorization=", 1)[-1].split(";")[0] if "CF_Authorization=" in cookie else ""
                    try:
                        claims = json.loads(base64.urlsafe_b64decode(token.split(".")[1] + "=="))
                    except Exception:
                        self._json(401, {"err": "no token"})
                        return
                    email = claims.get("email", "")
                    body = {"email": email, "name": access.names.get(email, ""), "id": email}
                    if email in access.pictures:
                        body["oidc_fields"] = {"picture": access.pictures[email]}
                    self._json(200, body)
                else:
                    self._json(404, {})

        return Handler
