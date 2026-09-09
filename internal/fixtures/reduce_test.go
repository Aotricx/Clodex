package fixtures

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aotricx/Clodex/internal/codexstream"
	"github.com/Aotricx/Clodex/internal/reducer"
)

func TestGoldenFixturesReduce(t *testing.T) {
	manifest, err := Load("testdata")
	if err != nil {
		t.Fatalf("Load(testdata) error = %v", err)
	}

	for _, fixture := range manifest.Fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", fixture.File))
			if err != nil {
				t.Fatal(err)
			}

			parser := codexstream.New(context.Background(), bytes.NewReader(data), codexstream.Options{})
			red := reducer.New()
			for {
				event, err := parser.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("parser.Next: %v", err)
				}
				_, err = red.Push(event)
				if unexpectedReduceError(fixture.Name, err) {
					t.Fatalf("reducer.Push %s: unexpected error: %v", event.Type, err)
				}
				// real_tool_roundtrip concatenates two Responses streams.
				// Reduce only through the first terminal; a second response
				// would be a new reducer in production.
				if fixture.Name == "real_tool_roundtrip" && event.Terminal() {
					break
				}
			}
		})
	}
}

func unexpectedReduceError(name string, err error) bool {
	if err == nil {
		return false
	}
	switch name {
	case "regression_terminal_only_completed", "regression_terminal_only_done":
		return !errors.Is(err, reducer.ErrEmptyCompletion)
	default:
		return true
	}
}
