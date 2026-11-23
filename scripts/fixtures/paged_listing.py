"""Read a whole GitHub list that is paged by offset without skipping or repeating an item."""
import json
import subprocess
import time

LISTING_READS = 5
GH_PAGE_SIZE = 100


def read_stable(read_page, page_size, what, reads=LISTING_READS, max_pages=None):
    """List every item. The API pages by offset, so deleting an item between two
    page reads moves later items into a page that was already read and one of
    them is never returned. A published item shifts later items the other way and
    shows up twice. A listing that took more than one request is therefore
    accepted only when no id repeated and two consecutive reads name the same
    items. A read that saw a repeated id was disturbed and is discarded. A draft
    published during the final read stays a draft in the result: that is a valid
    earlier snapshot, not a skipped release. This
    catches every single deletion or publication; it does not prove a snapshot
    against several overlapping changes that cancel out between two reads.

    read_page(page) returns the list for that 1-based page; every item has an
    "id". A page shorter than page_size ends the listing. A listing that is
    still full after max_pages pages is refused rather than cut short."""
    previous = None
    for _ in range(reads):
        items, paged = read_once(read_page, page_size, max_pages)
        state = None if items is None else [item["id"] for item in items]
        if items is not None and (not paged or previous == state):
            return items
        previous = state
    raise RuntimeError(f"release API listing did not hold still across {reads} reads: {what}")


def read_once(read_page, page_size, max_pages=None):
    """Return (items, whether it took several requests); items is None when an id repeated."""
    items, seen, page = [], set(), 1
    while True:
        if max_pages is not None and page > max_pages:
            raise RuntimeError(f"release API listing is longer than {max_pages} pages")
        batch = read_page(page)
        ids = [item["id"] for item in batch]
        if len(set(ids)) != len(ids) or not seen.isdisjoint(ids):
            return None, page > 1
        items.extend(batch)
        seen.update(ids)
        if len(batch) < page_size:
            return items, page > 1
        page += 1


def gh_listing(endpoint, timeout=300):
    """Read endpoint (which has no query string yet) through the gh CLI. The
    timeout covers the whole listing, including repeated reads."""
    page_size = GH_PAGE_SIZE
    deadline = time.monotonic() + timeout

    def read_page(page):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise subprocess.TimeoutExpired(["gh", "api", endpoint], timeout)
        return json.loads(subprocess.check_output(
            ["gh", "api", f"{endpoint}?per_page={page_size}&page={page}"], timeout=remaining))
    return read_stable(read_page, page_size, endpoint)
