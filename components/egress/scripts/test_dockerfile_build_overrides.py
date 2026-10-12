# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Check image pins and build snippets without installing packages or Docker."""

import ast
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

DOCKERFILE = Path(__file__).resolve().parents[1] / "Dockerfile"
RUNS = [line[4:] for line in DOCKERFILE.read_text().replace("\\\n", "").splitlines()
        if line.startswith("RUN ")]
GOPROXY_RUN = next(run for run in RUNS if "go env -w GOPROXY=" in run)
APT_RUN = next(run for run in RUNS if "APT_MIRROR" in run).split("&& apt-get update", 1)[0]
H2_CHECK_RUN = next(run for run in RUNS if "dependency_versions_supported" in run)
H2_CHECK_CODE = shlex.split(H2_CHECK_RUN)[-1]


class DockerfileH2DependencyTests(unittest.TestCase):
    def test_installed_pins_match_adapter_gate(self):
        adapter = ast.parse((DOCKERFILE.parent / "mitmscripts" / "credential_bound_h2.py").read_text())
        pins = next(ast.literal_eval(node.value) for node in adapter.body
                    if isinstance(node, ast.Assign)
                    and any(isinstance(target, ast.Name) and target.id == "PINS"
                            for target in node.targets))
        install = next(run for run in RUNS if "pip3 install" in run)
        requirements = [token for token in shlex.split(install) if "==" in token]
        self.assertEqual(sorted(requirements), sorted(f"{name}=={version}" for name, version in pins.items()))

    def test_build_checks_copied_adapter_with_its_runtime_gate(self):
        dockerfile = DOCKERFILE.read_text()
        self.assertLess(dockerfile.index("COPY components/egress/mitmscripts"),
                        dockerfile.index("RUN " + H2_CHECK_RUN.split("    &&", 1)[0].rstrip()))
        words = shlex.split(H2_CHECK_RUN)
        self.assertEqual(words[-4:-1], ["PYTHONPATH=/var/egress/mitmscripts", "python3", "-c"])
        for supported in (True, False):
            with self.subTest(supported=supported):
                check = Mock(return_value=supported)
                module = SimpleNamespace(dependency_versions_supported=check)
                with (
                    patch.dict(sys.modules, {"credential_bound_h2": module}),
                    self.assertRaises(SystemExit) as result,
                ):
                    exec(H2_CHECK_CODE, {})  # noqa: S102 - exercise the checked-in Dockerfile's Python check.
                check.assert_called_once_with()
                self.assertEqual(result.exception.code,
                                 0 if supported else "unsupported H2 dependency versions")

    def test_missing_adapter_fails_build_check(self):
        with (
            patch.dict(sys.modules, {"credential_bound_h2": None}),
            self.assertRaises(ModuleNotFoundError),
        ):
            exec(H2_CHECK_CODE, {})  # noqa: S102 - exercise the checked-in Dockerfile's Python check.


class DockerfileBuildOverrideTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.apt = self.root / "apt"
        (self.apt / "sources.list.d").mkdir(parents=True)
        self.events = self.root / "events"
        self.env = dict(os.environ, PATH=str(self.bin), EVENTS=str(self.events),
                        GOPROXY="", APT_MIRROR="", ENV_STATUS="0", SED_STATUS="0",
                        REAL_SED=shutil.which("sed"))
        self.command("go", 'echo "go $*" >> "$EVENTS"\n'
                     'if [ "$1" = env ]; then exit "$ENV_STATUS"; fi\n')
        self.command("sed", 'echo "sed" >> "$EVENTS"\n'
                     'if [ "$SED_STATUS" -ne 0 ]; then exit "$SED_STATUS"; fi\n'
                     'exec "$REAL_SED" "$@"\n')
        self.command("next-step", 'echo next-step >> "$EVENTS"\n')

    def command(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/sh\n" + body)
        path.chmod(0o755)

    def run_snippet(self, snippet):
        return subprocess.run(["/bin/sh", "-c", snippet], env=self.env,
                              text=True, capture_output=True, check=False)

    def calls(self):
        return self.events.read_text().splitlines() if self.events.exists() else []

    def run_apt(self):
        # Do not touch host APT files, run apt-get, or execute package cleanup.
        return self.run_snippet(APT_RUN.replace("/etc/apt", str(self.apt)) + " && next-step")

    def test_goproxy_empty_keeps_default_and_downloads(self):
        result = self.run_snippet(GOPROXY_RUN)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls(), ["go mod download"])

    def test_goproxy_override_is_applied_before_download(self):
        self.env["GOPROXY"] = "https://proxy.example.invalid"
        result = self.run_snippet(GOPROXY_RUN)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls(), ["go env -w GOPROXY=https://proxy.example.invalid",
                                        "go mod download"])

    def test_goproxy_env_failure_stops_before_download(self):
        self.env.update(GOPROXY="invalid", ENV_STATUS="23")
        result = self.run_snippet(GOPROXY_RUN)
        self.assertEqual(result.returncode, 23)
        self.assertEqual(self.calls(), ["go env -w GOPROXY=invalid"])

    def test_apt_empty_override_keeps_sources_untouched(self):
        source = self.apt / "sources.list"
        source.write_text("deb http://deb.debian.org/debian bookworm main\n")
        result = self.run_apt()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("deb.debian.org", source.read_text())
        self.assertEqual(self.calls(), ["next-step"])

    def test_apt_override_accepts_either_supported_layout(self):
        for layout in ["sources.list", "sources.list.d/debian.sources"]:
            with self.subTest(layout=layout):
                source = self.apt / layout
                source.write_text("http://deb.debian.org/debian-security\n"
                                  "http://security.debian.org/debian-security\n")
                self.env["APT_MIRROR"] = "mirror.example.invalid"
                result = self.run_apt()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(source.read_text(), "http://mirror.example.invalid/debian-security\n" * 2)
                source.unlink()
                self.assertEqual(self.calls(), ["sed", "next-step"])
                self.events.unlink()

    def test_apt_override_without_supported_files_fails(self):
        self.env["APT_MIRROR"] = "mirror.example.invalid"
        result = self.run_apt()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("requires a supported APT source file", result.stderr)
        self.assertEqual(self.calls(), [])

    def test_apt_sed_failure_stops_next_step(self):
        (self.apt / "sources.list.d/debian.sources").write_text("deb.debian.org\n")
        self.env.update(APT_MIRROR="mirror.example.invalid", SED_STATUS="24")
        result = self.run_apt()
        self.assertEqual(result.returncode, 24)
        self.assertEqual(self.calls(), ["sed"])


if __name__ == "__main__":
    unittest.main()
