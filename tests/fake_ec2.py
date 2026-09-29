"""A fake EC2 for the test suite: an HTTP server speaking the EC2 Query API
calls lux uses (RunInstances, TerminateInstances, DescribeInstances and
CreateTags), where an instance is a simulated host container that
boots lux-runner from its user data, as a real instance's AMI would.

luxd's EC2 provider is pointed at it with LUX_EC2_ENDPOINT, so the real
provider code runs; `run_tests.py --real-ec2` swaps in real AWS instead.
"""

from __future__ import annotations

import base64
import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs
from xml.sax.saxutils import escape

NS = "http://ec2.amazonaws.com/doc/2016-11-15/"


def _parse_user_data(userdata: str) -> dict[str, str]:
    """The runner's env out of whichever userData format luxd rendered
    (internal/hostboot): "ignition" (a JSON config; the env is a `data:`
    URL at /etc/lux/runner.env), "script" (a shell script exporting the
    env before the shared, unfilled bootstrap body), or "env" (plain
    KEY=value lines). Never executes anything; only ignition and script
    carry LUX_PROVIDER_ID appended by their (unexecuted) fetch step, so it
    is never present here — the fake sets it itself via --provider-id."""
    stripped = userdata.lstrip()
    if stripped.startswith("{"):
        cfg = json.loads(userdata)
        for f in cfg.get("storage", {}).get("files", []):
            if f["path"] == "/etc/lux/runner.env":
                data_url = f["contents"]["source"]
                _, b64 = data_url.split(",", 1)
                return _parse_env_lines(base64.b64decode(b64).decode())
        return {}
    lines = userdata.splitlines()
    if lines and lines[0].startswith("#!"):
        lines = lines[1:]
    if lines and lines[0].startswith("export "):
        # "script": a run of `export KEY='value'` lines the exports helper
        # wrote, then the shared bootstrap body (which reassigns
        # LUX_HOST_NAME its own default): stop at the first non-export line.
        env: dict[str, str] = {}
        for line in lines:
            if not line.startswith("export "):
                break
            k, v = line.removeprefix("export ").split("=", 1)
            if v.startswith("'") and v.endswith("'"):
                v = v[1:-1].replace("'\\''", "'")
            env[k] = v
        return env
    # "env": plain KEY=value lines, the whole content.
    return _parse_env_lines(userdata)


def _parse_env_lines(text: str) -> dict[str, str]:
    return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)


# The instance type the fake's launch templates stand for.
FAKE_TEMPLATE_TYPE = "m7i.large"


class FakeEC2:
    real = False

    def __init__(self, env, template: dict):
        self.env = env
        self.template = template
        self.lock = threading.Lock()
        self.instances: dict[str, dict] = {}  # id → {state, host, tags, userdata}
        self.calls: list[str] = []
        self.fail_launches = False
        self.no_boot = False  # launched instances never start a runner
        self.lose_reply = False
        self.deny_create_tags = False  # CreateTags refused, as without IAM's ec2:CreateTags
        self.notices: dict[str, dict] = {}  # id → spot instance-action
        self.server = ThreadingHTTPServer((env.gateway, 0), self._handler())
        self.url = f"http://{env.gateway}:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        with self.lock:
            ids = list(self.instances)
        for i in ids:
            self._terminate(i)

    # -- what tests look at ----------------------------------------------

    def running(self) -> list[dict]:
        with self.lock:
            return [dict(id=i, **v) for i, v in self.instances.items() if v["state"] == "running"]

    def userdata(self, instance_id: str) -> str:
        """The raw user data RunInstances sent for one instance, decoded:
        for a test that runs it for real on a simulated host, rather than
        only checking the env it decodes to."""
        with self.lock:
            return self.instances[instance_id]["userdata"]

    def orphan_next_launch(self):
        """The next RunInstances succeeds but its reply is lost (as when luxd
        stops right after the call)."""
        self.lose_reply = True

    def interrupt(self, instance_id: str, seconds: float = 120):
        """A spot interruption: the notice appears in the instance's metadata
        now, and EC2 terminates it `seconds` later."""
        at = time.time() + seconds
        with self.lock:
            self.notices[instance_id] = {"action": "terminate",
                                         "time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(at))}
        threading.Timer(seconds, self._terminate, args=(instance_id,)).start()

    def kill(self, instance_id: str):
        """An instance vanishing behind lux's back."""
        self._terminate(instance_id)

    def purge(self, instance_id: str):
        """EC2 forgetting a terminated instance (it answers NotFound)."""
        self._terminate(instance_id)
        with self.lock:
            self.instances.pop(instance_id, None)

    # -- the API -----------------------------------------------------------

    def _handler(self):
        fake = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_PUT(self):  # IMDSv2 session token
                self._imds(lambda iid: (200, "fake-imds-token"))

            def do_GET(self):  # the instance metadata a spot runner watches
                def action(iid):
                    with fake.lock:
                        notice = fake.notices.get(iid)
                    return (200, json.dumps(notice)) if notice else (404, "")
                self._imds(action)

            def _imds(self, answer):
                parts = self.path.split("/")  # /imds/<instance>/latest/...
                code, text = answer(parts[2]) if len(parts) > 2 and parts[1] == "imds" else (404, "")
                out = text.encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)

            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", 0))).decode()
                q = {k: v[0] for k, v in parse_qs(body).items()}
                action = q.get("Action", "")
                fake.calls.append(action)
                try:
                    xml = getattr(fake, "_" + action)(q)
                    code = 200
                except KeyError:
                    code, xml = 400, _error("InvalidAction", action)
                except FakeError as e:
                    code, xml = 400, _error(e.code, str(e))
                out = xml.encode()
                self.send_response(code)
                self.send_header("Content-Type", "text/xml")
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)

        return H

    def _RunInstances(self, q):
        if self.fail_launches:
            raise FakeError("InsufficientInstanceCapacity", "no capacity (fake)")
        userdata = base64.b64decode(q.get("UserData", "")).decode()
        env = _parse_user_data(userdata)
        tags = {}
        i = 1
        while f"TagSpecification.1.Tag.{i}.Key" in q:
            tags[q[f"TagSpecification.1.Tag.{i}.Key"]] = q.get(f"TagSpecification.1.Tag.{i}.Value", "")
            i += 1
        iid = "i-" + uuid.uuid4().hex[:17]
        with self.lock:
            self.instances[iid] = {"state": "pending", "host": None, "tags": tags, "env": env,
                                   "userdata": userdata,
                                   "launchTemplate": q.get("LaunchTemplate.LaunchTemplateId") or q.get("LaunchTemplate.LaunchTemplateName"),
                                   "instanceType": q.get("InstanceType"), "subnet": q.get("SubnetId"),
                                   "market": q.get("InstanceMarketOptions.MarketType")}
        threading.Thread(target=self._boot, args=(iid,), daemon=True).start()
        if self.lose_reply:
            self.lose_reply = False
            raise FakeError("RequestLimitExceeded", "the reply was lost (fake)")
        # As EC2: the reply names the type actually launched (the launch
        # template's when the request gives none) and the zone.
        itype = q.get("InstanceType") or FAKE_TEMPLATE_TYPE
        zone = (self.template.get("region") or "us-east-1") + "a"
        return (f'<RunInstancesResponse xmlns="{NS}"><reservationId>r-{uuid.uuid4().hex[:17]}</reservationId>'
                f"<instancesSet><item><instanceId>{iid}</instanceId><instanceType>{escape(itype)}</instanceType>"
                f"<placement><availabilityZone>{escape(zone)}</availabilityZone></placement>"
                f"<instanceState><code>0</code><name>pending</name>"
                f"</instanceState></item></instancesSet></RunInstancesResponse>")

    def _boot(self, iid: str):
        """Start the instance's host and, as its AMI's boot script would,
        lux-runner with the user data's environment."""
        with self.lock:
            inst = self.instances.get(iid)
            if not inst or inst["state"] != "pending":
                return
            name = inst["env"].get("LUX_HOST_NAME", iid)
        host = self.env.add_host(f"ec2-{iid[2:10]}")
        with self.lock:
            inst = self.instances.get(iid)
            booted = inst is not None and inst["state"] == "pending"
            if booted:
                inst["host"], inst["state"] = host, "running"
        if not booted:  # terminated while booting
            _remove(host)
            return
        if self.no_boot:
            return
        e = inst["env"]
        # The instance's metadata endpoint is this fake's, per instance.
        host.start_runner(self.env, e["LUX_HOST_TOKEN"], "--provider-id", iid, "--ec2-imds", f"{self.url}/imds/{iid}",
                          name=name, url=e.get("LUX_URL"))

    def _TerminateInstances(self, q):
        ids = _list(q, "InstanceId")
        items = ""
        for i in ids:
            if i not in self.instances:
                raise FakeError("InvalidInstanceID.NotFound", f"The instance ID '{i}' does not exist")
            prev = self.instances[i]["state"]
            self._terminate(i)
            items += (f"<item><instanceId>{i}</instanceId><currentState><code>32</code><name>shutting-down</name>"
                      f"</currentState><previousState><code>16</code><name>{prev}</name></previousState></item>")
        return f'<TerminateInstancesResponse xmlns="{NS}"><instancesSet>{items}</instancesSet></TerminateInstancesResponse>'

    def _terminate(self, iid: str):
        with self.lock:
            inst = self.instances.get(iid)
            if not inst or inst["state"] == "terminated":
                return
            inst["state"] = "terminated"
            host = inst["host"]
        if host is not None:
            _remove(host)

    def _CreateTags(self, q):
        if self.deny_create_tags:
            raise FakeError("UnauthorizedOperation", "You are not authorized to perform this operation. (fake: ec2:CreateTags)")
        ids = _list(q, "ResourceId")
        tags = {}
        i = 1
        while f"Tag.{i}.Key" in q:
            tags[q[f"Tag.{i}.Key"]] = q.get(f"Tag.{i}.Value", "")
            i += 1
        with self.lock:
            missing = [r for r in ids if r not in self.instances]
            if missing:
                raise FakeError("InvalidInstanceID.NotFound", f"The instance IDs '{', '.join(missing)}' do not exist")
            for r in ids:
                self.instances[r]["tags"].update(tags)
        return f'<CreateTagsResponse xmlns="{NS}"><return>true</return></CreateTagsResponse>'

    def _DescribeInstances(self, q):
        ids = _list(q, "InstanceId")
        filters = {}
        i = 1
        while f"Filter.{i}.Name" in q:
            filters[q[f"Filter.{i}.Name"]] = set(_list(q, f"Filter.{i}.Value"))
            i += 1
        with self.lock:
            missing = [i for i in ids if i not in self.instances]
            if missing:  # as EC2: unknown InstanceIds fail the call; filters do not
                raise FakeError("InvalidInstanceID.NotFound", f"The instance IDs '{', '.join(missing)}' do not exist")
            chosen = ids or list(self.instances)
            for name, values in filters.items():
                if name == "instance-id":
                    chosen = [c for c in chosen if c in values]
                elif name.startswith("tag:"):
                    chosen = [c for c in chosen if self.instances[c]["tags"].get(name[4:]) in values]
            items = "".join(
                f"<item><instanceId>{i}</instanceId><instanceState><code>16</code><name>{self.instances[i]['state']}</name>"
                f"</instanceState><tagSet>{_tags(self.instances[i]['tags'])}</tagSet></item>" for i in chosen)
        return (f'<DescribeInstancesResponse xmlns="{NS}"><reservationSet><item><reservationId>r-0</reservationId>'
                f"<instancesSet>{items}</instancesSet></item></reservationSet></DescribeInstancesResponse>")


class FakeError(Exception):
    def __init__(self, code: str, msg: str):
        super().__init__(msg)
        self.code = code


def _error(code: str, msg: str) -> str:
    return (f"<Response><Errors><Error><Code>{escape(code)}</Code><Message>{escape(msg)}</Message></Error></Errors>"
            f"<RequestID>{uuid.uuid4()}</RequestID></Response>")


def _tags(tags: dict) -> str:
    return "".join(f"<item><key>{escape(k)}</key><value>{escape(v)}</value></item>" for k, v in tags.items())


def _list(q: dict, prefix: str) -> list[str]:
    out, i = [], 1
    while f"{prefix}.{i}" in q:
        out.append(q[f"{prefix}.{i}"])
        i += 1
    return out


def _remove(host):
    from env import sh
    sh("docker", "rm", "-f", "-v", host.container, check=False)
