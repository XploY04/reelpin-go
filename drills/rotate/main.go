package main

import "fmt"

// DRILL — Rotate a slice to the right by k, IN PLACE (LC 189)
//
// Rules: 15 min. No AI, no Googling, no copying. Talk out loud.
// When done: `go run ./drills/rotate`
//
// Task:
//   Rotate nums to the right by k steps, in place. k can be >= len(nums).
//
// Example:
//   rotate([1,2,3,4,5,6,7], 3) -> [5,6,7,1,2,3,4]
//   rotate([-1,-100,3,99], 2)  -> [3,99,-1,-100]
//   rotate([1,2], 3)           -> [2,1]   (k=3 on len 2 == rotate by 1)
//
// Why this drill (slices):
//   - forces "in place" = mutate the backing array, no new slice.
//   - the neat O(n) trick: reverse the WHOLE slice, then reverse the first k,
//     then reverse the rest. Work out on paper why that rotates.
//
// Hints (concept only, no code):
//   - first: k %= len(nums)   (k can exceed the length; and guard len 0)
//   - write a helper reverse(s []int) that swaps ends inward with two pointers
//   - reverse(nums); reverse(nums[:k]); reverse(nums[k:])
//   - note nums[:k] and nums[k:] are windows into the SAME array — that's the point
//
// Follow-up to think about (don't code):
//   1. What's the time and space complexity of the reverse trick?
//   2. Why does rotating right by k == reversing all, then the two halves?

func rotate(nums []int, k int) {
	// TODO: your code here
	reverse := func (nums []int) {
		
		var n int = len(nums) / 2

		start := 0
		end := len(nums)-1

		for i := 0; i < n; i++ {
			temp := nums[start + i]
			nums[start + i] = nums[end - i]
			nums[end - i] = temp
		}
	}

	if len(nums) == 0 {
		return
	}

	k %= len(nums)
	reverse(nums)
	reverse(nums[:k])
	reverse(nums[k:])
}

func main() {
	a := []int{1, 2, 3, 4, 5, 6, 7}
	rotate(a, 3)
	fmt.Println(a) // want [5 6 7 1 2 3 4]

	b := []int{-1, -100, 3, 99}
	rotate(b, 2)
	fmt.Println(b) // want [3 99 -1 -100]

	c := []int{1, 2}
	rotate(c, 3)
	fmt.Println(c) // want [2 1]
}
