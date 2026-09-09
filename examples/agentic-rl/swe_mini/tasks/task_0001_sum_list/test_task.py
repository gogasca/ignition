from bug import sum_list


def test_empty():
    assert sum_list([]) == 0


def test_single():
    assert sum_list([7]) == 7


def test_many():
    assert sum_list([1, 2, 3, 4]) == 10


def test_negatives():
    assert sum_list([-2, 5, -3]) == 0


def test_floats():
    assert sum_list([0.5, 0.25, 0.25]) == 1.0
