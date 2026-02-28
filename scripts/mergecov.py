#!/usr/bin/env python3
"""Merge Go coverage profiles, taking the maximum hit count per block.

The coverage gate has to span both test suites. Large parts of internal/postgres are only
reachable with a real database, so a unit-only measurement understates the project by
about twenty points and would push someone towards mocking the database to make a number
go up — which is the opposite of what this codebase does deliberately.

Both profiles are produced with the same -coverpkg, so they describe the same set of
blocks. A block exercised by either suite is covered; summing the counts would
double-count blocks both suites reach, so the maximum is taken.

Usage:
    mergecov.py unit.out integration.out merged.out
"""
import sys


def load(path):
    """Read a coverage profile into {(block spec, statement count): hit count}."""
    blocks = {}
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            if not line or line.startswith("mode:"):
                continue
            # file.go:startLine.startCol,endLine.endCol numStatements hitCount
            spec, hits = line.rsplit(" ", 1)
            spec, statements = spec.rsplit(" ", 1)
            key = (spec, statements)
            blocks[key] = max(blocks.get(key, 0), int(hits))
    return blocks


def main(argv):
    if len(argv) < 3:
        print(__doc__, file=sys.stderr)
        return 2

    *inputs, output = argv

    merged = {}
    for path in inputs:
        for key, hits in load(path).items():
            merged[key] = max(merged.get(key, 0), hits)

    with open(output, "w") as out:
        # "set" rather than "count": the merge only preserves whether a block was hit.
        out.write("mode: set\n")
        for (spec, statements), hits in sorted(merged.items()):
            out.write(f"{spec} {statements} {hits}\n")

    print(f"merged {len(inputs)} profiles, {len(merged)} blocks -> {output}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
