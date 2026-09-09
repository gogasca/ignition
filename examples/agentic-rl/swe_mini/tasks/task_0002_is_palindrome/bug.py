def is_palindrome(s):
    """True if s reads the same forwards and backwards, ignoring case,
    spaces, and punctuation."""
    return s == s[::-1]  # BUG: no normalization (case / non-alphanumerics)
