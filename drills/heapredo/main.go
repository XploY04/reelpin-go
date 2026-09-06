package main

import "fmt"

// DRILL 3 — Top-K frequent, min-heap version, FROM MEMORY (LC 347)
//
// Rules: 20 min. No AI, no peeking at drills/topk/main.go, no Googling.
// Talk out loud. When done: `go run ./drills/heapredo`
//
// Implement topKFrequent using container/heap (a MIN-heap of size k).
// You will need to:
//   1. define a struct holding {word, count}
//   2. define a slice type over it and give it the 5 methods:
//        Len() int            (value receiver)
//        Less(i, j int) bool  (value receiver) -- "<" for a min-heap
//        Swap(i, j int)       (value receiver)
//        Push(x any)          (POINTER receiver -- append to the end)
//        Pop() any            (POINTER receiver -- remove & return the last)
//   3. count words into a map
//   4. heap.Init, then for each (word,count): heap.Push; if Len() > k, heap.Pop
//   5. pop everything into a result slice, filling BACK-TO-FRONT
//
// Remember the trap: call heap.Push(h, x) / heap.Pop(h) (package funcs),
// NOT your own h.Push / h.Pop methods.
//
// Add the imports yourself (container/heap, fmt, strings).

func topKFrequent(text string, k int) []string {
	// TODO: your code here
	return nil
}

func main() {
	fmt.Println(topKFrequent("the cat the dog the bird cat", 2)) // want [the cat]
	fmt.Println(topKFrequent("a a a b b c", 2))                  // want [a b]
	fmt.Println(topKFrequent("x y z", 5))                        // want all three, any order
}
