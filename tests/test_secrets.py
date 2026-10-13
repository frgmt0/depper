from __future__ import annotations

from contextlib import redirect_stdout
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

from deployer.config import DeployConfig, RemoteConfig, SecretsConfig, ServiceConfig
from deployer.remote import DeploymentError, Runner


SENTINEL = "synthetic-secret-not-for-output"


def make_config(repo: Path, secrets_file: Path | None = None, *, setup: tuple[str, ...] = (), healthcheck: str | None = None) -> DeployConfig:
    return DeployConfig(
        repo,
        ServiceConfig("api", "python app.py", "on-failure"),
        RemoteConfig("desktop", "/srv/api", setup, (), healthcheck),
        {"PORT": "8000", "SAFE_FOR_SETUP": "yes"},
        secrets=SecretsConfig(secrets_file) if secrets_file else None,
    )


class SecretsDeploymentTests(unittest.TestCase):
    def test_dotenv_overrides_toml_env_and_keeps_configured_custom_file_excluded_when_cli_override_is_external(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = (root / "repo").resolve()
            repo.mkdir()
            configured = repo / "credentials.prod"  # deliberately absent
            external = root / "override.env"
            external.write_text(f"PORT=9000\nTOKEN={SENTINEL}\n", encoding="utf-8")
            runner = Runner(make_config(repo, configured))

            values, source, excludes, count = runner._deployment_environment(external)

        self.assertEqual(values, {"PORT": "9000", "SAFE_FOR_SETUP": "yes", "TOKEN": SENTINEL})
        self.assertEqual(count, 2)
        self.assertIsNotNone(source)
        self.assertEqual(excludes, ("/credentials.prod",))

    def test_cli_override_is_resolved_from_cwd_while_toml_relative_file_is_resolved_from_repo(self) -> None:
        previous_cwd = Path.cwd()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            repo = root / "repo"
            cwd = root / "cwd"
            repo.mkdir()
            cwd.mkdir()
            (repo / "configured.env").write_text("SOURCE=repo-config\n", encoding="utf-8")
            (repo / "override.env").write_text("SOURCE=repo-override\n", encoding="utf-8")
            (cwd / "override.env").write_text("SOURCE=cwd-override\n", encoding="utf-8")
            runner = Runner(make_config(repo, repo / "configured.env"))
            try:
                os.chdir(cwd)
                values, source, _, _ = runner._deployment_environment(Path("override.env"))
            finally:
                os.chdir(previous_cwd)

        self.assertEqual(values["SOURCE"], "cwd-override")
        assert source is not None
        self.assertEqual(source.selected, cwd / "override.env")

    def test_malformed_or_missing_dotenv_stops_before_any_remote_operation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory).resolve()
            runner = Runner(make_config(repo, repo / "missing.env"))
            runner._run = Mock(side_effect=AssertionError("remote command must not run"))  # type: ignore[method-assign]

            with self.assertRaisesRegex(DeploymentError, "not readable") as caught:
                runner.deploy()

        runner._run.assert_not_called()
        self.assertNotIn("missing.env", str(caught.exception))

    def test_dry_run_reads_synthetic_dotenv_but_never_runs_remote_and_never_prints_its_values(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory).resolve()
            selected = repo / "private.env"
            selected.write_text(f"TOKEN={SENTINEL}\n", encoding="utf-8")
            runner = Runner(make_config(repo), dry_run=True)
            runner._run = Mock(side_effect=AssertionError("dry run must not call remote"))  # type: ignore[method-assign]
            output = io.StringIO()
            with redirect_stdout(output):
                runner.deploy(selected)

        runner._run.assert_not_called()
        self.assertNotIn(SENTINEL, output.getvalue())

    def test_runtime_secrets_are_not_exported_into_setup_or_healthcheck_shell_scripts(self) -> None:
        config = make_config(Path("/repository"), setup=("python setup.py",), healthcheck="python health.py")
        runner = Runner(config)
        runner.ssh = Mock()  # type: ignore[method-assign]

        runner._setup("/srv/api/releases/r1", "/srv/api/.deployer/environment.next")
        setup_script = runner.ssh.call_args.args[0]
        runner._activate("/srv/api", "/srv/api/releases/r1", "/srv/api/.deployer/environment", "$HOME/unit", "/srv/api/backup", "r1")
        activate_script = runner.ssh.call_args.args[0]

        self.assertIn("SAFE_FOR_SETUP", setup_script)
        self.assertIn("SAFE_FOR_SETUP", activate_script)
        self.assertNotIn(SENTINEL, setup_script)
        self.assertNotIn(SENTINEL, activate_script)

    def test_private_atomic_upload_script_never_contains_the_raw_value(self) -> None:
        runner = Runner(make_config(Path("/repository")))
        runner.ssh = Mock()  # type: ignore[method-assign]

        runner._upload(f"TOKEN={SENTINEL}\n", "/srv/api/.deployer/environment.next-r1")

        script = runner.ssh.call_args.args[0]
        self.assertIn("umask 077", script)
        self.assertIn("test ! -L", script)
        self.assertIn("mktemp", script)
        self.assertIn("trap", script)
        self.assertIn('mv -f "$temporary" "$destination"', script)
        self.assertNotIn(SENTINEL, script)


if __name__ == "__main__":
    unittest.main()
