package command

// Command is a sealed interface for all loop commands.
// Only types in this package can implement it.
type Command interface{ isCommand() }
