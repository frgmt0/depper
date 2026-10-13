from __future__ import annotations

import argparse
from pathlib import Path
import sys

from .config import ConfigError, load_config
from .remote import DeploymentError, Runner


def _positive(value: str) -> int:
    result = int(value)
    if result < 1: raise argparse.ArgumentTypeError("must be positive")
    return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="deployer", description="Deploy a repository to a user systemd service over SSH.")
    subs = parser.add_subparsers(dest="action")
    init = subs.add_parser("init", help="create a starter deploy.toml"); init.add_argument("repo", nargs="?", default=".")
    for action in ("deploy", "status", "logs", "restart", "stop", "rollback", "tunnel", "tunnel-status", "tunnel-logs", "tunnel-stop"):
        sub = subs.add_parser(action); sub.add_argument("repo", nargs="?", default=".")
        if action == "deploy":
            sub.add_argument("--dry-run", action="store_true", help="validate locally without connecting")
            sub.add_argument("--env", metavar="FILE", help="dotenv file to use for this deployment")
        if action in {"logs", "tunnel-logs"}: sub.add_argument("--lines", type=_positive, default=100)
    return parser


def init(repo: Path) -> None:
    manifest = repo / "deploy.toml"
    if manifest.exists(): raise DeploymentError(f"{manifest} already exists")
    name = repo.resolve().name.replace(" ", "-")
    manifest.write_text(f'''[service]
name = "{name}"
command = "python -m your_app"
restart = "on-failure"

[deploy]
host = "desktop"
# path = "/absolute/path/on/server"
# setup = ["python -m venv .venv", ".venv/bin/pip install -r requirements.txt"]
# healthcheck = "curl -fsS http://127.0.0.1:8000/health"

[env]
# PORT = "8000"

# [secrets]
# file = ".env.prod"
''')
    print(f"Created {manifest}")


def main(argv: list[str] | None = None) -> int:
    parser = build_parser(); items = list(sys.argv[1:] if argv is None else argv)
    commands = {"init", "deploy", "status", "logs", "restart", "stop", "rollback", "tunnel", "tunnel-status", "tunnel-logs", "tunnel-stop", "-h", "--help"}
    if not items: items = ["deploy"]
    elif items[0] not in commands:
        if len(items) > 1 and items[1] in commands - {"init", "-h", "--help"}:
            items = [items[1], items[0], *items[2:]]
        else: items.insert(0, "deploy")
    args = parser.parse_args(items)
    try:
        if args.action == "init": init(Path(args.repo)); return 0
        runner = Runner(load_config(Path(args.repo)), getattr(args, "dry_run", False))
        if args.action == "deploy":
            if args.env: runner.deploy(Path(args.env))
            else: runner.deploy()
        elif args.action == "status": runner.status()
        elif args.action == "logs": runner.logs(("-n", str(args.lines)))
        elif args.action in {"restart", "stop"}: runner.control(args.action)
        elif args.action == "rollback": runner.rollback()
        elif args.action.startswith("tunnel"):
            from .tunnel import TunnelManager
            tunnel = TunnelManager(runner, runner.base_path())
            if args.action == "tunnel":
                base = tunnel.base
                runner._acquire_lock(base)
                try: tunnel.ensure()
                finally: runner._release_lock(base)
            elif args.action == "tunnel-status": tunnel.status()
            elif args.action == "tunnel-logs": tunnel.logs(args.lines)
            elif args.action == "tunnel-stop": tunnel.stop()
        return 0
    except (ConfigError, DeploymentError) as error:
        print(f"deployer: {error}", file=sys.stderr); return 1


if __name__ == "__main__": raise SystemExit(main())
