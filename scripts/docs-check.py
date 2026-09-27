#!/usr/bin/env python3
"""Check documents against .agents/rules/450-doc-lifecycle.md.

Scope: every docs/specs/<subject>/ that has a README.md,
and every docs/plans/*.md whose name starts with no date.

Fails when a document lacks its line-3 Status line or uses a value outside its class,
when a subject README disagrees with the directory (rows vs files, Status, Class),
or when a relative Markdown link resolves to no file.
Standard library only.
"""

import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SPECS = os.path.join(ROOT, "docs", "specs")
PLANS = os.path.join(ROOT, "docs", "plans")

VALUES = {
    "living": {"draft", "current"},
    "finite": {"draft", "active", "deferred"},
}
STATUS_RE = re.compile(r"^Status: (?P<value>[a-z-]+) \((?P<date>\d{4}-\d{2}-\d{2})\)(?: — .*)?$")
ROW_RE = re.compile(r"^\|\s*`?(?P<file>[^`|]+?)`?\s*\|\s*(?P<cls>\w+)\s*\|\s*(?P<status>[a-z-]+)\s*\|")
DATED_RE = re.compile(r"^\d{4}-\d{2}-\d{2}")
# Inline links and images: [text](dest "title"), ![alt](dest), with <dest> allowed.
LINK_RE = re.compile(r"!?\[[^\]]*\]\(\s*(?:<(?P<angled>[^>]*)>|(?P<target>[^)\s]+))(?:\s+\"[^\"]*\")?\s*\)")
# Reference-style definitions: [id]: dest "title"
REF_RE = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*(?:<(?P<angled>[^>]*)>|(?P<target>\S+))", re.M)
FENCE_RE = re.compile(r"^\s{0,3}(?P<marker>`{3,}|~{3,})")

errors = []


def fail(path, msg):
    errors.append(f"{os.path.relpath(path, ROOT)}: {msg}")


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def status_of(path):
    """Return the Status value of a document, or None after recording the error."""
    lines = read(path).split("\n")
    line3 = lines[2] if len(lines) >= 3 else ""
    m = STATUS_RE.match(line3)
    if not m:
        fail(path, "line 3 is not 'Status: <value> (<YYYY-MM-DD>) — <optional clause>'")
        return None

    return m.group("value")


def check_document(path, cls):
    value = status_of(path)
    if value is not None and value not in VALUES[cls]:
        fail(path, f"Status {value!r} is not allowed for a {cls} document ({', '.join(sorted(VALUES[cls]))})")


def strip_fences(text):
    """Drop fenced code blocks; a block closes only on a fence of the same character and at least the same length."""
    out = []
    opening = None
    for line in text.split("\n"):
        m = FENCE_RE.match(line)
        if opening is None:
            if m:
                opening = m.group("marker")
            else:
                out.append(line)
        elif m and m.group("marker")[0] == opening[0] and len(m.group("marker")) >= len(opening):
            opening = None

    return "\n".join(out)


def is_external(target):
    return bool(re.match(r"^[a-z][a-z0-9+.-]*:", target, re.I)) or target.startswith("//") or target.startswith("#")


def check_links(path):
    base = os.path.dirname(path)
    text = strip_fences(read(path))
    matches = list(LINK_RE.finditer(text)) + list(REF_RE.finditer(text))
    for m in matches:
        target = m.group("angled") if m.group("angled") is not None else m.group("target")
        if is_external(target):
            continue

        rel = target.split("#", 1)[0]
        if not rel or os.path.exists(os.path.normpath(os.path.join(base, rel))):
            continue

        fail(path, f"relative link {target!r} resolves to no file")


def md_files(directory):
    out = []
    for dirpath, _, names in os.walk(directory):
        for n in sorted(names):
            if n.endswith(".md"):
                out.append(os.path.join(dirpath, n))

    return sorted(out)


def check_subject(subject_dir):
    readme = os.path.join(subject_dir, "README.md")
    rows = {}
    for line in read(readme).split("\n"):
        m = ROW_RE.match(line)
        if m and m.group("cls") in VALUES:
            name = m.group("file").strip()
            if name in rows:
                fail(readme, f"row `{name}` appears more than once")
            rows[name] = (m.group("cls"), m.group("status"))

    files = {os.path.relpath(p, subject_dir) for p in md_files(subject_dir)} - {"README.md"}
    for f in sorted(files - rows.keys()):
        fail(readme, f"file `{f}` has no row")
    for f in sorted(rows.keys() - files):
        fail(readme, f"row `{f}` names no file")

    for f in sorted(files & rows.keys()):
        cls, row_status = rows[f]
        path = os.path.join(subject_dir, f)
        if row_status not in VALUES[cls]:
            fail(readme, f"row `{f}`: Status {row_status!r} is not allowed for class {cls!r}")

        value = status_of(path)
        if value is not None and value != row_status:
            fail(readme, f"row `{f}` says {row_status!r} but the file says {value!r}")

    for path in md_files(subject_dir):
        check_links(path)


def main():
    checked = 0
    if os.path.isdir(SPECS):
        for name in sorted(os.listdir(SPECS)):
            subject_dir = os.path.join(SPECS, name)
            if os.path.isfile(os.path.join(subject_dir, "README.md")):
                check_subject(subject_dir)
                checked += 1

    if os.path.isdir(PLANS):
        for name in sorted(os.listdir(PLANS)):
            path = os.path.join(PLANS, name)
            if os.path.isfile(path) and name.endswith(".md") and not DATED_RE.match(name):
                check_document(path, "finite")
                check_links(path)
                checked += 1

    for e in errors:
        print(e, file=sys.stderr)

    if errors:
        print(f"docs-check: {len(errors)} error(s)", file=sys.stderr)
        return 1

    print(f"docs-check: ok ({checked} subject(s)/plan(s) checked)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
