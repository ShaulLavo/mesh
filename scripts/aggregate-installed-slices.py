#!/usr/bin/env python3
"""Recheck complete prepared installed coverage from one producer and its slices."""
import argparse
import json
from pathlib import Path

from fixtures import installed_slices


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("bundle", type=Path)
    parser.add_argument("preparation_output", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("consumers", nargs="+", type=Path)
    args = parser.parse_args()
    result = installed_slices.aggregate(args.bundle, args.consumers, args.preparation_output)
    args.output.write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
