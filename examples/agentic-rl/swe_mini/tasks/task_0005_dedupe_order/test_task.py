from bug import dedupe


def test_no_dupes():
    assert dedupe([1, 2, 3]) == [1, 2, 3]


def test_keeps_first_seen_order():
    assert dedupe([3, 1, 3, 2, 1]) == [3, 1, 2]


def test_strings():
    assert dedupe(["b", "a", "b", "c", "a"]) == ["b", "a", "c"]


def test_empty():
    assert dedupe([]) == []


def test_all_same():
    assert dedupe([7, 7, 7]) == [7]
