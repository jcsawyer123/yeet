package main

import "testing"

func TestListableAcceptsOldAndNewMarkers(t *testing.T) {
	const self = "self-uuid"
	cases := []struct {
		uuid, desc string
		want       bool
	}{
		{"a1", "yeet: lulu", true},
		{"a2", "yeet: peakflow", true},
		{"a3", "yeet - lulu", true},
		{"a4", "yeet:v2 project=p instance=i", true},
		{"a5", "yeet - v2 project=p instance=i", true},
		{"a6", "some other app", false},
		{"a7", "", false},
		{self, "yeet - yeet", false},
		{self, "yeet: yeet", false},
	}
	for _, c := range cases {
		if got := listable(self, c.uuid, c.desc); got != c.want {
			t.Errorf("listable(%q, %q) = %v, want %v", c.uuid, c.desc, got, c.want)
		}
	}
}
