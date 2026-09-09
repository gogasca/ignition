def binary_search(arr, target):
    """Return the index of target in the sorted list arr, or -1 if absent."""
    lo, hi = 0, len(arr) - 1
    while lo < hi:  # BUG: should be lo <= hi, or the last candidate is never checked
        mid = (lo + hi) // 2
        if arr[mid] == target:
            return mid
        elif arr[mid] < target:
            lo = mid + 1
        else:
            hi = mid - 1
    return -1
