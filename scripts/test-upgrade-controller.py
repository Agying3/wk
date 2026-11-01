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


if __name__ == "__main__":
    unittest.main()
