package claude

import (
	"errors"
	"testing"
)

func TestTranscriptPath(t *testing.T) {
	t.Parallel()
	const (
		home = "/home/u"
		sid  = "11111111-2222-3333-4444-555555555555"
	)
	const want = "/home/u/.claude/projects/-Users-x-y/11111111-2222-3333-4444-555555555555.jsonl"
	tests := []struct {
		name    string
		home    string
		cwd     string
		sid     string
		want    string
		wantErr bool
	}{
		{name: "happy path", home: home, cwd: "/Users/x/y", sid: sid, want: want},
		{name: "trailing slash normalizes to same path", home: home, cwd: "/Users/x/y/", sid: sid, want: want},
		{name: "sid with parent traversal rejected", home: home, cwd: "/Users/x/y", sid: "../etc/passwd", wantErr: true},
		{name: "sid with slash rejected", home: home, cwd: "/Users/x/y", sid: "a/b", wantErr: true},
		{name: "sid with backslash rejected", home: home, cwd: "/Users/x/y", sid: `a\b`, wantErr: true},
		{name: "non-hex sid rejected", home: home, cwd: "/Users/x/y", sid: "zzzzzzzz-2222-3333-4444-555555555555", wantErr: true},
		{name: "empty sid rejected", home: home, cwd: "/Users/x/y", sid: "", wantErr: true},
		{name: "uppercase hex sid accepted", home: home, cwd: "/Users/x/y", sid: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
			want: "/home/u/.claude/projects/-Users-x-y/AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE.jsonl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := transcriptPath(tt.home, tt.cwd, tt.sid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("transcriptPath() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var pe *PathError
				if !errors.As(err, &pe) {
					t.Fatalf("transcriptPath() err = %v, want *PathError", err)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("transcriptPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
