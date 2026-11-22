package worker

import (
	"errors"
	"sync"
	"testing"
)

type fakeCapture struct {
	mu      sync.Mutex
	aborted int
}

func (c *fakeCapture) Abort() { c.mu.Lock(); c.aborted++; c.mu.Unlock() }

func TestOpenAllReturnsEveryCaptureInStreamOrder(t *testing.T) {
	made := map[string]*fakeCapture{}
	var mu sync.Mutex
	got, err := openAll([]string{"stdout", "stderr"}, func(stream string) (*fakeCapture, error) {
		c := &fakeCapture{}
		mu.Lock()
		made[stream] = c
		mu.Unlock()
		return c, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != made["stdout"] || got[1] != made["stderr"] {
		t.Fatal("captures are not in stream order")
	}
	for _, c := range made {
		if c.aborted != 0 {
			t.Fatal("a capture that was kept was aborted")
		}
	}
}

// A failure in either stream must abort the captures that did open, whichever
// stream failed.
func TestOpenAllAbortsTheCapturesThatOpenedWhenAnotherFails(t *testing.T) {
	for _, failing := range []string{"stdout", "stderr"} {
		boom := errors.New("open " + failing)
		made := map[string]*fakeCapture{}
		var mu sync.Mutex
		got, err := openAll([]string{"stdout", "stderr"}, func(stream string) (*fakeCapture, error) {
			if stream == failing {
				return nil, boom
			}
			c := &fakeCapture{}
			mu.Lock()
			made[stream] = c
			mu.Unlock()
			return c, nil
		})
		if !errors.Is(err, boom) || got != nil {
			t.Fatalf("%s failing: got %v, %v", failing, got, err)
		}
		if len(made) != 1 {
			t.Fatalf("%s failing: %d captures opened", failing, len(made))
		}
		for _, c := range made {
			if c.aborted != 1 {
				t.Fatalf("%s failing: the opened capture was aborted %d times", failing, c.aborted)
			}
		}
	}
}
