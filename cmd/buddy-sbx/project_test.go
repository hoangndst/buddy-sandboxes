package main

import (
	"reflect"
	"testing"
)

func TestSplitRunArguments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		dash       int
		wantTarget string
		wantPath   string
		wantTask   []string
		wantErr    bool
	}{
		{name: "no project", args: []string{"codex"}, dash: -1, wantTarget: "codex"},
		{name: "project path", args: []string{"codex", "."}, dash: -1, wantTarget: "codex", wantPath: "."},
		{
			name: "task after separator", args: []string{"codex", ".", "Fix", "tests"}, dash: 2,
			wantTarget: "codex", wantPath: ".", wantTask: []string{"Fix", "tests"},
		},
		{
			name: "task without path", args: []string{"codex", "Fix", "tests"}, dash: 1,
			wantTarget: "codex", wantTask: []string{"Fix", "tests"},
		},
		{name: "reject extra positional", args: []string{"codex", ".", "Fix"}, dash: -1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, project, task, err := splitRunArguments(tc.args, tc.dash)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if target != tc.wantTarget || project != tc.wantPath || !reflect.DeepEqual(task, tc.wantTask) {
				t.Fatalf("got target=%q project=%q task=%q", target, project, task)
			}
		})
	}
}
