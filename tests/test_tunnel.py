from __future__ import annotations

import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import Mock

from deployer.config import DeployConfig, RemoteConfig, ServiceConfig, TunnelConfig
from deployer.remote import DeploymentError
from deployer.tunnel import TunnelManager, TunnelState


TUNNEL_ID = "d1b797cb-149e-4f9a-a22e-d9da4c7cf67b"


def config_for(service_name: str = "api") -> DeployConfig:
    return DeployConfig(
        Path("/repository"),
        ServiceConfig(service_name, "python app.py", "on-failure"),
        RemoteConfig("desktop", "/srv/api", (), (), None),
        {},
        TunnelConfig("api.demo.frgmt.xyz", "http://127.0.0.1:8080"),
    )


def completed(stdout: str = "") -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess([], 0, stdout, "")


class TunnelManagerTests(unittest.TestCase):
    def make_manager(self, *, service_name: str = "api") -> tuple[TunnelManager, Mock]:
        runner = Mock()
        runner.config = config_for(service_name)
        return TunnelManager(runner, "/srv/api"), runner

    def test_rejects_invalid_or_unowned_persisted_state_before_modifying_remote(self) -> None:
        invalid_states = (
            {"name": "deployer-api", "id": "not-a-uuid", "hostname": "api.demo.frgmt.xyz", "service": "http://127.0.0.1:8080"},
            {"name": "deployer-api", "id": TUNNEL_ID, "hostname": "api.demo.frgmt.xyz", "service": "http://127.0.0.1:8080", "extra": "field"},
        )
        for state in invalid_states:
            with self.subTest(state=state):
                manager, runner = self.make_manager()
                runner.ssh.return_value = completed(json.dumps(state))
                with self.assertRaisesRegex(DeploymentError, "state (is invalid|has an invalid tunnel id)"):
                    manager.ensure()
                runner._upload.assert_not_called()

    def test_existing_named_tunnel_is_not_adopted_or_modified(self) -> None:
        manager, runner = self.make_manager()
        runner.ssh.side_effect = [
            completed(),
            completed(),  # hostname preflight
            completed(json.dumps([{"id": TUNNEL_ID, "name": manager.name}])),
        ]

        with self.assertRaisesRegex(DeploymentError, "not owned"):
            manager.ensure()

        runner._upload.assert_not_called()
        scripts = [call.args[0] for call in runner.ssh.call_args_list]
        self.assertFalse(any("tunnel create" in script for script in scripts))
        self.assertFalse(any("route dns" in script for script in scripts))

    def test_existing_owned_state_is_idempotent_and_never_creates_another_tunnel(self) -> None:
        manager, runner = self.make_manager()
        state = {"name": manager.name, "id": TUNNEL_ID, "hostname": "api.demo.frgmt.xyz", "service": "http://127.0.0.1:8080"}
        runner.ssh.side_effect = [
            completed(json.dumps(state)),  # state()
            completed("/home/deploy\n"),  # remote home for credentials
            completed(),  # config validation
            completed(),  # DNS route
            completed("/usr/bin/cloudflared\n"),  # executable lookup
            completed(),  # unit installation
        ]

        result = manager.ensure()

        self.assertEqual(result.id, TUNNEL_ID)
        scripts = [call.args[0] for call in runner.ssh.call_args_list]
        self.assertFalse(any("tunnel create" in script for script in scripts))
        self.assertEqual(sum("route dns" in script for script in scripts), 1)
        config_contents = runner._upload.call_args_list[0].args[0]
        self.assertIn('"credentials-file": "/home/deploy/.cloudflared/', config_contents)
        self.assertNotIn("$HOME", config_contents)

    def test_dns_route_explicitly_disables_overwrite_and_clears_inherited_force_flag(self) -> None:
        manager, runner = self.make_manager()
        manager._route_dns(TunnelState(manager.name, TUNNEL_ID, "api.demo.frgmt.xyz", "http://127.0.0.1:8080"))

        script = runner.ssh.call_args.args[0]
        self.assertIn("unset TUNNEL_FORCE_PROVISIONING_DNS", script)
        self.assertIn("--overwrite-dns=false", script)
        self.assertNotIn("--overwrite-dns=true", script)

    def test_preexisting_dns_conflict_stops_before_any_cloudflared_change(self) -> None:
        manager, runner = self.make_manager()
        runner.ssh.side_effect = [completed(), DeploymentError("hostname already resolves")]

        with self.assertRaisesRegex(DeploymentError, "already resolves"):
            manager.ensure()

        self.assertEqual(runner.ssh.call_count, 2)
        self.assertIn("getaddrinfo", runner.ssh.call_args_list[1].args[0])
        runner._upload.assert_not_called()

    def test_tunnel_unit_name_cannot_collide_with_a_valid_application_unit(self) -> None:
        manager, _ = self.make_manager(service_name="api-tunnel")

        self.assertNotEqual(manager.unit_name, manager.config.unit_name)

    def test_unit_install_refuses_to_replace_a_unit_without_its_owned_marker(self) -> None:
        manager, runner = self.make_manager()
        runner.ssh.side_effect = [completed("/usr/bin/cloudflared\n"), completed()]
        state = TunnelState(manager.name, TUNNEL_ID, "api.demo.frgmt.xyz", "http://127.0.0.1:8080")

        manager._install_unit(state)

        install_script = runner.ssh.call_args_list[-1].args[0]
        self.assertIn("Refusing to overwrite an unowned tunnel unit", install_script)
        self.assertIn("grep -Fx", install_script)
        self.assertIn("deployer managed Cloudflare Tunnel: api", runner._upload.call_args.args[0])

    def test_create_failure_after_tunnel_creation_keeps_owned_state_for_safe_retry(self) -> None:
        manager, runner = self.make_manager()
        runner.ssh.side_effect = [
            completed(),  # no saved state
            completed(),  # hostname preflight
            completed("[]"),  # list before create
            completed("created credential details"),  # create
            completed("[]"),  # list after create fails to identify it
        ]

        with self.assertRaisesRegex(DeploymentError, "could not be identified"):
            manager.ensure()

        # Never fabricate state for a tunnel whose id cannot be established.
        runner._upload.assert_not_called()


if __name__ == "__main__":
    unittest.main()
