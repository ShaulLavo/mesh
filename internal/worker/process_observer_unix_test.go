//go:build linux || darwin

package worker

import (
	"reflect"
	"testing"
)

func TestOrderedLiveProcessGroupMemberIDs(t *testing.T) {
	tests := []struct {
		name      string
		groupID   int
		processes []observedProcessState
		want      []int
	}{
		{
			name:    "live group leader first",
			groupID: 100,
			processes: []observedProcessState{
				{pid: 103, groupID: 100, alive: true},
				{pid: 100, groupID: 100, alive: true},
				{pid: 102, groupID: 100, alive: true},
			},
			want: []int{100, 102, 103},
		},
		{
			name:    "surviving member replaces dead group leader",
			groupID: 100,
			processes: []observedProcessState{
				{pid: 100, groupID: 100, alive: false},
				{pid: 103, groupID: 100, alive: true},
				{pid: 102, groupID: 100, alive: true},
				{pid: 99, groupID: 99, alive: true},
				{pid: 102, groupID: 100, alive: true},
			},
			want: []int{102, 103},
		},
		{name: "invalid group", processes: []observedProcessState{{pid: 1, groupID: 1, alive: true}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := orderedLiveProcessGroupMemberIDs(test.groupID, test.processes); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ordered group members = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParsePSProcessStates(t *testing.T) {
	output := " 100 100 Z+\n 103 100 S+\nmalformed\n 102 100 R\n 0 100 S\n 104 nope S\n"
	want := []observedProcessState{
		{pid: 100, groupID: 100, alive: false},
		{pid: 103, groupID: 100, alive: true},
		{pid: 102, groupID: 100, alive: true},
	}
	if got := parsePSProcessStates(output); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed ps states = %#v, want %#v", got, want)
	}
}

func TestAppendSessionLeaderFallback(t *testing.T) {
	if got := appendSessionLeaderFallback([]int{102}, 10); !reflect.DeepEqual(got, []int{102, 10}) {
		t.Fatalf("candidates with fallback = %v", got)
	}
	if got := appendSessionLeaderFallback([]int{10, 102}, 10); !reflect.DeepEqual(got, []int{10, 102}) {
		t.Fatalf("candidates with duplicate fallback = %v", got)
	}
}
