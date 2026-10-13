"""A deliberately small, non-shell dotenv reader for deployment secrets."""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import os
import re
import stat


class DotenvError(ValueError):
    """A safe dotenv error: messages name a line but never echo source content."""


_KEY = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
_LIMIT = 1024 * 1024


@dataclass(frozen=True)
class SecretSource:
    selected: Path
    resolved: Path
    rsync_excludes: tuple[str, ...]


def source_rsync_excludes(repo: Path, selected: Path | None) -> tuple[str, ...]:
    """Exclude lexical and resolved in-repository names without requiring the file to exist."""
    if selected is None:
        return ()
    expanded = selected.expanduser()
    traversal = expanded if expanded.is_absolute() else Path.cwd() / expanded
    # Keep a normalized lexical spelling for rsync, but resolve the original traversal separately:
    # normalizing `symlink/../x` first can identify a different file than the kernel would open.
    lexical = Path(os.path.abspath(expanded))
    candidates = [lexical]
    try:
        candidates.append(traversal.resolve(strict=True))
    except OSError:
        pass
    excludes: list[str] = []
    repo_resolved = repo.resolve()
    for path in candidates:
        try:
            relative = path.relative_to(repo_resolved)
        except ValueError:
            continue
        # Anchor and escape rsync's pattern metacharacters so a secrets filename never broadens a sync.
        escaped = "".join({"\\": "\\\\", "[": "[[]", "]": "[]]", "*": "[*]", "?": "[?]"}.get(char, char) for char in relative.as_posix())
        excludes.append("/" + escaped)
    return tuple(dict.fromkeys(excludes))


def resolve_secret_source(repo: Path, selected: Path | None) -> SecretSource | None:
    """Validate a selected dotenv source without reading its contents."""
    if selected is None:
        return None
    expanded = selected.expanduser()
    traversal = expanded if expanded.is_absolute() else Path.cwd() / expanded
    lexical = Path(os.path.abspath(expanded))
    try:
        resolved = traversal.resolve(strict=True)
        details = resolved.stat()
    except OSError as exc:
        raise DotenvError("Selected dotenv file is not readable") from exc
    if not stat.S_ISREG(details.st_mode):
        raise DotenvError("Selected dotenv source must be a regular file")
    if details.st_size > _LIMIT:
        raise DotenvError("Selected dotenv source is larger than the 1 MiB limit")
    return SecretSource(lexical, resolved, source_rsync_excludes(repo, selected))


def read_dotenv(source: SecretSource) -> dict[str, str]:
    try:
        with source.resolved.open("rb") as handle:
            raw = handle.read(_LIMIT + 1)
    except OSError as exc:
        raise DotenvError("Selected dotenv file is not readable") from exc
    if len(raw) > _LIMIT:
        raise DotenvError("Selected dotenv source is larger than the 1 MiB limit")
    try:
        text = raw.decode("utf-8-sig")
    except UnicodeDecodeError as exc:
        raise DotenvError("Selected dotenv file must be UTF-8") from exc
    if "\x00" in text:
        nul_at = text.index("\x00")
        raise DotenvError(f"Invalid dotenv syntax at line {text.count(chr(10), 0, nul_at) + 1}")
    return parse_dotenv(text)


def parse_dotenv(text: str) -> dict[str, str]:
    """Parse assignments, comments, export, and quoted values without expansion."""
    values: dict[str, str] = {}
    pos, line, length = 0, 1, len(text)
    while pos < length:
        while pos < length and text[pos] in " \t\r": pos += 1
        if pos >= length: break
        if text[pos] == "\n": pos += 1; line += 1; continue
        if text[pos] == "#":
            end = text.find("\n", pos)
            if end < 0: break
            pos, line = end + 1, line + 1
            continue
        start_line = line
        export_end = pos + 6
        if text.startswith("export", pos) and export_end < length and text[export_end] in " \t":
            pos = export_end
            while pos < length and text[pos] in " \t": pos += 1
        matched = _KEY.match(text, pos)
        if matched is None: raise DotenvError(f"Invalid dotenv syntax at line {start_line}")
        key, pos = matched.group(), matched.end()
        while pos < length and text[pos] in " \t": pos += 1
        if pos >= length or text[pos] != "=": raise DotenvError(f"Invalid dotenv syntax at line {start_line}")
        pos += 1
        while pos < length and text[pos] in " \t": pos += 1
        value = ""
        if pos < length and text[pos] in "'\"":
            quote = text[pos]; pos += 1; pieces: list[str] = []
            while pos < length and text[pos] != quote:
                char = text[pos]
                if quote == '"' and char == "\\":
                    if pos + 1 >= length: raise DotenvError(f"Invalid dotenv syntax at line {start_line}")
                    nxt = text[pos + 1]
                    pieces.append({"n": "\n", "r": "\r", "t": "\t", "\\": "\\", '"': '"'}.get(nxt, "\\" + nxt))
                    pos += 2
                    continue
                pieces.append(char)
                if char == "\n": line += 1
                pos += 1
            if pos >= length: raise DotenvError(f"Unterminated quoted dotenv value at line {start_line}")
            pos += 1; value = "".join(pieces)
            while pos < length and text[pos] in " \t\r": pos += 1
            if pos < length and text[pos] == "#":
                end = text.find("\n", pos)
                if end < 0: pos = length
                else: pos, line = end + 1, line + 1
            elif pos < length and text[pos] == "\n": pos += 1; line += 1
            elif pos < length: raise DotenvError(f"Invalid dotenv syntax at line {start_line}")
        else:
            if pos < length and text[pos] == "#":
                value = ""
                end = text.find("\n", pos)
                if end < 0: pos = length
                else: pos, line = end + 1, line + 1
            else:
                end = text.find("\n", pos)
                if end < 0: end = length
                raw_value = text[pos:end].rstrip(" \t\r")
                comment = re.search(r"[ \t]#", raw_value)
                value = raw_value[:comment.start()].rstrip(" \t") if comment else raw_value
                pos = end
                if pos < length: pos += 1; line += 1
        values[key] = value
    return values
