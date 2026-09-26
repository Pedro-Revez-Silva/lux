"""The desired-state file (host/lux-host.toml): parse and validate.

Everything is checked before any reconcile step runs, so a bad file fails
the run without touching Postgres, luxd.toml, the units or the install.
The [luxd] table is a subset of luxd.toml (cmd/luxd/config.go) limited to
operator choices; infrastructure values (listen, public_url, database, s3,
console) come from SSM and are refused here.
"""
import dataclasses
import re
import tomllib

from .host import HostError

DEFAULT_REPO = "marcioapm/lux"

_DURATION = re.compile(r"^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$")
_SIZE = re.compile(r"^([0-9]+(\.[0-9]+)?)\s*([KMGT]i?)?B?$")
_VERSION = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$")
_REPO = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")


def _duration(v):
    return isinstance(v, str) and bool(_DURATION.match(v))


def _positive_int(v):
    return isinstance(v, int) and not isinstance(v, bool) and v > 0


def _positive_number(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0


def _size(v):
    return _positive_int(v) or (isinstance(v, str) and bool(_SIZE.match(v.strip())))


def _percent(v):
    return _positive_int(v) and v <= 100


# key -> (check, what the error says it wants)
_LUXD_KEYS = {
    "debug": (lambda v: isinstance(v, bool), "true or false"),
    "lease": (_duration, "a Go duration such as \"30s\""),
    "tick": (_duration, "a Go duration"),
    "scale_down_after": (_duration, "a Go duration"),
    "launch_timeout": (_duration, "a Go duration"),
    "provider_check_every": (_duration, "a Go duration"),
    "lost_grace": (_duration, "a Go duration"),
    "listing_lag": (_duration, "a Go duration"),
    "outdated_drain_percent": (_percent, "an integer from 1 to 100"),
}
_LUXD_TABLES = {
    "defaults": {
        "cpus": (_positive_number, "a positive number"),
        "memory": (_size, "a size such as \"8Gi\""),
        "disk": (_size, "a size such as \"50Gi\""),
        "pids": (_positive_int, "a positive integer"),
    },
    "history": {
        "sample_every": (_duration, "a Go duration"),
        "raw": (_duration, "a Go duration"),
        "minutes": (_duration, "a Go duration"),
        "hours": (_duration, "a Go duration"),
    },
}


@dataclasses.dataclass(frozen=True)
class Desired:
    # None: install nothing, and leave an existing install alone.
    lux_version: str | None
    release_base_url: str
    # Validated luxd.toml settings: top-level keys, and {table: {key: value}}.
    luxd: dict
    luxd_tables: dict


def _check_table(table: dict, allowed: dict, where: str, problems: list) -> dict:
    out = {}
    for k, v in table.items():
        if k not in allowed:
            problems.append(f"{where}.{k}: unknown key")
            continue
        check, want = allowed[k]
        if not check(v):
            problems.append(f"{where}.{k}: {v!r}: want {want}")
            continue
        out[k] = v
    return out


def parse(text: str, source: str = "lux-host.toml") -> Desired:
    try:
        doc = tomllib.loads(text)
    except tomllib.TOMLDecodeError as e:
        raise HostError(f"{source}: {e}") from None
    problems = []

    version = doc.pop("lux_version", None)
    if version is not None and not isinstance(version, str):
        problems.append(f"lux_version: {version!r}: want a release tag or \"none\"")
        version = None
    elif version in ("", "none"):
        version = None
    elif version is not None and not _VERSION.match(version):
        problems.append(f"lux_version: {version!r}: not a release tag")

    repo = doc.pop("release_repo", DEFAULT_REPO)
    if not isinstance(repo, str) or not _REPO.match(repo):
        problems.append(f"release_repo: {repo!r}: want \"owner/repo\"")
    base_url = doc.pop("release_base_url", None)
    if base_url is None and isinstance(repo, str):
        base_url = f"https://github.com/{repo}/releases/download"
    elif base_url is not None and (not isinstance(base_url, str) or not re.match(r"^https?://", base_url)):
        problems.append(f"release_base_url: {base_url!r}: want an http(s) URL")

    luxd = doc.pop("luxd", {})
    top, tables = {}, {}
    if not isinstance(luxd, dict):
        problems.append("luxd: want a table")
    else:
        for name, allowed in _LUXD_TABLES.items():
            sub = luxd.pop(name, {})
            if not isinstance(sub, dict):
                problems.append(f"luxd.{name}: want a table")
                continue
            checked = _check_table(sub, allowed, f"luxd.{name}", problems)
            if checked:
                tables[name] = checked
        top = _check_table(luxd, _LUXD_KEYS, "luxd", problems)

    for k in doc:
        problems.append(f"{k}: unknown key")
    if problems:
        raise HostError(f"{source}: " + "; ".join(problems))
    return Desired(
        lux_version=version,
        release_base_url=base_url.rstrip("/"),
        luxd=top,
        luxd_tables=tables,
    )
