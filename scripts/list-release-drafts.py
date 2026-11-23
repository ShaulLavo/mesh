#!/usr/bin/env python3
"""Print each draft release as "tag<TAB>target" for the release reservation."""
import os

from fixtures import paged_listing


def main():
    for release in paged_listing.gh_listing(f'repos/{os.environ["GITHUB_REPOSITORY"]}/releases'):
        if release["draft"] is True:
            print(f'{release["tag_name"]}\t{release["target_commitish"]}')


if __name__ == "__main__":
    main()
