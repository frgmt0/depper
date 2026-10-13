from __future__ import annotations

from pathlib import Path
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from deployer.config import DeployConfig, RemoteConfig, ServiceConfig, TunnelConfig
from deployer.remote import DeploymentError, Runner, _environment_file, _unit


def make_config(**overrides: object) -> DeployConfig:
    service = overrides.pop("service", ServiceConfig("api", "python3 app.py", "on-failure"))
    deploy = overrides.pop(
        "deploy",
        RemoteConfig("deploy@server", "/srv/api", (), (".git/", "private key;$.txt"), None),
    )
    env = overrides.pop("env", {"PORT": "8080"})
    tunnel = overrides.pop("tunnel", None)
    if overrides:
        raise AssertionError(f"unexpected overrides: {overrides}")
    return DeployConfig(Path("/source/repository"), service, deploy, env, tunnel)  # type: ignore[arg-type]


class RemoteTests(unittest.TestCase):
    def test_environment_file_escapes_quotes_and_backslashes(self) -> None:
        rendered = _environment_file({"B": 'say "hi" at C:\\work', "A": "plain"})
        self.assertEqual(rendered, 'A="plain"\nB="say \\"hi\\" at C:\\\\work"\n')

    def test_environment_file_preserves_actual_multiline_value_without_c_escaping(self) -> None:
        rendered = _environment_file({"MULTILINE": "first\nsecond"})

        self.assertEqual(rendered, 'MULTILINE="first\nsecond"\n')
        self.assertNotIn("first\\nsecond", rendered)

    def test_unit_uses_quoted_shell_command_and_configured_restart(self) -> None:
        config = make_config(service=ServiceConfig("api", "python3 app.py --label 'it works'", "always"))
        rendered = _unit(config, "/srv/api", "/srv/api/.deployer/environment")

        self.assertIn("WorkingDirectory=/srv/api/current", rendered)
        self.assertIn("EnvironmentFile=/srv/api/.deployer/environment", rendered)
        self.assertIn('ExecStart=/bin/sh "/srv/api/current/.deployer-start.sh"', rendered)
        self.assertIn("Restart=always", rendered)
        self.assertNotIn("Restart=on-failure", rendered)

    def test_rsync_excludes_and_remote_destination_are_discrete_argv_items(self) -> None:
        config = make_config()
        runner = Runner(config)
        runner._run = Mock()  # type: ignore[method-assign]

        runner._rsync("/srv/api/releases/release-1")

        runner._run.assert_called_once_with(
            [
                "rsync", "-az", "--exclude", ".git/", "--exclude", "private key;$.txt",
                "/source/repository/", "deploy@server:'/srv/api/releases/release-1'/",
            ]
        )

    def test_dry_run_deploy_never_executes_a_command(self) -> None:
        runner = Runner(make_config(), dry_run=True)
        runner._run = Mock(side_effect=AssertionError("dry run must not run a command"))  # type: ignore[method-assign]

        runner.deploy()

        runner._run.assert_not_called()

    def test_dry_run_with_tunnel_never_constructs_tunnel_manager_or_executes_remote_commands(self) -> None:
        runner = Runner(make_config(tunnel=TunnelConfig("api.demo.frgmt.xyz", "http://127.0.0.1:8080")), dry_run=True)
        runner._run = Mock(side_effect=AssertionError("dry run must not run a command"))  # type: ignore[method-assign]
        with patch("deployer.tunnel.TunnelManager") as tunnel_manager:
            runner.deploy()

        runner._run.assert_not_called()
        tunnel_manager.assert_not_called()

    def test_run_uses_argument_vector_and_translates_process_errors(self) -> None:
        runner = Runner(make_config())
        completed = subprocess.CompletedProcess(["ssh"], 0, "out", "")
        with patch("deployer.remote.subprocess.run", return_value=completed) as run:
            self.assertIs(runner._run(["ssh", "deploy@server", "bash -s"]), completed)
        run.assert_called_once_with(
            ["ssh", "deploy@server", "bash -s"], input=None, text=True, capture_output=False, check=True
        )
        failure = subprocess.CalledProcessError(1, ["ssh"], stderr="remote failed")
        with patch("deployer.remote.subprocess.run", side_effect=failure):
            with self.assertRaisesRegex(DeploymentError, "remote failed"):
                runner._run(["ssh"])

    def test_failed_setup_restores_previous_active_release(self) -> None:
        runner = Runner(make_config())
        runner.base_path = Mock(return_value="/srv/api")  # type: ignore[method-assign]
        runner.ssh = Mock()  # type: ignore[method-assign]
        runner._rsync = Mock()  # type: ignore[method-assign]
        runner._upload = Mock()  # type: ignore[method-assign]
        runner._setup = Mock(side_effect=DeploymentError("setup failed"))  # type: ignore[method-assign]
        runner._activate = Mock()  # type: ignore[method-assign]
        runner._restore = Mock()  # type: ignore[method-assign]
        runner._cleanup_candidates = Mock()  # type: ignore[method-assign]

        manager = SimpleNamespace(assert_section_present=Mock(), ensure=Mock())
        with patch("deployer.tunnel.TunnelManager", return_value=manager), patch("deployer.remote._rid", return_value="release-test"):
            with self.assertRaisesRegex(DeploymentError, "setup failed"):
                runner.deploy()

        runner._restore.assert_called_once_with(
            "/srv/api", "/srv/api/.deployer/environment", "$HOME/.config/systemd/user/deployer-api.service",
            "/srv/api/.deployer/transactions/release-test",
        )
        runner._activate.assert_not_called()
        runner._cleanup_candidates.assert_called_once_with("/srv/api", "release-test")

    def test_tunnel_failure_after_healthy_application_has_clear_nonzero_outcome_and_releases_lock(self) -> None:
        runner = Runner(make_config(tunnel=TunnelConfig("api.demo.frgmt.xyz", "http://127.0.0.1:8080")))
        runner.base_path = Mock(return_value="/srv/api")  # type: ignore[method-assign]
        runner._acquire_lock = Mock()  # type: ignore[method-assign]
        runner._release_lock = Mock()  # type: ignore[method-assign]
        runner.ssh = Mock()  # type: ignore[method-assign]
        runner._rsync = Mock()  # type: ignore[method-assign]
        runner._upload = Mock()  # type: ignore[method-assign]
        runner._setup = Mock()  # type: ignore[method-assign]
        runner._activate = Mock()  # type: ignore[method-assign]
        manager = SimpleNamespace(assert_section_present=Mock(), ensure=Mock(side_effect=DeploymentError("DNS route conflict")))
        with patch("deployer.tunnel.TunnelManager", return_value=manager), patch("deployer.remote._rid", return_value="release-test"):
            with self.assertRaisesRegex(DeploymentError, "Application deployed successfully, but tunnel setup failed: DNS route conflict"):
                runner.deploy()

        manager.assert_section_present.assert_not_called()
        manager.ensure.assert_called_once_with()
        runner._release_lock.assert_called_once_with("/srv/api")

    def test_setup_does_not_source_environment_values_as_shell_code(self) -> None:
        config = make_config(
            deploy=RemoteConfig("deploy@server", "/srv/api", ("python3 setup.py",), (), None),
            env={"PAYLOAD": "$(printf should-not-run)"},
        )
        runner = Runner(config)
        runner.ssh = Mock()  # type: ignore[method-assign]

        runner._setup("/srv/api/releases/release-1", "/srv/api/.deployer/environment.next")

        script = runner.ssh.call_args.args[0]
        self.assertNotIn(". /srv/api/.deployer/environment.next", script)

    def test_healthcheck_does_not_source_environment_values_as_shell_code(self) -> None:
        config = make_config(
            deploy=RemoteConfig("deploy@server", "/srv/api", (), (), "python3 healthcheck.py"),
            env={"PAYLOAD": "`printf should-not-run`"},
        )
        runner = Runner(config)
        runner.ssh = Mock()  # type: ignore[method-assign]

        runner._activate("/srv/api", "/srv/api/releases/release-1", "/srv/api/.deployer/environment", "$HOME/unit", "/srv/api/backup", "release-1")

        script = runner.ssh.call_args.args[0]
        self.assertNotIn('. "$environment"', script)

    def test_first_deploy_failure_removes_current_symlink_and_stops_service(self) -> None:
        runner = Runner(make_config())
        runner.ssh = Mock()  # type: ignore[method-assign]

        runner._restore("/srv/api", "/srv/api/.deployer/environment", "$HOME/unit", "/srv/api/backup")

        script = runner.ssh.call_args.args[0]
        self.assertIn('elif test -f "$backup/no-current"; then rm -f "$base/current"; fi', script)
        self.assertIn("systemctl --user disable --now deployer-api.service", script)

    def test_invalid_base_path_from_remote_is_rejected_before_use(self) -> None:
        runner = Runner(make_config())
        runner.ssh = Mock(return_value=subprocess.CompletedProcess([], 0, "relative/path\n", ""))  # type: ignore[method-assign]

        with self.assertRaisesRegex(DeploymentError, "invalid deployment path"):
            runner.base_path()


if __name__ == "__main__":
    unittest.main()
