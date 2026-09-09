import pytest

from bug import fib


@pytest.mark.parametrize(
    "n,expected",
    [(0, 0), (1, 1), (2, 1), (3, 2), (4, 3), (5, 5), (10, 55), (15, 610)],
)
def test_fib(n, expected):
    assert fib(n) == expected
