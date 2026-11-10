#!/usr/bin/env python3
"""Single-host, single-writer release handoff. No credentials on the command line.

Containers must be prepared with immutable images, identical persistent mounts,
unique release/control paths and restart policy 'no' for the retired release.
Only Linux host execution is supported. See README's seamless upgrade section.
"""
import argparse
import fcntl
import http.client
import json
import os
import socket
import subprocess
import time
import urllib.parse
import urllib.request


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=10)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def call(path, action="status", **params):
    conn = UnixHTTP(path)
    try:
        route = "/" + action
        if params:
            route += "?" + urllib.parse.urlencode(params)
        conn.request("GET" if action == "status" else "POST", route)
        response = conn.getresponse()
        body = response.read(65536)
        if response.status >= 300:
            raise RuntimeError(f"control {action}: HTTP {response.status}")
        return json.loads(body) if action == "status" else None
    finally:
        conn.close()


def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True)
    if result.returncode:
        # Docker errors may contain environment/configuration details.
        raise RuntimeError(f"docker {args[0]} failed (exit {result.returncode})")
    return result.stdout.strip()


def inspect(name):
    return json.loads(docker("inspect", name))[0]


def argument(info, flag):
    args = info["Config"].get("Cmd") or []
    for i, value in enumerate(args):
        if value == flag and i + 1 < len(args):
            return args[i + 1]
        if value.startswith(flag + "="):
            return value.split("=", 1)[1]
    raise RuntimeError(f"missing container argument {flag}")


def validate_containers(args):
    old, new, check = map(inspect, (args.old_container, args.new_container, args.check_container))
    if old["State"]["Status"] != "running" or new["State"]["Status"] not in ("created", "exited"):
        raise RuntimeError("old must be running and candidate must be stopped")
    if argument(old, "-release") != args.old_release or argument(new, "-release") != args.new_release:
        raise RuntimeError("container release mismatch")
    if argument(old, "-state-lock") != argument(new, "-state-lock"):
        raise RuntimeError("state lock paths differ")
    if argument(old, "-control") == argument(new, "-control"):
        raise RuntimeError("control socket paths must differ")
    mounts = lambda x: sorted((m["Source"], m["Destination"], m["RW"]) for m in x["Mounts"])
    if mounts(old) != mounts(new):
        raise RuntimeError("persistent mounts differ; prepare equivalent mounts before release")
    if new["Image"] != check["Image"] or "-preflight" not in (check["Config"].get("Cmd") or []):
        raise RuntimeError("preflight must use candidate image with -preflight")
    if old["HostConfig"]["RestartPolicy"]["Name"] not in ("no", ""):
        raise RuntimeError("old restart policy must be no to prevent reboot reactivating it")
    if new["HostConfig"]["RestartPolicy"]["Name"] not in ("no", ""):
        raise RuntimeError("candidate restart policy must be no during handoff")
    if check["State"]["Running"]:
        raise RuntimeError("preflight container must be stopped")
    if sorted(check["Config"].get("Env") or []) != sorted(new["Config"].get("Env") or []):
        raise RuntimeError("preflight environment differs from candidate")
    # Read-only preflight may differ only in mount write permissions.
    locations = lambda x: sorted((m["Source"], m["Destination"]) for m in x["Mounts"])
    if locations(check) != locations(new):
        raise RuntimeError("preflight must inspect the candidate's mounts")
    if getattr(args, "mode", "serial") == "overlap":
        if any(argument(info, "-upgrade-mode") != "overlap" for info in (old, new)):
            raise RuntimeError("both releases must support overlap; first migrate serially")
        if argument(old, "-coordination-dir") != argument(new, "-coordination-dir"):
            raise RuntimeError("coordination directories differ")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("old-container", "new-container", "check-container", "old-release", "new-release",
                 "old-control", "new-control", "gateway-control", "new-target", "journal"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--drain-seconds", type=float, default=20)
    p.add_argument("--start-seconds", type=float, default=20)
    p.add_argument("--mode", choices=("serial", "overlap"), default="serial")
    args = p.parse_args()
    if args.old_release == args.new_release or args.drain_seconds <= 0 or args.start_seconds <= 0:
        p.error("distinct releases and positive budgets required")
    os.umask(0o077)
    with open(args.journal + ".lock", "a", encoding="utf-8") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return release(args)


def release(args):
    if getattr(args, "mode", "serial") == "overlap":
        return release_overlap(args)
    if os.path.exists(args.journal):
        raise RuntimeError("journal already exists; inspect recorded stage before recovery, use a fresh journal for a new attempt")
    phase = "prepare"

    def record(value):
        nonlocal phase
        phase = value
        with open(args.journal + ".tmp", "w", encoding="utf-8") as f:
            json.dump({"phase": value, "old": args.old_release, "new": args.new_release, "at": time.time()}, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(args.journal + ".tmp", args.journal)
        directory = os.open(os.path.dirname(args.journal) or ".", os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        print(value, flush=True)

    def wait_for(predicate, timeout):
        end = time.monotonic() + timeout
        while True:
            if predicate():
                return
            if time.monotonic() >= end:
                raise TimeoutError("release stage exceeded budget")
            time.sleep(0.1)

    validate_containers(args)
    gateway = call(args.gateway_control)
    if gateway["release"] != args.old_release or gateway["held"]:
        raise RuntimeError("gateway release/phase mismatch")
    if gateway["wait_seconds"] < args.drain_seconds + args.start_seconds + 15:
        raise RuntimeError("gateway wait must cover drain + startup + 15 seconds commit/exit margin; check client/proxy deadlines too")
    target = urllib.parse.urlsplit(args.new_target)
    if target.scheme != "http" or target.hostname not in ("127.0.0.1", "::1") or not target.port or target.path or target.query or target.fragment or target.username:
        raise RuntimeError("new target must be an explicit loopback HTTP origin")
    old_status = call(args.old_control)
    if old_status["release"] != args.old_release:
        raise RuntimeError("old control identity mismatch")
    if old_status.get("paused") or old_status.get("background_paused") or not old_status["ready"]:
        raise RuntimeError("old release must be serving before starting a new handoff")
    record("preflight")
    docker("start", args.check_container)
    wait_for(lambda: not inspect(args.check_container)["State"]["Running"], args.start_seconds)
    if inspect(args.check_container)["State"]["ExitCode"] != 0:
        raise RuntimeError("candidate preflight failed; old release unchanged")

    old_stopped = False
    try:
        record("hold")
        call(args.gateway_control, "hold", expected=args.old_release)
        drain_end = time.monotonic() + args.drain_seconds
        def drain_wait(predicate):
            wait_for(predicate, max(0, drain_end - time.monotonic()))
        # Allow already admitted requests (including late backend dial) to enter
        # A before closing its direct admission barrier.
        drain_wait(lambda: call(args.gateway_control)["active"] == 0)
        call(args.old_control, "pause", release=args.old_release)
        drain_wait(lambda: call(args.old_control)["active"] == 0)
        call(args.old_control, "quiesce", release=args.old_release)
        record("drain")
        drain_wait(lambda: call(args.old_control)["background"] == 0)
        record("finalize")
        call(args.old_control, "stop", release=args.old_release)
        # The stop acknowledgment commits us to waiting for actual exit.
        old_stopped = True
        record("old_stopping")
        wait_for(lambda: not inspect(args.old_container)["State"]["Running"], args.start_seconds)
        if inspect(args.old_container)["State"]["ExitCode"] != 0:
            raise RuntimeError("old release did not exit cleanly")
        record("activate")
        docker("start", args.new_container)

        def ready():
            if not inspect(args.new_container)["State"]["Running"]:
                raise RuntimeError("candidate exited; inspect protected logs")
            try:
                state = call(args.new_control)
                if state["release"] != args.new_release or not state["ready"]:
                    return False
                # Verify the actual target too, not just its separate control socket.
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                with opener.open(args.new_target + "/readyz", timeout=2) as response:
                    return json.load(response)["release"] == args.new_release
            except (OSError, http.client.HTTPException):
                return False

        wait_for(ready, args.start_seconds)
        record("switch")
        call(args.gateway_control, "switch", expected=args.old_release, next=args.new_release, target=args.new_target)
        call(args.gateway_control, "resume", expected=args.new_release)
        record("serving")
    except BaseException:
        if not old_stopped and phase != "finalize":
            # No external work is replayed. A still owns its original state.
            call(args.old_control, "resume", release=args.old_release)
            call(args.gateway_control, "resume", expected=args.old_release)
            record("aborted_old_serving")
        else:
            # A failed/lost stop reply is ambiguous: never restart either writer
            # or restore a database snapshot automatically.
            record("recovery_required")
        raise


def release_overlap(args):
    """Switch relay traffic immediately; hold only the final management transfer."""
    if os.path.exists(args.journal):
        raise RuntimeError("journal already exists; inspect recorded stage before recovery")
    phase = "prepare"

    def record(value):
        nonlocal phase
        phase = value
        with open(args.journal + ".tmp", "w", encoding="utf-8") as f:
            json.dump({"phase": value, "mode": "overlap", "old": args.old_release,
                       "new": args.new_release, "at": time.time()}, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(args.journal + ".tmp", args.journal)
        directory = os.open(os.path.dirname(args.journal) or ".", os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        print(value, flush=True)

    def wait_for(predicate, seconds):
        deadline = time.monotonic() + seconds
        while True:
            if predicate():
                return
            if time.monotonic() >= deadline:
                raise TimeoutError("release stage exceeded budget")
            time.sleep(0.1)

    def protocol(state):
        return state.get("mode") == "overlap" and state.get("overlap_protocol") == 1

    def backend_ready():
        if not inspect(args.new_container)["State"]["Running"]:
            raise RuntimeError("candidate exited; inspect protected logs")
        try:
            status = call(args.new_control)
        except (OSError, http.client.HTTPException):
            return False
        if status.get("release") != args.new_release or not status.get("ready"):
            return False
        if not protocol(status) or status.get("coordination_domain") != domain or status.get("background_owner"):
            raise RuntimeError("candidate overlap coordination or standby mismatch")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(args.new_target + "/readyz", timeout=2) as response:
                return json.load(response).get("release") == args.new_release
        except (OSError, http.client.HTTPException):
            return False

    def release_counts(release):
        status = call(args.gateway_control)
        if status.get("recovery_required"):
            raise RuntimeError("gateway recovery required")
        return status.get("releases", {}).get(release, {})

    validate_containers(args)
    gateway, old = call(args.gateway_control), call(args.old_control)
    if (not protocol(gateway) or gateway.get("release") != args.old_release
            or gateway.get("management_release") != args.old_release or gateway.get("held")
            or gateway.get("management_held") or gateway.get("recovery_required")
            or gateway.get("transition") or set(gateway.get("releases", {})) != {args.old_release}):
        raise RuntimeError("gateway not in a stable overlap state")
    if not protocol(old) or old.get("release") != args.old_release or not old.get("ready") or not old.get("background_owner") or old.get("paused") or old.get("background_paused"):
        raise RuntimeError("old release must be the serving overlap owner")
    domain = old.get("coordination_domain")
    if not isinstance(domain, str) or not domain:
        raise RuntimeError("missing coordination domain")
    if gateway["wait_seconds"] < args.drain_seconds + 2 * args.start_seconds + 15:
        raise RuntimeError("management queue budget must cover final drain, stop and promotion")
    target = urllib.parse.urlsplit(args.new_target)
    if target.scheme != "http" or target.hostname not in ("127.0.0.1", "::1") or not target.port or target.path or target.query or target.fragment or target.username is not None:
        raise RuntimeError("new target must be an explicit loopback HTTP origin")
    record("preflight")
    docker("start", args.check_container)
    wait_for(lambda: not inspect(args.check_container)["State"]["Running"], args.start_seconds)
    if inspect(args.check_container)["State"]["ExitCode"] != 0:
        raise RuntimeError("candidate preflight failed; old release unchanged")
    started = switched = stop_committed = False
    try:
        record("standby")
        started = True
        docker("start", args.new_container)
        wait_for(backend_ready, args.start_seconds)
        record("pause_background")
        call(args.old_control, "pause-background", release=args.old_release)
        record("switch_live")
        call(args.gateway_control, "switch-live", expected=args.old_release,
             next=args.new_release, target=args.new_target)
        switched = True
        record("drain_old_relay")
        wait_for(lambda: release_counts(args.old_release).get("relay_active") == 0, args.drain_seconds)
        wait_for(lambda: call(args.old_control).get("background") == 0, args.drain_seconds)
        record("hold_management")
        call(args.gateway_control, "hold-management", expected=args.old_release)
        wait_for(lambda: release_counts(args.old_release).get("management_active") == 0, args.drain_seconds)
        wait_for(lambda: call(args.old_control).get("active") == 0, args.drain_seconds)
        call(args.old_control, "pause", release=args.old_release)
        call(args.old_control, "quiesce", release=args.old_release)
        record("finalize")
        # An error or lost reply from stop has an ambiguous commit outcome.
        stop_committed = True
        call(args.old_control, "stop", release=args.old_release)
        record("old_stopping")
        wait_for(lambda: not inspect(args.old_container)["State"]["Running"], args.start_seconds)
        if inspect(args.old_container)["State"]["ExitCode"] != 0:
            raise RuntimeError("old release did not exit cleanly")
        record("promote")
        call(args.new_control, "activate-management", release=args.new_release)
        call(args.new_control, "activate-background", release=args.new_release)
        status = call(args.new_control)
        if not protocol(status) or status.get("coordination_domain") != domain or not status.get("background_owner") or not status.get("ready"):
            raise RuntimeError("candidate owner promotion incomplete")
        call(args.gateway_control, "switch-management", expected=args.old_release, next=args.new_release)
        call(args.gateway_control, "resume-management", expected=args.new_release)
        call(args.gateway_control, "retire", expected=args.new_release, release=args.old_release)
        call(args.gateway_control, "complete", expected=args.new_release)
        record("serving")
    except BaseException:
        if stop_committed or phase == "switch_live":
            record("recovery_required")
            raise
        try:
            current = call(args.gateway_control)
            if current.get("recovery_required"):
                raise RuntimeError("gateway recovery required")
            if switched:
                call(args.old_control, "resume", release=args.old_release)
                call(args.gateway_control, "rollback-live", expected=args.new_release, next=args.old_release)
                call(args.gateway_control, "resume-management", expected=args.old_release)
                wait_for(lambda: release_counts(args.new_release).get("relay_active") == 0, args.drain_seconds)
            elif current.get("release") != args.old_release:
                raise RuntimeError("gateway switch outcome unknown")
            if started and inspect(args.new_container)["State"]["Running"]:
                call(args.new_control, "pause", release=args.new_release)
                wait_for(lambda: call(args.new_control).get("active") == 0, args.drain_seconds)
                call(args.new_control, "quiesce", release=args.new_release)
                wait_for(lambda: call(args.new_control).get("background") == 0, args.drain_seconds)
                call(args.new_control, "stop", release=args.new_release)
                wait_for(lambda: not inspect(args.new_container)["State"]["Running"], args.start_seconds)
            call(args.old_control, "resume", release=args.old_release)
            if switched:
                call(args.gateway_control, "retire", expected=args.old_release, release=args.new_release)
                call(args.gateway_control, "complete", expected=args.old_release)
            record("aborted_old_serving")
        except BaseException:
            record("recovery_required")
        raise


if __name__ == "__main__":
    main()
