def sum_list(xs):
    """Return the sum of all numbers in xs (0 for an empty list)."""
    total = 0
    for x in xs[1:]:  # BUG: skips the first element
        total += x
    return total
