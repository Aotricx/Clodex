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
		{name: "same offset distinct stops request order", stops: []string{"EN", "END"}, chunks: []string{"xEND"}, wantOutput: "x", wantMatch: "EN"},
		{name: "same offset longer first", stops: []string{"END", "EN"}, chunks: []string{"xEND"}, wantOutput: "x", wantMatch: "END"},
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

func TestScannerOneShotAgreesWithByteFeedsForOverlappingStops(t *testing.T) {
	tests := []struct {
		name      string
		stops     []string
		input     string
		wantText  string
		wantMatch string
	}{
		{
			name:      "shorter interior stop",
			stops:     []string{"cd", "abcdef"},
			input:     "abcdef",
			wantText:  "",
			wantMatch: "abcdef",
		},
		{
			name:      "shorter prefix of longer",
			stops:     []string{"END", "EN"},
			input:     "xEND",
			wantText:  "x",
			wantMatch: "END",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oneShot, err := New(test.stops)
			if err != nil {
				t.Fatal(err)
			}
			oneText := oneShot.Feed(test.input).Text + oneShot.Finish().Text

			byteScanner, err := New(test.stops)
			if err != nil {
				t.Fatal(err)
			}
			var byteText string
			for i := 0; i < len(test.input); i++ {
				byteText += byteScanner.Feed(test.input[i : i+1]).Text
			}
			byteText += byteScanner.Finish().Text

			if oneText != test.wantText || oneShot.Match() != test.wantMatch || !oneShot.Stopped() {
				t.Fatalf("one-shot output/match/stopped = %q/%q/%v, want %q/%q/true", oneText, oneShot.Match(), oneShot.Stopped(), test.wantText, test.wantMatch)
			}
			if byteText != oneText || byteScanner.Match() != oneShot.Match() || byteScanner.Stopped() != oneShot.Stopped() {
				t.Fatalf("1-byte output/match/stopped = %q/%q/%v, one-shot = %q/%q/%v", byteText, byteScanner.Match(), byteScanner.Stopped(), oneText, oneShot.Match(), oneShot.Stopped())
			}
		})
	}
}
