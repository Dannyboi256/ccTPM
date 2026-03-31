package tui

import (
	"charm.land/lipgloss/v2"
)

var (
	titleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("170"))

	paneHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("39"))

	statLabelStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("245"))

	statValueStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("255"))

	requestRowStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))

	errorRowStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("196"))

	inflightRowStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("214"))

	borderStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63"))

	aggregateStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240"))

	tpmStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("46"))
)
