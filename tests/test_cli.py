from __future__ import annotations

from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from deployer.cli import main
from deployer.config import DeployConfig, RemoteConfig, ServiceConfig


def config_for(repo: Path) -> DeployConfig:
    return DeployConfig(repo, ServiceConfig("api", "python app.py", "on-failure"), RemoteConfig("server", None, (), (), None), {})


class CliTests(unittest.TestCase):
    def test_documented_repo_then_action_form_deploys_in_dry_run_mode(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = config_for(root)
            runner = Mock()
            with patch("deployer.cli.load_config", return_value=config) as load, patch("deployer.cli.Runner", return_value=runner) as runner_type:
                result = main([str(root), "deploy", "--dry-run"])

        self.assertEqual(result, 0)
        load.assert_called_once_with(root)
        runner_type.assert_called_once_with(config, True)
        runner.deploy.assert_called_once_with()

    def test_logs_constrains_line_count_and_passes_it_as_an_argument(self) -> None:
        root = Path("/repository")
        config = config_for(root)
        runner = Mock()
        with patch("deployer.cli.load_config", return_value=config), patch("deployer.cli.Runner", return_value=runner):
            result = main(["logs", str(root), "--lines", "25"])

        self.assertEqual(result, 0)
        runner.logs.assert_called_once_with(("-n", "25"))

    def test_deploy_forwards_env_file_as_a_path_only_to_deploy(self) -> None:
        root = Path("/repository")
        config = config_for(root)
        runner = Mock()
        with patch("deployer.cli.load_config", return_value=config), patch("deployer.cli.Runner", return_value=runner):
            result = main(["deploy", str(root), "--env", "synthetic.env"])

        self.assertEqual(result, 0)
        runner.deploy.assert_called_once_with(Path("synthetic.env"))

    def test_non_deploy_commands_do_not_receive_or_read_an_env_file(self) -> None:
        root = Path("/repository")
        config = config_for(root)
        runner = Mock()
        with patch("deployer.cli.load_config", return_value=config), patch("deployer.cli.Runner", return_value=runner):
            result = main(["status", str(root)])

        self.assertEqual(result, 0)
        runner.status.assert_called_once_with()
        runner.deploy.assert_not_called()


if __name__ == "__main__":
    unittest.main()
