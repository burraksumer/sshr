package ui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

type styles struct {
	title, dim, accent, selected, row, status, key, err lipgloss.Style
}

func newStyles(dark bool) styles {
	ld := lipgloss.LightDark(dark)
	accent := ld(lipgloss.Color("#3b5bdb"), lipgloss.Color("#8caaee"))
	dim := ld(lipgloss.Color("#868e96"), lipgloss.Color("#737994"))
	var selFg color.Color = ld(lipgloss.Color("#ffffff"), lipgloss.Color("#232634"))
	return styles{
		title:    lipgloss.NewStyle().Bold(true).Foreground(accent),
		dim:      lipgloss.NewStyle().Foreground(dim),
		accent:   lipgloss.NewStyle().Foreground(accent),
		selected: lipgloss.NewStyle().Background(accent).Foreground(selFg).Bold(true),
		row:      lipgloss.NewStyle(),
		status:   lipgloss.NewStyle().Foreground(dim),
		key:      lipgloss.NewStyle().Foreground(accent).Bold(true),
		err:      lipgloss.NewStyle().Foreground(lipgloss.Color("#e03131")),
	}
}
