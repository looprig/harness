package components

import "testing"

func TestInputBoxValueResetSetValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		set   string
		reset bool
		want  string
	}{
		{name: "set value", set: "hi", want: "hi"},
		{name: "set then reset", set: "hi", reset: true, want: ""},
		{name: "set empty", set: "", want: ""},
		{name: "set multiline", set: "a\nb", want: "a\nb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := NewInputBox()
			b.SetValue(tt.set)
			if tt.reset {
				b.Reset()
			}
			if got := b.Value(); got != tt.want {
				t.Errorf("Value() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInputBoxResizeView(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	b.Resize(40)
	if got := b.View(); got == "" {
		t.Error("View() = empty after Resize(40), want non-empty")
	}
}

func TestInputBoxFocus(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	if cmd := b.Focus(); cmd == nil {
		t.Error("Focus() = nil cmd, want non-nil (Blink)")
	}
}
