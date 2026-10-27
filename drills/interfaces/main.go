package main

import "fmt"

// ============================================================================
// SESSION DRILL — Part A: revision (blank re-solve) · Part B: new topic
// Rules: no AI, no Googling, no peeking at your old files. Talk out loud.
// Run: `go run ./drills/interfaces`
// ============================================================================

// ----------------------------------------------------------------------------
// PART A — REVISION (due today). Re-solve each from MEMORY. ~10 min total.
// If any needs a hint, it stays confidence 2; if clean+fast, it graduates.
// ----------------------------------------------------------------------------

// A1. dedup a sorted slice in place, return nums[:k].  (Two pointers)
func dedup(nums []int) []int {
	// TODO (from memory)
	if len(nums) == 0 {
		return nil
	}

	length := len(nums) 
	for i := 0; i < length; i++ {
		
	}
	return nil
}

// A2. rotate right by k, in place.  (reverse-all, reverse-first-k, reverse-rest)
func rotate(nums []int, k int) {
	// TODO (from memory)
}

// A3. group anagrams — THIS TIME use the [26]int count key, not sorting.
//     (two anagrams share the same letter-count array; array is a valid map key)
func groupAnagrams(strs []string) [][]string {
	// TODO (from memory, count-key version)
	return nil
}

// ----------------------------------------------------------------------------
// PART B — NEW TOPIC: interfaces (Shape / Stringer / type switch / sort)
// ----------------------------------------------------------------------------
//
// 1. Define a Shape interface with Area() float64 and Perimeter() float64.
// 2. Implement two types that satisfy it:
//      Circle{ R float64 }        Area = pi*r*r      Perimeter = 2*pi*r
//      Rectangle{ W, H float64 }  Area = w*h         Perimeter = 2*(w+h)
// 3. Give each a String() string method (satisfy fmt.Stringer) so they print
//    like "Circle(r=2)" / "Rectangle(3x4)".
// 4. TotalArea(shapes []Shape) float64 — sum the areas (polymorphism over the interface).
// 5. Describe(s Shape) string — use a TYPE SWITCH to return
//      "circle" / "rectangle" / "unknown" based on the concrete type.
// 6. In main, sort a []Shape by Area ascending (sort.Slice) and print them —
//    Stringer makes the print readable.
//
// Concepts this proves: implicit satisfaction, []Shape polymorphism, Stringer,
// type assertion/switch, and sort.Slice over an interface slice.

// TODO: Shape interface + Circle + Rectangle + methods + TotalArea + Describe

func main() {
	// --- Part A checks ---
	fmt.Println("A1:", dedup([]int{0, 0, 1, 1, 2, 3, 3}))          // [0 1 2 3]
	nums := []int{1, 2, 3, 4, 5, 6, 7}
	rotate(nums, 3)
	fmt.Println("A2:", nums)                                        // [5 6 7 1 2 3 4]
	fmt.Println("A3:", groupAnagrams([]string{"eat", "tea", "bat"})) // {eat,tea} {bat}

	// --- Part B checks (uncomment once implemented) ---
	// shapes := []Shape{Circle{R: 2}, Rectangle{W: 3, H: 4}, Circle{R: 1}}
	// fmt.Println("TotalArea:", TotalArea(shapes))
	// sort.Slice(shapes, func(i, j int) bool { return shapes[i].Area() < shapes[j].Area() })
	// fmt.Println("sorted:", shapes) // Stringer prints them; ascending by area
	// for _, s := range shapes {
	// 	fmt.Println(Describe(s), "->", s)
	// }
}
