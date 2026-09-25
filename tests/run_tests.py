#!/usr/bin/env python3
"""Entry point for the lux end-to-end suite.

    uv run python run_tests.py                   # build, bring everything up, run all suites
    uv run python run_tests.py --infra-only      # only check the environment itself
    uv run python run_tests.py suites/test_x.py  # one suite; -x, -k work as in pytest
    uv run python run_tests.py --keep            # keep containers and database to debug
    uv run python run_tests.py --hosts 3         # more simulated hosts
    uv run python run_tests.py --real-ec2        # EC2 suites against real AWS (nightly)
    uv run python run_tests.py --serve           # a lux to develop against (see serve.py)
    uv run python run_tests.py -j 4              # 4 environments, suites split between them

Each invocation gets its own database, bucket, Docker network and hosts, so
runs do not collide with each other or with a development instance.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from build import build_agent_images, build_binaries, build_fake_image, build_nested_image  # noqa: E402
from env import TestEnvironment  # noqa: E402

TESTS_DIR = Path(__file__).resolve().parent


def main() -> None:
    parser = argparse.ArgumentParser(description="Run the lux end-to-end suite")
    parser.add_argument("--infra-only", action="store_true", help="only check the environment itself")
    parser.add_argument("--keep", action="store_true", help="keep containers and database after the run")
    parser.add_argument("--hosts", type=int, default=2, help="number of simulated hosts")
    parser.add_argument("--real-ec2", action="store_true", help="run EC2 suites against real AWS")
    parser.add_argument("--serve", action="store_true", help="bring lux up with runners and a tenant, and keep it up")
    parser.add_argument("--detach", action="store_true", help="with --serve: return once it is up")
    parser.add_argument("--down", nargs="?", const="", metavar="ENV_JSON", help="take a detached --serve down")
    parser.add_argument("--image", action="append", default=[], metavar="REF",
                        help="with --serve: preload an image from the local Docker into every host (repeatable)")
    parser.add_argument("-j", "--jobs", type=int, default=1,
                        help="environments to run suites in, in parallel (each its own luxd, database, hosts)")
    args, pytest_args = parser.parse_known_args()

    if args.down is not None:
        from serve import down
        down(args.down or None)
        return

    print("building:")
    built = build_binaries()
    fake_image = build_fake_image(built.get("lux-fake"))
    # Agent images are big: build (and ship to hosts) only when their suite runs.
    agent_images = build_agent_images(any("agents" in a for a in pytest_args))

    # The nested image is only for its suite (a full run, or one naming it).
    # Suites named on the command line (paths); options and their values
    # are not.
    selected = [a for a in pytest_args if a.startswith("suites/") or a.endswith(".py")]
    nested = build_nested_image() if not selected or any("nested" in a for a in selected) else None
    images = {**agent_images, "nested": nested, **{ref: ref for ref in args.image}}
    binaries = {k: str(v) if v else None for k, v in built.items()}

    if args.jobs > 1 and not (args.serve or args.infra_only):
        sys.exit(run_parallel(args, pytest_args, selected, fake_image, binaries, images))

    env = TestEnvironment(n_hosts=args.hosts)
    env.binaries = binaries
    env.extra["images"] = images
    print(f"run id:   {env.run_id}")
    print(f"logs:     {env.log_dir}")

    if args.serve:
        from serve import serve
        try:
            serve(env, fake_image, args.detach)
        except BaseException:
            env.teardown()
            raise
        return

    exit_code = 1
    try:
        env.setup(fake_image=fake_image, luxd=not args.infra_only)
        print(f"network:  {env.network} ({env.subnet}, gateway {env.gateway})")
        print(f"hosts:    {', '.join(f'{h.name}={h.ip}' for h in env.hosts)}")
        if env.binaries.get("luxd") and not args.infra_only:
            print(f"luxd:     {env.luxd_url}")
        print()

        os.environ["LUX_TEST_ENV"] = str(env.env_file)
        if not args.infra_only:
            env.hand_over_luxd()
        if args.real_ec2:
            os.environ["LUX_TEST_REAL_EC2"] = "1"

        if args.infra_only:
            pytest_args += ["-m", "infra"]
        if not any(not a.startswith("-") for a in pytest_args if a not in ("infra",)):
            pytest_args = [str(TESTS_DIR / "suites"), *pytest_args]

        result = subprocess.run([sys.executable, "-m", "pytest", *pytest_args], cwd=TESTS_DIR)
        exit_code = result.returncode
    finally:
        env.teardown(keep=args.keep)
        print(f"\nlogs: {env.log_dir}")
        if args.keep:
            print(f"environment kept — network {env.network}, database {env.db_name}, bucket {env.bucket}")

    sys.exit(exit_code)


# Suites that take longest go first, so the split is even and the slowest
# ones are not left for last. Seconds from a full run; unknown ones count 10.
SUITE_SECONDS_FILE = TESTS_DIR / ".suite-seconds.json"


def _suite_seconds() -> dict[str, float]:
    try:
        return json.loads(SUITE_SECONDS_FILE.read_text())
    except (OSError, ValueError):
        return {}


def _split(suites: list[str], jobs: int, seconds: dict[str, float]) -> list[list[str]]:
    """Greedy longest-first: each suite to the least loaded job."""
    def weight(s):
        return seconds.get(Path(s).name, 10.0)
    loads = [[0.0, []] for _ in range(jobs)]
    for s in sorted(suites, key=weight, reverse=True):
        least = min(loads, key=lambda l: l[0])
        least[0] += weight(s)
        least[1].append(s)
    return [l[1] for l in loads if l[1]]


def run_parallel(args, pytest_args, selected, fake_image, binaries, images) -> int:
    """Runs suite files across args.jobs environments at once, each with its
    own luxd, database, bucket, network and hosts (as separate invocations
    would have). Shared Postgres and MinIO, and the images, are made once."""
    # Splitting is by suite file: node ids and pytest options that take a
    # path (--deselect, --ignore) would be taken for suites, so those runs
    # stay serial (use -j 1).
    if any("::" in a for a in pytest_args) or any(a.split("=")[0] in ("--deselect", "--ignore", "--ignore-glob")
                                                  for a in pytest_args):
        print("-j splits by suite file: node ids, --deselect and --ignore need a serial run (-j 1)")
        return 2
    options = [a for a in pytest_args if a not in selected]
    suites = selected or sorted(str(p.relative_to(TESTS_DIR)) for p in (TESTS_DIR / "suites").glob("test_*.py"))
    # A partial run (-k, -m, -x) does not say how long whole suites take.
    whole = not any(a in ("-x", "--exitfirst") or a.startswith(("-k", "-m")) for a in options)
    groups = _split(suites, args.jobs, _suite_seconds())
    envs = [TestEnvironment(n_hosts=args.hosts) for _ in groups]
    for e in envs:
        e.binaries, e.extra["images"] = binaries, images
    print(f"{len(groups)} environments: " + ", ".join(e.run_id for e in envs))

    def setup(e):
        e.setup(fake_image=fake_image, luxd=True)

    def run(i):
        e = envs[i]
        e.hand_over_luxd()
        env = {**os.environ, "LUX_TEST_ENV": str(e.env_file)}
        if args.real_ec2:
            env["LUX_TEST_REAL_EC2"] = "1"
        log = Path(e.log_dir) / "pytest.log"
        t0 = time.time()
        with open(log, "w") as out:
            r = subprocess.run([sys.executable, "-m", "pytest", *groups[i], *options, "--durations=0",
                                "--durations-min=0", "--color=no"], cwd=TESTS_DIR, stdout=out,
                               stderr=subprocess.STDOUT, env=env)
        return i, r.returncode, time.time() - t0, log

    code = 0
    logs = []
    t0 = time.time()
    try:
        # One at a time: shared services start once, and environments do not
        # race to create them.
        setup(envs[0])
        with concurrent.futures.ThreadPoolExecutor(len(envs)) as pool:
            list(pool.map(setup, envs[1:]))
            futures = [pool.submit(run, i) for i in range(len(envs))]
            for f in concurrent.futures.as_completed(futures):
                i, rc, secs, log = f.result()
                logs.append(log)
                # 5: nothing in this group matched -k/-m; others may have.
                ok = rc in (0, 5)
                code = code or (0 if ok else rc)
                lines = log.read_text(errors="replace").splitlines()
                tail = [l for l in lines if l.strip()][-1:]
                print(f"[{envs[i].run_id}] {'ok' if ok else 'FAILED'} in {secs:.0f}s: "
                      f"{' '.join(Path(s).name for s in groups[i])}\n    {tail[0] if tail else ''}")
                if not ok:
                    print("\n".join("    " + l for l in lines if l.startswith(("FAILED", "ERROR"))))
    finally:
        with concurrent.futures.ThreadPoolExecutor(len(envs)) as pool:
            list(pool.map(lambda e: e.teardown(keep=args.keep), envs))
    if whole:
        _record_suite_seconds(logs)
    print(f"\nall environments done in {time.time() - t0:.0f}s; logs: " + " ".join(e.log_dir for e in envs))
    return code


def _record_suite_seconds(logs: list[Path]) -> None:
    """Remembers each suite's time for the next split (from --durations)."""
    seconds = _suite_seconds()
    for log in logs:
        per: dict[str, float] = {}
        for s, name in re.findall(r"^([0-9.]+)s (?:call|setup|teardown)\s+(\S+)", log.read_text(errors="replace"), re.M):
            suite = Path(name.split("::")[0]).name
            per[suite] = per.get(suite, 0.0) + float(s)
        seconds.update({k: round(v, 1) for k, v in per.items()})
    try:
        SUITE_SECONDS_FILE.write_text(json.dumps(seconds, indent=1, sort_keys=True))
    except OSError:
        pass


if __name__ == "__main__":
    main()
