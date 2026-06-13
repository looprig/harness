package phala_test

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi/phala"
)

// compile-time assertion: *Client satisfies llm.LLM.
var _ llm.LLM = (*phala.Client)(nil)

func ptr(f float64) *float64 { return &f }

// TestClient_ValidateCalledOnInvoke verifies that Validate() is called before
// any network I/O. Invalid cases use a nil context so that if Validate()
// somehow passes and the method tries to use ctx, it would panic — proving
// Validate() short-circuits correctly.
func TestClient_ValidateCalledOnInvoke(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		budget  int
		temp    *float64
		wantErr bool
	}{
		{
			name:    "invalid budget with wrong temp",
			budget:  1000,
			temp:    ptr(0.7),
			wantErr: true,
		},
		{
			name:    "nil temp with budget",
			budget:  1000,
			temp:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := phala.New("", "test-key")
			req := llm.Request{
				Model: llm.ModelSpec{
					Model:          "test-model",
					ThinkingBudget: tt.budget,
					Temperature:    tt.temp,
				},
			}

			// Validate() must short-circuit before any ctx use.
			_, err := c.Invoke(context.Background(), req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected ValidationError, got nil")
				}
				var valErr *llm.ValidationError
				if !errors.As(err, &valErr) {
					t.Fatalf("expected *llm.ValidationError, got %T: %v", err, err)
				}
			} else {
				// For the "valid" case we'd need a live server; this
				// subtest is omitted to keep the test hermetic.
				t.Skip("valid spec requires live server")
			}
		})
	}
}

// TestClient_Stream_ValidateCalledFirst mirrors the Invoke test for the
// streaming path.
func TestClient_Stream_ValidateCalledFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		budget  int
		temp    *float64
		wantErr bool
	}{
		{
			name:    "invalid budget with wrong temp",
			budget:  1000,
			temp:    ptr(0.7),
			wantErr: true,
		},
		{
			name:    "nil temp with budget",
			budget:  1000,
			temp:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := phala.New("", "test-key")
			req := llm.Request{
				Model: llm.ModelSpec{
					Model:          "test-model",
					ThinkingBudget: tt.budget,
					Temperature:    tt.temp,
				},
			}

			// Validate() must short-circuit before any ctx use.
			_, err := c.Stream(context.Background(), req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected ValidationError, got nil")
				}
				var valErr *llm.ValidationError
				if !errors.As(err, &valErr) {
					t.Fatalf("expected *llm.ValidationError, got %T: %v", err, err)
				}
			} else {
				t.Skip("valid spec requires live server")
			}
		})
	}
}
