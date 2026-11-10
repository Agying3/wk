#!/usr/bin/env python3
"""Controller failure-injection tests; no Docker daemon or provider access."""
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("upgrade", Path(__file__).with_name("seamless-upgrade.py"))
upgrade = importlib.util.module_from_spec(spec)
spec.loader.exec_module(upgrade)


class ControllerTest(unittest.TestCase):
    def validate_env(self, candidate_env, preflight_env):
        containers = {}
        for name, release in [("old", "a"), ("new", "b"), ("check", "b")]:
            containers[name] = {
                "State": {"Status": "running" if name == "old" else "created", "Running": name == "old"},
                "Image": "previous" if name == "old" else "candidate",
                "Config": {"Cmd": ["-preflight"] if name == "check" else ["-release", release, "-state-lock", "/writer", "-control", name + ".sock"],
                           "Env": preflight_env if name == "check" else candidate_env},
                "HostConfig": {"RestartPolicy": {"Name": "no"}},
                "Mounts": [{"Source": "/data", "Destination": "/app/data", "RW": name != "check"}],
            }
        args = SimpleNamespace(old_container="old", new_container="new", check_container="check", old_release="a", new_release="b")
        with patch.object(upgrade, "inspect", side_effect=lambda name: containers[name]):
            upgrade.validate_containers(args)

    def test_preflight_accepts_environment_order_only_changes(self):
        env = ["WB2A_LISTEN=127.0.0.1:7864", "TZ=Asia/Shanghai", "PATH=/usr/bin"]
        self.validate_env(env, list(reversed(env)))

    def test_preflight_rejects_changed_missing_extra_and_duplicate_environment(self):
        env = ["WB2A_LISTEN=127.0.0.1:7864", "TZ=Asia/Shanghai"]
        for other in [["WB2A_LISTEN=127.0.0.1:7865", "TZ=Asia/Shanghai"], env[:-1], env + ["EXTRA=1"], env + [env[0]]]:
            with self.subTest(environment=other), self.assertRaisesRegex(RuntimeError, "preflight environment differs"):
                self.validate_env(env, other)

    def simulate(self, failure=None):
        with tempfile.TemporaryDirectory() as tmp:
            journal = str(Path(tmp) / "release.json")
            argv = ["upgrade", "--old-container", "old", "--new-container", "new", "--check-container", "check",
                    "--old-release", "a", "--new-release", "b", "--old-control", "old.sock", "--new-control", "new.sock",
                    "--gateway-control", "gateway.sock", "--new-target", "http://127.0.0.1:7865", "--journal", journal,
                    "--drain-seconds", "0.01"]
            states = {"old": True, "new": False, "check": False}
            events = []

            def inspect(name):
                return {"State": {"Running": states[name], "ExitCode": 0}}

            def docker(action, name):
                if action == "start":
                    states[name] = name != "check"
                return ""

            def call(path, action="status", **params):
                events.append((path, action))
                if path == "old.sock" and action == "stop":
                    states["old"] = False
                    if failure == "lost_stop":
                        raise OSError("injected lost acknowledgment")
                if action == "status":
                    if path == "gateway.sock":
                        return {"release": "a", "held": False, "active": 0, "wait_seconds": 90}
                    return {"release": "a" if path == "old.sock" else "b", "active": 0,
                            "background": 1 if failure == "busy" else 0, "ready": True}
                if action == "switch" and failure == "switch":
                    raise OSError("injected switch failure")

            class Opener:
                def open(self, *a, **kw):
                    return io.BytesIO(b'{"release":"b"}')

            with patch.object(sys, "argv", argv), patch.object(upgrade, "validate_containers"), \
                 patch.object(upgrade, "inspect", inspect), patch.object(upgrade, "docker", docker), \
                 patch.object(upgrade, "call", call), patch.object(upgrade.urllib.request, "build_opener", return_value=Opener()):
                if failure:
                    with self.assertRaises((TimeoutError, OSError)):
                        upgrade.main()
                else:
                    upgrade.main()
            return json.loads(Path(journal).read_text())["phase"], events, states

    def test_success(self):
        phase, events, states = self.simulate()
        self.assertEqual(phase, "serving")
        self.assertFalse(states["old"])
        self.assertTrue(states["new"])
        self.assertLess(events.index(("old.sock", "stop")), events.index(("gateway.sock", "switch")))

    def test_busy_drain_restores_old(self):
        phase, events, states = self.simulate("busy")
        self.assertEqual(phase, "aborted_old_serving")
        self.assertIn(("old.sock", "resume"), events)
        self.assertIn(("gateway.sock", "resume"), events)
        self.assertTrue(states["old"])
        self.assertFalse(states["new"])

    def test_ambiguous_stop_never_restarts_writer(self):
        phase, events, states = self.simulate("lost_stop")
        self.assertEqual(phase, "recovery_required")
        self.assertNotIn(("old.sock", "resume"), events)
        self.assertFalse(states["new"])

    def test_switch_failure_does_not_restore_old_database(self):
        phase, events, states = self.simulate("switch")
        self.assertEqual(phase, "recovery_required")
        self.assertTrue(states["new"])
        self.assertFalse(states["old"])
        self.assertNotIn(("old.sock", "resume"), events)

    def simulate_overlap(self, failure=None):
        with tempfile.TemporaryDirectory() as tmp:
            args = SimpleNamespace(old_container="old", new_container="new", check_container="check",
                old_release="a", new_release="b", old_control="old.sock", new_control="new.sock",
                gateway_control="gateway.sock", new_target="http://127.0.0.1:7865",
                journal=str(Path(tmp) / "overlap.json"), mode="overlap", drain_seconds=0.01, start_seconds=0.02)
            running = {"old": True, "new": False, "check": False}
            gateway = {"release": "a", "management_release": "a", "held": False, "management_held": False,
                       "recovery_required": False, "wait_seconds": 90, "mode": "overlap", "overlap_protocol": 1}
            releases = {"a": {"relay_active": 1 if failure == "busy" else 0, "management_active": 0}}
            owners = {"a": True, "b": False}
            events = []

            def inspect(name):
                return {"State": {"Running": running[name], "ExitCode": 0}}

            def docker(action, name):
                events.append((name, action))
                if action == "start": running[name] = name != "check"
                return ""

            def call(path, action="status", **params):
                events.append((path, action))
                if path == "gateway.sock":
                    if action == "status":
                        state = dict(gateway)
                        state["releases"] = dict(releases)
                        return state
                    if action == "switch-live":
                        gateway["release"] = "b"
                        releases["b"] = {"relay_active": 0, "management_active": 0}
                        if failure == "lost_switch": raise OSError("injected lost live-switch reply")
                    elif action == "rollback-live": gateway["release"] = "a"
                    elif action == "hold-management": gateway["management_held"] = True
                    elif action == "switch-management": gateway["management_release"] = params["next"]
                    elif action == "resume-management": gateway["management_held"] = False
                    elif action == "retire": del releases[params["release"]]
                    return None
                release = "a" if path == "old.sock" else "b"
                if action == "status":
                    return {"release": release, "ready": True, "mode": "overlap", "overlap_protocol": 1,
                            "coordination_domain": "different" if failure == "domain" and release == "b" else "shared",
                            "background_owner": owners[release], "background_paused": False,
                            "paused": False, "active": 0, "background": 0}
                if action == "stop":
                    running["old" if release == "a" else "new"] = False
                    owners[release] = False
                    if failure == "lost_stop" and release == "a": raise OSError("injected lost stop reply")
                elif action == "activate-background": owners[release] = True
                return None

            class Opener:
                def open(self, *a, **kw): return io.BytesIO(b'{"release":"b"}')

            with patch.object(upgrade, "validate_containers"), patch.object(upgrade, "inspect", inspect), \
                 patch.object(upgrade, "docker", docker), patch.object(upgrade, "call", call), \
                 patch.object(upgrade.urllib.request, "build_opener", return_value=Opener()):
                if failure:
                    with self.assertRaises((TimeoutError, OSError, RuntimeError)): upgrade.release(args)
                else: upgrade.release(args)
            return json.loads(Path(args.journal).read_text())["phase"], events, running, gateway

    def test_overlap_switches_before_old_stop_without_global_hold(self):
        phase, events, running, gateway = self.simulate_overlap()
        self.assertEqual(phase, "serving")
        self.assertTrue(running["new"])
        self.assertFalse(running["old"])
        self.assertEqual(gateway["management_release"], "b")
        self.assertLess(events.index(("gateway.sock", "switch-live")), events.index(("old.sock", "stop")))
        self.assertLess(events.index(("old.sock", "stop")), events.index(("new.sock", "activate-background")))
        self.assertNotIn(("gateway.sock", "hold"), events)

    def test_overlap_busy_drain_rolls_back_before_stop(self):
        phase, events, running, gateway = self.simulate_overlap("busy")
        self.assertEqual(phase, "aborted_old_serving")
        self.assertTrue(running["old"])
        self.assertFalse(running["new"])
        self.assertEqual(gateway["release"], "a")
        self.assertIn(("gateway.sock", "rollback-live"), events)
        self.assertNotIn(("old.sock", "stop"), events)

    def test_overlap_ambiguous_switch_or_stop_requires_recovery(self):
        for failure in ["lost_switch", "lost_stop"]:
            with self.subTest(failure=failure):
                phase, events, _, _ = self.simulate_overlap(failure)
                self.assertEqual(phase, "recovery_required")
                self.assertNotIn(("gateway.sock", "rollback-live"), events)
                self.assertNotIn(("old.sock", "resume"), events)

    def test_overlap_candidate_domain_mismatch_never_switches(self):
        phase, events, running, gateway = self.simulate_overlap("domain")
        self.assertEqual(phase, "aborted_old_serving")
        self.assertTrue(running["old"])
        self.assertFalse(running["new"])
        self.assertEqual(gateway["release"], "a")
        self.assertNotIn(("gateway.sock", "switch-live"), events)


if __name__ == "__main__":
    unittest.main()
