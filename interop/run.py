#!/usr/bin/env python3
"""Small bounded Docker runner for upstream transport binaries and black-box peers.

No Python packages required. Every run writes results, including unsupported pairs,
and exits nonzero for any non-pass. Public network probes require --public.
"""
import argparse
import json
import math
from pathlib import Path
import re
import socket
import subprocess
import time
import urllib.request
import uuid

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
UPSTREAM = "4ade567bfea81d291278a272825a7d35e73b40dd"
SOURCES = {
    "go": ("libp2p/go-libp2p", "dd26469c52a7ba555095df0f8088ae1fff8c7627", "images/go/transport-fix.patch", "GoUpstream.Dockerfile"),
    "rust": ("libp2p/rust-libp2p", "70082df7e6181722630eabc5de5373733aac9a21", "images/rust/v0.56/transport-fix.patch", "RustUpstream.Dockerfile"),
}
KUBO = "ipfs/kubo:v0.40.0@sha256:e08c602bf97f138a0ec5b42155b4fd4532852919250b11b4aadbea2bf42c0e10"
LIGHTHOUSE = "sigp/lighthouse:v8.0.1@sha256:774535b450d498de37d1c1a305cd36777bcf85bf0bc06c1ac54ef2552e7d81c2"
REDIS = "redis:7.4.2-bookworm@sha256:fbdbaea47b9ae4ecc2082ecdb4e1cea81e32176ffb1dcf643d422ad07427e5d9"


class Runner:
    def __init__(self, out):
        self.out = out
        out.mkdir(parents=True, exist_ok=True)
        self.prefix = "nu-interop-" + uuid.uuid4().hex[:10]
        self.network = self.prefix
        self.containers = []
        self.results = []
        self.network_created = False

    def cmd(self, args, timeout=180, check=True, log=None, cwd=ROOT):
        args = [str(a) for a in args]
        if log:
            with (self.out / log).open("a") as f:
                f.write("\n$ " + " ".join(args) + "\n")
                f.flush()
                p = subprocess.run(args, cwd=cwd, stdout=f, stderr=subprocess.STDOUT, timeout=timeout)
        else:
            p = subprocess.run(args, cwd=cwd, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        if check and p.returncode:
            raise RuntimeError(f"{args[:4]} exited {p.returncode}; " + (f"see {log}" if log else p.stderr + p.stdout))
        return p

    def docker(self, *args, **kw):
        return self.cmd(["docker", *args], **kw)

    def init_network(self):
        if not self.network_created:
            self.docker("network", "create", "--internal", self.network)
            self.network_created = True

    def start(self, name, image, args=(), env=None, options=()):
        name = self.prefix + "-" + name
        self.containers.append(name)
        command = ["run", "-d", "--name", name, "--network", self.network, *options]
        for k, v in (env or {}).items():
            command += ["-e", f"{k}={v}"]
        self.docker(*command, image, *args)
        return name

    def logs(self, name):
        p = self.docker("logs", name, check=False)
        return p.stdout + p.stderr

    def remove(self, name):
        (self.out / (name + ".log")).write_text(self.logs(name))
        self.docker("rm", "-fv", name, check=False)
        self.containers.remove(name)

    def wait_ready(self, predicate, seconds=90):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            value = predicate()
            if value:
                return value
            time.sleep(0.25)  # bounded readiness polling, not a fixed startup delay
        raise TimeoutError("container readiness deadline exceeded")

    def record(self, name, fn):
        print(name, flush=True)
        try:
            detail = fn()
            row = {"name": name, "status": "pass", "detail": detail}
        except Exception as e:
            row = {"name": name, "status": "fail", "error": str(e)}
        self.results.append(row)
        self.save()
        print(json.dumps(row), flush=True)

    def save(self):
        (self.out / "results.json").write_text(json.dumps({
            "recordedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "upstream": UPSTREAM,
            "nuRevision": self.cmd(["git", "rev-parse", "HEAD"]).stdout.strip(),
            "workingTreeDirty": bool(self.cmd(["git", "status", "--porcelain"]).stdout),
            "sources": SOURCES, "kubo": KUBO, "lighthouse": LIGHTHOUSE,
            "results": self.results,
        }, indent=2) + "\n")

    def build_nu(self):
        self.docker("build", "-f", HERE / "Dockerfile", "-t", "nu-interop:local", ROOT,
                    timeout=900, log="build-nu.log")

    def build_upstream(self, impl):
        repo, revision, patch, dockerfile = SOURCES[impl]
        source = HERE / ".cache" / (impl + "-" + revision)
        source.mkdir(parents=True, exist_ok=True)
        if not (source / ".prepared").exists():
            self.cmd(["git", "init", source], log=f"source-{impl}.log")
            self.cmd(["git", "-C", source, "fetch", "--depth=1", "https://github.com/" + repo, revision], log=f"source-{impl}.log")
            self.cmd(["git", "-C", source, "checkout", "--force", "FETCH_HEAD"], log=f"source-{impl}.log")
            url = f"https://raw.githubusercontent.com/libp2p/unified-testing/{UPSTREAM}/transport/{patch}"
            with urllib.request.urlopen(url, timeout=60) as response:
                (source / "transport.patch").write_bytes(response.read())
            self.cmd(["git", "-C", source, "apply", source / "transport.patch"], log=f"source-{impl}.log")
            (source / ".prepared").touch()
        image = "nu-interop-upstream-" + impl + ":" + revision[:12]
        self.docker("build", "-f", HERE / dockerfile, "-t", image, source,
                    timeout=2700, log=f"build-{impl}.log")
        self.docker("image", "inspect", image, log=f"image-{impl}.log")
        return image

    def transport(self, impl):
        if impl == "js":
            with urllib.request.urlopen(f"https://raw.githubusercontent.com/libp2p/unified-testing/{UPSTREAM}/transport/images.yaml", timeout=60) as response:
                (self.out / "upstream-images.yaml").write_bytes(response.read())
            for direction in ("nu-to-js", "js-to-nu"):
                self.results.append({"name": direction, "status": "unsupported", "error":
                    "Pinned unified-testing js-v3.x supports tcp/ws/wss only, not quic-v1. Not a passing test; Phase 0a gate remains open."})
            self.save()
            return
        image = self.build_upstream(impl)
        self.init_network()
        redis = self.start("redis", REDIS, options=("--network-alias", "redis"))
        self.wait_ready(lambda: self.docker("exec", redis, "redis-cli", "ping", check=False).stdout.strip() == "PONG")
        try:
            for dialer, listener, name in (("nu-interop:local", image, "nu-to-" + impl), (image, "nu-interop:local", impl + "-to-nu")):
                self.record(name, lambda d=dialer, l=listener, n=name: self.pair(d, l, n))
        finally:
            self.remove(redis)

    def pair(self, dialer, listener, name):
        env = {"TRANSPORT": "quic-v1", "LISTENER_IP": "0.0.0.0", "REDIS_ADDR": "redis:6379",
               "TEST_KEY": name, "TEST_TIMEOUT_SECS": "120", "IS_DIALER": "false"}
        # No SECURE_CHANNEL or MUXER: QUIC provides both. Rust rejects empty values.
        l = self.start(name + "-listener", listener, env=env)
        d = None
        try:
            env["IS_DIALER"] = "true"
            d = self.start(name + "-dialer", dialer, env=env)
            exit_code = self.docker("wait", d, timeout=135).stdout.strip()
            logs = self.logs(d)
            if exit_code != "0":
                raise RuntimeError(f"dialer exit={exit_code}: {logs[-4000:]}")
            values = {}
            for field in ("handshake_plus_one_rtt", "ping_rtt"):
                match = re.search(r"^ {2,4}" + field + r": ([0-9.eE+-]+)$", logs, re.M)
                if not match:
                    raise RuntimeError("missing upstream YAML latency field: " + field)
                values[field] = float(match[1])
                if not math.isfinite(values[field]) or values[field] <= 0:
                    raise RuntimeError("invalid latency: " + field)
            if not re.search(r"^ {2,4}unit: ms$", logs, re.M):
                raise RuntimeError("missing latency units")
            return values
        finally:
            if d:
                self.remove(d)
            self.remove(l)

    def probe(self, name, addr, ping, key_type=None, public=False):
        container = self.prefix + "-probe-" + name
        self.containers.append(container)
        try:
            p = self.docker("run", "--name", container, "--network", "bridge" if public else self.network,
                            "nu-interop:local", "-mode", "probe", "-addr", addr, "-ping", ping,
                            "-timeout", "20s", timeout=40, check=False)
            (self.out / (name + ".jsonl")).write_text(p.stdout)
            (self.out / (name + ".stderr")).write_text(p.stderr)
            if p.returncode:
                raise RuntimeError(p.stdout + p.stderr)
            result = json.loads(p.stdout)
            if not result["connected"] or not result["identified"]:
                raise RuntimeError("probe did not connect and identify")
            if key_type is not None and result["keyType"] != key_type:
                raise RuntimeError("wrong authenticated identity key type")
            if ping != "none" and result["pingRTTMilllis"] <= 0:
                raise RuntimeError("probe did not ping")
            return result
        finally:
            self.remove(container)

    def blackbox(self, kind):
        self.init_network()
        if kind == "kubo":
            # Isolated network plus empty bootstrap list: never joins public Amino.
            script = ('ipfs init --profile=test >/dev/null && '
                      'ipfs config --json Bootstrap "[]" && '
                      'ipfs config --json Addresses.Swarm \'["/ip4/0.0.0.0/udp/4001/quic-v1"]\' && '
                      'exec ipfs daemon --routing=none --migrate=true')
            c = self.start(kind, KUBO, args=("-ec", script), options=("--entrypoint", "/bin/sh"))
        else:
            c = self.start(kind, LIGHTHOUSE, args=("lighthouse", "bn", "--network", "mainnet", "--datadir", "/tmp/lh",
                "--allow-insecure-genesis-sync", "--execution-endpoint", "http://127.0.0.1:8551",
                "--execution-jwt-secret-key", "00" * 32, "--disable-upnp", "--disable-enr-auto-update",
                "--listen-address", "0.0.0.0"))
        try:
            ip = self.docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", c).stdout.strip()
            if kind == "kubo":
                self.wait_ready(lambda: "Daemon is ready" in self.logs(c))
                peer = self.docker("exec", c, "ipfs", "config", "Identity.PeerID").stdout.strip()
                port, ping, key = 4001, "libp2p", None
            else:
                match = self.wait_ready(lambda: re.search(r"/udp/9001/quic-v1/p2p/([A-Za-z0-9]+)", self.logs(c)))
                peer, port, ping, key = match[1], 9001, "eth2", 2
            return self.probe(kind, f"/ip4/{ip}/udp/{port}/quic-v1/p2p/{peer}", ping, key)
        finally:
            self.remove(c)

    def amino(self):
        for peer in json.loads((HERE / "amino.json").read_text()):
            def check(p=peer):
                ip = socket.gethostbyname(p["host"])
                return self.probe(p["host"], f'/ip4/{ip}/udp/{p["port"]}/quic-v1/p2p/{p["peer"]}', "none", public=True)
            self.record("amino-" + peer["host"], check)

    def cleanup(self):
        for c in self.containers[:]:
            self.remove(c)
        if self.network_created:
            self.docker("network", "rm", self.network, check=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("suite", choices=("transport", "blackbox", "amino"))
    parser.add_argument("--impl", choices=("go", "rust", "js", "all"), default="all")
    parser.add_argument("--public", action="store_true", help="explicitly permit connections to pinned public Amino peers")
    parser.add_argument("--out", type=Path, default=HERE / "results")
    args = parser.parse_args()
    if args.suite == "amino" and not args.public:
        parser.error("amino requires --public; this contacts third-party infrastructure")
    runner = Runner(args.out.resolve())
    try:
        runner.build_nu()
        if args.suite == "transport":
            for impl in (["go", "rust", "js"] if args.impl == "all" else [args.impl]):
                try:
                    runner.transport(impl)
                except Exception as e:
                    runner.results.append({"name": "setup-" + impl, "status": "fail", "error": str(e)})
        elif args.suite == "blackbox":
            for kind in ("kubo", "lighthouse"):
                runner.record(kind, lambda k=kind: runner.blackbox(k))
        else:
            runner.amino()
    except Exception as e:
        runner.results.append({"name": "setup", "status": "fail", "error": str(e)})
    finally:
        runner.save()
        runner.cleanup()
    print(json.dumps(runner.results, indent=2))
    return 0 if runner.results and all(r["status"] == "pass" for r in runner.results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
