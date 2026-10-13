"""Opt-in management of a locally configured Cloudflare Tunnel on the deploy host."""
from __future__ import annotations

import json
import uuid
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

from .remote import DeploymentError, _plain_path, _prelude, _q, _script, _unit_arg

if TYPE_CHECKING:
    from .remote import Runner


@dataclass(frozen=True)
class TunnelState:
    name: str
    id: str
    hostname: str
    service: str

    @classmethod
    def from_json(cls, raw: dict[str, Any]) -> "TunnelState":
        if set(raw) != {"name", "id", "hostname", "service"} or not all(isinstance(raw.get(key), str) and raw[key] for key in ("name", "id", "hostname", "service")):
            raise DeploymentError("Owned tunnel state is invalid; refusing to modify it")
        try:
            uuid.UUID(raw["id"])
        except ValueError as exc:
            raise DeploymentError("Owned tunnel state is invalid (bad tunnel id); refusing to modify it") from exc
        return cls(raw["name"], raw["id"], raw["hostname"], raw["service"])


class TunnelManager:
    """Manages only a tunnel recorded in this deployment's private state file."""

    def __init__(self, runner: "Runner", base: str):
        self.runner = runner
        self.config = runner.config
        self.base = base
        self.state_path = f"{base}/.deployer/tunnel.json"
        self.config_path = f"{base}/.deployer/cloudflared.yml"
        self.unit_path = f"$HOME/.config/systemd/user/{self.unit_name}"

    @property
    def name(self) -> str:
        return f"deployer-{self.config.service.name}"

    @property
    def unit_name(self) -> str:
        # @ cannot appear in a service name, so this cannot collide with a deployer app unit.
        return f"deployer-tunnel@{self.config.service.name}.service"

    def state(self) -> TunnelState | None:
        result = self.runner.ssh(_script("set -eu", f"if test -f {_q(self.state_path)}; then cat {_q(self.state_path)}; fi"), capture=True)
        if not result.stdout.strip():
            return None
        try:
            data = json.loads(result.stdout)
        except json.JSONDecodeError as exc:
            raise DeploymentError("Owned tunnel state is not valid JSON; refusing to modify it") from exc
        if not isinstance(data, dict):
            raise DeploymentError("Owned tunnel state is invalid; refusing to modify it")
        return TunnelState.from_json(data)

    def assert_section_present(self) -> None:
        if self.config.tunnel is None and self.state() is not None:
            raise DeploymentError("This service owns a public tunnel. Removing [tunnel] does not delete its DNS route or credentials; restore it or clean up the owned tunnel manually.")

    def ensure(self) -> TunnelState:
        wanted = self.config.tunnel
        if wanted is None:
            raise DeploymentError("[tunnel] is required for this command")
        state = self.state()
        if state is not None:
            if state.name != self.name:
                raise DeploymentError("Owned tunnel state belongs to a different service; refusing to modify it")
            if state.hostname != wanted.hostname or state.service != wanted.service:
                raise DeploymentError("Changing [tunnel] hostname or service is refused; stop and remove the owned tunnel explicitly before changing it")
        else:
            state = self._create_owned(wanted.hostname, wanted.service)
        self._write_config(state)
        self._route_dns(state)
        self._install_unit(state)
        return state

    def _cloudflared(self, *args: str, capture: bool = False) -> str:
        result = self.runner.ssh(_script("set -eu", "unset TUNNEL_FORCE_PROVISIONING_DNS", "cloudflared " + " ".join(_q(arg) for arg in args)), capture=capture)
        return result.stdout

    def _create_owned(self, hostname: str, service: str) -> TunnelState:
        self._preflight_hostname(hostname)
        listed = self._cloudflared("tunnel", "list", "--output", "json", capture=True)
        try:
            tunnels = json.loads(listed)
        except json.JSONDecodeError as exc:
            raise DeploymentError("cloudflared returned invalid tunnel list data") from exc
        if not isinstance(tunnels, list):
            raise DeploymentError("cloudflared returned invalid tunnel list data")
        if any(isinstance(item, dict) and item.get("name") == self.name for item in tunnels):
            raise DeploymentError(f"Tunnel {self.name!r} already exists but is not owned by this deployment; refusing to adopt it")
        # Creating prints credential details on some cloudflared releases: capture it and never relay it.
        self._cloudflared("tunnel", "create", self.name, capture=True)
        listed = self._cloudflared("tunnel", "list", "--output", "json", capture=True)
        try:
            tunnels = json.loads(listed)
            matching = [item for item in tunnels if isinstance(item, dict) and item.get("name") == self.name and isinstance(item.get("id"), str)]
        except json.JSONDecodeError as exc:
            raise DeploymentError("cloudflared returned invalid tunnel list data after creation") from exc
        if len(matching) != 1:
            raise DeploymentError("Created tunnel could not be identified; it was not adopted")
        state = TunnelState.from_json({"name": self.name, "id": matching[0]["id"], "hostname": hostname, "service": service})
        self._write_state(state)
        return state

    def _preflight_hostname(self, hostname: str) -> None:
        # A positive answer can be an inherited wildcard record; fail closed before creating a tunnel.
        code = "import socket,sys\ntry:\n socket.getaddrinfo(sys.argv[1], None)\nexcept socket.gaierror as err:\n missing={socket.EAI_NONAME,getattr(socket,'EAI_NODATA',socket.EAI_NONAME)}\n if err.errno in missing: sys.exit(0)\n print('DNS lookup failed: '+str(err),file=sys.stderr); sys.exit(2)\nelse:\n print('hostname already resolves; refusing to risk a DNS conflict',file=sys.stderr); sys.exit(1)"
        self.runner.ssh(_script("set -eu", f"python3 -c {_q(code)} {_q(hostname)}"))

    def _write_state(self, state: TunnelState) -> None:
        content = json.dumps({"name": state.name, "id": state.id, "hostname": state.hostname, "service": state.service}, sort_keys=True) + "\n"
        self.runner._upload(content, self.state_path, "600")

    def _write_config(self, state: TunnelState) -> None:
        remote_home = self.runner.ssh(_script("set -eu", "printf '%s\\n' \"$HOME\""), capture=True).stdout.strip()
        if not _plain_path(remote_home):
            raise DeploymentError("remote host returned an invalid home directory for cloudflared")
        # JSON is valid YAML, so this avoids a hand-rolled YAML quoting scheme.
        content = json.dumps({"tunnel": state.id, "credentials-file": f"{remote_home}/.cloudflared/{state.id}.json", "ingress": [{"hostname": state.hostname, "service": state.service}, {"service": "http_status:404"}]}, indent=2) + "\n"
        self.runner._upload(content, self.config_path, "600")
        self._cloudflared("--config", self.config_path, "tunnel", "ingress", "validate")

    def _route_dns(self, state: TunnelState) -> None:
        # `false` and the cleared environment prevent an inherited force-overwrite setting.
        self._cloudflared("tunnel", "route", "dns", "--overwrite-dns=false", state.id, state.hostname)

    def _install_unit(self, state: TunnelState) -> None:
        executable = self.runner.ssh(_script("set -eu", "command -v cloudflared"), capture=True).stdout.strip()
        if not _plain_path(executable):
            raise DeploymentError("cloudflared is not installed at a safe absolute path on the deploy host")
        marker = f"Description=deployer managed Cloudflare Tunnel: {self.config.service.name}"
        unit = _script("[Unit]", marker, "After=network-online.target", "Wants=network-online.target", "", "[Service]", "Type=simple", f"ExecStart={_unit_arg(executable)} --no-autoupdate --config {_unit_arg(self.config_path)} tunnel run {state.id}", "Restart=always", "RestartSec=3", "", "[Install]", "WantedBy=default.target")
        candidate = f"{self.base}/.deployer/tunnel-unit.next"
        self.runner._upload(unit, candidate, "600")
        connected = "import json,sys\nrows=json.load(sys.stdin)\nrow=next((x for x in rows if x.get('id')==sys.argv[1]),None)\nsys.exit(0 if row and row.get('connections') else 1)"
        self.runner.ssh(_prelude() + _script("set -eu", 'mkdir -p "$(dirname ' + self.unit_path + ')"', f"if test -e {self.unit_path} && ! grep -Fx {_q(marker)} {self.unit_path} >/dev/null; then echo 'Refusing to overwrite an unowned tunnel unit' >&2; exit 1; fi", f"mv -f {_q(candidate)} {self.unit_path}", "systemctl --user daemon-reload", f"systemctl --user enable { _q(self.unit_name) } >/dev/null", f"systemctl --user restart { _q(self.unit_name) }", "sleep 2", f"systemctl --user is-active --quiet { _q(self.unit_name) }", f"for try in 1 2 3 4 5; do cloudflared tunnel list --output json | python3 -c {_q(connected)} {_q(state.id)} && exit 0; sleep 2; done", "echo 'Tunnel service started but did not establish a Cloudflare connection' >&2", "exit 1"))

    def status(self) -> None:
        self.runner.ssh(_prelude() + _script(f"systemctl --user status {_q(self.unit_name)} --no-pager"))

    def logs(self, lines: int = 100) -> None:
        self.runner.ssh(_prelude() + _script(f"journalctl --user-unit {_q(self.unit_name)} --no-pager -n {lines}"))

    def stop(self) -> None:
        if self.state() is None:
            raise DeploymentError("No owned tunnel is configured for this service")
        self.runner.ssh(_prelude() + _script(f"systemctl --user disable --now {_q(self.unit_name)}"))
