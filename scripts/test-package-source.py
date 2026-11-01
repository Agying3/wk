#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
import importlib.util
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("package_source", Path(__file__).with_name("package-source.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class SourceInventoryTests(unittest.TestCase):
    def test_runtime_data_and_path_escape_are_excluded(self):
        for name in (".env", "config.json", ".build-test.json", "auths/a.json", "data/state.db",
                     "backups/a.json", "internal/../../config.json", "/etc/passwd",
                     "react/node_modules/x/index.js", "internal/testdata/secret.pem", "tmp/source.go"):
            self.assertFalse(module.allowed(name), name)

    def test_build_inputs_and_test_fixtures_are_preserved(self):
        for name in ("LICENSE", "NOTICE", "LICENSES/dependencies.txt", ".env.example",
                     "config.example.json", "internal/auth/auth.go", "internal/auth/testdata/example.json",
                     "react/package-lock.json", "react/src/main.jsx", "scripts/build-release.sh"):
            self.assertTrue(module.allowed(name), name)

    def test_archive_retains_actual_changes_but_rejects_runtime_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "src"
            root.mkdir()
            required = ["LICENSE", "NOTICE", "go.mod", "cmd/server/main.go", "scripts/build-release.sh"]
            for name in required:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("current uncommitted source")
            (root / "react").mkdir()
            (root / "react/package-lock.json").write_text(json.dumps({"packages": {}}))
            (root / "config.json").write_text("private runtime fixture")
            manifest = root / "SOURCE_FILES.txt"
            manifest.write_text("\n".join(required + ["react/package-lock.json", "config.json"]))
            vendor = Path(temporary) / "vendor"
            vendor.mkdir()
            (vendor / "modules.txt").write_text("# vendored fixture")
            output = Path(temporary) / "source.tar.gz"
            module.package(root.resolve(), output, vendor)
            with tarfile.open(output) as archive:
                names = archive.getnames()
                self.assertNotIn("config.json", names)
                self.assertIn("vendor/modules.txt", names)
                self.assertEqual(archive.extractfile("cmd/server/main.go").read(), b"current uncommitted source")
                self.assertIn("cmd/server/main.go", json.load(archive.extractfile("SOURCE_MANIFEST.json"))["sha256"])


if __name__ == "__main__":
    unittest.main()
