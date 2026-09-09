def dedupe(xs):
    """Return the items of xs with duplicates removed, keeping the order of
    first appearance."""
    return list(set(xs))  # BUG: set() drops the original ordering
