package serve

// readServer is the rig-free holder for the stateless read plane. It owns only the
// dependencies a pure read needs — the Reader and the capability document's feature
// list — so the read routes can be served by a process that hosts no agent and
// therefore has no session factory to supply (the BFF case).
//
// The full server[S, O] embeds this holder, so the read handlers are promoted onto it
// and the live/control server keeps serving all ten routes from one value. Splitting
// the receiver, not the logic, is the whole point: there is exactly one
// implementation of each read route.
//
// It carries no request state — one readServer serves every request.
type readServer struct {
	reader   Reader
	features []string
}
