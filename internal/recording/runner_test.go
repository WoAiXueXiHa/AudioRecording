package recording

import "testing"

func TestNewRunnerConcurrency(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, err := NewRunner(nil, nil, n); err == nil {
			t.Fatalf("accepted %d", n)
		}
	}
	r, err := NewRunner(nil, nil, 3)
	if err != nil || cap(r.slots) != 3 {
		t.Fatal("invalid slots", err)
	}
}
