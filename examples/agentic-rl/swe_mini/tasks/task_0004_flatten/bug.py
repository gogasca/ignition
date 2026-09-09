def flatten(items):
    """Flatten an arbitrarily nested list of values into a single flat list,
    preserving left-to-right order. Non-list values are kept as-is."""
    out = []
    for item in items:
        if isinstance(item, list):
            out.append(item)  # BUG: appends the sub-list instead of flattening it
        else:
            out.append(item)
    return out
