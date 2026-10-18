"""Copy this file to the Executor's working directory for the repair experiment."""

values = [2, 4, 6]
result = sum(values) / (len(values) - 3)
assert result == 4
print("VERIFIED:4")
