package chutes

// APIBaseForTest exposes the client's apiBase to external (package chutes_test)
// tests so they can assert construction-time defaulting.
func (c *Client) APIBaseForTest() string { return c.apiBase }
