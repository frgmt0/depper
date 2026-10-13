from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
import re
import tomllib
from typing import Any
from urllib.parse import urlparse


class ConfigError(ValueError):
    pass


_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$")
_ENV = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
_HOST = re.compile(r"^(?:[A-Za-z0-9_.-]+@)?[A-Za-z0-9][A-Za-z0-9_.-]*$")
_FQDN_LABEL = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")
_DEFAULT_EXCLUDES = (".git/", ".env", ".env.*", "deploy.toml", ".cloudflared/", ".cache/", "node_modules/", "venv/", ".venv/", "__pycache__/", ".pytest_cache/", ".mypy_cache/", ".ruff_cache/")


@dataclass(frozen=True)
class ServiceConfig:
    name: str
    command: str
    restart: str


@dataclass(frozen=True)
class RemoteConfig:
    host: str
    path: str | None
    setup: tuple[str, ...]
    exclude: tuple[str, ...]
    healthcheck: str | None


@dataclass(frozen=True)
class TunnelConfig:
    hostname: str
    service: str


@dataclass(frozen=True)
class SecretsConfig:
    file: Path


@dataclass(frozen=True)
class DeployConfig:
    repo: Path
    service: ServiceConfig
    deploy: RemoteConfig
    env: dict[str, str] = field(repr=False)
    tunnel: TunnelConfig | None = None
    secrets: SecretsConfig | None = None

    @property
    def unit_name(self) -> str:
        return f"deployer-{self.service.name}.service"


def _dict(value: Any, section: str, required: bool = True) -> dict[str, Any]:
    if value is None and not required:
        return {}
    if not isinstance(value, dict):
        raise ConfigError(f"[{section}] must be a table")
    return value


def _str(table: dict[str, Any], key: str, section: str, required: bool = True) -> str | None:
    value = table.get(key)
    if value is None and not required:
        return None
    if not isinstance(value, str) or not value.strip():
        raise ConfigError(f"[{section}].{key} must be a non-empty string")
    return value


def _list(table: dict[str, Any], key: str, section: str) -> tuple[str, ...]:
    value = table.get(key, [])
    if not isinstance(value, list) or any(not isinstance(x, str) or not x.strip() for x in value):
        raise ConfigError(f"[{section}].{key} must be a list of non-empty strings")
    return tuple(value)


def load_config(repo: Path) -> DeployConfig:
    repo = repo.expanduser().resolve()
    try:
        with (repo / "deploy.toml").open("rb") as f:
            raw = tomllib.load(f)
    except FileNotFoundError as exc:
        raise ConfigError(f"No deploy.toml found in {repo}") from exc
    except tomllib.TOMLDecodeError as exc:
        raise ConfigError(f"Invalid deploy.toml: {exc}") from exc
    if not isinstance(raw, dict) or set(raw) - {"service", "deploy", "env", "tunnel", "secrets"}:
        raise ConfigError("deploy.toml may contain only [service], [deploy], [env], [tunnel], and [secrets]")
    service = _dict(raw.get("service"), "service")
    deploy = _dict(raw.get("deploy"), "deploy")
    env_raw = _dict(raw.get("env"), "env", False)
    tunnel_raw = _dict(raw.get("tunnel"), "tunnel", False)
    secrets_raw = _dict(raw.get("secrets"), "secrets", False)
    if set(service) - {"name", "command", "restart"}:
        raise ConfigError("Unknown [service] setting")
    if set(deploy) - {"host", "path", "setup", "exclude", "healthcheck"}:
        raise ConfigError("Unknown [deploy] setting")
    if set(tunnel_raw) - {"hostname", "service"}:
        raise ConfigError("Unknown [tunnel] setting")
    if set(secrets_raw) - {"file"}:
        raise ConfigError("Unknown [secrets] setting")
    name = _str(service, "name", "service")
    command = _str(service, "command", "service")
    host = _str(deploy, "host", "deploy")
    assert name and command and host
    if not _NAME.fullmatch(name):
        raise ConfigError("[service].name must be a safe systemd unit name (max 63 chars)")
    if any(c in command for c in "\0\r\n"):
        raise ConfigError("[service].command may not contain NUL or a newline")
    if not _HOST.fullmatch(host):
        raise ConfigError("[deploy].host must be a safe SSH alias or user@host")
    restart = _str(service, "restart", "service", False) or "on-failure"
    if restart not in {"on-failure", "always", "no"}:
        raise ConfigError("[service].restart must be on-failure, always, or no")
    path = _str(deploy, "path", "deploy", False)
    if path is not None and (not path.startswith("/") or any(c in path for c in "\0\r\n\\%\"'") or any(c.isspace() for c in path)):
        raise ConfigError("[deploy].path must be an absolute, plain remote path (no spaces, quotes, backslashes, or percent signs)")
    setup = _list(deploy, "setup", "deploy")
    exclude = _list(deploy, "exclude", "deploy")
    healthcheck = _str(deploy, "healthcheck", "deploy", False)
    if healthcheck and any(c in healthcheck for c in "\0\r\n"):
        raise ConfigError("[deploy].healthcheck may not contain NUL or a newline")
    for item in exclude:
        if any(c in item for c in "\0\r\n") or item.startswith("/") or ".." in Path(item).parts:
            raise ConfigError("[deploy].exclude entries must be safe relative patterns")
    if any(any(c in item for c in "\0\r\n") for item in setup):
        raise ConfigError("[deploy].setup commands may not contain NUL or a newline")
    env: dict[str, str] = {}
    for key, value in env_raw.items():
        if not isinstance(key, str) or not _ENV.fullmatch(key):
            raise ConfigError(f"Invalid [env] name: {key!r}")
        if not isinstance(value, str) or any(c in value for c in "\0\r\n"):
            raise ConfigError(f"[env].{key} must be a single-line string")
        env[key] = value
    tunnel: TunnelConfig | None = None
    if tunnel_raw:
        hostname = _str(tunnel_raw, "hostname", "tunnel")
        origin = _str(tunnel_raw, "service", "tunnel")
        assert hostname and origin
        if any(ord(c) < 32 or ord(c) == 127 for c in origin):
            raise ConfigError("[tunnel].service may not contain control characters")
        hostname = hostname.lower().rstrip(".")
        labels = hostname.split(".")
        if len(labels) < 3 or any(not _FQDN_LABEL.fullmatch(label) for label in labels):
            raise ConfigError("[tunnel].hostname must be an exact public FQDN with at least three labels")
        parsed = urlparse(origin)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.params or parsed.path not in {"", "/"}:
            raise ConfigError("[tunnel].service must be a plain http:// or https:// origin without credentials, query, fragment, or path")
        try:
            port = parsed.port
        except ValueError as exc:
            raise ConfigError("[tunnel].service has an invalid port") from exc
        if port is None or not 1 <= port <= 65535:
            raise ConfigError("[tunnel].service must include an explicit origin port")
        tunnel = TunnelConfig(hostname, origin.rstrip("/"))
    secrets: SecretsConfig | None = None
    if secrets_raw:
        source = _str(secrets_raw, "file", "secrets")
        assert source is not None
        if any(ord(char) < 32 or ord(char) == 127 for char in source):
            raise ConfigError("[secrets].file may not contain control characters")
        candidate = Path(source).expanduser()
        secrets = SecretsConfig(candidate if candidate.is_absolute() else repo / candidate)
    return DeployConfig(repo, ServiceConfig(name, command, restart), RemoteConfig(host, path, setup, tuple(dict.fromkeys((*_DEFAULT_EXCLUDES, *exclude))), healthcheck), env, tunnel, secrets)
