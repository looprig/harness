package gate_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/gate"
)

func TestReviewEnums(t *testing.T) {
	t.Parallel()

	t.Run("risk is a closed domain", func(t *testing.T) {
		t.Parallel()
		inputs := []string{"low", "medium", "high", "critical"}
		want := []gate.ReviewRisk{
			gate.ReviewRiskLow,
			gate.ReviewRiskMedium,
			gate.ReviewRiskHigh,
			gate.ReviewRiskCritical,
		}
		got := make([]gate.ReviewRisk, 0, len(inputs))
		for _, input := range inputs {
			value, ok := gate.ParseReviewRisk(input)
			if !ok {
				t.Fatalf("ParseReviewRisk(%q) ok = false, want true", input)
			}
			got = append(got, value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed risks = %v, want %v", got, want)
		}
		assertRejectedRisk(t, "")
		assertRejectedRisk(t, "LOW")
		assertRejectedRisk(t, "unknown-value")
	})

	t.Run("authorization is a closed domain", func(t *testing.T) {
		t.Parallel()
		inputs := []string{"unknown", "low", "medium", "high"}
		want := []gate.ReviewAuthorization{
			gate.ReviewAuthorizationUnknown,
			gate.ReviewAuthorizationLow,
			gate.ReviewAuthorizationMedium,
			gate.ReviewAuthorizationHigh,
		}
		got := make([]gate.ReviewAuthorization, 0, len(inputs))
		for _, input := range inputs {
			value, ok := gate.ParseReviewAuthorization(input)
			if !ok {
				t.Fatalf("ParseReviewAuthorization(%q) ok = false, want true", input)
			}
			got = append(got, value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed authorizations = %v, want %v", got, want)
		}
		assertRejectedAuthorization(t, "")
		assertRejectedAuthorization(t, "UNKNOWN")
		assertRejectedAuthorization(t, "unknown-value")
	})

	t.Run("recommendation is a closed domain", func(t *testing.T) {
		t.Parallel()
		inputs := []string{"allow", "needs_human"}
		want := []gate.ReviewRecommendation{
			gate.ReviewAllow,
			gate.ReviewNeedsHuman,
		}
		got := make([]gate.ReviewRecommendation, 0, len(inputs))
		for _, input := range inputs {
			value, ok := gate.ParseReviewRecommendation(input)
			if !ok {
				t.Fatalf("ParseReviewRecommendation(%q) ok = false, want true", input)
			}
			got = append(got, value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed recommendations = %v, want %v", got, want)
		}
		assertRejectedRecommendation(t, "")
		assertRejectedRecommendation(t, "approve")
		assertRejectedRecommendation(t, "unknown-value")
	})

	t.Run("status is a closed domain", func(t *testing.T) {
		t.Parallel()
		inputs := []string{
			"allowed",
			"needs_human",
			"not_applicable",
			"timed_out",
			"failed",
			"cancelled",
			"stale",
		}
		want := []gate.ReviewStatus{
			gate.ReviewStatusAllowed,
			gate.ReviewStatusNeedsHuman,
			gate.ReviewStatusNotApplicable,
			gate.ReviewStatusTimedOut,
			gate.ReviewStatusFailed,
			gate.ReviewStatusCancelled,
			gate.ReviewStatusStale,
		}
		got := make([]gate.ReviewStatus, 0, len(inputs))
		for _, input := range inputs {
			value, ok := gate.ParseReviewStatus(input)
			if !ok {
				t.Fatalf("ParseReviewStatus(%q) ok = false, want true", input)
			}
			got = append(got, value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed statuses = %v, want %v", got, want)
		}
		assertRejectedStatus(t, "")
		assertRejectedStatus(t, "approved")
		assertRejectedStatus(t, "unknown-value")
	})

	t.Run("risk category is a closed domain", func(t *testing.T) {
		t.Parallel()
		inputs := []string{
			"data_exfiltration",
			"credential_access",
			"credential_probing",
			"destructive_local",
			"destructive_shared",
			"persistent_security_weakening",
			"production_mutation",
			"protected_source_control",
			"untrusted_code_execution",
			"mutable_network",
			"prompt_injection",
			"authorization_conflict",
			"target_ambiguity",
			"insufficient_evidence",
		}
		want := allReviewCategories()
		got := make([]gate.ReviewRiskCategory, 0, len(inputs))
		for _, input := range inputs {
			value, ok := gate.ParseReviewRiskCategory(input)
			if !ok {
				t.Fatalf("ParseReviewRiskCategory(%q) ok = false, want true", input)
			}
			got = append(got, value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed categories = %v, want %v", got, want)
		}
		assertRejectedCategory(t, "")
		assertRejectedCategory(t, "network")
		assertRejectedCategory(t, "unknown-value")
	})
}

func TestReviewCategories(t *testing.T) {
	t.Parallel()

	if got := len(allReviewCategories()); got != gate.MaxReviewCategories {
		t.Fatalf("closed category domain has %d values, want MaxReviewCategories %d", got, gate.MaxReviewCategories)
	}

	tests := []struct {
		name       string
		categories []gate.ReviewRiskCategory
		wantReason gate.ReviewValidationReason
	}{
		{name: "empty is valid", categories: nil},
		{name: "one is valid", categories: []gate.ReviewRiskCategory{gate.ReviewCategoryMutableNetwork}},
		{name: "maximum closed domain is valid", categories: allReviewCategories()},
		{
			name: "duplicate is rejected",
			categories: []gate.ReviewRiskCategory{
				gate.ReviewCategoryMutableNetwork,
				gate.ReviewCategoryMutableNetwork,
			},
			wantReason: gate.ReviewValidationDuplicate,
		},
		{
			name:       "unknown is rejected",
			categories: []gate.ReviewRiskCategory{"untrusted-raw-category"},
			wantReason: gate.ReviewValidationUnsupported,
		},
		{
			name:       "zero is rejected",
			categories: []gate.ReviewRiskCategory{""},
			wantReason: gate.ReviewValidationUnsupported,
		},
		{
			name:       "over maximum is rejected before content inspection",
			categories: make([]gate.ReviewRiskCategory, gate.MaxReviewCategories+1),
			wantReason: gate.ReviewValidationTooMany,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := gate.ValidateReviewCategories(tt.categories)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("ValidateReviewCategories() error = %v, want nil", err)
				}
				return
			}
			var validationErr *gate.ReviewValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateReviewCategories() error = %T, want *gate.ReviewValidationError", err)
			}
			if validationErr.Field != gate.ReviewValidationFieldCategories {
				t.Errorf("error Field = %q, want %q", validationErr.Field, gate.ReviewValidationFieldCategories)
			}
			if validationErr.Reason != tt.wantReason {
				t.Errorf("error Reason = %q, want %q", validationErr.Reason, tt.wantReason)
			}
			if strings.Contains(err.Error(), "untrusted-raw-category") {
				t.Errorf("error leaks untrusted category: %q", err)
			}
		})
	}
}

func allReviewCategories() []gate.ReviewRiskCategory {
	return []gate.ReviewRiskCategory{
		gate.ReviewCategoryDataExfiltration,
		gate.ReviewCategoryCredentialAccess,
		gate.ReviewCategoryCredentialProbing,
		gate.ReviewCategoryDestructiveLocal,
		gate.ReviewCategoryDestructiveShared,
		gate.ReviewCategoryPersistentSecurityWeakening,
		gate.ReviewCategoryProductionMutation,
		gate.ReviewCategoryProtectedSourceControl,
		gate.ReviewCategoryUntrustedCodeExecution,
		gate.ReviewCategoryMutableNetwork,
		gate.ReviewCategoryPromptInjection,
		gate.ReviewCategoryAuthorizationConflict,
		gate.ReviewCategoryTargetAmbiguity,
		gate.ReviewCategoryInsufficientEvidence,
	}
}

func assertRejectedRisk(t *testing.T, input string) {
	t.Helper()
	if got, ok := gate.ParseReviewRisk(input); ok || got != "" {
		t.Errorf("ParseReviewRisk(%q) = (%q, %t), want zero, false", input, got, ok)
	}
}

func assertRejectedAuthorization(t *testing.T, input string) {
	t.Helper()
	if got, ok := gate.ParseReviewAuthorization(input); ok || got != "" {
		t.Errorf("ParseReviewAuthorization(%q) = (%q, %t), want zero, false", input, got, ok)
	}
}

func assertRejectedRecommendation(t *testing.T, input string) {
	t.Helper()
	if got, ok := gate.ParseReviewRecommendation(input); ok || got != "" {
		t.Errorf("ParseReviewRecommendation(%q) = (%q, %t), want zero, false", input, got, ok)
	}
}

func assertRejectedStatus(t *testing.T, input string) {
	t.Helper()
	if got, ok := gate.ParseReviewStatus(input); ok || got != "" {
		t.Errorf("ParseReviewStatus(%q) = (%q, %t), want zero, false", input, got, ok)
	}
}

func assertRejectedCategory(t *testing.T, input string) {
	t.Helper()
	if got, ok := gate.ParseReviewRiskCategory(input); ok || got != "" {
		t.Errorf("ParseReviewRiskCategory(%q) = (%q, %t), want zero, false", input, got, ok)
	}
}
