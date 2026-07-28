package gate

// CanonicalReviewContextSizeForTest exposes the private canonical projection
// only to the external package tests in this module.
func CanonicalReviewContextSizeForTest(context ReviewContext) (int, error) {
	return permissionReviewContextEncodedSize(context)
}
