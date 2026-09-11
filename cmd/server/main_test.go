package main

import (
	"os"
	"testing"
)

func TestWorkerConcurrency(t *testing.T) {
	old, exists := os.LookupEnv("WORKER_CONCURRENCY")
	t.Cleanup(func() {
		if exists {
			os.Setenv("WORKER_CONCURRENCY", old)
		} else {
			os.Unsetenv("WORKER_CONCURRENCY")
		}
	})
	os.Unsetenv("WORKER_CONCURRENCY")
	n, err := workerConcurrency()
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	for _, raw := range []string{"", "0", "-1", "abc", "99999999999999999999999"} {
		t.Setenv("WORKER_CONCURRENCY", raw)
		if _, err := workerConcurrency(); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	t.Setenv("WORKER_CONCURRENCY", "1")
	n, err = workerConcurrency()
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
