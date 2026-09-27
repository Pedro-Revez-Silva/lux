"""Generate a host config fixture for cmd/luxd's real config loader test."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from luxhost import desired, luxdconf  # noqa: E402

if len(sys.argv) != 2 or sys.argv[1] not in ("access", "key"):
    raise SystemExit("usage: render_config.py access|key")
access = sys.argv[1] == "access"

infra = {
    "luxd_port": "7070",
    "public_url": "https://lux.example.com",
    "db_name": "lux",
    "blob_bucket": "lux-blobs-test",
    "cf_access_team": "acme" if access else "",
    "cf_access_aud": "aud-tag" if access else "",
}
if access:
    state = ('[luxd]\ndebug = true\n[luxd.defaults]\nmemory = "16Gi"\n'
             '[console.cloudflare_access]\noperators = ["operator@example.com", "second@example.com"]\n'
             'default_tenant = "ten_aaaaaaaaaaaaaaaa"\n')
else:
    state = (Path(__file__).resolve().parents[1] / "lux-host.toml").read_text()
want = desired.parse(state)
sys.stdout.write(luxdconf.render(infra, "eu-north-1", want, {"app_password": "test-password"},
                                 "10.60.0.10", "/usr/local/lib/lux/runner"))
