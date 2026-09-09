from bug import flatten


def test_flat_unchanged():
    assert flatten([1, 2, 3]) == [1, 2, 3]


def test_one_level():
    assert flatten([1, [2, 3], 4]) == [1, 2, 3, 4]


def test_deep():
    assert flatten([1, [2, [3, [4, 5]]], 6]) == [1, 2, 3, 4, 5, 6]


def test_empty_sublists():
    assert flatten([[], [1], [], [2, []]]) == [1, 2]


def test_strings_are_not_lists():
    assert flatten(["ab", ["cd", ["ef"]]]) == ["ab", "cd", "ef"]
