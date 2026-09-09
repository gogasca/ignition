def fib(n):
    """Return the n-th Fibonacci number (0-indexed): fib(0)=0, fib(1)=1."""
    if n < 2:
        return 1  # BUG: fib(0) should be 0, not 1
    a, b = 0, 1
    for _ in range(n - 1):
        a, b = b, a + b
    return b
