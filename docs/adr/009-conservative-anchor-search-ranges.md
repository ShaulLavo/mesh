# 009. Anchor pages advertise conservative search ranges

Status: accepted
Date: 2026-11-16

The first signed anchor proves a wider direct window than the historical seed
ladder. Its signed lower bound can therefore precede the last seed's bound.
Copying that value into the next page breaks the monotone order required by
the reader's binary search.

An anchor page's `anchorsFrom` is a conservative search bound. The publisher
takes the maximum of the proved lower bound and the preceding index bound.
Signed metadata retains its exact direct range. The reader authenticates that
metadata and requires the indexed range to be contained within its signed
range, with the same anchor ordinal and publication time. Versions, times and
index bounds still increase, and existing page prefixes stay unchanged.

Narrowing a range only removes indexed shortcuts. It retains the edge from the
preceding anchor whenever that edge was proved: the preceding index bound is
at most the preceding ordinal. Exact manifest, receipt, signature and automatic
rollback checks still authorize each hop. Binary search and per-pass discovery
limits are unchanged. Direct admission to the head uses its signed window.

Removing range-order validation would invalidate binary search. Changing old
seed entries or signed metadata would alter immutable history. Neither is
needed to represent the narrower search range.
