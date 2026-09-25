package scheduler

import "testing"

func TestIsDoneState(t *testing.T) {
	done := []string{"pausedUP", "stalledUP", "uploading", "checkingUP", "queuedUP"}
	for _, s := range done {
		if !isDoneState(s) {
			t.Errorf("expected %q to be done", s)
		}
	}
	notDone := []string{"downloading", "stalledDL", "metaDL", "checkingDL", "queuedDL", "forcedDL", "forcedUP", "missingFiles", "error", ""}
	for _, s := range notDone {
		if isDoneState(s) {
			t.Errorf("expected %q NOT to be done", s)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{1024 * 1024 * 1024 * 1024, "1.00 TB"},
	}
	for _, c := range cases {
		got := humanSize(c.in)
		if got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
