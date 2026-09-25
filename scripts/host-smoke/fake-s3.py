"""The smallest S3 luxd needs to start: HeadBucket answers 200."""
import http.server
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def _ok(self):
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_HEAD = do_GET = do_PUT = _ok

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
