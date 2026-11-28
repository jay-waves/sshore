package termui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Randomart renders a raw digest with OpenSSH's board and ASCII tiles.
// The caller supplies the optional labels for the top and bottom borders.
func Randomart(digest []byte, header, footer string) string {
	const width, height = 17, 9
	const marks = " .o+=*BOX@%&#/^"
	var field [height][width]int
	x, y := width/2, height/2
	for _, value := range digest {
		// Each pair of bits selects one diagonal move, clamped at the edges.
		for range 4 {
			x = min(width-1, max(0, x+2*int(value&1)-1))
			y = min(height-1, max(0, y+2*int(value>>1&1)-1))
			field[y][x] = min(field[y][x]+1, len(marks)-1)
			value >>= 2
		}
	}
	var image strings.Builder
	image.WriteString(randomartBorder(width, header))
	for row, cells := range field {
		image.WriteByte('|')
		for col, visits := range cells {
			switch {
			case col == x && row == y:
				image.WriteByte('E')
			case col == width/2 && row == height/2:
				image.WriteByte('S')
			default:
				image.WriteByte(marks[visits])
			}
		}
		image.WriteString("|\n")
	}
	image.WriteString(randomartBorder(width, footer))
	return image.String()
}

func randomartBorder(width int, label string) string {
	if label != "" {
		label = "[" + ansi.Truncate(CleanText(label), width-2, "…") + "]"
	}
	padding := width - ansi.StringWidth(label)
	return "+" + strings.Repeat("-", padding/2) + label + strings.Repeat("-", (padding+1)/2) + "+\n"
}
