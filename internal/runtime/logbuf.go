package runtime

import (
	"bytes"
	"sync"
)

// LogBuffer keeps the last N lines written to it in memory.
type LogBuffer struct {
	mu      sync.Mutex
	lines   [][]byte
	partial []byte
	base    uint64 // global index of lines[0]
	max     int
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 2000
	}
	return &LogBuffer{max: max}
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := append(b.partial, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := make([]byte, i)
		copy(line, data[:i])
		b.appendLocked(line)
		data = data[i+1:]
	}
	b.partial = append([]byte(nil), data...)
	return len(p), nil
}

func (b *LogBuffer) appendLocked(line []byte) {
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		drop := len(b.lines) - b.max
		b.lines = append([][]byte(nil), b.lines[drop:]...)
		b.base += uint64(drop)
	}
}

// Total returns the global index one past the last line.
func (b *LogBuffer) Total() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base + uint64(len(b.lines))
}

// Tail returns the last n lines (newest last).
func (b *LogBuffer) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, 0, n)
	for _, l := range b.lines[len(b.lines)-n:] {
		out = append(out, string(l))
	}
	return out
}

// Since returns lines whose global index is >= from.
func (b *LogBuffer) Since(from uint64) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if from < b.base {
		from = b.base
	}
	total := b.base + uint64(len(b.lines))
	if from >= total {
		return nil
	}
	start := int(from - b.base)
	out := make([]string, 0, len(b.lines)-start)
	for _, l := range b.lines[start:] {
		out = append(out, string(l))
	}
	return out
}
