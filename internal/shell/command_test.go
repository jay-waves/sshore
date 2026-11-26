package shell

import (
	"slices"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name, value string
		want        []string
		invalid     bool
	}{
		{name: "empty"},
		{name: "blank", value: "  "},
		{name: "program with options", value: "code --wait", want: []string{"code", "--wait"}},
		{name: "quoted executable", value: `"C:\\Program Files\\Editor\\editor.exe" --wait`, want: []string{`C:\Program Files\Editor\editor.exe`, "--wait"}},
		{name: "literal path", value: `'C:\Program Files\Editor\editor.exe' --wait`, want: []string{`C:\Program Files\Editor\editor.exe`, "--wait"}},
		{name: "literal arguments", value: `vim "" '$HOME' *.txt`, want: []string{"vim", "", "$HOME", "*.txt"}},
		{name: "unclosed quote", value: `vim "file name`, invalid: true},
		{name: "empty executable", value: `"" --wait`, invalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			argv, err := ParseCommand(test.value)
			if (err != nil) != test.invalid || !slices.Equal(argv, test.want) {
				t.Fatalf("ParseCommand(%q) = %q, %v; want %q, invalid=%t", test.value, argv, err, test.want, test.invalid)
			}
		})
	}
}
