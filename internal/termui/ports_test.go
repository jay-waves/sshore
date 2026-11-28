package termui

import (
	"slices"
	"testing"
)

func TestPortProcessSortKeepsPortOrderForTies(t *testing.T) {
	rows := []PortRow{
		{Port: "22", Process: "sshd"},
		{Port: "53"},
		{Port: "80", Process: "Caddy"},
		{Port: "443", Process: "caddy"},
		{Port: "5432", Process: "postgres"},
	}
	slices.SortStableFunc(rows, comparePortProcess)
	var ports []string
	for _, row := range rows {
		ports = append(ports, row.Port)
	}
	if want := []string{"80", "443", "5432", "22", "53"}; !slices.Equal(ports, want) {
		t.Fatalf("process-sorted ports = %v, want %v", ports, want)
	}
}
