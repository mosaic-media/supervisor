#!/usr/bin/env python3
# VENDORED from architecture/scripts/adr_index.py — do not edit here.
# The source of truth is the architecture repository.
"""Generate a repository's decision-record index.

Writes ``docs/adr/README.md``: one row per record, plus the records held in
other repositories that this one's records cite.

The index exists because dispersing the corpus solves *where* a record lives
without solving *how much* a reader must hold. A repository receiving eighty
records has replaced a remote corpus nobody reads with a local one nobody
reads, and the second failure looks exactly like the first. The index is the
bounded thing an agent reads instead.

It is generated, never hand-written, for the reason the corpus it indexes
exists: a hand-kept second list of what a directory contains is a copy, and a
copy drifts. ``--check`` fails when the committed file is not what this script
would write, which is what makes that guarantee mechanical rather than a habit.

Usage:
    python scripts/adr_index.py                    # write docs/adr/README.md
    python scripts/adr_index.py --check            # fail if it would change
    python scripts/adr_index.py --repo platform --adr-dir docs/adr
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass
from pathlib import Path

# A citation of a record held in another repository, as it is written: a label
# and the URL it links to. Deliberately unlike `ADR 0012`, so the old spelling
# can be found and refused rather than silently resolving to a different record.
# The URL is captured rather than reconstructed — this file cannot know another
# repository's filenames, and a link built from a guess is not a link.
FOREIGN = re.compile(r"\[([a-z][a-z0-9-]*)#(\d+)\]\((https?://[^)]+)\)")

GITHUB = "https://github.com/mosaic-media"


@dataclass(frozen=True)
class Record:
    number: int
    title: str
    status: str
    filename: str


# The first word of a Status says whether the record is agreed, and nothing else
# (architecture#5). Built-ness is the other axis and is said separately in prose,
# because a record is commonly half built and no keyword carries "the producing
# half exists and the consuming half does not". A "Built" status conflated the
# two: eleven records carried it, and every one of them was also agreed.
AGREEMENT_WORDS = frozenset({"Proposed", "Accepted", "Superseded"})


def status_summary(text: str, limit: int = 140) -> str:
    """The Status line's first sentence, as plain text.

    A Status line is prose and several run to a paragraph. The index carries
    enough to tell built from proposed from superseded; the record itself
    carries the rest, and duplicating more of it here would be a second copy of
    exactly the kind this repository keeps deleting.
    """
    # Links collapse to their text — except a citation of another repository's
    # record, which keeps its link. That is the one link in a Status line a
    # reader of the index actually needs, and stripping it leaves a label
    # pointing nowhere.
    text = re.sub(
        r"\[([^\]]+)\]\(([^)]+)\)",
        lambda m: m.group(0) if re.fullmatch(r"[a-z][a-z0-9-]*#\d+", m.group(1)) else m.group(1),
        text,
    )
    # Emphasis and code markers are stripped rather than preserved: this is one
    # cell of a table, and truncating prose mid-`**bold**` or mid-`code` emits a
    # dangling marker that corrupts the row it sits in.
    text = text.replace("`", "").replace("*", "")
    text = re.sub(r"\|", "\\|", text)  # a pipe would end the cell early
    text = re.sub(r"\s+", " ", text).strip()
    if len(text) <= limit:
        return text
    # Whole sentences up to the limit. One sentence is often too few — a Status
    # reading "Proposed. Nothing here is built." says far more than "Proposed."
    # A sentence ends at a period or semicolon *followed by whitespace*. Without
    # that condition the period in `v1.ArtworkCandidate` reads as a full stop
    # and the summary ends on a fragment.
    bounds = [m.end() for m in re.finditer(r"[.;](?=\s)", text)] + [len(text)]
    kept, start = "", 0
    for end in bounds:
        piece = text[start:end]
        if kept and len(kept) + len(piece) > limit:
            break
        kept += piece
        start = end
    kept = kept.strip()
    if not kept:
        kept = text[:limit].rsplit(" ", 1)[0]
    return kept if kept.rstrip().endswith((".", ";")) else kept + "…"


def read_record(path: Path) -> Record:
    lines = path.read_text().split("\n")
    if not lines[0].startswith("# "):
        raise SystemExit(f"{path}: first line is not a heading")
    if re.match(r"^# \d+\.", lines[0]):
        raise SystemExit(f"{path}: heading carries a number; it belongs in the filename only")
    title = lines[0][2:].strip()

    status: list[str] = []
    for line in lines[1:]:
        if line.startswith("**Date:**"):
            break
        if line.startswith("**Status:**"):
            status.append(line[len("**Status:**") :])
        elif status:
            status.append(line)
    if not status:
        raise SystemExit(f"{path}: no **Status:** line")
    first = status[0].strip().split()[0].rstrip(".,;:") if status[0].strip() else ""
    if first not in AGREEMENT_WORDS:
        raise SystemExit(
            f"{path}: Status starts with {first!r}; it must start with one of "
            f"{', '.join(sorted(AGREEMENT_WORDS))} (architecture#5). Whether a "
            f"record is built is said separately in the same line, in prose."
        )

    m = re.match(r"^(\d+)-", path.name)
    if not m:
        raise SystemExit(f"{path}: filename does not start with a number")
    return Record(int(m.group(1)), title, status_summary(" ".join(status)), path.name)


def foreign_citations(records: list[Path], repo: str) -> dict[str, dict[int, str]]:
    """Records in *other* repositories that this repository's records cite.

    Maps repository -> {number: url}, with the URL taken from the citation
    itself so the index links to the record rather than to its directory.
    """
    out: dict[str, dict[int, str]] = {}
    for path in records:
        for other, number, url in FOREIGN.findall(path.read_text()):
            if other != repo:
                out.setdefault(other, {})[int(number)] = url
    return out


def render(repo: str, records: list[Record], foreign: dict[str, dict[int, str]]) -> str:
    out = [
        "# Decision records",
        "",
        f"Every architectural decision `{repo}` owns, newest last. Generated by "
        "`scripts/adr_index.py` — do not edit by hand.",
        "",
        "A record is cited from another repository as "
        f"`{repo}#N`, written as a link to the file. The number lives in the "
        "filename and in this table; it is deliberately absent from the "
        "record's own heading, so a record's anchor survives being renumbered.",
        "",
        "| # | Record | Status |",
        "|---|---|---|",
    ]
    for r in records:
        out.append(f"| {r.number} | [{r.title}]({r.filename}) | {r.status} |")
    out.append("")

    if foreign:
        out += [
            "## Records this repository depends on",
            "",
            "Decisions held elsewhere that these records cite. They bind work here "
            "and are not repeated — follow the link.",
            "",
        ]
        for other in sorted(foreign):
            out.append(f"**`{other}`**")
            out.append("")
            for n in sorted(foreign[other]):
                out.append(f"- [{other}#{n}]({foreign[other][n]})")
            out.append("")
    return "\n".join(out)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--adr-dir", type=Path, default=Path("docs/adr"))
    ap.add_argument("--repo", default=None, help="repository name (default: cwd name)")
    ap.add_argument("--check", action="store_true", help="fail if the committed index is stale")
    args = ap.parse_args()

    repo = args.repo or Path.cwd().name
    paths = sorted(p for p in args.adr_dir.glob("*.md") if p.name != "README.md")
    if not paths:
        raise SystemExit(f"No records in {args.adr_dir}")

    records = sorted((read_record(p) for p in paths), key=lambda r: r.number)
    seen: dict[int, str] = {}
    for r in records:
        if r.number in seen:
            raise SystemExit(f"Duplicate record number {r.number}: {seen[r.number]} and {r.filename}")
        seen[r.number] = r.filename

    text = render(repo, records, foreign_citations(paths, repo))
    target = args.adr_dir / "README.md"

    if args.check:
        current = target.read_text() if target.exists() else ""
        if current != text:
            raise SystemExit(f"{target} is stale — run scripts/adr_index.py")
        print(f"{target}: up to date ({len(records)} records)")
        return

    target.write_text(text)
    print(f"{target}: {len(records)} records")


if __name__ == "__main__":
    main()
