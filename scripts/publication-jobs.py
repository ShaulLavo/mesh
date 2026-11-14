#!/usr/bin/env python3
"""Select complete native publication jobs and aggregate their exact evidence."""
import argparse
import json
from pathlib import Path
import re

from fixtures import release_worker_completion as completion


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("inventory", "aggregate"))
    parser.add_argument("release_directory", type=Path, help="exact signed draft JSON assets and their complete worker evidence closure")
    parser.add_argument("platform", choices=("linux/amd64", "linux/arm64", "darwin/arm64"))
    parser.add_argument("harness", help="immutable native installed harness source SHA")
    parser.add_argument("--inputs", type=Path, help="downloaded publication-task job directories")
    parser.add_argument("--output", type=Path, help="new aggregate platform directory")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}", args.harness):
        parser.error("harness must be an exact source SHA")
    if args.command == "inventory":
        if args.inputs or args.output:
            parser.error("inventory writes JSON to stdout and takes no aggregation paths")
        print(json.dumps(completion.task_inventory(args.release_directory, args.platform, args.harness), sort_keys=True))
        return
    if not args.inputs or not args.output:
        parser.error("aggregation requires --inputs and --output")
    completion.aggregate_tasks(args.release_directory, args.platform, args.harness, args.inputs, args.output)


if __name__ == "__main__":
    main()
