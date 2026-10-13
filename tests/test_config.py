from __future__ import annotations

from pathlib import Path
import tempfile
import textwrap
import unittest

from deployer.config import ConfigError, load_config


class LoadConfigTests(unittest.TestCase):
    def write_config(self, root: Path, contents: str) -> None:
        (root / "deploy.toml").write_text(textwrap.dedent(contents), encoding="utf-8")

    def test_loads_valid_config_and_adds_safe_default_excludes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_config(
                root,
                '''
                [service]
                name = "api-worker.v2"
                command = "python3 service.py --workers 2"

                [deploy]
                host = "deploy@host.example"
                path = "/srv/apps/api"
                setup = ["python3 -m venv .venv"]
                exclude = ["build/", ".git/"]
                healthcheck = "curl --fail http://127.0.0.1:8080/health"

                [env]
                PORT = "8080"
                FEATURE_FLAG = "yes"
                ''',
            )

            config = load_config(root)

            self.assertEqual(config.repo, root.resolve())
            self.assertEqual(config.unit_name, "deployer-api-worker.v2.service")
            self.assertEqual(config.service.restart, "on-failure")
            self.assertEqual(config.deploy.path, "/srv/apps/api")
            self.assertEqual(config.deploy.setup, ("python3 -m venv .venv",))
            self.assertEqual(config.env, {"PORT": "8080", "FEATURE_FLAG": "yes"})
            self.assertIn(".git/", config.deploy.exclude)
            self.assertIn(".env", config.deploy.exclude)
            self.assertIn("build/", config.deploy.exclude)
            self.assertEqual(config.deploy.exclude.count(".git/"), 1)

    def test_rejects_missing_or_unknown_configuration(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaisesRegex(ConfigError, "No deploy.toml"):
                load_config(root)

            self.write_config(
                root,
                '''
                [service]
                name = "api"
                command = "python api.py"
                unexpected = "setting"
                [deploy]
                host = "server"
                ''',
            )
            with self.assertRaisesRegex(ConfigError, "Unknown \\[service\\] setting"):
                load_config(root)

    def test_rejects_unsafe_or_invalid_values(self) -> None:
        cases = {
            "unsafe service name": '''
                [service]
                name = "api; rm -rf /"
                command = "python app.py"
                [deploy]
                host = "server"
            ''',
            "invalid restart": '''
                [service]
                name = "api"
                command = "python app.py"
                restart = "unless-stopped"
                [deploy]
                host = "server"
            ''',
            "relative remote path": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                path = "apps/api"
            ''',
            "carriage return remote path": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                path = "/srv/api\\rExecStart=/bin/false"
            ''',
            "systemd-ambiguous remote path": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                path = "/srv/path with % sign"
            ''',
            "absolute exclude": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                exclude = ["/private-key"]
            ''',
            "parent exclude": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                exclude = ["../private-key"]
            ''',
            "ssh option host": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "-oProxyCommand=evil"
            ''',
            "whitespace in host": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server -oProxyCommand=evil"
            ''',
            "rsync separator in host": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server:2222"
            ''',
            "slash in host": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server/path"
            ''',
            "multiline setup": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                setup = ["echo one\\ntouch /tmp/surprise"]
            ''',
            "nul setup": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                setup = ["echo one\\u0000"]
            ''',
            "carriage return healthcheck": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                healthcheck = "true\\rfalse"
            ''',
            "carriage return environment": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                [env]
                TOKEN = "value\\rmore"
            ''',
            "unsafe environment name": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                [env]
                "BAD-NAME" = "value"
            ''',
            "non-string environment value": '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                [env]
                PORT = 8080
            ''',
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for description, contents in cases.items():
                with self.subTest(description=description):
                    self.write_config(root, contents)
                    with self.assertRaises(ConfigError):
                        load_config(root)

    def test_rejects_non_table_sections_and_non_string_lists(self) -> None:
        cases = (
            "service = 'nope'\n[deploy]\nhost = 'server'\n",
            "[service]\nname = 'api'\ncommand = 'python app.py'\n[deploy]\nhost = 'server'\nsetup = [123]\n",
            "[service]\nname = 'api'\ncommand = 'python app.py'\n[deploy]\nhost = 'server'\nexclude = 'build/'\n",
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for contents in cases:
                with self.subTest(contents=contents):
                    self.write_config(root, contents)
                    with self.assertRaises(ConfigError):
                        load_config(root)

    def test_loads_and_normalizes_a_valid_tunnel(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_config(
                root,
                '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                [tunnel]
                hostname = "Api.Demo.frgmt.xyz."
                service = "http://127.0.0.1:8080/"
                ''',
            )

            tunnel = load_config(root).tunnel

        self.assertIsNotNone(tunnel)
        assert tunnel is not None
        self.assertEqual(tunnel.hostname, "api.demo.frgmt.xyz")
        self.assertEqual(tunnel.service, "http://127.0.0.1:8080")

    def test_rejects_invalid_tunnel_hostnames_and_origins(self) -> None:
        cases = (
            ("wildcard hostname", "*.frgmt.xyz", "http://127.0.0.1:8080"),
            ("not enough hostname labels", "frgmt.xyz", "http://127.0.0.1:8080"),
            ("hostname with underscore", "api_bad.frgmt.xyz", "http://127.0.0.1:8080"),
            ("missing origin port", "api.demo.frgmt.xyz", "http://127.0.0.1"),
            ("credentials in origin", "api.demo.frgmt.xyz", "https://user:pass@127.0.0.1:443"),
            ("origin path", "api.demo.frgmt.xyz", "http://127.0.0.1:8080/internal"),
            ("origin query", "api.demo.frgmt.xyz", "http://127.0.0.1:8080/?debug=1"),
            ("origin tab control character", "api.demo.frgmt.xyz", "http://127.0.0.1:\\t8080"),
            ("invalid origin scheme", "api.demo.frgmt.xyz", "tcp://127.0.0.1:8080"),
            ("invalid zero origin port", "api.demo.frgmt.xyz", "http://127.0.0.1:0"),
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for description, hostname, origin in cases:
                with self.subTest(description=description):
                    self.write_config(
                        root,
                        f'''\
                        [service]
                        name = "api"
                        command = "python app.py"
                        [deploy]
                        host = "server"
                        [tunnel]
                        hostname = "{hostname}"
                        service = "{origin}"
                        ''',
                    )
                    with self.assertRaises(ConfigError):
                        load_config(root)

    def test_secrets_file_is_repository_relative_and_env_values_are_hidden_from_repr(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_config(
                root,
                '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                [env]
                TOKEN = "synthetic-secret-value"
                [secrets]
                file = "ops/credentials.prod"
                ''',
            )
            config = load_config(root)

        self.assertIsNotNone(config.secrets)
        assert config.secrets is not None
        self.assertEqual(config.secrets.file, root.resolve() / "ops" / "credentials.prod")
        self.assertNotIn("synthetic-secret-value", repr(config))

    def test_secrets_file_allows_an_explicit_external_path(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_config(
                root,
                '''
                [service]
                name = "api"
                command = "python app.py"
                [deploy]
                host = "server"
                [secrets]
                file = "/tmp/deployer-synthetic.env"
                ''',
            )
            config = load_config(root)

        assert config.secrets is not None
        self.assertEqual(config.secrets.file, Path("/tmp/deployer-synthetic.env"))


if __name__ == "__main__":
    unittest.main()
