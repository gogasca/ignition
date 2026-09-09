import pytest

from bug import binary_search

ARR = [1, 3, 5, 7, 9, 11, 13]


@pytest.mark.parametrize("i,v", list(enumerate(ARR)))
def test_finds_each_element(i, v):
    assert binary_search(ARR, v) == i


def test_single_element_present():
    assert binary_search([42], 42) == 0


def test_absent_returns_minus_one():
    assert binary_search(ARR, 8) == -1


def test_empty():
    assert binary_search([], 1) == -1
