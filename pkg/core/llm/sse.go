package llm

import (
	"bufio"
	"io"
	"strings"
)

// sse reads Server-Sent Events from a stream. Only the data lines of each
// event are kept, joined with "\n" as the spec requires.
type sse struct {
	r *bufio.Reader
}

func newSSE(r io.Reader) *sse { return &sse{r: bufio.NewReader(r)} }

// next returns the next event's data. ok is false at end of stream. A blank
// line ends an event; lines that begin with ":" are comments and dropped.
func (s *sse) next() (data string, ok bool, err error) {
	var lines []string
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if len(lines) > 0 {
					return strings.Join(lines, "\n"), true, nil
				}
				return "", false, nil
			}
			return "", false, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(lines) > 0 {
				return strings.Join(lines, "\n"), true, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			lines = append(lines, strings.TrimPrefix(v, " "))
		}
	}
}
