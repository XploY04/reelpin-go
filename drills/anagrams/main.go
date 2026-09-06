package main

import (
	"fmt"
	"sort"
)

// DRILL — Group Anagrams (LC 49)
//
// Rules: 15 min. No AI, no Googling, no copying. Talk out loud.
// When done: `go run ./drills/anagrams`
//
// Task:
//   Given a slice of strings, group the anagrams together. Return a slice of
//   groups. Order of the groups doesn't matter; order within a group should
//   follow input order.
//
// Example:
//   groupAnagrams(["eat","tea","tan","ate","nat","bat"])
//     -> [["eat","tea","ate"], ["tan","nat"], ["bat"]]   (any group order)
//
// Why this drill (maps + strings):
//   - the whole trick is a CANONICAL KEY: two words are anagrams iff their
//     sorted letters are equal. So "eat" and "tea" both key to "aet".
//   - map[string][]string : key = sorted letters, value = the words.
//   - then collect the map's values into the result.
//
// Hints (concept only, no code):
//   - to build the key: b := []byte(word); sort those bytes; key := string(b)
//     (sort.Slice with a func(i,j int) bool comparator, like you did before)
//   - append each word to groups[key]
//   - remember: map iteration order is random — that's fine, group order is free
//
// Follow-ups to think about (don't code):
//   1. Complexity? (n words, k = max word length) — the sort per word matters.
//   2. Could you key without sorting? (hint: a 26-length letter-count signature)

func groupAnagrams(strs []string) [][]string {
	groups := make(map[string][]string)
	for _, v := range strs {
		b := []byte(v)
		sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
		key := string(b)
		groups[key] = append(groups[key], v)
	}
	var ans [][]string
	for k := range groups {
		// fmt.Println(groups)
		ans = append(ans, groups[k])
	} 
	return ans
}

func main() {
	fmt.Println(groupAnagrams([]string{"eat", "tea", "tan", "ate", "nat", "bat"}))
	// want 3 groups: {eat,tea,ate} {tan,nat} {bat}  (any order)
	fmt.Println(groupAnagrams([]string{""}))       // want [[""]]
	fmt.Println(groupAnagrams([]string{"a"}))      // want [["a"]]
	fmt.Println(groupAnagrams([]string{"ab", "ba", "abc"})) // want {ab,ba} {abc}
}
