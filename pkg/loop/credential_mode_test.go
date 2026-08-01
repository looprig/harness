package loop

import "testing"

func TestCredentialModeValues(t *testing.T) {
	tests := []struct {
		name string
		got  CredentialMode
		want CredentialMode
	}{
		{name: "gateway-backed", got: CredentialGatewayBacked, want: "gateway-backed"},
		{name: "native-auth", got: CredentialNativeAuth, want: "native-auth"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("credential mode = %q, want %q", tt.got, tt.want)
			}
		})
	}
}
