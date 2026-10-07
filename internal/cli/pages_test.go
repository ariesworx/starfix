package cli

import (
	"errors"
	"slices"
	"strconv"
	"testing"
)

// fakePages serves entries 1..total newest page first, size at a time
// (limit, when given, caps it), as a paged read does.
func fakePages(total, size int, calls *int) func(string, int) ([]int, string, int, error) {
	return func(before string, limit int) ([]int, string, int, error) {
		*calls++
		end := total
		if before != "" {
			n, err := strconv.Atoi(before)
			if err != nil {
				return nil, "", 0, errors.New("bad cursor")
			}
			end = n - 1
		}
		n := size
		if limit > 0 {
			n = min(n, limit)
		}
		start := max(end-n+1, 1)
		var out []int
		for i := start; i <= end; i++ {
			out = append(out, i)
		}
		earlier := ""
		if start > 1 {
			earlier = strconv.Itoa(start)
		}
		return out, earlier, total, nil
	}
}

func TestPages(t *testing.T) {
	tests := []struct {
		name               string
		total, size, want  int
		got                []int
		earlier, wantCalls int
	}{
		{name: "all in one page", total: 3, size: 10, got: []int{1, 2, 3}, wantCalls: 1},
		{name: "all over pages", total: 7, size: 3, got: []int{1, 2, 3, 4, 5, 6, 7}, wantCalls: 3},
		{name: "newest few", total: 7, size: 3, want: 2, got: []int{6, 7}, earlier: 5, wantCalls: 1},
		{name: "newest across pages", total: 7, size: 3, want: 5, got: []int{3, 4, 5, 6, 7}, earlier: 2, wantCalls: 2},
		{name: "more than there are", total: 2, size: 3, want: 9, got: []int{1, 2}, wantCalls: 1},
		{name: "none", total: 0, size: 3, wantCalls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, earlier, err := pages(tc.want, fakePages(tc.total, tc.size, &calls))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.got) || earlier != tc.earlier || calls != tc.wantCalls {
				t.Errorf("pages(%d) over %d in pages of %d = %v, %d earlier, %d calls; want %v, %d, %d",
					tc.want, tc.total, tc.size, got, earlier, calls, tc.got, tc.earlier, tc.wantCalls)
			}
		})
	}
}
