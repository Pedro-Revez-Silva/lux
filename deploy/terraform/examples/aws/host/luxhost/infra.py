"""Infrastructure values: /etc/lux/host.json (first boot) and SSM (every run).

host.json is written once by cloud-init and names only what locating the
rest needs: the region, the SSM prefix and where the config repo is checked
out. Everything Terraform can change later (bucket names, public URL,
Access team/AUD, the tunnel token's parameter name, the config repo URL,
ref and deploy key) is an SSM parameter under the prefix, re-read on every
run, since aws_instance.control ignores user_data changes.
"""
import dataclasses
import json

from .host import Host, HostError

REQUIRED = [
    "public_url",
    "blob_bucket",
    "backup_bucket",
    "db_name",
    "luxd_port",
    "pg_data_volume_id",
    "tunnel_token_parameter",
    "config_repo_url",
    "config_repo_ref",
]
# Absent means empty: SSM refuses empty String values, so Terraform only
# creates these when they are set.
OPTIONAL = ["cf_access_team", "cf_access_aud", "config_repo_deploy_key_parameter"]


@dataclasses.dataclass(frozen=True)
class Bootstrap:
    region: str
    ssm_prefix: str
    checkout: str
    config_repo_path: str

    @property
    def host_dir(self) -> str:
        path = self.config_repo_path.strip("/")
        return f"{self.checkout}/{path}/host" if path else f"{self.checkout}/host"


def load_bootstrap(path: str) -> Bootstrap:
    try:
        with open(path) as f:
            doc = json.load(f)
        return Bootstrap(
            region=doc["region"],
            ssm_prefix=doc["ssm_prefix"].rstrip("/"),
            checkout=doc["checkout"],
            config_repo_path=doc.get("config_repo_path", ""),
        )
    except (OSError, ValueError, KeyError, AttributeError) as e:
        raise HostError(f"{path}: {e!r}") from None


def get_parameters(host: Host, names: list, region: str) -> dict:
    """Values by name for the String parameters that exist (up to SSM's 10
    names per GetParameters call, batched)."""
    values = {}
    for i in range(0, len(names), 10):
        out = host.run([
            "aws", "ssm", "get-parameters",
            "--names", *names[i:i + 10],
            "--region", region,
            "--output", "json",
        ])
        try:
            for p in json.loads(out.stdout)["Parameters"]:
                values[p["Name"]] = p["Value"]
        except (ValueError, KeyError, TypeError) as e:
            raise HostError(f"aws ssm get-parameters: unexpected output: {e!r}") from None
    return values


def get_secure_parameter(host: Host, name: str, region: str) -> str:
    out = host.run([
        "aws", "ssm", "get-parameter",
        "--name", name,
        "--with-decryption",
        "--region", region,
        "--query", "Parameter.Value",
        "--output", "text",
    ])
    return out.stdout.rstrip("\n")


def load_infra(host: Host, boot: Bootstrap) -> dict:
    """Every REQUIRED and OPTIONAL key's value, by short name."""
    names = {f"{boot.ssm_prefix}/{k}": k for k in REQUIRED + OPTIONAL}
    got = get_parameters(host, list(names), boot.region)
    missing = [n for n, k in names.items() if k in REQUIRED and n not in got]
    if missing:
        raise HostError(f"SSM parameters missing: {', '.join(missing)}")
    infra = {k: got.get(n, "") for n, k in names.items()}
    if not infra["luxd_port"].isdigit():
        raise HostError(f"{boot.ssm_prefix}/luxd_port: {infra['luxd_port']!r} is not a port")
    return infra
