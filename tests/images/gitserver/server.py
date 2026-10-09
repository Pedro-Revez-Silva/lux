"""git smart HTTP with basic auth. Repos live under /repos/<name>.git.

GIT_TOKEN: the password every request must carry. Requests are logged to
/repos/requests.log (method, path, whether it was authenticated) so tests
can see what reached the forge.
"""

import base64
import http.server
import os
import subprocess
import time

TOKEN = os.environ["GIT_TOKEN"]
ROOT = "/repos"


class Handler(http.server.BaseHTTPRequestHandler):
    def _authorized(self) -> bool:
        h = self.headers.get("Authorization", "")
        if not h.startswith("Basic "):
            return False
        try:
            _, pw = base64.b64decode(h[6:]).decode().split(":", 1)
        except Exception:
            return False
        return pw == TOKEN

    def _handle(self):
        ok = self._authorized()
        with open(f"{ROOT}/requests.log", "a") as f:
            f.write(f"{self.command} {self.path} {'auth' if ok else 'anon'}\n")
        if not ok:
            self.send_response(401)
            self.send_header("WWW-Authenticate", 'Basic realm="git"')
            self.end_headers()
            return
        path, _, query = self.path.partition("?")
        # A repository with a delay file answers its ref advertisement that
        # many seconds late: a clone or fetch of it is slow.
        repo = path.split(".git/", 1)[0]
        if path.endswith("/info/refs") and os.path.exists(f"{ROOT}{repo}.git/delay"):
            with open(f"{ROOT}{repo}.git/delay") as f:
                time.sleep(float(f.read().strip() or 0))
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        env = {
            **os.environ,
            "GIT_PROJECT_ROOT": ROOT,
            "GIT_HTTP_EXPORT_ALL": "1",
            "PATH_INFO": path,
            "QUERY_STRING": query,
            "REQUEST_METHOD": self.command,
            "CONTENT_TYPE": self.headers.get("Content-Type", ""),
            "CONTENT_LENGTH": str(len(body)),
            "REMOTE_USER": "lux",
        }
        out = subprocess.run(["/usr/libexec/git-core/git-http-backend"], input=body, env=env,
                             capture_output=True).stdout
        head, _, rest = out.partition(b"\r\n\r\n")
        status = 200
        headers = []
        for line in head.decode().split("\r\n"):
            if not line:
                continue
            k, _, v = line.partition(":")
            if k.lower() == "status":
                status = int(v.strip().split()[0])
            else:
                headers.append((k, v.strip()))
        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(rest)))
        self.end_headers()
        self.wfile.write(rest)

    do_GET = _handle
    do_POST = _handle

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
