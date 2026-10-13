from __future__ import annotations

import base64
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
import secrets
import shlex
import subprocess
from typing import Iterable

from .config import DeployConfig
from .dotenv import DotenvError, SecretSource, read_dotenv, resolve_secret_source, source_rsync_excludes


class DeploymentError(RuntimeError): pass


def _q(value: str) -> str: return shlex.quote(value)
def _rid() -> str: return "release-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(4)
def _prelude() -> str: return 'set -eu\nexport XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"\nexport DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"\n'
def _script(*lines: str) -> str: return "\n".join(lines) + "\n"
def _plain_path(value: str) -> bool: return value.startswith("/") and not any(c in value for c in "\0\r\n\\%\"'") and not any(c.isspace() for c in value)


def _environment_file(values: dict[str, str]) -> str:
    def escape(value: str) -> str:
        # systemd EnvironmentFile keeps literal quoted newlines; it does not C-unescape \n.
        return value.replace("\\", "\\\\").replace('"', '\\"')
    return "".join(f'{key}="{escape(value)}"\n' for key, value in sorted(values.items()))


def _exports(values: dict[str, str]) -> list[str]:
    """Shell-safe exports for setup and health checks (never source EnvironmentFile)."""
    return [f"export {key}={_q(value)}" for key, value in sorted(values.items())]


def _unit_arg(value: str) -> str:
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"').replace("$", "$$").replace("%", "%%") + '"'


def _unit_path(value: str, *, execstart: bool = False) -> str:
    # systemd path directives are not shell words; quotes become part of the path.
    escaped = value.replace("\\", "\\x5c").replace(" ", "\\x20").replace("%", "%%")
    return escaped.replace("$", "$$") if execstart else escaped


def _unit(config: DeployConfig, base: str, envfile: str) -> str:
    script = f"{base}/current/.deployer-start.sh"
    return _script("[Unit]", f"Description=deployer: {config.service.name}", "After=network.target", "", "[Service]", "Type=simple", f"WorkingDirectory={_unit_path(base + '/current')}", f"EnvironmentFile={_unit_path(envfile)}", f"ExecStart=/bin/sh {_unit_arg(script)}", f"Restart={config.service.restart}", "RestartSec=3", "", "[Install]", "WantedBy=default.target")


@dataclass
class Runner:
    config: DeployConfig
    dry_run: bool = False

    def _run(self, args: list[str], *, input: str | None = None, capture: bool = False) -> subprocess.CompletedProcess[str]:
        if self.dry_run:
            print("DRY RUN:", " ".join(_q(x) for x in args)); return subprocess.CompletedProcess(args, 0, "", "")
        try: return subprocess.run(args, input=input, text=True, capture_output=capture, check=True)
        except FileNotFoundError as exc: raise DeploymentError(f"Required program not found: {args[0]}") from exc
        except subprocess.CalledProcessError as exc: raise DeploymentError((exc.stderr or exc.stdout or "remote command failed").strip()) from exc

    def ssh(self, script: str, *, capture: bool = False) -> subprocess.CompletedProcess[str]:
        return self._run(["ssh", self.config.deploy.host, "bash -s"], input=script, capture=capture)

    def base_path(self) -> str:
        if self.dry_run: return self.config.deploy.path or f"~/.local/share/deployer/{self.config.service.name}"
        assignment = _q(self.config.deploy.path) if self.config.deploy.path else f'"$HOME/.local/share/deployer/{self.config.service.name}"'
        out = self.ssh(_script("set -eu", f"base={assignment}", 'test ! -L "$base/.deployer" || { echo "Refusing a symlink deployment metadata directory" >&2; exit 1; }', 'mkdir -p "$base/releases" "$base/.deployer/transactions"', 'test ! -L "$base/.deployer/transactions" || { echo "Refusing a symlink deployment transaction directory" >&2; exit 1; }', 'chmod 700 "$base/.deployer" "$base/.deployer/transactions"', "printf '%s\\n' \"$base\""), capture=True).stdout.strip()
        if not _plain_path(out): raise DeploymentError("remote host returned an invalid deployment path")
        return out

    def _acquire_lock(self, base: str) -> None:
        self.ssh(_script("set -eu", f"lock={_q(base + '/.deployer/lock')}", 'if ! mkdir "$lock"; then echo "Another deployer operation is already running for this service" >&2; exit 1; fi', 'chmod 700 "$lock"'))

    def _release_lock(self, base: str) -> None:
        self.ssh(_script("set -eu", f"rmdir {_q(base + '/.deployer/lock')}"))

    def _deployment_environment(self, override: Path | None) -> tuple[dict[str, str], SecretSource | None, tuple[str, ...], int]:
        configured = self.config.secrets.file if self.config.secrets else None
        selected = override if override is not None else configured
        try:
            source = resolve_secret_source(self.config.repo, selected)
            file_values = read_dotenv(source) if source is not None else {}
        except DotenvError as exc:
            raise DeploymentError(str(exc)) from exc
        excludes = tuple(dict.fromkeys((*source_rsync_excludes(self.config.repo, configured), *(source.rsync_excludes if source else ()))))
        return ({**self.config.env, **file_values}, source, excludes, len(file_values))

    def deploy(self, env_file: Path | None = None) -> None:
        environment_values, secret_source, secret_excludes, secret_count = self._deployment_environment(env_file)
        if self.dry_run:
            target = self.config.deploy.path or f"~/.local/share/deployer/{self.config.service.name}"
            print(f"DRY RUN: deploy {self.config.service.name} to {self.config.deploy.host}:{target}")
            print(f"  restart={self.config.service.restart}; setup steps={len(self.config.deploy.setup)}; healthcheck={'yes' if self.config.deploy.healthcheck else 'no'}")
            print(f"  excludes={', '.join(self.config.deploy.exclude)}")
            if secret_source is not None: print(f"  dotenv={secret_source.selected}; variables={secret_count}")
            return
        base, rid = self.base_path(), _rid(); release = f"{base}/releases/{rid}"; envfile = f"{base}/.deployer/environment"; backup = f"{base}/.deployer/transactions/{rid}"; unit = f"$HOME/.config/systemd/user/{self.config.unit_name}"
        from .tunnel import TunnelManager
        tunnel = TunnelManager(self, base)
        if self.config.tunnel is None:
            tunnel.assert_section_present()
        self._acquire_lock(base)
        try:
            self.ssh(_script("set +xv", "set -eu", "umask 077", f"mkdir -p {_q(release)} {_q(backup)}", f"chmod 700 {_q(backup)}")); self._rsync(release, secret_excludes); self._upload(_environment_file(environment_values), f"{envfile}.next-{rid}"); self._upload("#!/bin/sh\nexec " + self.config.service.command + "\n", f"{release}/.deployer-start.sh", "700"); self._upload(_unit(self.config, base, envfile), f"{base}/.deployer/unit.next-{rid}")
            try: self._setup(release, f"{envfile}.next-{rid}"); self._activate(base, release, envfile, unit, backup, rid)
            except DeploymentError as error:
                try: self._restore(base, envfile, unit, backup)
                except DeploymentError as restore_error: raise DeploymentError(f"deployment failed: {error}; automatic restore also failed: {restore_error}") from restore_error
                raise
            if self.config.tunnel is not None:
                try: tunnel.ensure()
                except DeploymentError as error: raise DeploymentError(f"Application deployed successfully, but tunnel setup failed: {error}") from error
            print(f"Deployed {self.config.service.name} to {self.config.deploy.host} ({rid})")
        finally:
            try: self._cleanup_candidates(base, rid)
            finally: self._release_lock(base)

    def _rsync(self, release: str, extra_excludes: Iterable[str] = ()) -> None:
        args = ["rsync", "-az"]
        for item in dict.fromkeys((*self.config.deploy.exclude, *extra_excludes)): args += ["--exclude", item]
        # rsync passes this suffix to a remote shell; keep the remote absolute path one quoted word.
        self._run(args + [str(self.config.repo) + "/", f"{self.config.deploy.host}:'{release}'/"])

    def _upload(self, contents: str, destination: str, mode: str = "600") -> None:
        encoded = base64.b64encode(contents.encode()).decode()
        self.ssh(_script("set +xv", "set -eu", "umask 077", f"destination={_q(destination)}", 'test ! -L "$destination" || { echo "Refusing a symlink upload destination" >&2; exit 1; }', 'parent=$(dirname "$destination")', 'test -d "$parent"', 'temporary=$(mktemp "$parent/.deployer-upload.XXXXXX")', "trap 'rm -f \"$temporary\"' EXIT HUP INT TERM", f"printf %s {_q(encoded)} | base64 -d > \"$temporary\"", f"chmod {mode} \"$temporary\"", 'mv -f "$temporary" "$destination"', "trap - EXIT HUP INT TERM"))

    def _cleanup_candidates(self, base: str, rid: str) -> None:
        self.ssh(_script("set +xv", "set -eu", f"rm -f {_q(base + '/.deployer/environment.next-' + rid)} {_q(base + '/.deployer/unit.next-' + rid)}"))

    def _setup(self, release: str, candidate_env: str) -> None:
        for command in self.config.deploy.setup: self.ssh(_script("set -eu", *_exports(self.config.env), f"cd {_q(release)}", f"/bin/sh -lc {_q(command)}"))

    def _activate(self, base: str, release: str, envfile: str, unit: str, backup: str, rid: str) -> None:
        lines = [f"base={_q(base)}", f"backup={_q(backup)}", f"environment={_q(envfile)}", f"unit={unit}", 'mkdir -p "$(dirname "$unit")"', 'test ! -L "$environment" || { echo "Refusing a symlink environment file" >&2; exit 1; }', 'if test -e "$environment"; then cp -p "$environment" "$backup/environment"; else : > "$backup/no-environment"; fi', 'if test -e "$unit"; then cp -p "$unit" "$backup/unit"; else : > "$backup/no-unit"; fi', 'if test -L "$base/current"; then readlink "$base/current" > "$backup/current"; else : > "$backup/no-current"; fi', f"mv -f {_q(envfile + '.next-' + rid)} \"$environment\"", f"mv -f {_q(base + '/.deployer/unit.next-' + rid)} \"$unit\"", f"ln -s {_q(release)} \"$base/.deployer/current.next-{rid}\"", f"mv -Tf \"$base/.deployer/current.next-{rid}\" \"$base/current\"", "systemctl --user daemon-reload", f"systemctl --user enable {_q(self.config.unit_name)} >/dev/null", f"systemctl --user restart {_q(self.config.unit_name)}", "sleep 3", f"systemctl --user is-active --quiet {_q(self.config.unit_name)}", f"test \"$(systemctl --user show -p NRestarts --value {_q(self.config.unit_name)})\" -eq 0"]
        if self.config.deploy.healthcheck: lines += [*_exports(self.config.env), 'cd "$base/current"', f"/bin/sh -lc {_q(self.config.deploy.healthcheck)}"]
        lines += [f'if test -f "$backup/current"; then ln -s "$(cat "$backup/current")" "$base/.deployer/previous.next-{rid}"; mv -Tf "$base/.deployer/previous.next-{rid}" "$base/.deployer/previous"; printf \'%s\\n\' "$backup" > "$base/.deployer/previous-transaction"; fi']
        self.ssh(_prelude() + _script(*lines))

    def _restore(self, base: str, envfile: str, unit: str, backup: str) -> None:
        lines = [f"base={_q(base)}", f"backup={_q(backup)}", f"environment={_q(envfile)}", f"unit={unit}", 'if test -f "$backup/environment"; then cp -p "$backup/environment" "$environment"; elif test -f "$backup/no-environment"; then rm -f "$environment"; fi', 'if test -f "$backup/unit"; then cp -p "$backup/unit" "$unit"; elif test -f "$backup/no-unit"; then systemctl --user disable --now ' + _q(self.config.unit_name) + ' || true; rm -f "$unit"; fi', 'if test -f "$backup/current"; then ln -s "$(cat "$backup/current")" "$base/.deployer/current.restore"; mv -Tf "$base/.deployer/current.restore" "$base/current"; elif test -f "$backup/no-current"; then rm -f "$base/current"; fi', "systemctl --user daemon-reload", f"if test -f \"$backup/unit\"; then systemctl --user restart {_q(self.config.unit_name)}; fi"]
        self.ssh(_prelude() + _script(*lines))

    def rollback(self) -> None:
        base = self.base_path(); envfile = f"{base}/.deployer/environment"; unit = f"$HOME/.config/systemd/user/{self.config.unit_name}"
        lines = [f"base={_q(base)}", f"environment={_q(envfile)}", f"unit={unit}", 'test -L "$base/.deployer/previous" || { echo "No previous release is available" >&2; exit 1; }', 'tx=$(cat "$base/.deployer/previous-transaction")', 'test -f "$tx/unit" && test -f "$tx/environment" || { echo "No previous service configuration is available" >&2; exit 1; }', 'old=$(readlink "$base/.deployer/previous")', 'test -d "$old" || { echo "Previous release no longer exists" >&2; exit 1; }', 'cp -p "$tx/environment" "$environment"', 'cp -p "$tx/unit" "$unit"', 'ln -s "$old" "$base/.deployer/current.rollback"', 'mv -Tf "$base/.deployer/current.rollback" "$base/current"', "systemctl --user daemon-reload", f"systemctl --user restart {_q(self.config.unit_name)}", "sleep 3", f"systemctl --user is-active --quiet {_q(self.config.unit_name)}"]
        self._acquire_lock(base)
        try: self.ssh(_prelude() + _script(*lines))
        finally: self._release_lock(base)

    def status(self) -> None: self.ssh(_prelude() + _script(f"systemctl --user status {_q(self.config.unit_name)} --no-pager"))
    def logs(self, args: Iterable[str]) -> None: self.ssh(_prelude() + _script(f"journalctl --user-unit {_q(self.config.unit_name)} --no-pager {' '.join(_q(x) for x in args)}"))
    def control(self, action: str) -> None:
        if action not in {"restart", "stop"}: raise ValueError(action)
        self.ssh(_prelude() + _script(f"systemctl --user {action} {_q(self.config.unit_name)}"))
