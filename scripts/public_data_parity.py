#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Compare a reference JSON answer with SwarmMemo public_data's answer for one dataset.

    scripts/public_data_parity.py DATASET REFERENCE.json SWARMMEMO.json [options]

REFERENCE is the answer of the adapter the dataset was ported from (a plain
dict). SWARMMEMO is a public_data item ({"dataset":..., "data":...}), a whole
service.call answer ({"result": item}), or a bulk answer with --index N.

Values are compared, not prose: numbers within a tolerance, strings and
booleans exactly. Keys only SwarmMemo has are listed as extras (a failure only
with --strict-extra). A key the reference lacks, or holds as null, empty or
false, matches a missing, null, empty or false SwarmMemo value. Provenance
text (notes, source descriptions, fences) is ignored; --ignore adds patterns.

Exit status: 0 when the values agree, 1 when they differ, 2 on bad input.
Standard library only.
"""
from __future__ import annotations

import argparse
import fnmatch
import json
import math
import sys
from typing import Any

# Path patterns (fnmatch; a path is ".key.sub", a list element is "#")
# ignored by default: prose and provenance that the port words differently.
DEFAULT_IGNORE = (
    "*.note", "*.notes", "*_note", "*.untrusted", "*.end_of_untrusted*", "*.retrieved_at", "*.today",
    "*.sources", "*_source", "*.source_url", "*.latest_fetched_at", "*.fred_series", "*.partial_errors",
    "*.warning", "*.rate_stale_reason", "*.calendar_staleness_warning", "*.calendar_source",
    "*.nowcasts#.measure", "*.source_errors#.error",
)

# Lists whose elements are matched by these keys rather than by position.
LIST_KEYS = {
    "nowcasts": ("name", "target_period"),
    "release_matched": ("release",),
    "bills": ("bill",),
    "days": ("date",),
    "observations": ("date",),
}


def empty(v: Any) -> bool:
    return v is None or v is False or (isinstance(v, (str, list, dict)) and len(v) == 0)


def unwrap_swarmmemo(doc: Any, index: int | None) -> tuple[Any, list[str]]:
    """The data of a public_data item, and warnings about it (stale, errors)."""
    warnings: list[str] = []
    if isinstance(doc, dict) and "result" in doc and isinstance(doc["result"], dict):
        doc = doc["result"]
    if isinstance(doc, dict) and "results" in doc:
        if index is None:
            raise ValueError("a bulk answer needs --index")
        doc = doc["results"][index]
    if not isinstance(doc, dict) or "data" not in doc or "dataset" not in doc:
        raise ValueError("not a public_data item (no dataset and data)")
    if doc.get("error"):
        raise ValueError(f"the item failed: {doc['error']}")
    if doc.get("stale"):
        warnings.append(f"the SwarmMemo value is stale (fetched_at {doc.get('fetched_at')})")
    return doc["data"], warnings


def normalise_reference(dataset: str, ref: Any) -> Any:
    """Reshape a reference answer where the port's schema differs in shape."""
    if dataset == "cb_policy_rates" and isinstance(ref, dict) and "banks" not in ref and isinstance(ref.get("bank"), str):
        code = ref["bank"].split(" ")[0]
        top = {k: ref[k] for k in ("today", "what", "calendar_staleness_warning") if k in ref}
        top["banks"] = {code: {k: v for k, v in ref.items() if k not in top}}
        return top
    return ref


def ignored(path: str, patterns: tuple[str, ...]) -> bool:
    return any(fnmatch.fnmatchcase(path, p) for p in patterns)


def numbers_equal(a: Any, b: Any, abs_tol: float, rel_tol: float) -> bool:
    return math.isclose(float(a), float(b), rel_tol=rel_tol, abs_tol=abs_tol)


def is_number(v: Any) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool)


class Comparison:
    def __init__(self, patterns: tuple[str, ...], abs_tol: float, rel_tol: float):
        self.patterns, self.abs_tol, self.rel_tol = patterns, abs_tol, rel_tol
        self.diffs: list[dict[str, Any]] = []
        self.extras: list[str] = []

    def diff(self, path: str, ref: Any, got: Any, why: str) -> None:
        self.diffs.append({"path": path or ".", "reference": ref, "swarmmemo": got, "why": why})

    def compare(self, ref: Any, got: Any, path: str = "") -> None:
        if ignored(path, self.patterns):
            return
        if empty(ref) and empty(got):
            return
        if is_number(ref) and is_number(got):
            if not numbers_equal(ref, got, self.abs_tol, self.rel_tol):
                self.diff(path, ref, got, "number")
            return
        if isinstance(ref, dict) and isinstance(got, dict):
            for k, v in ref.items():
                p = f"{path}.{k}"
                if k not in got:
                    if not empty(v) and not ignored(p, self.patterns):
                        self.diff(p, v, None, "missing in swarmmemo")
                    continue
                self.compare(v, got[k], p)
            for k in got:
                if k not in ref and not empty(got[k]) and not ignored(f"{path}.{k}", self.patterns):
                    self.extras.append(f"{path}.{k}")
            return
        if isinstance(ref, list) and isinstance(got, list):
            self.compare_lists(ref, got, path)
            return
        if type(ref) is not type(got) or ref != got:
            self.diff(path, ref, got, "value")

    def compare_lists(self, ref: list, got: list, path: str) -> None:
        name = path.rsplit(".", 1)[-1].rstrip("#")
        keys = LIST_KEYS.get(name)
        if keys and all(isinstance(x, dict) for x in ref + got):
            index = {tuple(x.get(k) for k in keys): x for x in got}
            for x in ref:
                k = tuple(x.get(k) for k in keys)
                if k not in index:
                    self.diff(f"{path}#{list(k)}", x, None, "element missing in swarmmemo")
                    continue
                self.compare(x, index[k], f"{path}#")
            return
        if len(ref) != len(got):
            self.diff(path, len(ref), len(got), "list length")
        for a, b in zip(ref, got):
            self.compare(a, b, f"{path}#")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("dataset")
    ap.add_argument("reference")
    ap.add_argument("swarmmemo")
    ap.add_argument("--index", type=int, help="the item of a bulk answer")
    ap.add_argument("--abs-tol", type=float, default=1e-9)
    ap.add_argument("--rel-tol", type=float, default=1e-9)
    ap.add_argument("--ignore", action="append", default=[], help="another path pattern to ignore (repeatable)")
    ap.add_argument("--no-default-ignore", action="store_true", help="compare prose and provenance too")
    ap.add_argument("--strict-extra", action="store_true", help="fail on keys only SwarmMemo has")
    ap.add_argument("--json", action="store_true", help="print a JSON report")
    args = ap.parse_args(argv)
    try:
        with open(args.reference, encoding="utf-8") as f:
            ref = json.load(f)
        with open(args.swarmmemo, encoding="utf-8") as f:
            got, warnings = unwrap_swarmmemo(json.load(f), args.index)
    except (OSError, ValueError, IndexError, KeyError) as e:
        print(f"public_data_parity: {e}", file=sys.stderr)
        return 2
    patterns = tuple(args.ignore) + (() if args.no_default_ignore else DEFAULT_IGNORE)
    c = Comparison(patterns, args.abs_tol, args.rel_tol)
    c.compare(normalise_reference(args.dataset, ref), got)
    failed = bool(c.diffs) or (args.strict_extra and bool(c.extras))
    if args.json:
        print(json.dumps({"dataset": args.dataset, "ok": not failed, "differences": c.diffs, "extras": c.extras, "warnings": warnings}, indent=1))
    else:
        for w in warnings:
            print(f"warning: {w}")
        for d in c.diffs:
            print(f"DIFF {d['path']} ({d['why']}): reference={json.dumps(d['reference'])} swarmmemo={json.dumps(d['swarmmemo'])}")
        if c.extras:
            print(f"extra keys in swarmmemo: {', '.join(c.extras)}")
        print(f"{args.dataset}: {'differs' if failed else 'agrees'} ({len(c.diffs)} differences)")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
