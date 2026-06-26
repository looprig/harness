package claude

import (
	"reflect"
	"testing"
)

func TestWhitelistEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		parent []string
		allow  []string
		extra  map[string]string
		want   []string
	}{
		{
			name:   "allow-listed keys pass through, secret excluded",
			parent: []string{"PATH=/usr/bin", "SECRET_TOKEN=shh", "HOME=/home/u"},
			allow:  []string{"PATH", "HOME"},
			extra:  nil,
			want:   []string{"PATH=/usr/bin", "HOME=/home/u"},
		},
		{
			name:   "extra credential appended after parent",
			parent: []string{"PATH=/usr/bin"},
			allow:  []string{"PATH"},
			extra:  map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
			want:   []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-test"},
		},
		{
			name:   "extra shadows parent key and wins",
			parent: []string{"PATH=/parent/bin", "HOME=/home/u"},
			allow:  []string{"PATH", "HOME"},
			extra:  map[string]string{"PATH": "/extra/bin"},
			want:   []string{"HOME=/home/u", "PATH=/extra/bin"},
		},
		{
			name:   "empty allow yields only extra (sorted)",
			parent: []string{"PATH=/usr/bin", "HOME=/home/u"},
			allow:  nil,
			extra:  map[string]string{"B": "2", "A": "1"},
			want:   []string{"A=1", "B=2"},
		},
		{
			name:   "value containing equals keeps everything after first equals",
			parent: []string{"LANG=en_US.UTF-8", "FOO=a=b=c"},
			allow:  []string{"FOO"},
			extra:  nil,
			want:   []string{"FOO=a=b=c"},
		},
		{
			name:   "parent element without equals is skipped",
			parent: []string{"BROKEN", "TERM=xterm"},
			allow:  []string{"BROKEN", "TERM"},
			extra:  nil,
			want:   []string{"TERM=xterm"},
		},
		{
			name:   "empty parent and empty extra yields empty",
			parent: nil,
			allow:  []string{"PATH"},
			extra:  nil,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := whitelistEnv(tt.parent, tt.allow, tt.extra)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("whitelistEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}
