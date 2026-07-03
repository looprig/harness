package aci

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/llm"
)

// The live gateway's claimed digests from testdata/report_aci1.json. These are
// the arbiter: the JCS projections in identity.go must reproduce them byte-for-
// byte or recomputed digests will not equal the report's own claimed values.
const (
	claimedWorkloadID     = "sha256:3def476b72026924f9d88f7b339b2e211553fc2486c6a974d5f330162f183eda"
	claimedKeysetDigest   = "sha256:46cdea445e5f2abcb78a5db0a630e7ba2127bc4f24033aa628b49e310e3928e2"
	identityPublicKeyAlgo = "ecdsa-secp256k1"
	identityPublicKeyHex  = "04d3b51dcb45d74434a76fc1b7e2bc152cf81190eab43bdbf5c2c624321232c76ac9122f8b93480663b987a38e2b3b1f42c7ca8e84736c9741175c07eeca62d382"
)

// fixtureReport parses the authoritative aci/1 report fixture for tests that
// need the live keyset/identity and the gateway's claimed digests.
func fixtureReport(t *testing.T) *Report {
	t.Helper()
	rep, err := ParseReport(readFixture(t))
	if err != nil {
		t.Fatalf("ParseReport(fixture) error = %v, want nil", err)
	}
	return rep
}

// strPtr is a test helper returning a pointer to its argument (for the optional
// subject field).
func strPtr(s string) *string { return &s }

// TestWorkloadIDMatchesFixture proves the identity-key projection is byte-exact:
// the recomputed workload_id must equal the live gateway's claimed value.
func TestWorkloadIDMatchesFixture(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)

	got, err := workloadID(rep.Attestation.Keyset.Identity)
	if err != nil {
		t.Fatalf("workloadID() error = %v, want nil", err)
	}
	if got != rep.WorkloadID {
		t.Errorf("workloadID() = %q, want fixture claimed %q", got, rep.WorkloadID)
	}
	if got != claimedWorkloadID {
		t.Errorf("workloadID() = %q, want constant claimed %q", got, claimedWorkloadID)
	}
}

// TestWorkloadKeysetDigestMatchesFixture proves the full-keyset projection is
// byte-exact: the recomputed workload_keyset_digest must equal the live
// gateway's claimed value.
func TestWorkloadKeysetDigestMatchesFixture(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)

	got, err := workloadKeysetDigest(rep.Attestation.Keyset)
	if err != nil {
		t.Fatalf("workloadKeysetDigest() error = %v, want nil", err)
	}
	if got != rep.WorkloadKeysetDigest {
		t.Errorf("workloadKeysetDigest() = %q, want fixture claimed %q", got, rep.WorkloadKeysetDigest)
	}
	if got != claimedKeysetDigest {
		t.Errorf("workloadKeysetDigest() = %q, want constant claimed %q", got, claimedKeysetDigest)
	}
}

// TestWorkloadIDMutations confirms any change to the identity public key changes
// the workload_id, so the digest actually binds the projected fields.
func TestWorkloadIDMutations(t *testing.T) {
	t.Parallel()

	base := WorkloadIdentity{
		PublicKey: PublicKey{Algo: identityPublicKeyAlgo, PublicKeyHex: identityPublicKeyHex},
	}
	baseDigest, err := workloadID(base)
	if err != nil {
		t.Fatalf("workloadID(base) error = %v, want nil", err)
	}
	if baseDigest != claimedWorkloadID {
		t.Fatalf("workloadID(base) = %q, want claimed %q", baseDigest, claimedWorkloadID)
	}

	tests := []struct {
		name string
		id   WorkloadIdentity
	}{
		{
			name: "flip a byte in public_key hex",
			id: WorkloadIdentity{PublicKey: PublicKey{
				Algo:         identityPublicKeyAlgo,
				PublicKeyHex: "14d3b51dcb45d74434a76fc1b7e2bc152cf81190eab43bdbf5c2c624321232c76ac9122f8b93480663b987a38e2b3b1f42c7ca8e84736c9741175c07eeca62d382",
			}},
		},
		{
			name: "change algo",
			id: WorkloadIdentity{PublicKey: PublicKey{
				Algo:         "ed25519",
				PublicKeyHex: identityPublicKeyHex,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := workloadID(tt.id)
			if err != nil {
				t.Fatalf("workloadID() error = %v, want nil", err)
			}
			if got == baseDigest {
				t.Errorf("workloadID() = %q, want != base %q", got, baseDigest)
			}
		})
	}
}

// fixtureKeyset returns a deep-ish copy of the live keyset (fresh slices) so a
// mutation row can alter one field without touching the parsed fixture or other
// rows.
func fixtureKeyset(t *testing.T) Keyset {
	t.Helper()
	ks := fixtureReport(t).Attestation.Keyset
	out := ks
	out.ReceiptSigningKeys = append([]KeyEntry(nil), ks.ReceiptSigningKeys...)
	out.E2EEPublicKeys = append([]KeyEntry(nil), ks.E2EEPublicKeys...)
	out.TLSPublicKeys = append([]TLSBinding(nil), ks.TLSPublicKeys...)
	if ks.Identity.Subject != nil {
		s := *ks.Identity.Subject
		out.Identity.Subject = &s
	}
	return out
}

// TestWorkloadKeysetDigestMutations confirms every projected field of the keyset
// participates in the digest: changing any one yields a different digest from the
// pristine fixture.
func TestWorkloadKeysetDigestMutations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(ks *Keyset)
	}{
		{
			name: "flip a byte in identity public key hex",
			mutate: func(ks *Keyset) {
				ks.Identity.PublicKey.PublicKeyHex = "14" + ks.Identity.PublicKey.PublicKeyHex[2:]
			},
		},
		{
			name:   "change keyset_epoch.version",
			mutate: func(ks *Keyset) { ks.Epoch.Version = 2 },
		},
		{
			name:   "change keyset_epoch.not_after",
			mutate: func(ks *Keyset) { ks.Epoch.NotAfter = 0 },
		},
		{
			name:   "mutate a receipt_signing_keys entry key_id",
			mutate: func(ks *Keyset) { ks.ReceiptSigningKeys[0].KeyID = "tampered" },
		},
		{
			name:   "mutate an e2ee_public_keys entry public_key",
			mutate: func(ks *Keyset) { ks.E2EEPublicKeys[0].PublicKeyHex = "00" + ks.E2EEPublicKeys[0].PublicKeyHex[2:] },
		},
		{
			name:   "alter a tls domain",
			mutate: func(ks *Keyset) { ks.TLSPublicKeys[0].Domain = "evil.example.com" },
		},
		{
			name:   "drop a tls domain (omit key)",
			mutate: func(ks *Keyset) { ks.TLSPublicKeys[0].Domain = "" },
		},
		{
			name:   "mutate a tls spki_sha256",
			mutate: func(ks *Keyset) { ks.TLSPublicKeys[0].SPKISHA256Hex = "00" + ks.TLSPublicKeys[0].SPKISHA256Hex[2:] },
		},
		{
			name:   "set a non-nil subject (was null)",
			mutate: func(ks *Keyset) { ks.Identity.Subject = strPtr("operator@example.com") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base, err := workloadKeysetDigest(fixtureKeyset(t))
			if err != nil {
				t.Fatalf("workloadKeysetDigest(base) error = %v, want nil", err)
			}
			ks := fixtureKeyset(t)
			tt.mutate(&ks)
			got, err := workloadKeysetDigest(ks)
			if err != nil {
				t.Fatalf("workloadKeysetDigest(mutated) error = %v, want nil", err)
			}
			if got == base {
				t.Errorf("workloadKeysetDigest(mutated) = %q, want != base %q", got, base)
			}
		})
	}
}

// TestWorkloadKeysetDigestEdgeCases covers deterministic empty key lists and the
// subject null-vs-present distinction (proves the JSON null handling).
func TestWorkloadKeysetDigestEdgeCases(t *testing.T) {
	t.Parallel()

	empty := Keyset{
		Identity:           WorkloadIdentity{PublicKey: PublicKey{Algo: "ed25519", PublicKeyHex: "ab"}},
		Epoch:              KeysetEpoch{Version: 0, NotAfter: 0},
		ReceiptSigningKeys: nil,
		E2EEPublicKeys:     []KeyEntry{},
		TLSPublicKeys:      nil,
	}

	t.Run("empty key lists digest deterministically", func(t *testing.T) {
		t.Parallel()
		a, err := workloadKeysetDigest(empty)
		if err != nil {
			t.Fatalf("workloadKeysetDigest(empty) error = %v, want nil", err)
		}
		b, err := workloadKeysetDigest(empty)
		if err != nil {
			t.Fatalf("workloadKeysetDigest(empty) error = %v, want nil", err)
		}
		if a != b {
			t.Errorf("workloadKeysetDigest(empty) not deterministic: %q != %q", a, b)
		}
	})

	t.Run("subject nil vs present differ", func(t *testing.T) {
		t.Parallel()
		nilSubject := empty
		nilSubject.Identity.Subject = nil
		withSubject := empty
		withSubject.Identity.Subject = strPtr("subj")

		a, err := workloadKeysetDigest(nilSubject)
		if err != nil {
			t.Fatalf("workloadKeysetDigest(nil subject) error = %v, want nil", err)
		}
		b, err := workloadKeysetDigest(withSubject)
		if err != nil {
			t.Fatalf("workloadKeysetDigest(present subject) error = %v, want nil", err)
		}
		if a == b {
			t.Errorf("nil-subject and present-subject digests must differ, both = %q", a)
		}
	})

	t.Run("empty-string subject vs nil differ", func(t *testing.T) {
		t.Parallel()
		// An empty-string subject is a present JSON string "" — distinct from a
		// nil subject (JSON null). The pointer model distinguishes them.
		nilSubject := empty
		nilSubject.Identity.Subject = nil
		emptyStr := empty
		emptyStr.Identity.Subject = strPtr("")

		a, err := workloadKeysetDigest(nilSubject)
		if err != nil {
			t.Fatalf("workloadKeysetDigest(nil subject) error = %v, want nil", err)
		}
		b, err := workloadKeysetDigest(emptyStr)
		if err != nil {
			t.Fatalf("workloadKeysetDigest(empty-string subject) error = %v, want nil", err)
		}
		if a == b {
			t.Errorf("nil-subject (null) and empty-string-subject digests must differ, both = %q", a)
		}
	})
}

// TestVerifyIdentityDigests exercises the chain helper Task 2.8 will call: nil on
// the pristine fixture, a typed *llm.AttestationError{Reason: keyset_digest_mismatch}
// on any tamper that breaks either claimed digest.
func TestVerifyIdentityDigests(t *testing.T) {
	t.Parallel()

	t.Run("pristine fixture verifies", func(t *testing.T) {
		t.Parallel()
		if err := verifyIdentityDigests(fixtureReport(t)); err != nil {
			t.Errorf("verifyIdentityDigests(pristine) = %v, want nil", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(rep *Report)
	}{
		{
			name: "tampered workload_id claim",
			mutate: func(rep *Report) {
				rep.WorkloadID = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			},
		},
		{
			name: "tampered workload_keyset_digest claim",
			mutate: func(rep *Report) {
				rep.WorkloadKeysetDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			},
		},
		{
			name: "mutated identity key (workload_id no longer matches claim)",
			mutate: func(rep *Report) {
				rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex = "14" + rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex[2:]
			},
		},
		{
			name: "mutated keyset (keyset_digest no longer matches claim)",
			mutate: func(rep *Report) {
				rep.Attestation.Keyset.Epoch.Version = 2
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := fixtureReport(t)
			tt.mutate(rep)
			err := verifyIdentityDigests(rep)
			if err == nil {
				t.Fatalf("verifyIdentityDigests(mutated) = nil, want error")
			}
			var attErr *llm.AttestationError
			if !errors.As(err, &attErr) {
				t.Fatalf("verifyIdentityDigests() error = %T (%v), want *llm.AttestationError", err, err)
			}
			if attErr.Reason != reasonKeysetDigestMismatch {
				t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonKeysetDigestMismatch)
			}
		})
	}
}
