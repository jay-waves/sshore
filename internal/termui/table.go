package termui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type TableCell struct {
	Text  string
	Color Color
}

// WriteTable lays out a header and rows. The last column uses the space left
// by the fixed columns. A width of zero produces unbounded plain output.
func WriteTable(out io.Writer, rows [][]TableCell, screenWidth, currentRow int, color bool) {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return
	}
	columns := len(rows[0])
	last := columns - 1
	type measuredCell struct {
		TableCell
		width int
	}
	cells := make([][]measuredCell, len(rows))
	widths := make([]int, columns)
	for i, row := range rows {
		cells[i] = make([]measuredCell, columns)
		for j, cell := range row[:min(len(row), columns)] {
			cell.Text = CleanText(cell.Text)
			width := ansi.StringWidth(cell.Text)
			cells[i][j] = measuredCell{TableCell: cell, width: width}
			widths[j] = max(widths[j], width)
		}
	}

	limit := -1
	bordered := false
	if screenWidth > 0 {
		limit = screenWidth
		for _, width := range widths[:last] {
			limit -= width + 2
		}
		// A border adds one vertical rule and one extra cell per column,
		// plus the two padding cells around the last column.
		borderLimit := limit - columns - 3
		bordered = screenWidth >= 64 && borderLimit > 0 && cells[0][last].width <= borderLimit
		if bordered {
			limit = borderLimit
		} else if limit <= 0 {
			columns = last
		}
	}
	if columns > last && limit >= 0 && widths[last] > limit {
		widths[last] = 0
		for _, row := range cells {
			cell := &row[last]
			if cell.width > limit {
				cell.Text = ansi.Truncate(cell.Text, limit, "…")
				cell.width = ansi.StringWidth(cell.Text)
			}
			widths[last] = max(widths[last], cell.width)
		}
	}

	var edges []string
	divider := ""
	if bordered {
		edges = make([]string, columns)
		for j, width := range widths {
			edges[j] = strings.Repeat("─", width+2)
		}
		divider = Render(color, Muted, "│")
		writeTableBorder(out, edges, "╭", "┬", "╮", color)
	}
	for i, row := range cells {
		var line strings.Builder
		line.WriteString(divider)
		for j, cell := range row[:columns] {
			style := cell.Color
			if bordered {
				if i == 0 {
					style = Accent
				} else if i == currentRow {
					style = Info
				}
				line.WriteByte(' ')
			}
			line.WriteString(Render(color && (bordered || (i != 0 && i != currentRow)) && style != "", style, cell.Text))
			if bordered {
				line.WriteString(strings.Repeat(" ", widths[j]-cell.width+1))
				line.WriteString(divider)
			} else if j < columns-1 {
				line.WriteString(strings.Repeat(" ", widths[j]-cell.width+2))
			}
		}
		text := line.String()
		if !bordered {
			text = Render(color && i == 0, Heading, text)
			text = Render(color && i == currentRow, Accent, text)
		}
		fmt.Fprintln(out, text)
		if bordered && i == 0 {
			writeTableBorder(out, edges, "├", "┼", "┤", color)
		}
	}
	if bordered {
		writeTableBorder(out, edges, "╰", "┴", "╯", color)
	}
}

func writeTableBorder(out io.Writer, edges []string, left, middle, right string, color bool) {
	fmt.Fprintln(out, Render(color, Muted, left+strings.Join(edges, middle)+right))
}
