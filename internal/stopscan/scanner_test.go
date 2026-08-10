package stopscan

import "testing"

func TestScannerMatchesWithinAndAcrossDeltas(t *testing.T) {
	tests := []struct {
		name       string
		stops      []string
		chunks     []string
		wantOutput string
		wantMatch  string
	}{
		{name: "within", stops: []string{"STOP"}, chunks: []string{"before STOP after"}, wantOutput: "before ", wantMatch: "STOP"},
		{name: "across", stops: []string{"<END>"}, chunks: []string{"before <E", "ND> after"}, wantOutput: "before ", wantMatch: "<END>"},
		{name: "overlapping earliest", stops: []string{"bc", "abc"}, chunks: []string{"zabc"}, wantOutput: "z", wantMatch: "abc"},
		{name: "request order breaks exact tie", stops: []string{"END", "END"}, chunks: []string{"aEND"}, wantOutput: "a", wantMatch: "END"},
		{name: "unicode", stops: []string{"🛑終"}, chunks: []string{"alpha 🛑", "終 omega"}, wantOutput: "alpha ", wantMatch: "🛑終"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scanner, err := New(test.stops)
			if err != nil {
				t.Fatal(err)
			}
			var output string
			for _, chunk := range test.chunks {
				result := scanner.Feed(chunk)
				output += result.Text
			}
			output += scanner.Finish().Text
			if output != test.wantOutput || scanner.Match() != test.wantMatch || !scanner.Stopped() {
				t.Fatalf("output/match/stopped = %q/%q/%v, want %q/%q/true", output, scanner.Match(), scanner.Stopped(), test.wantOutput, test.wantMatch)
			}
		})
	}
}

func TestScannerFlushesSafePrefixesAndFinishTail(t *testing.T) {
	scanner, err := New([]string{"STOP", "START"})
	if err != nil {
		t.Fatal(err)
	}
	first := scanner.Feed("hello ST")
	if first.Text != "hello " || first.Matched != "" {
		t.Fatalf("first = %#v", first)
	}
	second := scanner.Feed("ill going")
	if second.Text != "STill going" || second.Matched != "" {
		t.Fatalf("second = %#v", second)
	}
	finished := scanner.Finish()
	if finished.Text != "" || scanner.Stopped() {
		t.Fatalf("finish = %#v, stopped=%v", finished, scanner.Stopped())
	}
}

func TestScannerNoStopsPassesThroughImmediately(t *testing.T) {
	scanner, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := scanner.Feed("everything"); result.Text != "everything" || result.Matched != "" {
		t.Fatalf("Feed = %#v", result)
	}
}

func TestScannerStopsPermanentlyAtFirstMatch(t *testing.T) {
	scanner, err := New([]string{"END", "STOP"})
	if err != nil {
		t.Fatal(err)
	}
	first := scanner.Feed("one END two STOP")
	second := scanner.Feed("ignored")
	finish := scanner.Finish()
	if first.Text != "one " || first.Matched != "END" || second != (Result{}) || finish != (Result{}) {
		t.Fatalf("results = %#v / %#v / %#v", first, second, finish)
	}
}

func TestScannerRejectsEmptyStopSequence(t *testing.T) {
	if _, err := New([]string{"END", ""}); err == nil {
		t.Fatal("New accepted empty stop sequence")
	}
}
