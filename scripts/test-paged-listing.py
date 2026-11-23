#!/usr/bin/env python3
"""Offset-paged GitHub listings never skip or repeat an item, even when the list changes between pages."""
import contextlib
import importlib
import io
import json
import os
import unittest
import urllib.parse
from unittest import mock

from fixtures import draft_intake, paged_listing

uploader = importlib.import_module("upload-signed-draft")
drafts = importlib.import_module("list-release-drafts")


class PagedApi:
    """Serve items newest first by offset, like the GitHub list API, applying
    mutations[n](items) right after the n-th request has been answered, or
    mutations[(page, k)](items) right after page is requested for the k-th time."""

    def __init__(self, items, mutations=None):
        self.items, self.mutations, self.pages = items, mutations or {}, []

    def get(self, endpoint):
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(endpoint).query)
        size, page = int(query["per_page"][0]), int(query["page"][0])
        batch = json.loads(json.dumps(self.items[(page - 1) * size:page * size]))
        self.pages.append(page)
        for key in (len(self.pages), (page, self.pages.count(page))):
            if key in self.mutations:
                self.mutations[key](self.items)
        return batch

    def check_output(self, command, **_):
        return json.dumps(self.get(command[2])).encode()


def numbered(count, **fields):
    return [dict(id=number, **fields) for number in range(count, 0, -1)]


def delete_first(items):
    items.pop(0)


def insert_first(number):
    return lambda items: items.insert(0, {"id": number})


class StableReadTests(unittest.TestCase):
    def read(self, api, size=4, **options):
        return paged_listing.read_stable(
            lambda page: api.get(f"endpoint?per_page={size}&page={page}"), size, "endpoint", **options)

    def test_a_listing_that_fits_one_page_takes_one_request(self):
        api = PagedApi(numbered(3))
        self.assertEqual([item["id"] for item in self.read(api)], [3, 2, 1])
        self.assertEqual(api.pages, [1])

    def test_a_stable_multi_page_listing_is_read_twice(self):
        api = PagedApi(numbered(10))
        self.assertEqual([item["id"] for item in self.read(api)], list(range(10, 0, -1)))
        self.assertEqual(api.pages, [1, 2, 3, 1, 2, 3])

    def test_an_item_deleted_between_pages_is_not_skipped(self):
        api = PagedApi(numbered(10), {1: lambda items: items.pop(1)})
        # Item 9 is deleted after page 1; item 6 slides into page 1's range, so a single pass misses it.
        self.assertEqual([item["id"] for item in self.read(api)], [10, 8, 7, 6, 5, 4, 3, 2, 1])

    def test_an_item_published_between_pages_is_not_repeated(self):
        api = PagedApi(numbered(10), {1: insert_first(11)})
        self.assertEqual([item["id"] for item in self.read(api)], list(range(11, 0, -1)))

    def test_an_item_that_came_and_went_cannot_hide_another(self):
        # A published item makes page 2 repeat id 5 and is gone before page 3, which would hide item 3
        # from a read that only deduplicated. It happens in both of the first two reads.
        api = PagedApi(numbered(6), {(1, 1): insert_first(100), (2, 1): delete_first,
                                     (1, 2): insert_first(101), (2, 2): delete_first})
        self.assertEqual([item["id"] for item in self.read(api, 2)], [6, 5, 4, 3, 2, 1])

    def test_a_page_that_repeats_an_id_is_refused(self):
        api = mock.Mock(get=mock.Mock(return_value=[{"id": 1}, {"id": 1}]))
        with self.assertRaisesRegex(RuntimeError, "did not hold still"):
            self.read(api, 2)
        self.assertEqual(api.get.call_count, paged_listing.LISTING_READS)

    def test_a_listing_that_never_holds_still_is_refused(self):
        api = PagedApi(numbered(1000), {number: (lambda items: items.pop(1)) for number in range(1, 200, 2)})
        with self.assertRaisesRegex(RuntimeError, "did not hold still"):
            self.read(api, 30)

    def test_a_listing_longer_than_the_page_bound_is_refused(self):
        with self.assertRaisesRegex(RuntimeError, "longer than 3 pages"):
            self.read(PagedApi(numbered(20)), 2, max_pages=3)


class GhListingTests(unittest.TestCase):
    """The call sites read through gh with the real endpoints and a small page size."""

    def setUp(self):
        patcher = mock.patch.object(paged_listing, "GH_PAGE_SIZE", 4)
        patcher.start()
        self.addCleanup(patcher.stop)

    def read_with(self, api, function, *arguments):
        with mock.patch.object(paged_listing.subprocess, "check_output", side_effect=api.check_output):
            return function(*arguments)

    def test_finding_a_draft_survives_a_release_deleted_between_pages(self):
        releases = [dict(id=number, tag_name=f"v0.1.{number}") for number in range(10, 0, -1)]
        api = PagedApi(releases, {1: delete_first})
        # Release 6 is the first of page 2; deleting release 10 after page 1 moves it into page 1's range.
        found = self.read_with(api, draft_intake.find_release, "v0.1.6", "a" * 40, None)
        self.assertEqual(found["id"], 6)

    def test_live_asset_comparison_survives_an_asset_deleted_between_pages(self):
        fields = dict(size=1, state="uploaded", digest="sha256:" + "0" * 64)
        assets = [dict(id=number, name=f"asset{number}", **fields) for number in range(10, 0, -1)]
        recorded = dict(id=7, frozen_baselines=[], assets=json.loads(json.dumps(assets)))
        live = dict(id=7, draft=True, tag_name="v0.1.9", target_commitish="b" * 40, url="https://api.github.com/repos/ShaulLavo/mesh/releases/7",
                    html_url="https://github.com/ShaulLavo/mesh/releases/untagged-7")
        transient = dict(id=99, name="transient", **fields)
        api = PagedApi([transient] + assets, {1: delete_first})

        def get(command, **_):
            if command[2].endswith("/releases/7"):
                return json.dumps(live).encode()
            return api.check_output(command)
        with mock.patch.object(draft_intake, "verify", return_value=recorded), \
                mock.patch.object(paged_listing.subprocess, "check_output", side_effect=get):
            self.assertEqual(draft_intake.compare_live("directory", "v0.1.9", "b" * 40, 1)["id"], 7)

    def test_draft_listing_for_the_reservation_survives_a_release_deleted_between_pages(self):
        releases = [dict(id=number, draft=number % 2 == 0, tag_name=f"v0.1.{number}", target_commitish=f"{number:040d}")
                    for number in range(10, 0, -1)]
        api = PagedApi(releases, {1: lambda items: items.pop(1)})
        output = io.StringIO()
        with mock.patch.dict(os.environ, GITHUB_REPOSITORY="ShaulLavo/mesh"), contextlib.redirect_stdout(output), \
                mock.patch.object(paged_listing.subprocess, "check_output", side_effect=api.check_output):
            drafts.main()
        # Release 9 is deleted after page 1; draft 6 would otherwise be skipped.
        self.assertEqual(output.getvalue().splitlines(),
                         [f"v0.1.{number}\t{number:040d}" for number in (10, 8, 6, 4, 2)])


class UploadInventoryTests(unittest.TestCase):
    def publisher(self, api, candidate):
        publisher = uploader.Publisher.__new__(uploader.Publisher)
        publisher.api, publisher.release = api, "repos/ShaulLavo/mesh/releases/7"
        publisher.candidate = dict(assets=candidate, frozen_baselines=[])
        publisher.assets = {f"asset{number}": None for number in range(1, 201)}
        publisher.validate = lambda: None
        return publisher

    def test_inventory_survives_an_asset_deleted_between_pages(self):
        fields = dict(size=1, state="uploaded", digest="sha256:" + "0" * 64)
        assets = [dict(id=number, name=f"asset{number}", **fields) for number in range(150, 0, -1)]
        # Asset 51 is the first of page 2. A transient asset deleted after page 1 would move it into page 1's range.
        api = PagedApi([dict(id=999, name="transient", **fields)] + assets, {1: delete_first})
        api.request = lambda method, endpoint, *_: api.get(endpoint)
        inventory = self.publisher(api, [dict(assets[99], id=51, name="asset51")]).inventory()
        self.assertEqual(sorted(inventory, key=lambda name: int(name[5:])), [f"asset{number}" for number in range(1, 151)])


if __name__ == "__main__":
    unittest.main()
