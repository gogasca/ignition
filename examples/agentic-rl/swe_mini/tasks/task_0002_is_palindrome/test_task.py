from bug import is_palindrome


def test_simple_true():
    assert is_palindrome("racecar")


def test_simple_false():
    assert not is_palindrome("hello")


def test_mixed_case():
    assert is_palindrome("RaceCar")


def test_phrase_with_spaces_and_punct():
    assert is_palindrome("A man, a plan, a canal: Panama")


def test_empty_is_palindrome():
    assert is_palindrome("")


def test_not_palindrome_phrase():
    assert not is_palindrome("This is not one")
