package pipeline

import "strings"

// Chunking bounds. A chunk is what search will embed later, so it has to be
// small enough to be specific and large enough to carry context.
const (
	MaxChunkRunes = 2_000
	ChunkOverlap  = 200
)

// Chunk splits a transcript deterministically, preferring paragraph then
// sentence boundaries so a chunk rarely starts mid-thought. The same transcript
// always produces the same chunks, which is what makes re-embedding decidable.
func Chunk(transcript string) []string {
	text := strings.TrimSpace(transcript)
	if text == "" {
		return nil
	}

	runes := []rune(text)
	if len(runes) <= MaxChunkRunes {
		return []string{text}
	}

	chunks := []string{}
	start := 0
	for start < len(runes) {
		end := start + MaxChunkRunes
		if end >= len(runes) {
			chunks = append(chunks, strings.TrimSpace(string(runes[start:])))
			break
		}

		cut := boundary(runes, start, end)
		chunk := strings.TrimSpace(string(runes[start:cut]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}

		next := cut - ChunkOverlap
		if next <= start {
			// The overlap must never make the walk stand still.
			next = cut
		}
		start = next
	}
	return chunks
}

// boundary looks backwards from the hard limit for a paragraph break, then a
// sentence end, then a space, and gives up on the hard limit.
func boundary(runes []rune, start, end int) int {
	window := end - start
	search := window / 4
	if search < 1 {
		return end
	}

	for offset := 0; offset < search; offset++ {
		index := end - offset
		if index <= start+1 {
			break
		}
		if runes[index-1] == '\n' && runes[index-2] == '\n' {
			return index
		}
	}
	for offset := 0; offset < search; offset++ {
		index := end - offset
		if index <= start+1 {
			break
		}
		switch runes[index-1] {
		case '.', '!', '?', '\n':
			return index
		}
	}
	for offset := 0; offset < search; offset++ {
		index := end - offset
		if index <= start+1 {
			break
		}
		if runes[index-1] == ' ' {
			return index
		}
	}
	return end
}
