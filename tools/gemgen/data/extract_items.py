#!/usr/bin/env python3
"""One-off: extract SEMI E5 Table 3 (Data Item Dictionary) into items.yaml.

Not part of the Go build. Regenerate items.yaml only when Table 3 changes.
Mirrors the format-code mapping in tools/gemgen/load.go (deriveBinding/deriveGoType);
Go's ValidateItems re-derives and cross-checks, so any drift here fails generation.
"""
import re
import sys

SRC = "/home/arlo/semi_standards/markdowns/e005-00-0813/e005-00-0813.md"
START, END = 400, 966  # Table 3 body rows (inclusive) in the local markdown

FORMAT_MAP = {
    "0": ["L"], "10": ["B"], "11": ["BOOLEAN"], "20": ["A"], "21": ["J"], "22": ["W"],
    "30": ["I8"], "31": ["I1"], "32": ["I2"], "34": ["I4"],
    "40": ["F8"], "44": ["F4"],
    "50": ["U8"], "51": ["U1"], "52": ["U2"], "54": ["U4"],
    "3()": ["I1", "I2", "I4", "I8"], "4()": ["F4", "F8"], "5()": ["U1", "U2", "U4", "U8"],
}
GOTYPE = {"A": "string", "J": "string", "W": "string", "B": "byte", "BOOLEAN": "bool",
          "I1": "int8", "I2": "int16", "I4": "int32", "I8": "int64",
          "U1": "uint8", "U2": "uint16", "U4": "uint32", "U8": "uint64",
          "F4": "float32", "F8": "float64"}

# Curated overrides for named Table 3 rows whose Format cell is blank or
# contains an unparseable/corrupted token in the local markdown. A named row
# with an empty/unparseable Format cell that is NOT listed here is a hard
# extraction failure (see main), so no named item is ever silently dropped.
# Each value is the format-token list the item should carry.
#
# RPMSOURLOC's Format cell is blank in the local copy, but its description is
# "The LocationID ... Conforms to OBJID" — identical semantics to its adjacent
# sibling RPMDESTLOC (e005-00-0813.md:787), which the same table gives explicit
# format 20 (ASCII). Both are LocationID strings, so RPMSOURLOC is A (fixed
# string). This is grounded in the sibling row, not guessed.
#
# PARAMVAL (e005-00-0813.md:660), PDEATTRIBUTEVALUE (:668), and PRPAUSEEVENT
# (:704) each have a corrupted list-format token ("1" or "00") that isn't a
# real E5 format code — Table 1 only defines list as format 0. Each row's own
# description confirms list semantics: PARAMVAL's "Values that are lists are
# restricted to lists of single items of the same format type", PDEATTRIBUTE-
# VALUE's Values cell literally says "00 used for list of strings", and
# PRPAUSEEVENT's description opens with "The list of event identifiers". All
# three corrupted tokens are corrected to "0" (list) alongside their other,
# correctly-parsed format tokens.
#
# SPR (:836) has no format code at all — its Format cell reads "Device
# Dependent" and its Values cell repeats "Device dependent" — a genuinely
# equipment-defined item with no fixed SECS-II representation. Given an empty
# formats list, so binding derives to open (never "exactly one known format",
# the fixed-binding precondition) with no fabricated format token.
#
# An override ALWAYS wins over the raw cell (main() checks OVERRIDES before
# attempting to parse the row's own Format text) -- only add an entry for a
# row that is genuinely blank/corrupted. If a future revision of the local E5
# copy fixes one of these rows' Format cell, the override here would keep
# silently masking the now-valid raw text; check whether an entry is still
# needed before re-running extraction against an updated source file.
OVERRIDES = {
    "RPMSOURLOC": ["20"],  # e005-00-0813.md:788, blank cell; inherits RPMDESTLOC's format 20 (A)
    "PARAMVAL": ["0", "10", "11", "20", "3()", "4()", "5()"],  # :660, "1" -> "0" (list)
    "PDEATTRIBUTEVALUE": ["0", "11", "20", "21", "51"],  # :668, "00" -> "0" (list)
    "PRPAUSEEVENT": ["0"],  # :704, "00" -> "0" (list)
    "SPR": [],  # :836, "Device Dependent" has no format code; open binding, no formats
}

def clean(cell):
    cell = cell.replace("<br>", " ")
    cell = re.sub(r"<[^>]+>", " ", cell)
    return re.sub(r"\s+", " ", cell).strip()

def expand_formats(cell):
    out = []
    for tok in re.split(r"[,\s]+", clean(cell)):
        if not tok:
            continue
        if tok not in FORMAT_MAP:
            sys.exit(f"unknown format token {tok!r}")
        for c in FORMAT_MAP[tok]:
            if c not in out:
                out.append(c)
    return out

def yaml_str(s):
    if s != s.strip() or re.search(r"""[:#\[\]{},&*!|>'"%@`]""", s):
        return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'
    return s

def main():
    rows, order = {}, []
    with open(SRC) as f:
        lines = f.readlines()
    for lineno, ln in enumerate(lines[START - 1:END], start=START):
        if not ln.lstrip().startswith("|"):
            continue
        cells = [c.strip() for c in ln.strip().strip("|").split("|")]
        if len(cells) < 3:
            continue
        name = re.sub(r"\s*\(cont\.\)$", "", clean(cells[0]))
        if name in ("", "Name") or not re.match(r"^[A-Za-z]", name):
            continue  # header, separator, or non-named continuation artifact
        if name in rows:
            continue  # first occurrence wins (merges page-break repeats)
        # A curated OVERRIDES entry always wins over the raw cell: some rows
        # (e.g. PARAMVAL) have a NON-empty Format cell that still contains a
        # corrupted/unparseable token, which would otherwise hard-fail inside
        # expand_formats() before the "blank cell" fallback below is ever
        # reached. Checking OVERRIDES first covers both the blank-cell case
        # (RPMSOURLOC) and the corrupted-token case uniformly.
        if name in OVERRIDES:
            formats = expand_formats(" ".join(OVERRIDES[name]))
        else:
            formats = expand_formats(cells[1])
            if not formats:
                # This is a NAMED row (passed the name filter, first occurrence) whose
                # Format cell is blank/unparseable. Never drop it silently: fail loudly
                # with the source line + name so a grounded OVERRIDES entry must be added.
                sys.exit(f"{SRC}:{lineno}: named item {name!r} has an empty or unparseable "
                         f"Format cell; add an OVERRIDES[{name!r}] entry (its format tokens, "
                         f"grounded in E5) — refusing to silently drop a named row")
        rows[name] = (formats, clean(cells[2]))
        order.append(name)
    for name in sorted(order):
        formats, desc = rows[name]
        fixed = len(formats) == 1 and formats[0] != "L"
        print(f"{name}:")
        print(f"  formats: [{', '.join(formats)}]")
        print(f"  binding: {'fixed' if fixed else 'open'}")
        if fixed:
            print(f"  goType: {GOTYPE[formats[0]]}")
        if desc:
            print(f"  description: {yaml_str(desc)}")
        print("  source: e5")

if __name__ == "__main__":
    main()
