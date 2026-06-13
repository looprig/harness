package content

// Role identifies the author of a message in a conversation thread.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Message is the base type for all conversation turns: a role and an ordered
// sequence of content blocks. Typed message structs embed this so the role and
// blocks are always accessible via the embedded field.
type Message struct {
	Role   Role
	Blocks []*Block
}

// UserMessage is a turn authored by the human user.
type UserMessage struct{ Message }

// AIMessage is a turn authored by the AI model.
type AIMessage struct{ Message }

// SystemMessage carries a system prompt that shapes model behavior.
type SystemMessage struct{ Message }

// ToolMessage carries the result of a tool invocation back to the model.
// ToolUseID ties this result to the specific ToolUseBlock that requested it.
type ToolMessage struct {
	Message
	ToolUseID string
}

// Conversation is a sealed interface: only message types defined in this
// package can participate in a conversation thread. The unexported marker
// method prevents external packages from satisfying the interface accidentally,
// keeping the discriminated union closed.
type Conversation interface{ isMessage() }

func (*UserMessage) isMessage()   {}
func (*AIMessage) isMessage()     {}
func (*SystemMessage) isMessage() {}
func (*ToolMessage) isMessage()   {}

// AgenticMessages is an ordered conversation thread. A nil or empty slice is a
// valid zero value representing an empty thread.
type AgenticMessages []Conversation
