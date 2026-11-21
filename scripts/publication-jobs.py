#!/usr/bin/env python3
"""Select complete native publication jobs and aggregate their exact evidence."""
import argparse
import json
import os
from pathlib import Path
import re

from fixtures import release_publication_platform as platforms
from fixtures import release_worker_completion as completion, release_worker_groups as groups


def legacy_arguments(parser):
    parser.add_argument("release_directory", type=Path, help="exact signed draft JSON assets and their complete worker evidence closure")
    parser.add_argument("platform", choices=platforms.PLATFORMS)
    parser.add_argument("harness", help="immutable native installed harness source SHA")
    parser.add_argument("--inputs", type=Path, help="downloaded publication-task job directories")
    parser.add_argument("--output", type=Path, help="new aggregate platform directory")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    legacy_arguments(commands.add_parser("inventory"))
    legacy_arguments(commands.add_parser("aggregate"))
    plan = commands.add_parser("reduced-plan", help="derive a plan from authenticated logical draft assets and source reader indexes")
    plan.add_argument("release_directory", type=Path)
    plan.add_argument("physical_draft", type=Path)
    plan.add_argument("harness", help="immutable native installed harness source SHA")
    plan.add_argument("reader_indexes", type=Path, help="parent of the three native SOURCE reader packet directories")
    plan.add_argument("output", type=Path, help="new plan file")
    record = commands.add_parser("record-platform", help="require the native installed, original-reader and cleanup outcomes")
    record.add_argument("output", type=Path, help="completed installed fixture output with its readers/ output")
    record.add_argument("platform", choices=platforms.PLATFORMS)
    record.add_argument("plan", type=Path)
    record.add_argument("plan_sha256")
    args = parser.parse_args()
    if hasattr(args, "harness") and not re.fullmatch(r"[0-9a-f]{40}", args.harness):
        parser.error("harness must be an exact source SHA")
    if args.command in ("reduced-plan", "record-platform"):
        from fixtures import installed_publication
        installed_publication.require_main()
    if args.command == "reduced-plan":
        value = platforms.prepare_plan(args.release_directory, args.physical_draft, args.harness,
                                       args.reader_indexes, int(os.environ["GITHUB_RUN_ID"]))
        raw = groups.encode(value)
        if len(raw) > groups.MAX_DOCUMENT:
            parser.error("publication plan exceeds the document bound")
        with args.output.open("xb") as stream:
            stream.write(raw)
        print(groups.digest(raw))
        return
    if args.command == "record-platform":
        value = platforms.load_plan(args.plan, args.plan_sha256)
        platforms.record_platform(args.output, value, args.plan_sha256, args.platform,
                                  int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"]))
        return
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
