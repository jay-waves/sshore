package main

import "testing"

func TestRequestPickerIdentityAndFilter(t *testing.T) {
	ids := []string{"first-id", "second-id"}
	labels := []string{"first-id · alpha · upload /tmp", "second-id · beta · download /tmp"}
	for _, tc := range []struct {
		answers []string
		want    string
	}{
		{[]string{"/beta", "#1", "#2"}, "second-id"},
		{[]string{"/beta", "/", "first-id"}, "first-id"},
		{[]string{""}, ""},
	} {
		got, err := chooseLocalItem(&stubSetupInput{answers: tc.answers}, ids, labels, "Request: ", "request")
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatal(got, err)
		}
	}
}
