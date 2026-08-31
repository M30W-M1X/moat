// Command wordcount prints how often each word appears in a text file. It is
// the sample workload for the Moat demo. The demo copies this directory into
// a sandbox, runs the test, builds the binary, and copies the binary out.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// countWords reads whitespace-separated words from r. It returns each distinct
// word in lower case, with punctuation trimmed, and the number of times it
// appears.
func countWords(r io.Reader) map[string]int {
	sc := bufio.NewScanner(r)
	sc.Split(bufio.ScanWords)
	counts := map[string]int{}
	for sc.Scan() {
		w := strings.ToLower(strings.Trim(sc.Text(), `.,!?;:"'()[]`))
		if w != "" {
			counts[w]++
		}
	}
	return counts
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: wordcount FILE")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	counts := countWords(f)
	words := make([]string, 0, len(counts))
	for w := range counts {
		words = append(words, w)
	}
	sort.Slice(words, func(i, j int) bool {
		if counts[words[i]] != counts[words[j]] {
			return counts[words[i]] > counts[words[j]]
		}
		return words[i] < words[j]
	})
	for _, w := range words {
		fmt.Printf("%4d  %s\n", counts[w], w)
	}
}
