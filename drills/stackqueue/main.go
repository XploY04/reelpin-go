package main

import "fmt"

// DRILL — Stack (LIFO) and Queue (FIFO) types
//
// Rules: 20 min. No AI, no Googling, no copying. Talk out loud.
// When done: `go run ./drills/stackqueue`
//
// Build two small types backed by a []int slice, each with methods.
//
// Stack (Last In First Out):
//   Push(v int)          add on top
//   Pop() (int, bool)    remove & return top; ok=false if empty  (comma-ok!)
//   Peek() (int, bool)   look at top without removing
//   Len() int
//   IsEmpty() bool
//
// Queue (First In First Out):
//   Enqueue(v int)       add at back
//   Dequeue() (int, bool) remove & return front; ok=false if empty
//   Peek() (int, bool)   look at front
//   Len() int
//   IsEmpty() bool
//
// Why this drill (structs + methods + RECEIVERS):
//   - Push/Pop/Enqueue/Dequeue MUTATE the underlying slice (append / reslice
//     changes len), so they MUST use POINTER receivers (*Stack, *Queue).
//     Try a value receiver on Push and watch the change vanish — that's the lesson.
//   - Len/Peek/IsEmpty only read; but by the consistency rule, make ALL methods
//     pointer receivers once any of them needs to be.
//   - Pop/Dequeue return (int, bool) — same comma-ok idea as map lookups.
//
// Hints (concept only, no code):
//   - type Stack struct { items []int }
//   - Push: s.items = append(s.items, v)
//   - Pop: read last (len-1), reslice s.items = s.items[:len-1], return it, true
//   - Queue Enqueue = append; Dequeue = take items[0], reslice items = items[1:]
//
// Follow-up to think about (don't code):
//   1. What happens to Pop/Peek on an empty stack if you DON'T guard len? (panic)
//   2. Queue Dequeue via items[1:] keeps the backing array growing (memory leak
//      over time). How would a production queue avoid that? (ring buffer / index)

type Stack struct {
	// TODO
	arr []int
}

func (a *Stack) Push(v int) {
	a.arr = append(a.arr, v)
}

func (a *Stack) Pop() (int, bool) {
	if len(a.arr) == 0 {
		return 0, false
	}
	h := a.arr[len(a.arr)-1]
	a.arr = a.arr[:len(a.arr)-1]
	return h, true
}

func (a *Stack) Peek() (int, bool) {
	if len(a.arr) == 0 {
		return 0, false
	}
	return a.arr[len(a.arr)-1], true
}

func (a *Stack) Len() int {
	return len(a.arr)
}

func (a *Stack) IsEmpty() bool {
	if len(a.arr) == 0 {
		return true
	}
	return false
}

type Queue struct {
	// TODO
	arr []int
}

func (a *Queue) Enqueue(v int) {
	a.arr = append(a.arr, v)
}

func (a *Queue) Dequeue() (int, bool) {
	if len(a.arr) == 0 {
		return 0, false
	}
	h := a.arr[0]
	a.arr = a.arr[1:]
	return h, true
}

func (a *Queue) Peek() (int, bool) {
	if len(a.arr) == 0 {
		return 0, false
	}
	return a.arr[0], true
}

func (a *Queue) Len() int {
	return len(a.arr)
}

func (a *Queue) IsEmpty() bool {
	if len(a.arr) == 0 {
		return true
	}
	return false
}

func main() {
	s := &Stack{}
	s.Push(1)
	s.Push(2)
	s.Push(3)
	fmt.Println(s.Len())  // 3
	fmt.Println(s.Pop())  // 3 true
	fmt.Println(s.Peek()) // 2 true
	fmt.Println(s.Pop())  // 2 true
	fmt.Println(s.Pop())  // 1 true
	fmt.Println(s.Pop())  // 0 false  (empty)

	q := &Queue{}
	q.Enqueue(1)
	q.Enqueue(2)
	q.Enqueue(3)
	fmt.Println(q.Len())      // 3
	fmt.Println(q.Dequeue())  // 1 true
	fmt.Println(q.Peek())     // 2 true
	fmt.Println(q.Dequeue())  // 2 true
	fmt.Println(q.Dequeue())  // 3 true
	fmt.Println(q.Dequeue())  // 0 false  (empty)
}
