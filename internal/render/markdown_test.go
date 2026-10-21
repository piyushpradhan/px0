package render

import "testing"

func TestHeadingIDsFollowGitHub(t *testing.T) {
	t.Parallel()
	ids := &headingIDs{seen: map[string]bool{}}
	for _, c := range []struct{ in, want string }{
		{"Getting Started!", "getting-started"},
		{"snake_case API", "snake_case-api"},
		{"Überblick", "überblick"},
		{"Getting Started", "getting-started-1"},
		{"???", "section"},
	} {
		if got := string(ids.Generate([]byte(c.in), 0)); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
