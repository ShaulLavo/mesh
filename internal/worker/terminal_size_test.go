package worker

import "testing"

func TestReadSessionLeaderTerminalSizeRejectsInvalidPID(t *testing.T) {
	for _, pid := range []int{-1, 0} {
		cols, rows, ok := ReadSessionLeaderTerminalSize(pid)
		if ok || cols != 0 || rows != 0 {
			t.Fatalf("ReadSessionLeaderTerminalSize(%d) = %dx%d, %t; want unavailable", pid, cols, rows, ok)
		}
	}
}

func TestObservedTerminalSizeRequiresPositiveBoundedDimensions(t *testing.T) {
	tests := []struct {
		name       string
		cols, rows int
		wantOK     bool
	}{
		{name: "smallest", cols: 1, rows: 1, wantOK: true},
		{name: "largest cell count", cols: maxTerminalDimension, rows: maxTerminalCells / maxTerminalDimension, wantOK: true},
		{name: "zero columns", cols: 0, rows: 24},
		{name: "zero rows", cols: 80, rows: 0},
		{name: "negative columns", cols: -1, rows: 24},
		{name: "negative rows", cols: 80, rows: -1},
		{name: "columns too large", cols: maxTerminalDimension + 1, rows: 1},
		{name: "rows too large", cols: 1, rows: maxTerminalDimension + 1},
		{name: "too many cells", cols: maxTerminalDimension, rows: maxTerminalCells/maxTerminalDimension + 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cols, rows, ok := observedTerminalSize(test.cols, test.rows)
			if ok != test.wantOK {
				t.Fatalf("observedTerminalSize(%d, %d) ok = %t, want %t", test.cols, test.rows, ok, test.wantOK)
			}
			if ok {
				if cols != test.cols || rows != test.rows {
					t.Fatalf("observedTerminalSize(%d, %d) = %dx%d", test.cols, test.rows, cols, rows)
				}
			} else if cols != 0 || rows != 0 {
				t.Fatalf("invalid observedTerminalSize(%d, %d) = %dx%d, want zero values", test.cols, test.rows, cols, rows)
			}
		})
	}
}
