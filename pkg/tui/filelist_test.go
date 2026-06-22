package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveAtToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		wantPart  string
		wantMatch bool
	}{
		{name: "empty", input: "", wantMatch: false},
		{name: "no at", input: "hello world", wantMatch: false},
		{name: "bare at", input: "@", wantPart: "", wantMatch: true},
		{name: "at a name", input: "@Make", wantPart: "Make", wantMatch: true},
		{name: "at after text", input: "explain @main.go", wantPart: "main.go", wantMatch: true},
		{name: "at into a dir", input: "@src/fo", wantPart: "src/fo", wantMatch: true},
		{name: "finished by trailing space", input: "@Make ", wantMatch: false},
		{name: "finished by trailing newline", input: "@Make\n", wantMatch: false},
		{name: "at in the middle of a word is not a token", input: "user@host", wantMatch: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotPart, gotMatch := activeAtToken(tt.input)
			if gotMatch != tt.wantMatch || gotPart != tt.wantPart {
				t.Errorf("activeAtToken(%q) = (%q, %v), want (%q, %v)", tt.input, gotPart, gotMatch, tt.wantPart, tt.wantMatch)
			}
		})
	}
}

func TestSplitPartial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		partial           string
		wantDir, wantPref string
	}{
		{name: "empty lists cwd", partial: "", wantDir: ".", wantPref: ""},
		{name: "bare name", partial: "Make", wantDir: ".", wantPref: "Make"},
		{name: "into a dir", partial: "src/fo", wantDir: "src", wantPref: "fo"},
		{name: "trailing slash lists the dir", partial: "src/", wantDir: "src", wantPref: ""},
		{name: "root", partial: "/", wantDir: "/", wantPref: ""},
		{name: "absolute prefix", partial: "/etc/ho", wantDir: "/etc", wantPref: "ho"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotDir, gotPref := splitPartial(tt.partial)
			if gotDir != tt.wantDir || gotPref != tt.wantPref {
				t.Errorf("splitPartial(%q) = (%q, %q), want (%q, %q)", tt.partial, gotDir, gotPref, tt.wantDir, tt.wantPref)
			}
		})
	}
}

func TestListFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, f := range []string{"Makefile", "main.go", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	bases := func(part string) (names []string, dirs []bool) {
		for _, it := range listFiles(part) {
			names = append(names, filepath.Base(it.Path))
			dirs = append(dirs, it.IsDir)
		}
		return names, dirs
	}

	t.Run("lists dir, hides dotfiles, marks dirs", func(t *testing.T) {
		names, dirs := bases(dir + "/")
		want := []string{"Makefile", "main.go", "sub"} // sorted; .hidden excluded
		if len(names) != len(want) {
			t.Fatalf("names = %v, want %v", names, want)
		}
		for i := range want {
			if names[i] != want[i] {
				t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
			}
		}
		if dirs[len(dirs)-1] != true { // "sub" is the directory
			t.Errorf("expected last entry %q to be a directory", names[len(names)-1])
		}
	})

	t.Run("prefix filters case-insensitively", func(t *testing.T) {
		if names, _ := bases(dir + "/make"); len(names) != 1 || names[0] != "Makefile" {
			t.Errorf("bases(make) = %v, want [Makefile]", names)
		}
	})

	t.Run("dot prefix reveals dotfiles", func(t *testing.T) {
		if names, _ := bases(dir + "/."); len(names) != 1 || names[0] != ".hidden" {
			t.Errorf("bases(.) = %v, want [.hidden]", names)
		}
	})

	t.Run("missing dir yields nothing", func(t *testing.T) {
		if got := listFiles(filepath.Join(dir, "nope") + "/"); got != nil {
			t.Errorf("listFiles(missing) = %v, want nil", got)
		}
	})
}
