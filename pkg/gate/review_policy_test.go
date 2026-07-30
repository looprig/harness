package gate_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/gate"
)

func TestPermissionReviewDecisionReasonClosedDomain(t *testing.T) {
	t.Parallel()
	values := []gate.ReviewDecisionReason{
		gate.ReviewDecisionEligible,
		gate.ReviewDecisionInvalidPolicy,
		gate.ReviewDecisionInvalidAssessment,
		gate.ReviewDecisionBasisMismatch,
		gate.ReviewDecisionRecommendation,
		gate.ReviewDecisionRiskCeiling,
		gate.ReviewDecisionAuthorization,
		gate.ReviewDecisionAbsoluteHuman,
		gate.ReviewDecisionMaterialTruncation,
		gate.ReviewDecisionNoApplicableClassifier,
		gate.ReviewDecisionClassifierStatus,
	}
	for _, value := range values {
		value := value
		t.Run(string(value), func(t *testing.T) {
			t.Parallel()
			if !value.Valid() {
				t.Fatalf("%q.Valid() = false", value)
			}
			if parsed, ok := gate.ParseReviewDecisionReason(string(value)); !ok || parsed != value {
				t.Fatalf("ParseReviewDecisionReason(%q) = (%q,%v)", value, parsed, ok)
			}
		})
	}
	for _, value := range []gate.ReviewDecisionReason{"", "unknown", "ELIGIBLE"} {
		if value.Valid() {
			t.Fatalf("%q.Valid() = true", value)
		}
		if _, ok := gate.ParseReviewDecisionReason(string(value)); ok {
			t.Fatalf("ParseReviewDecisionReason(%q) succeeded", value)
		}
	}
}

func TestPermissionReviewDefaultPolicyMatrix(t *testing.T) {
	t.Parallel()
	policy := mustDefaultPermissionReviewPolicy(t, "gate-policy-v1")
	risks := []gate.ReviewRisk{
		gate.ReviewRiskLow,
		gate.ReviewRiskMedium,
		gate.ReviewRiskHigh,
		gate.ReviewRiskCritical,
	}
	authorizations := []gate.ReviewAuthorization{
		gate.ReviewAuthorizationUnknown,
		gate.ReviewAuthorizationLow,
		gate.ReviewAuthorizationMedium,
		gate.ReviewAuthorizationHigh,
	}
	recommendations := []gate.ReviewRecommendation{
		gate.ReviewAllow,
		gate.ReviewNeedsHuman,
	}
	for _, risk := range risks {
		for _, authorization := range authorizations {
			for _, recommendation := range recommendations {
				risk, authorization, recommendation := risk, authorization, recommendation
				name := string(risk) + "/" + string(authorization) + "/" + string(recommendation)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					subject := validPermissionReviewSubject(t)
					assessment := validPermissionAssessment(subject, risk, authorization, recommendation)
					got := gate.EvaluatePermissionAssessment(policy, subject, assessment)
					wantEligible := recommendation == gate.ReviewAllow &&
						risk != gate.ReviewRiskCritical &&
						(risk != gate.ReviewRiskHigh ||
							authorization == gate.ReviewAuthorizationMedium ||
							authorization == gate.ReviewAuthorizationHigh)
					if got.Eligible != wantEligible {
						t.Fatalf("Eligible = %v, want %v (reason %q)", got.Eligible, wantEligible, got.Reason)
					}
					if wantEligible && got.Reason != gate.ReviewDecisionEligible {
						t.Fatalf("Reason = %q, want eligible", got.Reason)
					}
					if recommendation == gate.ReviewNeedsHuman && got.Reason != gate.ReviewDecisionRecommendation {
						t.Fatalf("Reason = %q, want recommendation", got.Reason)
					}
				})
			}
		}
	}
}

func TestPermissionReviewPolicyUsesClosedDomainOrdering(t *testing.T) {
	t.Parallel()
	risks := []gate.ReviewRisk{
		gate.ReviewRiskLow,
		gate.ReviewRiskMedium,
		gate.ReviewRiskHigh,
		gate.ReviewRiskCritical,
	}
	maximums := []gate.ReviewRisk{
		gate.ReviewRiskLow,
		gate.ReviewRiskMedium,
		gate.ReviewRiskHigh,
	}
	for maximumIndex, maximum := range maximums {
		policy, err := gate.NewPermissionReviewPolicy(
			"gate-policy-v1",
			maximum,
			map[gate.ReviewRisk]gate.ReviewAuthorization{
				gate.ReviewRiskLow:    gate.ReviewAuthorizationUnknown,
				gate.ReviewRiskMedium: gate.ReviewAuthorizationUnknown,
				gate.ReviewRiskHigh:   gate.ReviewAuthorizationMedium,
			},
			nil,
			0,
		)
		if err != nil {
			t.Fatalf("NewPermissionReviewPolicy(maximum %q) error = %v", maximum, err)
		}
		for riskIndex, risk := range risks {
			subject := validPermissionReviewSubject(t)
			assessment := validPermissionAssessment(
				subject,
				risk,
				gate.ReviewAuthorizationHigh,
				gate.ReviewAllow,
			)
			got := gate.EvaluatePermissionAssessment(policy, subject, assessment)
			wantEligible := risk != gate.ReviewRiskCritical && riskIndex <= maximumIndex
			if got.Eligible != wantEligible {
				t.Fatalf("risk %q with maximum %q: Eligible = %v, want %v", risk, maximum, got.Eligible, wantEligible)
			}
		}
	}

	authorizations := []gate.ReviewAuthorization{
		gate.ReviewAuthorizationUnknown,
		gate.ReviewAuthorizationLow,
		gate.ReviewAuthorizationMedium,
		gate.ReviewAuthorizationHigh,
	}
	// Exercised against ReviewRiskMedium (not Low): the policy's own shape
	// validation now requires MinimumAuthorization[Medium] >=
	// MinimumAuthorization[Low] (in addition to the pre-existing High >=
	// Medium), so Medium is the risk tier whose minimum can range across the
	// full authorization domain here while Low (fixed at Unknown, rank 0)
	// and High (fixed at High, rank 3) stay outside that range.
	for minimumIndex, minimum := range authorizations {
		policy, err := gate.NewPermissionReviewPolicy(
			"gate-policy-v1",
			gate.ReviewRiskHigh,
			map[gate.ReviewRisk]gate.ReviewAuthorization{
				gate.ReviewRiskLow:    gate.ReviewAuthorizationUnknown,
				gate.ReviewRiskMedium: minimum,
				gate.ReviewRiskHigh:   gate.ReviewAuthorizationHigh,
			},
			nil,
			0,
		)
		if err != nil {
			t.Fatalf("NewPermissionReviewPolicy(minimum %q) error = %v", minimum, err)
		}
		for authorizationIndex, authorization := range authorizations {
			subject := validPermissionReviewSubject(t)
			assessment := validPermissionAssessment(
				subject,
				gate.ReviewRiskMedium,
				authorization,
				gate.ReviewAllow,
			)
			got := gate.EvaluatePermissionAssessment(policy, subject, assessment)
			wantEligible := authorizationIndex >= minimumIndex
			if got.Eligible != wantEligible {
				t.Fatalf("authorization %q with minimum %q: Eligible = %v, want %v", authorization, minimum, got.Eligible, wantEligible)
			}
		}
	}
}

func TestPermissionReviewPolicyConstructionAndOwnership(t *testing.T) {
	t.Parallel()
	minimum := map[gate.ReviewRisk]gate.ReviewAuthorization{
		gate.ReviewRiskLow:    gate.ReviewAuthorizationLow,
		gate.ReviewRiskMedium: gate.ReviewAuthorizationMedium,
		gate.ReviewRiskHigh:   gate.ReviewAuthorizationHigh,
	}
	absolute := []gate.ReviewRiskCategory{gate.ReviewCategoryCredentialAccess}
	policy, err := gate.NewPermissionReviewPolicy(
		" policy-v1 ",
		gate.ReviewRiskMedium,
		minimum,
		absolute,
		gate.ReviewTruncationToolEntry,
	)
	if err != nil {
		t.Fatalf("NewPermissionReviewPolicy() error = %v", err)
	}
	if policy.Revision != " policy-v1 " {
		t.Fatalf("Revision = %q, want exact spelling", policy.Revision)
	}
	minimum[gate.ReviewRiskLow] = gate.ReviewAuthorizationUnknown
	absolute[0] = gate.ReviewCategoryTargetAmbiguity
	if policy.MinimumAuthorization[gate.ReviewRiskLow] != gate.ReviewAuthorizationLow ||
		policy.AbsoluteHuman[0] != gate.ReviewCategoryCredentialAccess {
		t.Fatal("constructed policy aliases inputs")
	}
}

// TestPermissionReviewPolicySealed proves the Sealed() developer-experience
// accessor: a hand-built literal PermissionReviewPolicy{} (bypassing
// NewPermissionReviewPolicy/DefaultPermissionReviewPolicy) reports false, so
// a consumer boundary (e.g. rig.WithPermissionReviewPolicy) can fail fast at
// configuration time instead of discovering the same zero seal later at
// EvaluatePermissionAssessment, which already fails closed on it regardless.
func TestPermissionReviewPolicySealed(t *testing.T) {
	t.Parallel()
	var zero gate.PermissionReviewPolicy
	if zero.Sealed() {
		t.Fatal("zero-value PermissionReviewPolicy{}.Sealed() = true, want false")
	}
	constructed, err := gate.DefaultPermissionReviewPolicy("policy-v1")
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy: %v", err)
	}
	if !constructed.Sealed() {
		t.Fatal("constructor-built PermissionReviewPolicy.Sealed() = false, want true")
	}
}

func TestPermissionReviewPolicyRejectsInvalidAndRelaxedValues(t *testing.T) {
	t.Parallel()
	validMinimum := func() map[gate.ReviewRisk]gate.ReviewAuthorization {
		return map[gate.ReviewRisk]gate.ReviewAuthorization{
			gate.ReviewRiskLow:    gate.ReviewAuthorizationUnknown,
			gate.ReviewRiskMedium: gate.ReviewAuthorizationUnknown,
			gate.ReviewRiskHigh:   gate.ReviewAuthorizationMedium,
		}
	}
	tests := []struct {
		name     string
		revision string
		maximum  gate.ReviewRisk
		minimum  map[gate.ReviewRisk]gate.ReviewAuthorization
		absolute []gate.ReviewRiskCategory
		material gate.ReviewTruncationMask
	}{
		{name: "blank revision", revision: " ", maximum: gate.ReviewRiskHigh, minimum: validMinimum()},
		{name: "invalid utf8 revision", revision: string([]byte{0xff}), maximum: gate.ReviewRiskHigh, minimum: validMinimum()},
		{name: "long revision", revision: strings.Repeat("r", gate.MaxPermissionReviewPolicyRevisionBytes+1), maximum: gate.ReviewRiskHigh, minimum: validMinimum()},
		{name: "critical maximum", revision: "r", maximum: gate.ReviewRiskCritical, minimum: validMinimum()},
		{name: "unknown maximum", revision: "r", maximum: "", minimum: validMinimum()},
		{name: "nil minimum", revision: "r", maximum: gate.ReviewRiskHigh},
		{name: "missing low", revision: "r", maximum: gate.ReviewRiskHigh, minimum: map[gate.ReviewRisk]gate.ReviewAuthorization{gate.ReviewRiskMedium: gate.ReviewAuthorizationUnknown, gate.ReviewRiskHigh: gate.ReviewAuthorizationMedium}},
		{name: "extra critical", revision: "r", maximum: gate.ReviewRiskHigh, minimum: func() map[gate.ReviewRisk]gate.ReviewAuthorization {
			value := validMinimum()
			value[gate.ReviewRiskCritical] = gate.ReviewAuthorizationHigh
			return value
		}()},
		{name: "unknown authorization", revision: "r", maximum: gate.ReviewRiskHigh, minimum: func() map[gate.ReviewRisk]gate.ReviewAuthorization {
			value := validMinimum()
			value[gate.ReviewRiskLow] = ""
			return value
		}()},
		{name: "relaxed high authorization", revision: "r", maximum: gate.ReviewRiskHigh, minimum: func() map[gate.ReviewRisk]gate.ReviewAuthorization {
			value := validMinimum()
			value[gate.ReviewRiskHigh] = gate.ReviewAuthorizationLow
			return value
		}()},
		{name: "high less than medium", revision: "r", maximum: gate.ReviewRiskHigh, minimum: map[gate.ReviewRisk]gate.ReviewAuthorization{
			gate.ReviewRiskLow: gate.ReviewAuthorizationUnknown, gate.ReviewRiskMedium: gate.ReviewAuthorizationHigh, gate.ReviewRiskHigh: gate.ReviewAuthorizationMedium,
		}},
		{name: "medium less than low", revision: "r", maximum: gate.ReviewRiskHigh, minimum: map[gate.ReviewRisk]gate.ReviewAuthorization{
			gate.ReviewRiskLow: gate.ReviewAuthorizationHigh, gate.ReviewRiskMedium: gate.ReviewAuthorizationLow, gate.ReviewRiskHigh: gate.ReviewAuthorizationHigh,
		}},
		{name: "duplicate absolute", revision: "r", maximum: gate.ReviewRiskHigh, minimum: validMinimum(), absolute: []gate.ReviewRiskCategory{gate.ReviewCategoryCredentialAccess, gate.ReviewCategoryCredentialAccess}},
		{name: "invalid absolute", revision: "r", maximum: gate.ReviewRiskHigh, minimum: validMinimum(), absolute: []gate.ReviewRiskCategory{"other"}},
		{name: "unsupported material", revision: "r", maximum: gate.ReviewRiskHigh, minimum: validMinimum(), material: 1 << 15},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := gate.NewPermissionReviewPolicy(tt.revision, tt.maximum, tt.minimum, tt.absolute, tt.material); err == nil {
				t.Fatal("NewPermissionReviewPolicy() error = nil")
			}
		})
	}
}

func TestPermissionReviewAssessmentValidationAndPolicy(t *testing.T) {
	t.Parallel()
	policy := mustDefaultPermissionReviewPolicy(t, "gate-policy-v1")
	tests := []struct {
		name   string
		mutate func(*gate.PermissionReviewSubject, *gate.PermissionAssessment, *gate.PermissionReviewPolicy)
		reason gate.ReviewDecisionReason
	}{
		{name: "forged subject digest", mutate: func(s *gate.PermissionReviewSubject, _ *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			s.Basis.SubjectDigest[0] ^= 1
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "basis mismatch", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Basis.ClassifierRevision = "other"
		}, reason: gate.ReviewDecisionBasisMismatch},
		{name: "policy revision mismatch", mutate: func(s *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			s.Basis.GatePolicyRevision = "other"
			a.Basis = s.Basis
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "invalid risk", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Risk = "other"
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "invalid authorization", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Authorization = "other"
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "invalid recommendation", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Recommendation = "other"
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "duplicate category", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Categories = []gate.ReviewRiskCategory{gate.ReviewCategoryCredentialAccess, gate.ReviewCategoryCredentialAccess}
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "invalid utf8 rationale", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Rationale = string([]byte{0xff})
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "long rationale", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Rationale = strings.Repeat("x", gate.MaxPermissionReviewRationaleBytes+1)
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "non-low blank rationale", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Risk = gate.ReviewRiskMedium
			a.Rationale = " "
		}, reason: gate.ReviewDecisionInvalidAssessment},
		{name: "absolute category", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, p *gate.PermissionReviewPolicy) {
			a.Categories = []gate.ReviewRiskCategory{gate.ReviewCategoryCredentialAccess}
			p.AbsoluteHuman = []gate.ReviewRiskCategory{gate.ReviewCategoryCredentialAccess}
		}, reason: gate.ReviewDecisionInvalidPolicy},
		{name: "critical", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Risk = gate.ReviewRiskCritical
			a.Rationale = "critical risk"
		}, reason: gate.ReviewDecisionRiskCeiling},
		{name: "over maximum", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, p *gate.PermissionReviewPolicy) {
			p.MaximumAutoRisk = gate.ReviewRiskLow
			a.Risk = gate.ReviewRiskMedium
			a.Rationale = "medium risk"
		}, reason: gate.ReviewDecisionInvalidPolicy},
		{name: "authorization", mutate: func(_ *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			a.Risk = gate.ReviewRiskHigh
			a.Authorization = gate.ReviewAuthorizationLow
			a.Rationale = "high risk"
		}, reason: gate.ReviewDecisionAuthorization},
		{name: "intrinsic material", mutate: func(s *gate.PermissionReviewSubject, a *gate.PermissionAssessment, _ *gate.PermissionReviewPolicy) {
			s.Context.Entries[0].Content = "p\n…[review context truncated]…\ns"
			s.Context.Entries[0].Truncated = true
			s.Context.Truncation.Applied = gate.ReviewTruncationUserEntry
			s.Context.Truncation.Material = gate.ReviewTruncationUserEntry
			digest, err := gate.SubjectDigest(*s)
			if err != nil {
				panic(err)
			}
			s.Basis.SubjectDigest = digest
			a.Basis = s.Basis
		}, reason: gate.ReviewDecisionMaterialTruncation},
		{name: "additional material", mutate: func(s *gate.PermissionReviewSubject, a *gate.PermissionAssessment, p *gate.PermissionReviewPolicy) {
			s.Context.Truncation.Applied = gate.ReviewTruncationAssistantEntry
			s.Context.Entries = append([]gate.ReviewContextEntry{{
				Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantMessage,
				Content: "p\n…[review context truncated]…\ns", Truncated: true,
			}}, s.Context.Entries...)
			digest, err := gate.SubjectDigest(*s)
			if err != nil {
				panic(err)
			}
			s.Basis.SubjectDigest = digest
			a.Basis = s.Basis
			p.MaterialTruncation = gate.ReviewTruncationAssistantEntry
		}, reason: gate.ReviewDecisionInvalidPolicy},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subject := validPermissionReviewSubject(t)
			assessment := validPermissionAssessment(subject, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow)
			localPolicy := cloneReviewPolicy(policy)
			tt.mutate(&subject, &assessment, &localPolicy)
			got := gate.EvaluatePermissionAssessment(localPolicy, subject, assessment)
			if got.Eligible || got.Reason != tt.reason {
				t.Fatalf("decision = %#v, want false/%q", got, tt.reason)
			}
			if assessment.Rationale != "" && strings.Contains(string(got.Reason), assessment.Rationale) {
				t.Fatal("decision reason leaked rationale")
			}
		})
	}
}

func TestPermissionReviewAssessmentCannotStampMalformedContext(t *testing.T) {
	t.Parallel()

	subject := validPermissionReviewSubject(t)
	subject.Context.Entries[0].Content =
		"prefix\n…[review context truncated]…\nsuffix"
	subject.Context.Entries[0].Truncated = true
	subject.Context.Truncation.Applied = gate.ReviewTruncationUserEntry
	digest, err := gate.SubjectDigest(subject)
	if err == nil {
		subject.Basis.SubjectDigest = digest
	}
	assessment := validPermissionAssessment(
		subject,
		gate.ReviewRiskLow,
		gate.ReviewAuthorizationUnknown,
		gate.ReviewAllow,
	)
	got := gate.EvaluatePermissionAssessment(
		mustDefaultPermissionReviewPolicy(t, "gate-policy-v1"),
		subject,
		assessment,
	)
	if got.Eligible || got.Reason != gate.ReviewDecisionInvalidAssessment {
		t.Fatalf("decision = %#v, want invalid assessment", got)
	}
}

func TestPermissionReviewAssessmentRejectsUnexplainedTruncationMasks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*gate.ReviewContext)
	}{
		{name: "active action", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationActiveAction
		}},
		{name: "user", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationUserEntry
		}},
		{name: "assistant", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationAssistantEntry
		}},
		{name: "tool", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationToolEntry
		}},
		{name: "block", mutate: func(c *gate.ReviewContext) {
			c.Truncation.Applied = gate.ReviewTruncationBlock
		}},
		{name: "partial material", mutate: func(c *gate.ReviewContext) {
			c.Entries[0].Content = "p\n…[review context truncated]…\ns"
			c.Entries[0].Truncated = true
			c.Truncation.Applied = gate.ReviewTruncationUserEntry |
				gate.ReviewTruncationBlock
			c.Truncation.Material = gate.ReviewTruncationUserEntry
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subject := validPermissionReviewSubject(t)
			tt.mutate(&subject.Context)
			digest, err := gate.SubjectDigest(subject)
			if err == nil {
				subject.Basis.SubjectDigest = digest
			}
			assessment := validPermissionAssessment(
				subject,
				gate.ReviewRiskLow,
				gate.ReviewAuthorizationUnknown,
				gate.ReviewAllow,
			)
			got := gate.EvaluatePermissionAssessment(
				mustDefaultPermissionReviewPolicy(t, "gate-policy-v1"),
				subject,
				assessment,
			)
			if got.Eligible || got.Reason != gate.ReviewDecisionInvalidAssessment {
				t.Fatalf("decision = %#v, want invalid assessment", got)
			}
		})
	}
}

func TestPermissionReviewPolicyRevalidatesMutation(t *testing.T) {
	t.Parallel()
	policy := mustDefaultPermissionReviewPolicy(t, "gate-policy-v1")
	policy.MinimumAuthorization[gate.ReviewRiskCritical] = gate.ReviewAuthorizationHigh
	subject := validPermissionReviewSubject(t)
	got := gate.EvaluatePermissionAssessment(
		policy,
		subject,
		validPermissionAssessment(subject, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow),
	)
	if got.Eligible || got.Reason != gate.ReviewDecisionInvalidPolicy {
		t.Fatalf("decision = %#v, want invalid policy", got)
	}
}

func TestPermissionReviewPolicySealRejectsEveryPostConstructionMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*gate.PermissionReviewPolicy)
	}{
		{name: "revision", mutate: func(p *gate.PermissionReviewPolicy) { p.Revision = "gate-policy-v2" }},
		{name: "maximum weaker", mutate: func(p *gate.PermissionReviewPolicy) { p.MaximumAutoRisk = gate.ReviewRiskCritical }},
		{name: "maximum stronger", mutate: func(p *gate.PermissionReviewPolicy) { p.MaximumAutoRisk = gate.ReviewRiskLow }},
		{name: "minimum weaker", mutate: func(p *gate.PermissionReviewPolicy) {
			p.MinimumAuthorization[gate.ReviewRiskHigh] = gate.ReviewAuthorizationUnknown
		}},
		{name: "minimum stronger", mutate: func(p *gate.PermissionReviewPolicy) {
			p.MinimumAuthorization[gate.ReviewRiskLow] = gate.ReviewAuthorizationHigh
		}},
		{name: "absolute append", mutate: func(p *gate.PermissionReviewPolicy) {
			p.AbsoluteHuman = append(p.AbsoluteHuman, gate.ReviewCategoryCredentialAccess)
		}},
		{name: "absolute reorder", mutate: func(p *gate.PermissionReviewPolicy) {
			p.AbsoluteHuman[0], p.AbsoluteHuman[1] = p.AbsoluteHuman[1], p.AbsoluteHuman[0]
		}},
		{name: "absolute clear", mutate: func(p *gate.PermissionReviewPolicy) { p.AbsoluteHuman = nil }},
		{name: "material add", mutate: func(p *gate.PermissionReviewPolicy) {
			p.MaterialTruncation |= gate.ReviewTruncationAssistantEntry
		}},
		{name: "material clear", mutate: func(p *gate.PermissionReviewPolicy) { p.MaterialTruncation = 0 }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy, err := gate.NewPermissionReviewPolicy(
				"gate-policy-v1",
				gate.ReviewRiskHigh,
				defaultMinimumForTest(),
				[]gate.ReviewRiskCategory{
					gate.ReviewCategoryCredentialAccess,
					gate.ReviewCategoryDataExfiltration,
				},
				gate.ReviewTruncationUserEntry,
			)
			if err != nil {
				t.Fatalf("NewPermissionReviewPolicy() error = %v", err)
			}
			tt.mutate(&policy)
			subject := validPermissionReviewSubject(t)
			got := gate.EvaluatePermissionAssessment(
				policy,
				subject,
				validPermissionAssessment(
					subject,
					gate.ReviewRiskLow,
					gate.ReviewAuthorizationUnknown,
					gate.ReviewAllow,
				),
			)
			if got.Eligible || got.Reason != gate.ReviewDecisionInvalidPolicy {
				t.Fatalf("decision = %#v, want invalid policy", got)
			}
		})
	}
}

func TestPermissionReviewPolicyRequiresExactSubjectRevision(t *testing.T) {
	t.Parallel()
	policy := mustDefaultPermissionReviewPolicy(t, "other-policy")
	subject := validPermissionReviewSubject(t)
	assessment := validPermissionAssessment(
		subject,
		gate.ReviewRiskLow,
		gate.ReviewAuthorizationUnknown,
		gate.ReviewAllow,
	)
	got := gate.EvaluatePermissionAssessment(policy, subject, assessment)
	if got.Eligible || got.Reason != gate.ReviewDecisionInvalidPolicy {
		t.Fatalf("decision = %#v, want invalid policy", got)
	}
}

func TestCombinePermissionAssessments(t *testing.T) {
	t.Parallel()
	policy := mustDefaultPermissionReviewPolicy(t, "gate-policy-v1")
	first := validPermissionReviewSubject(t)
	second := permissionReviewSubjectWithClassifierRevision(t, first, "command-safety-v2")
	classifiers := mustPermissionClassifierSet(
		t,
		validPermissionClassifier(t, "first", first.Basis.ClassifierRevision),
		validPermissionClassifier(t, "second", second.Basis.ClassifierRevision),
	)
	allowFirst := validPermissionAssessment(first, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow)
	allowSecond := validPermissionAssessment(second, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow)
	human := allowFirst
	human.Recommendation = gate.ReviewNeedsHuman
	tests := []struct {
		name     string
		outcomes []gate.PermissionAssessmentOutcome
		reason   gate.ReviewDecisionReason
		eligible bool
	}{
		{name: "all allowed", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, reason: gate.ReviewDecisionEligible, eligible: true},
		{name: "all allowed with recorded observations", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst,
				Observations: []gate.ObservationRequirement{{Target: "/workspace/a", Token: "tok-a"}}},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, reason: gate.ReviewDecisionEligible, eligible: true},
		{name: "non applicable is neutral", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Status: gate.ReviewStatusNotApplicable},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, reason: gate.ReviewDecisionEligible, eligible: true},
		{name: "all non applicable", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Status: gate.ReviewStatusNotApplicable},
			{Subject: second, Status: gate.ReviewStatusNotApplicable},
		}, reason: gate.ReviewDecisionNoApplicableClassifier},
		{name: "human", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: human},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, reason: gate.ReviewDecisionRecommendation},
		{name: "failure", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusFailed},
		}, reason: gate.ReviewDecisionClassifierStatus},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gate.CombinePermissionAssessments(policy, classifiers, tt.outcomes)
			if got.Eligible != tt.eligible || got.Reason != tt.reason {
				t.Fatalf("decision = %#v, want eligible=%v reason=%q", got, tt.eligible, tt.reason)
			}
		})
	}
	for _, status := range []gate.ReviewStatus{
		"",
		gate.ReviewStatusNeedsHuman,
		gate.ReviewStatusTimedOut,
		gate.ReviewStatusFailed,
		gate.ReviewStatusCancelled,
		gate.ReviewStatusStale,
	} {
		got := gate.CombinePermissionAssessments(policy, classifiers, []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: status},
			{Subject: second, Status: gate.ReviewStatusNotApplicable},
		})
		if got.Eligible || got.Reason != gate.ReviewDecisionClassifierStatus {
			t.Fatalf("status %q decision = %#v", status, got)
		}
	}
}

func TestCombinePermissionAssessmentsRequiresCompleteRegisteredSet(t *testing.T) {
	t.Parallel()
	policy := mustDefaultPermissionReviewPolicy(t, "gate-policy-v1")
	first := validPermissionReviewSubject(t)
	second := permissionReviewSubjectWithClassifierRevision(t, first, "command-safety-v2")
	classifiers := mustPermissionClassifierSet(
		t,
		validPermissionClassifier(t, "first", first.Basis.ClassifierRevision),
		validPermissionClassifier(t, "second", second.Basis.ClassifierRevision),
	)
	allowFirst := validPermissionAssessment(first, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow)
	allowSecond := validPermissionAssessment(second, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow)
	invented := permissionReviewSubjectWithClassifierRevision(t, first, "invented-v1")
	tests := []struct {
		name     string
		set      gate.PermissionClassifierSet
		outcomes []gate.PermissionAssessmentOutcome
	}{
		{name: "no registered set"},
		{name: "missing allowed member", set: classifiers, outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: second, Applicable: true, Status: gate.ReviewStatusFailed},
		}},
		{name: "missing non applicable member", set: classifiers, outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
		}},
		{name: "missing failed member", set: classifiers, outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
		}},
		{name: "extra invented member", set: classifiers, outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
			{Subject: invented, Status: gate.ReviewStatusNotApplicable},
		}},
		{name: "invented revision replaces registered", set: classifiers, outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
			{Subject: invented, Status: gate.ReviewStatusNotApplicable},
		}},
		{name: "reversed", set: classifiers, outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
		}},
		{name: "duplicate classifier revision", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst},
		}, set: classifiers},
		{name: "assessment bound to another classifier", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
			{Subject: second, Status: gate.ReviewStatusNotApplicable},
		}, set: classifiers},
		{name: "non applicable carries assessment", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Status: gate.ReviewStatusNotApplicable, Assessment: allowFirst},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, set: classifiers},
		{name: "failed carries assessment", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusFailed, Assessment: allowFirst},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, set: classifiers},
		{name: "non applicable carries observations", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Status: gate.ReviewStatusNotApplicable,
				Observations: []gate.ObservationRequirement{{Target: "/workspace/a", Token: "tok-a"}}},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, set: classifiers},
		{name: "allowed with malformed observation", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst,
				Observations: []gate.ObservationRequirement{{Target: "/workspace/a", Token: ""}}},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, set: classifiers},
		{name: "allowed with too many observations", outcomes: []gate.PermissionAssessmentOutcome{
			{Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowFirst,
				Observations: tooManyObservationRequirements()},
			{Subject: second, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: allowSecond},
		}, set: classifiers},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gate.CombinePermissionAssessments(policy, tt.set, tt.outcomes)
			if got.Eligible || got.Reason != gate.ReviewDecisionInvalidAssessment {
				t.Fatalf("decision = %#v, want invalid assessment", got)
			}
		})
	}
}

func TestCombinePermissionAssessmentsRejectsDifferentCommonSubject(t *testing.T) {
	t.Parallel()
	policy := mustDefaultPermissionReviewPolicy(t, "gate-policy-v1")
	first := validPermissionReviewSubject(t)
	second := permissionReviewSubjectWithClassifierRevision(t, first, "command-safety-v2")
	classifiers := mustPermissionClassifierSet(
		t,
		validPermissionClassifier(t, "first", first.Basis.ClassifierRevision),
		validPermissionClassifier(t, "second", second.Basis.ClassifierRevision),
	)
	basis := second.Basis
	basis.SubjectDigest = [32]byte{}
	request := second.Request.Clone()
	request.Summary = "different but valid request"
	different, err := gate.NewPermissionReviewSubject(basis, request, second.Context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject(different) error = %v", err)
	}
	got := gate.CombinePermissionAssessments(policy, classifiers, []gate.PermissionAssessmentOutcome{
		{
			Subject: first, Applicable: true, Status: gate.ReviewStatusAllowed,
			Assessment: validPermissionAssessment(first, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow),
		},
		{
			Subject: different, Applicable: true, Status: gate.ReviewStatusAllowed,
			Assessment: validPermissionAssessment(different, gate.ReviewRiskLow, gate.ReviewAuthorizationUnknown, gate.ReviewAllow),
		},
	})
	if got.Eligible || got.Reason != gate.ReviewDecisionInvalidAssessment {
		t.Fatalf("decision = %#v, want invalid assessment", got)
	}
}

func mustPermissionClassifierSet(
	t *testing.T,
	classifiers ...gate.PermissionClassifier,
) gate.PermissionClassifierSet {
	t.Helper()
	set, err := gate.NewPermissionClassifierSet(classifiers...)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet() error = %v", err)
	}
	return set
}

func permissionReviewSubjectWithClassifierRevision(
	t *testing.T,
	base gate.PermissionReviewSubject,
	revision string,
) gate.PermissionReviewSubject {
	t.Helper()
	basis := base.Basis
	basis.SubjectDigest = [32]byte{}
	basis.ClassifierRevision = revision
	subject, err := gate.NewPermissionReviewSubject(basis, base.Request, base.Context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject(%q) error = %v", revision, err)
	}
	if subject.Basis.SubjectDigest == base.Basis.SubjectDigest {
		t.Fatal("classifier-specific subjects have equal full digests")
	}
	return subject
}

func mustDefaultPermissionReviewPolicy(t *testing.T, revision string) gate.PermissionReviewPolicy {
	t.Helper()
	policy, err := gate.DefaultPermissionReviewPolicy(revision)
	if err != nil {
		t.Fatalf("DefaultPermissionReviewPolicy() error = %v", err)
	}
	return policy
}

func defaultMinimumForTest() map[gate.ReviewRisk]gate.ReviewAuthorization {
	return map[gate.ReviewRisk]gate.ReviewAuthorization{
		gate.ReviewRiskLow:    gate.ReviewAuthorizationUnknown,
		gate.ReviewRiskMedium: gate.ReviewAuthorizationUnknown,
		gate.ReviewRiskHigh:   gate.ReviewAuthorizationMedium,
	}
}

func validPermissionReviewSubject(t *testing.T) gate.PermissionReviewSubject {
	t.Helper()
	basis, request, context := validPermissionReviewSubjectInput()
	subject, err := gate.NewPermissionReviewSubject(basis, request, context)
	if err != nil {
		t.Fatalf("NewPermissionReviewSubject() error = %v", err)
	}
	return subject
}

func validPermissionAssessment(
	subject gate.PermissionReviewSubject,
	risk gate.ReviewRisk,
	authorization gate.ReviewAuthorization,
	recommendation gate.ReviewRecommendation,
) gate.PermissionAssessment {
	rationale := ""
	if risk != gate.ReviewRiskLow {
		rationale = "bounded explanation"
	}
	return gate.PermissionAssessment{
		Basis: subject.Basis, Risk: risk, Authorization: authorization,
		Recommendation: recommendation, Rationale: rationale,
	}
}

// tooManyObservationRequirements returns one more than
// gate.MaxObservationRequirementsPerAssessment individually-valid
// requirements, so a test can prove CombinePermissionAssessments bounds the
// aggregate count rather than just each requirement's own shape.
func tooManyObservationRequirements() []gate.ObservationRequirement {
	out := make([]gate.ObservationRequirement, gate.MaxObservationRequirementsPerAssessment+1)
	for i := range out {
		out[i] = gate.ObservationRequirement{Target: "/workspace/a", Token: "tok"}
	}
	return out
}

func cloneReviewPolicy(policy gate.PermissionReviewPolicy) gate.PermissionReviewPolicy {
	clone := policy
	clone.MinimumAuthorization = make(map[gate.ReviewRisk]gate.ReviewAuthorization, len(policy.MinimumAuthorization))
	for risk, authorization := range policy.MinimumAuthorization {
		clone.MinimumAuthorization[risk] = authorization
	}
	clone.AbsoluteHuman = append([]gate.ReviewRiskCategory(nil), policy.AbsoluteHuman...)
	return clone
}

func TestPermissionReviewPolicyDefaultShape(t *testing.T) {
	t.Parallel()
	got := mustDefaultPermissionReviewPolicy(t, "r")
	wantMinimum := map[gate.ReviewRisk]gate.ReviewAuthorization{
		gate.ReviewRiskLow: gate.ReviewAuthorizationUnknown, gate.ReviewRiskMedium: gate.ReviewAuthorizationUnknown, gate.ReviewRiskHigh: gate.ReviewAuthorizationMedium,
	}
	if got.MaximumAutoRisk != gate.ReviewRiskHigh ||
		!reflect.DeepEqual(got.MinimumAuthorization, wantMinimum) ||
		len(got.AbsoluteHuman) != 0 ||
		got.MaterialTruncation != 0 {
		t.Fatalf("default = %#v", got)
	}
}
