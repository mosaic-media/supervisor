#!/usr/bin/env python3
# VENDORED from architecture/scripts/adr_lint.py — do not edit here.
# The source of truth is the architecture repository.
"""Refuse a decision-record citation that cannot be resolved.

Three rules, and the first is the one this exists for:

1. **A citation must name its repository.** ``ADR 0012`` is refused; a record
   is cited as ``platform#12``. Once every repository numbers from 1, the bare
   form is ambiguous across thirteen of them — and it fails *open*. A stale
   ``ADR 0012`` in ``platform`` does not dangle after the split; it resolves,
   quietly, to that repository's own twelfth record, which is a different
   decision. No 404, no red test, and the citation still reads as valid. That
   is strictly worse than a dead link, and nothing in the fleet could detect it.

2. **A citation must resolve.** ``platform#12`` must name a record in
   ``platform``'s index. Checked in full when the sibling repositories are on
   disk (``--fleet``); otherwise only the citing repository's own records are
   resolved and foreign ones are checked for form alone. Say which ran — a
   check that quietly did less is how this corpus got here.

3. **A link's target must be the record its label names.** A label and a URL are
   two statements of one fact, and they drift: rewriting `ADR 0015` to
   `platform#11` while leaving the URL pointing at `0015-…` produces a citation
   that reads correctly and resolves to the old path. Nineteen of these existed
   before this rule did, all in doc comments — the one place a Markdown link
   appears inside a file that is not Markdown.

4. **A cross-repository citation in Markdown must be a link.** Prose can carry
   ``platform#12`` where no URL is possible — a Go comment, a test name — but a
   Markdown file can always link, and a reader in another repository has no
   other way to reach it.

Usage:
    python scripts/adr_lint.py --repo architecture --fleet /home/user
    python scripts/adr_lint.py --repo platform --count     # report, do not fail
"""

from __future__ import annotations

import argparse
import re
import sys
from collections import Counter, defaultdict
from pathlib import Path

REPOS = {
    "architecture", "platform", "supervisor", "sdk", "contracts", "web", "registry",
    "module-stremio-addons", "module-aiostreams", "module-cinemeta",
    "module-fanart-tv", "module-remote-playback", "module-tmdb",
}

# The old spelling, in every form the corpus actually uses.
UNQUALIFIED = re.compile(r"\bADR[\s-]?(\d{1,4})\b")
# The new one. The repository name is checked against REPOS, not the pattern,
# so `issue#12` or a CSS id never reads as a citation.
QUALIFIED = re.compile(r"\b([a-z][a-z0-9-]*)#(\d+)\b")
MD_LINK = re.compile(r"\[([^\]]*)\]\(([^)]+)\)")
# A qualified citation carrying a target, in either the inline Markdown form or
# Go's link-reference form. Both appear in doc comments.
QUALIFIED_LINK = re.compile(r"\[([a-z][a-z0-9-]*)#(\d+)\](?:\(|\: )([^)\s]+)")

SKIP_DIRS = {".git", "node_modules", "vendor", "site", "dist", "build", "__pycache__", ".venv"}
SKIP_NAMES = {"package-lock.json", "go.sum"}
TEXT_SUFFIXES = {
    ".md", ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".py", ".sh", ".yml", ".yaml",
    ".json", ".proto", ".toml", ".txt", ".sql", ".css", ".html", "",
}


def text_files(root: Path):
    for path in sorted(root.rglob("*")):
        if not path.is_file() or path.name in SKIP_NAMES:
            continue
        if SKIP_DIRS & set(path.relative_to(root).parts):
            continue
        if path.suffix.lower() not in TEXT_SUFFIXES:
            continue
        try:
            yield path, path.read_text()
        except (UnicodeDecodeError, OSError):
            continue


def index_filenames(repo_root: Path) -> dict[int, str]:
    """Record number -> filename, for checking that a link points where it says."""
    d = repo_root / "docs" / "adr"
    if not d.is_dir():
        return {}
    return {int(p.name[:4]): p.name for p in d.glob("*.md") if p.name != "README.md"}


def index_numbers(repo_root: Path) -> set[int] | None:
    """Record numbers a repository owns, read from its generated index."""
    index = repo_root / "docs" / "adr" / "README.md"
    if not index.exists():
        return None
    return {int(m.group(1)) for m in re.finditer(r"^\|\s*(\d+)\s*\|", index.read_text(), re.M)}


def linked_spans(text: str) -> list[tuple[int, int]]:
    """Character ranges of Markdown link *text*, where a citation counts as linked."""
    return [(m.start(1), m.end(1)) for m in MD_LINK.finditer(text)]


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--repo", required=True, help="repository being linted")
    ap.add_argument("--root", type=Path, default=Path("."), help="its working tree")
    ap.add_argument("--fleet", type=Path, default=None, help="directory holding the sibling repositories")
    ap.add_argument("--count", action="store_true", help="report and exit 0 — for establishing a baseline")
    ap.add_argument(
        "--max-unqualified", type=int, default=0,
        help="ratchet: fail if more than N unqualified citations remain. The migration "
             "cannot rewrite 4,618 sites in one commit, and a check that merely reports "
             "until then is a check nobody turns on. Set it to today's count and lower it "
             "as records move; it can only go down, so progress cannot silently reverse.",
    )
    ap.add_argument("--show", type=int, default=8, help="examples to print per rule")
    ap.add_argument(
        "--exclude", action="append", default=[],
        help="glob of paths to skip, repeatable. Migration scaffolding belongs here: a "
             "document *about* the citation forms quotes both of them, and quoting is "
             "not citing.",
    )
    args = ap.parse_args()

    own = index_numbers(args.root)
    fleet: dict[str, set[int]] = {}
    if args.fleet:
        for name in REPOS:
            if (nums := index_numbers(args.fleet / name)) is not None:
                fleet[name] = nums
    if own is not None:
        fleet[args.repo] = own

    names: dict[str, dict[int, str]] = {}
    if args.fleet:
        for name in REPOS:
            names[name] = index_filenames(args.fleet / name)
    names.setdefault(args.repo, index_filenames(args.root))

    unqualified: list[str] = []
    dangling: list[str] = []
    unlinked: list[str] = []
    mistargeted: list[str] = []
    per_number: Counter[int] = Counter()
    per_kind: Counter[str] = Counter()

    for path, text in text_files(args.root):
        rel = path.relative_to(args.root)
        if any(rel.match(pattern) for pattern in args.exclude):
            continue
        spans = linked_spans(text) if path.suffix == ".md" else []
        line_at = [0]
        for ch in text:
            line_at.append(line_at[-1] + (ch == "\n"))

        for m in UNQUALIFIED.finditer(text):
            unqualified.append(f"{rel}:{line_at[m.start()] + 1}: {m.group(0)}")
            per_number[int(m.group(1))] += 1
            per_kind[path.suffix or "(none)"] += 1

        for m in QUALIFIED_LINK.finditer(text):
            repo, number, url = m.group(1), int(m.group(2)), m.group(3)
            want = names.get(repo, {}).get(number)
            if want and not url.endswith(want):
                mistargeted.append(
                    f"{rel}:{line_at[m.start()] + 1}: {repo}#{number} links to "
                    f"{url.split('/')[-1]} — that record is {want}"
                )

        for m in QUALIFIED.finditer(text):
            repo, number = m.group(1), int(m.group(2))
            if repo not in REPOS:
                continue
            known = fleet.get(repo)
            if known is not None and number not in known:
                dangling.append(f"{rel}:{line_at[m.start()] + 1}: {m.group(0)} — no such record in {repo}")
            if path.suffix == ".md" and repo != args.repo:
                if not any(a <= m.start() and m.end() <= b for a, b in spans):
                    unlinked.append(f"{rel}:{line_at[m.start()] + 1}: {m.group(0)} is not a link")

    resolved = sorted(fleet) if fleet else []
    print(f"repo: {args.repo}")
    print(f"resolving against: {', '.join(resolved) if resolved else 'nothing (no index found)'}")
    if args.fleet and len(fleet) < len(REPOS):
        missing = sorted(REPOS - set(fleet))
        print(f"  no index yet in: {', '.join(missing)} — foreign citations to these are unchecked")
    print()
    print(f"unqualified citations : {len(unqualified)}")
    print(f"dangling repo#N       : {len(dangling)}")
    print(f"unlinked cross-repo   : {len(unlinked)}")
    print(f"link target mismatch  : {len(mistargeted)}")

    if unqualified and args.count:
        print(f"\ndistinct records cited unqualified: {len(per_number)}")
        print("by file type: " + ", ".join(f"{k} {v}" for k, v in per_kind.most_common(6)))
    for label, items in (("unqualified", unqualified), ("dangling", dangling),
                         ("unlinked", unlinked), ("mistargeted", mistargeted)):
        for line in items[: args.show]:
            print(f"  [{label}] {line}")
        if len(items) > args.show:
            print(f"  [{label}] … and {len(items) - args.show} more")

    if args.count:
        sys.exit(0)

    over = len(unqualified) > args.max_unqualified
    if over:
        print(f"\nFAIL: {len(unqualified)} unqualified citations, ceiling is {args.max_unqualified}")
    if len(unqualified) < args.max_unqualified:
        print(
            f"\nThe ceiling is {args.max_unqualified} and only {len(unqualified)} remain — "
            "lower it in the gate, or it stops ratcheting."
        )
    sys.exit(1 if (over or dangling or unlinked or mistargeted) else 0)


if __name__ == "__main__":
    main()
