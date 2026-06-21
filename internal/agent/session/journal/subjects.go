package journal

import (
	"strconv"
	"strings"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// subjectRoot is the constant first two tokens of every journal subject:
// "urvi.session". Concrete subjects extend it with the session id and a kind.
const subjectRoot = "urvi.session"

// streamPrefix is the constant prefix of a per-session JetStream stream name.
// NATS stream names disallow '.', ' ', '*' and '>', so the prefix uses '_' and
// the uuid (whose dashes are legal) is appended verbatim.
const streamPrefix = "urvi_session_"

// Subject-leaf tokens. They are the trailing token of a concrete subject and the
// single source of truth for both the builders and the parser.
const (
	leafSession = "session" // session-scoped enduring events
	leafLoop    = "loop"    // marks a loop-scoped subject; followed by <lid>.<leaf>
	leafEvent   = "event"   // a loop's enduring events
	leafCommand = "cmd"     // commands targeting a loop (the intent log)
	leafFence   = "fence"   // internal LeaseFence{epoch} handover records
)

// SubjectKind classifies a parsed journal subject so the replayer can route a
// record without re-parsing the string. It is a closed enum: every concrete
// subject builder maps to exactly one kind.
type SubjectKind uint8

const (
	// SubjectSessionEvent is "urvi.session.<sid>.session" — session-scoped events.
	SubjectSessionEvent SubjectKind = iota
	// SubjectLoopEvent is "urvi.session.<sid>.loop.<lid>.event" — a loop's events.
	SubjectLoopEvent
	// SubjectLoopCommand is "urvi.session.<sid>.loop.<lid>.cmd" — the intent log.
	SubjectLoopCommand
	// SubjectFence is "urvi.session.<sid>.fence" — internal LeaseFence records.
	SubjectFence
)

// String renders the kind for logs and test failures.
func (k SubjectKind) String() string {
	switch k {
	case SubjectSessionEvent:
		return "session-event"
	case SubjectLoopEvent:
		return "loop-event"
	case SubjectLoopCommand:
		return "loop-command"
	case SubjectFence:
		return "fence"
	default:
		return "SubjectKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// IsEvent reports whether the kind names a subject the EventReplayer decodes —
// session events and loop events only. Commands and fences live on separate
// subjects the replayer never decodes.
func (k SubjectKind) IsEvent() bool {
	return k == SubjectSessionEvent || k == SubjectLoopEvent
}

// SubjectParseError reports a subject string that does not match any concrete
// journal subject shape. It is the typed fail-closed error at the replay-routing
// boundary: an unrecognised subject is never silently classified.
type SubjectParseError struct {
	Subject string
	Reason  string
}

func (e *SubjectParseError) Error() string {
	return "journal: parse subject " + strconv.Quote(e.Subject) + ": " + e.Reason
}

// StreamName returns the per-session JetStream stream name for sessionID. The
// uuid's dashes are legal in a stream name; the rest of the name is constant, so
// no forbidden rune ('.', ' ', '*', '>') can appear.
func StreamName(sessionID uuid.UUID) string {
	return streamPrefix + sessionID.String()
}

// SessionEventSubject returns the subject carrying a session's session-scoped
// enduring events: "urvi.session.<sid>.session".
func SessionEventSubject(sessionID uuid.UUID) string {
	return subjectRoot + "." + sessionID.String() + "." + leafSession
}

// LoopEventSubject returns the subject carrying a loop's enduring events:
// "urvi.session.<sid>.loop.<lid>.event".
func LoopEventSubject(sessionID, loopID uuid.UUID) string {
	return loopSubjectPrefix(sessionID, loopID) + "." + leafEvent
}

// LoopCommandSubject returns the subject carrying commands targeting a loop (the
// intent log): "urvi.session.<sid>.loop.<lid>.cmd".
func LoopCommandSubject(sessionID, loopID uuid.UUID) string {
	return loopSubjectPrefix(sessionID, loopID) + "." + leafCommand
}

// allLoopsEventSubject returns the wildcard subject matching every loop's event
// subject in a session: "urvi.session.<sid>.loop.*.event". The '*' wildcards exactly
// the loop-id token, so it captures all loops' events and ONLY events (never .cmd).
// It is built from the same constant leaves as the concrete builders so the filter
// cannot drift from what the writer emits. It is the EventReplayer's all-loops filter
// (ReplayRequest.LoopID == zero).
func allLoopsEventSubject(sessionID uuid.UUID) string {
	return subjectRoot + "." + sessionID.String() + "." + leafLoop + ".*." + leafEvent
}

// FenceSubject returns the subject carrying a session's internal LeaseFence
// records: "urvi.session.<sid>.fence".
func FenceSubject(sessionID uuid.UUID) string {
	return subjectRoot + "." + sessionID.String() + "." + leafFence
}

// loopSubjectPrefix returns "urvi.session.<sid>.loop.<lid>", the shared head of
// every loop-scoped subject.
func loopSubjectPrefix(sessionID, loopID uuid.UUID) string {
	return subjectRoot + "." + sessionID.String() + "." + leafLoop + "." + loopID.String()
}

// IsEventSubject reports whether subj is a subject the EventReplayer decodes (a
// session-event or loop-event subject). It returns false — never an error — for
// command, fence, and malformed subjects, so it can be used as a plain filter.
func IsEventSubject(subj string) bool {
	kind, _, _, err := ParseSubject(subj)
	if err != nil {
		return false
	}
	return kind.IsEvent()
}

// ParseSubject classifies subj and extracts its session id (always) and loop id
// (zero for session-event and fence subjects). It fails closed with a
// *SubjectParseError on any subject that does not exactly match one of the four
// concrete shapes — a wrong prefix, an unknown leaf, a malformed uuid, a wildcard
// token, or a stray trailing token.
func ParseSubject(subj string) (SubjectKind, uuid.UUID, uuid.UUID, error) {
	var zero uuid.UUID
	toks := strings.Split(subj, ".")
	// Every concrete subject begins with the two root tokens then the session id.
	if len(toks) < 4 || toks[0] != "urvi" || toks[1] != leafSession {
		return 0, zero, zero, &SubjectParseError{Subject: subj, Reason: "not a urvi.session subject"}
	}
	sid, err := parseToken(subj, toks[2])
	if err != nil {
		return 0, zero, zero, err
	}
	switch toks[3] {
	case leafSession:
		if len(toks) != 4 {
			return 0, zero, zero, &SubjectParseError{Subject: subj, Reason: "trailing tokens after session leaf"}
		}
		return SubjectSessionEvent, sid, zero, nil
	case leafFence:
		if len(toks) != 4 {
			return 0, zero, zero, &SubjectParseError{Subject: subj, Reason: "trailing tokens after fence leaf"}
		}
		return SubjectFence, sid, zero, nil
	case leafLoop:
		return parseLoopSubject(subj, toks, sid)
	default:
		return 0, zero, zero, &SubjectParseError{Subject: subj, Reason: "unknown session-level leaf " + strconv.Quote(toks[3])}
	}
}

// parseLoopSubject parses the tail of a "...loop.<lid>.<leaf>" subject. toks is the
// already-split subject and sid the parsed session id; the loop subject is exactly
// six tokens (urvi.session.<sid>.loop.<lid>.<leaf>).
func parseLoopSubject(subj string, toks []string, sid uuid.UUID) (SubjectKind, uuid.UUID, uuid.UUID, error) {
	var zero uuid.UUID
	if len(toks) != 6 {
		return 0, zero, zero, &SubjectParseError{Subject: subj, Reason: "loop subject must be urvi.session.<sid>.loop.<lid>.<leaf>"}
	}
	lid, err := parseToken(subj, toks[4])
	if err != nil {
		return 0, zero, zero, err
	}
	switch toks[5] {
	case leafEvent:
		return SubjectLoopEvent, sid, lid, nil
	case leafCommand:
		return SubjectLoopCommand, sid, lid, nil
	default:
		return 0, zero, zero, &SubjectParseError{Subject: subj, Reason: "unknown loop leaf " + strconv.Quote(toks[5])}
	}
}

// parseToken parses an id token, mapping the uuid parse failure onto the subject
// error type so callers see a single typed error at this boundary.
func parseToken(subj, tok string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := id.UnmarshalText([]byte(tok)); err != nil {
		return uuid.UUID{}, &SubjectParseError{Subject: subj, Reason: "malformed uuid token " + strconv.Quote(tok)}
	}
	return id, nil
}
