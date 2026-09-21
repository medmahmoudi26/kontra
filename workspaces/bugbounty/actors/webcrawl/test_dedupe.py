"""dedupe.py is a verbatim copy of crawl4ai's fingerprinting. These are the golden fixtures that
say so — if the two ever drift, this fails here rather than a crawl quietly deduping differently
in each actor.

The numbers are MEASURED, not chosen. They are the reason `simhash_distance` defaults to 3 and
the reason it must not be raised much: the gap between "same route, different object id" (0) and
"different METHOD" (11) is the whole safety margin.
"""
import pytest

from dedupe import (canonical_string, hamming_distance, mask_object_ids, near_duplicate,
                    normalize_host, request_fingerprint, simhash64, skeleton_string)

BASE = "https://a.example.com/product/1001"


def test_canonical_string_is_the_v1_layout():
    # Param NAMES sorted, VALUES dropped; header NAMES lowercased and sorted; host port-stripped.
    got = canonical_string("get", "https://A.Example.com:443/p?b=2&a=1", ["X-Tok", "Accept"])
    assert got == "GET a.example.com /p?a&b ['accept', 'x-tok'] "


def test_empty_text_is_pinned_to_zero():
    # The library fingerprints "" to a non-zero constant, which would make every empty shape a
    # near-duplicate of every other and suppress them as a group.
    assert simhash64("") == 0


@pytest.mark.parametrize("seg,masked", [
    ("/product/1001", "/product/#"),      # a run of digits
    ("/u/9f3a2b1c", "/u/#"),              # hex blob containing a digit
    ("/decade", "/decade"),               # hex-looking, no digit — a real route
    ("/facade", "/facade"),
    ("/story-77", "/story-77"),           # partially-numeric slug: NOT an id
    ("/v2/x", "/v2/x"),                   # a version: NOT an id
])
def test_only_unambiguous_object_ids_are_masked(seg, masked):
    assert mask_object_ids(seg) == masked


def test_the_measured_distances_that_set_the_radius():
    same_shape = hamming_distance(request_fingerprint("GET", BASE),
                                  request_fingerprint("GET", "https://a.example.com/product/1002"))
    method = hamming_distance(request_fingerprint("GET", BASE),
                              request_fingerprint("POST", BASE))
    host = hamming_distance(request_fingerprint("GET", BASE),
                            request_fingerprint("GET", "https://b.example.com/product/1001"))
    route = hamming_distance(request_fingerprint("GET", BASE),
                             request_fingerprint("GET", "https://a.example.com/settings/email"))

    assert same_shape == 0, "masking must collapse a generated path space at ANY radius"
    # Everything structural has to stay outside the default radius of 3, with room to spare.
    for label, bits in (("method", method), ("host", host), ("route", route)):
        assert bits > 8, f"a different {label} is only {bits} bits away — the radius is unsafe"


def test_near_duplicate_uses_the_radius():
    a = request_fingerprint("GET", BASE)
    b = request_fingerprint("GET", "https://a.example.com/product/9999")
    c = request_fingerprint("GET", "https://a.example.com/settings/email")
    assert near_duplicate(b, [a], 3) is True
    assert near_duplicate(c, [a], 3) is False


def test_normalize_host_strips_port_and_case():
    assert normalize_host("A.Example.COM:8443") == "a.example.com"
    assert normalize_host("") == ""


def test_skeleton_differs_from_canonical_only_by_masking():
    assert skeleton_string("GET", BASE) == canonical_string("GET", BASE).replace("1001", "#")
