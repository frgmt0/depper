from __future__ import annotations

from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from deployer.dotenv import DotenvError, parse_dotenv, read_dotenv, resolve_secret_source, source_rsync_excludes


class DotenvTests(unittest.TestCase):
    def test_parses_comments_exports_quotes_crlf_and_multiline_without_expansion(self) -> None:
        values = parse_dotenv(
            "# heading\r\n"
            "export PORT = 8080 # deployment port\r\n"
            "HASH=value#is-literal\r\n"
            "SINGLE='literal $() `backtick` ${VARIABLE} # hash'\r\n"
            'DOUBLE="line\\nnext\\t\\\\slash\\\"quote"\r\n'
            'MULTILINE="first\nsecond"\r\n'
            "UNQUOTED=$() `backtick` ${VARIABLE}\r\n"
        )

        self.assertEqual(values, {
            "PORT": "8080",
            "HASH": "value#is-literal",
            "SINGLE": "literal $() `backtick` ${VARIABLE} # hash",
            "DOUBLE": "line\nnext\t\\slash\"quote",
            "MULTILINE": "first\nsecond",
            "UNQUOTED": "$() `backtick` ${VARIABLE}",
        })

    def test_malformed_dotenv_errors_are_content_safe(self) -> None:
        secret = "synthetic-should-never-appear"
        for text in (f"TOKEN={secret}\nnot an assignment\n", f"TOKEN=\"{secret}"):
            with self.subTest(text=text[:5]):
                with self.assertRaises(DotenvError) as caught:
                    parse_dotenv(text)
                self.assertRegex(str(caught.exception), r"line [12]")
                self.assertNotIn(secret, str(caught.exception))

    def test_trailing_comments_at_eof_preserve_assignment_and_empty_unquoted_value(self) -> None:
        values = parse_dotenv('QUOTED="value" # final comment\nEMPTY= # final comment')

        self.assertEqual(values, {"QUOTED": "value", "EMPTY": ""})

    def test_selected_source_is_regular_file_and_its_exact_rsync_paths_are_excluded(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory).resolve()
            nested = repo / "ops" / "prod[1]*?.env"
            nested.parent.mkdir()
            nested.write_text("TOKEN=synthetic\n", encoding="utf-8")
            alias = repo / "selected.env"
            alias.symlink_to(nested.relative_to(repo))

            source = resolve_secret_source(repo, alias)

        assert source is not None
        self.assertEqual(source.selected, alias.absolute())
        self.assertEqual(source.resolved, nested.resolve())
        self.assertEqual(source.rsync_excludes, ("/selected.env", "/ops/prod[[]1[]][*][?].env"))

    def test_missing_or_nonregular_sources_fail_before_reading(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            for selected in (repo / "missing.env", repo):
                with self.subTest(selected=selected.name):
                    with self.assertRaisesRegex(DotenvError, "(not readable|regular file)"):
                        resolve_secret_source(repo, selected)

    def test_unreadable_source_error_does_not_echo_contents(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            selected = repo / "private.env"
            selected.write_text("TOKEN=synthetic", encoding="utf-8")
            source = resolve_secret_source(repo, selected)
            assert source is not None
            with patch.object(Path, "open", side_effect=OSError("permission denied")):
                with self.assertRaisesRegex(DotenvError, "not readable") as caught:
                    read_dotenv(source)
        self.assertNotIn("synthetic", str(caught.exception))

    def test_escaped_exclude_does_not_transfer_a_special_character_secret_filename(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory).resolve() / "repo"
            destination = Path(directory).resolve() / "destination"
            repo.mkdir()
            (repo / "public.txt").write_text("public", encoding="utf-8")
            secret = repo / "credentials[prod]*?.txt"
            secret.write_text("synthetic-secret", encoding="utf-8")
            excludes = source_rsync_excludes(repo, secret)
            command = ["rsync", "-a"]
            for pattern in excludes:
                command.extend(("--exclude", pattern))
            command.extend((str(repo) + "/", str(destination) + "/"))

            subprocess.run(command, check=True)

            self.assertTrue((destination / "public.txt").is_file())
            self.assertFalse((destination / secret.name).exists())

    def test_external_parent_source_does_not_produce_an_invalid_rsync_parent_pattern(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = (Path(directory) / "repo").resolve()
            repo.mkdir()

            excludes = source_rsync_excludes(repo, repo / ".." / "external.env")

        self.assertEqual(excludes, ())

    def test_symlink_parent_traversal_reads_original_resolved_source_and_excludes_both_paths(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory).resolve() / "repo"
            real_child = repo / "real" / "child"
            real_child.mkdir(parents=True)
            (repo / "real" / "secret.env").write_text("SOURCE=resolved-target\n", encoding="utf-8")
            # This distinct file catches an incorrect abspath-before-resolve implementation.
            (repo / "secret.env").write_text("SOURCE=lexical-parent\n", encoding="utf-8")
            (repo / "alias").symlink_to(Path("real") / "child")

            source = resolve_secret_source(repo, repo / "alias" / ".." / "secret.env")
            assert source is not None
            values = read_dotenv(source)

        self.assertEqual(values, {"SOURCE": "resolved-target"})
        self.assertEqual(source.rsync_excludes, ("/secret.env", "/real/secret.env"))


if __name__ == "__main__":
    unittest.main()
