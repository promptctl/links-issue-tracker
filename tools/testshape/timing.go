package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// event is the subset of test2json's output this tool reads (see
// $GOROOT/src/cmd/internal/test2json). A nil Elapsed means the event carries
// no duration.
type event struct {
	Time    time.Time
	Action  string
	Package string
	Test    string
	Elapsed *float64
}

// timedTest is one top-level test the stream ran: its verdict and its wall
// time.
type timedTest struct {
	Package string
	Name    string
	Verdict string // pass, fail, skip, or unfinished
	Elapsed time.Duration
}

// openTest is a top-level test between its start and its verdict.
type openTest struct {
	started time.Time
	// children is the sum of its direct subtests' elapsed. go test's own
	// elapsed for a parent excludes subtests that ran in parallel after it
	// returned, so a parent's time is the larger of the two.
	children time.Duration
}

// readStream reads one `go test -json` stream and returns every top-level
// test that ran, in stream order. Lines that are not JSON events (build
// errors, a panic trace) are copied to w, surfaced rather than swallowed.
// [LAW:no-silent-failure]
func readStream(r io.Reader, w io.Writer) ([]timedTest, error) {
	type key struct{ pkg, test string }
	open := map[key]*openTest{}
	var order []key
	var tests []timedTest
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var ev event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			fmt.Fprintf(w, "%s\n", sc.Bytes())
			continue
		}
		top, sub, isSub := strings.Cut(ev.Test, "/")
		switch {
		case ev.Test == "" && isVerdict(ev.Action):
			// The package ended: a test still open got no verdict, so the
			// package's end stops its clock.
			var rest []key
			for _, k := range order {
				if k.pkg != ev.Package {
					rest = append(rest, k)
					continue
				}
				tests = append(tests, timedTest{k.pkg, k.test, "unfinished", ev.Time.Sub(open[k].started)})
				delete(open, k)
			}
			order = rest
		case ev.Test == "":
		case !isSub && ev.Action == "run":
			k := key{ev.Package, top}
			open[k] = &openTest{started: ev.Time}
			order = append(order, k)
		case isVerdict(ev.Action):
			k := key{ev.Package, top}
			ot := open[k]
			if ot == nil {
				continue
			}
			if isSub {
				if !strings.Contains(sub, "/") {
					ot.children += seconds(ev.Elapsed)
				}
				continue
			}
			tests = append(tests, timedTest{ev.Package, top, ev.Action, max(seconds(ev.Elapsed), ot.children)})
			delete(open, k)
			order = removeKey(order, k)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading go test -json stream: %w", err)
	}
	for _, k := range order {
		fmt.Fprintf(w, "testshape: the stream ended before %s %s did; it is not counted\n", k.pkg, k.test)
	}
	return tests, nil
}

func isVerdict(action string) bool {
	return action == "pass" || action == "fail" || action == "skip"
}

func removeKey[K comparable](keys []K, k K) []K {
	for i, x := range keys {
		if x == k {
			return append(keys[:i:i], keys[i+1:]...)
		}
	}
	return keys
}

// seconds converts test2json's Elapsed, where nil means no duration, to a
// Duration.
func seconds(elapsed *float64) time.Duration {
	if elapsed == nil {
		return 0
	}
	return time.Duration(*elapsed * float64(time.Second))
}
