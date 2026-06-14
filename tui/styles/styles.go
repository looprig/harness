// Package styles holds the shared lipgloss styles and glamour helpers for the
// Nexus CLI TUI. It is a leaf package: it depends only on charm libraries and
// must never import the tui package or any of its other subpackages.
package styles

import (
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// Dot is the leading marker rendered before assistant/markdown blocks.
const Dot = "● "

// Role styles (exported so package tui can use them).
var (
	UserStyle        = lipgloss.NewStyle().Bold(true)
	SystemStyle      = lipgloss.NewStyle().Faint(true)
	ErrorStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // red
	InterruptedStyle = lipgloss.NewStyle().Faint(true).Italic(true)
	StatusStyle      = lipgloss.NewStyle().Faint(true)
)

// NewMarkdownRenderer builds a glamour renderer for the given wrap width.
// Returns an error if glamour fails to construct (caller decides fallback).
func NewMarkdownRenderer(width int) (*glamour.TermRenderer, error) {
	return glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width),
	)
}
