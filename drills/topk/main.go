package main

import (
	"bufio"
	"container/heap"
	"fmt"
	"io"
	"strings"
)

// ============================================================================
// CONCEPT 1: Min-heap for Top-K  (container/heap)
// ============================================================================
//
// A binary heap is a complete binary tree stored in a flat slice:
//   node i's children live at 2i+1 and 2i+2; its parent at (i-1)/2.
// A MIN-heap keeps the smallest element at the root (index 0).
// push = add at the end, "bubble up".  pop = remove root, "bubble down".
// Both are O(log n). Peeking the min is O(1).
//
// Top-K trick: keep a min-heap of size K, keyed by count. For every word,
// push it; if the heap grows past K, pop the SMALLEST count. Whatever survives
// is the K largest. That's O(n log K) instead of O(n log n) for a full sort.

type wordCount struct {
	word  string
	count int
}

// minHeap is a slice of wordCount. We make it satisfy heap.Interface by
// implementing 5 methods. heap.Interface = sort.Interface (Len/Less/Swap) + Push + Pop.
type minHeap []wordCount

// Len/Less/Swap use VALUE receivers: they read/swap but never change the length.
func (h minHeap) Len() int { return len(h) }

// Less defines the ordering. "<" here = MIN-heap (smallest count at the root).
// Flip to ">" and you'd have a max-heap. This one line is the whole difference.
func (h minHeap) Less(i, j int) bool { return h[i].count < h[j].count }

func (h minHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

// Push/Pop use POINTER receivers because they change the slice's length.
// IMPORTANT: these are the RAW ends of the slice, not heap-ordered operations.
// You never call these yourself. heap.Push/heap.Pop (below) call them AND do
// the bubble-up/down to keep the heap valid. This is the #1 confusion with
// container/heap: your Push just appends, the package's heap.Push reorders.
func (h *minHeap) Push(x any) {
	*h = append(*h, x.(wordCount))
}

func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]  // heap.Pop has already swapped the min to the end for us
	*h = old[:n-1]    // shrink the slice by one
	return item
}

func topKFrequent(text string, k int) []string {
	counts := map[string]int{}
	for _, w := range strings.Fields(text) {
		counts[w]++
	}

	h := &minHeap{}
	heap.Init(h) // establishes the heap invariant on an (empty) slice
	for word, count := range counts {
		heap.Push(h, wordCount{word, count}) // package func: append + bubble up
		if h.Len() > k {
			heap.Pop(h) // package func: pull the smallest count out
		}
	}

	// The heap now holds the K largest, but pop yields them smallest-first.
	// Fill the result back-to-front so the biggest ends up at index 0.
	res := make([]string, h.Len())
	for i := len(res) - 1; i >= 0; i-- {
		res[i] = heap.Pop(h).(wordCount).word
	}
	return res
}

// ============================================================================
// CONCEPT 2: bufio.Scanner  (streaming — the "10GB file" answer)
// ============================================================================
//
// bufio.Scanner reads from any io.Reader (a file, network conn, os.Stdin) in
// bounded-size chunks. You never hold the whole input in memory — only one
// token at a time. That's how you count words in a 10GB file on a laptop.
//
//   sc.Scan()  advances to the next token, returns false at EOF
//   sc.Text()  returns the current token as a string
//   sc.Split(bufio.ScanWords)  splits on whitespace instead of the default lines
//
// For a real file you'd do:  f, _ := os.Open(path); defer f.Close(); NewScanner(f)
// Here we use strings.NewReader so the demo runs without a file on disk.

func streamWordCount(r io.Reader) map[string]int {
	counts := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Split(bufio.ScanWords) // token = one word
	for sc.Scan() {
		counts[sc.Text()]++
	}
	// ponytail: skipping sc.Err() check for the demo; a real reader checks it after the loop.
	return counts
}

func main() {
	fmt.Println("--- Concept 1: top-K via min-heap ---")
	fmt.Println(topKFrequent("the cat the dog the bird cat", 2)) // [the cat]
	fmt.Println(topKFrequent("a a a b b c", 2))                  // [a b]

	fmt.Println("--- Concept 2: streaming word count ---")
	r := strings.NewReader("go is  fun   go go rocks")
	fmt.Println(streamWordCount(r)) // map[fun:1 go:3 is:1 rocks:1]
}
