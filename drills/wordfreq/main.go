package main

import (
	"fmt"
	"sort"
	"strings"
)

// DRILL 1 — Word frequency, top N
//
// Rules: 15 min. No AI, no Googling, no copying. Talk out loud.
// When done: `go run ./drills/wordfreq` and check the output.
//
// Task:
//   Given a string of text, return the N most frequent words, most-frequent
//   first. Break ties alphabetically (so the result is deterministic).
//
// Signature to implement:
//   func topWords(text string, n int) []string
//
// Example:
//   topWords("the cat the dog the bird cat", 2)  ->  ["the", "cat"]
//     ("the" appears 3x, "cat" 2x)
//   topWords("b a c a b", 2)  ->  ["a", "b"]
//     (both "a" and "b" appear 2x; tie broken alphabetically -> a before b)
//
// Constraints / hints (concepts this tests, no code given):
//   - split on spaces (strings.Fields handles multiple spaces)
//   - count with a map[string]int
//   - you can't sort a map — extract keys into a slice, then sort.Slice
//   - the tie-break is the tricky part: sort by count DESC, then word ASC
//   - if n is bigger than the number of distinct words, return them all
//
// Interviewer follow-ups I'll ask after (don't code them yet, just think):
//   1. What's the time complexity? Can you avoid the full sort if n is small?
//   2. What if the input is 10GB and won't fit in memory?

func topWords(text string, n int) []string {
	wordMap := make(map[string]int)
	for _, word := range strings.Fields(text) {
		wordMap[word]++
	}
	fmt.Println(wordMap)
	keys := make([]string, 0, len(wordMap))
	for word := range wordMap {
		keys = append(keys, word)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if wordMap[a] != wordMap[b] {
			return wordMap[a] > wordMap[b]
		}
		return a < b
	})
	if n > len(keys) {
		n = len(keys)
	}
	return keys[:n]
}

func main() {
	fmt.Println(topWords("the cat the dog the bird cat", 2)) // want [the cat]
	fmt.Println(topWords("b a c a b", 2))                    // want [a b]
	fmt.Println(topWords("only one word here here here", 5)) // want [here here one only word]
}
