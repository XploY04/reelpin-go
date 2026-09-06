# Drill / DSA revision log

Spaced repetition. Re-solve **from a blank file** on the `next-review` date.
Confidence: 1 = barely (next day) · 2 = got it, needed a hint (3 days) · 3 = clean & fast (1 week → 2 weeks → 1 month).
Weekday re-solves that don't fit the 2.5h cap slide to the weekend DSA block.

| Problem | Pattern | First solved | Conf | Next review | Notes |
|---------|---------|--------------|-----:|-------------|-------|
| dedup (LC 26) | Two Pointers / slice in place | 2026-08-03 | 2 | 2026-08-06 (DUE) | works; use `for fast:=1; fast<n; fast++` idiom (advance fast every step) |
| rotate (LC 189) | Reverse trick / block swap | 2026-08-03 | 2 | 2026-08-06 (DUE) | reverse-all → reverse-first-k → reverse-rest. Pull `reverse` to package scope. |
| group anagrams (LC 49) | Canonical key / map | 2026-08-05 | 2 | 2026-08-08 | sort-key done; re-solve with the [26]int count key (array-as-map-key, O(n·k)) |
| Stack + Queue | Structs / pointer receivers | 2026-08-05 | 3 | 2026-08-12 | clean; pointer receivers for mutation. `IsEmpty` → `return len==0` |

## Concepts landed (Anki-worthy)
- Slice = header {ptr,len,cap}; a window over a backing array, not the data.
- append: len<cap writes in place (aliases!); len==cap allocates new + copies (no longer shares).
- reverse/rotate/block-swap in place → 3 reversals, O(n) time, O(1) space.
- Go tuple swap: `a, b = b, a` (no temp).
