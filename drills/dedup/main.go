package main

import "fmt"

// DRILL 2 — Remove duplicates from a SORTED slice, in place (LC 26)
//
// Rules: 15 min. No AI, no Googling, no copying. Talk out loud.
// When done: `go run ./drills/dedup`
//
// Task:
//   Given a slice of ints sorted in ascending order, remove the duplicates
//   IN PLACE so each element appears once. Return the deduplicated slice
//   (a re-slice of the input, not a new allocation).
//
// Example:
//   dedup([]int{1, 1, 2})           -> [1 2]
//   dedup([]int{0,0,1,1,1,2,2,3,4}) -> [0 1 2 3 4]
//   dedup([]int{})                  -> []
//   dedup([]int{5})                 -> [5]
//
// Why this drill (Go fundamentals):
//   - "in place" means you do NOT make a new slice. You overwrite the front
//     of the input and return nums[:k]. This forces you to understand that a
//     slice is a window (ptr,len,cap) over a backing array.
//   - The pattern is TWO POINTERS: a slow pointer marks where the next unique
//     value goes; a fast pointer scans ahead. You'll reuse this constantly.
//
// Hints (concept only, no code):
//   - handle the empty slice first (len 0 -> return it as is)
//   - slow starts at 0. fast scans from 1.
//   - when nums[fast] != nums[slow], advance slow and copy nums[fast] there.
//   - the answer length is slow+1. return nums[:slow+1].
//
// Follow-ups to think about (don't code):
//   1. Why is returning nums[:k] safe even though the tail is "garbage"?
//   2. What changes if the slice is NOT sorted?

func dedup(nums []int) []int {
	// TODO: your code here
	if len(nums) == 0 {
		return nil
	}

	n := len(nums)
	slow := 0
	fast := 1
	for fast < n {
		if nums[fast] != nums[slow] {
			slow++
			nums[slow] = nums[fast]
		} else {
			fast++
		}
	}
	return nums[:slow+1]
}

func main() {
	fmt.Println(dedup([]int{1, 1, 2}))                  // want [1 2]
	fmt.Println(dedup([]int{0, 0, 1, 1, 1, 2, 2, 3, 4})) // want [0 1 2 3 4]
	fmt.Println(dedup([]int{}))                          // want []
	fmt.Println(dedup([]int{5}))                         // want [5]
}
