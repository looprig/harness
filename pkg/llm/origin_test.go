package llm

import "testing"

func TestOriginZeroValueIsCustom(t *testing.T) {
	t.Parallel()
	var o Origin
	if o != OriginCustom {
		t.Fatalf("zero Origin = %v, want OriginCustom (fail-safe)", o)
	}
}

func TestOriginString(t *testing.T) {
	tests := []struct {
		name string
		o    Origin
		want string
	}{
		{name: "custom", o: OriginCustom, want: "custom"},
		{name: "catalog", o: OriginCatalog, want: "catalog"},
		{name: "unknown falls back to custom", o: Origin(99), want: "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.o.String(); got != tt.want {
				t.Errorf("Origin(%d).String() = %q, want %q", tt.o, got, tt.want)
			}
		})
	}
}
