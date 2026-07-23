package release

import "testing"

func TestValidVersion(t *testing.T) {
	t.Parallel()
	valid := []string{
		"0.0.0",
		"1.2.3",
		"1.2.3-alpha.1",
		"1.2.3-alpha-1+build.7",
	}
	invalid := []string{
		"",
		"1",
		"1.2",
		"01.2.3",
		"1.02.3",
		"1.2.03",
		"1.2.3-01",
		"1.2.3+",
		"1.2.3+bad!",
		"1.2.3-alpha..1",
	}
	for _, value := range valid {
		if !ValidVersion(value) {
			t.Errorf("ValidVersion(%q) = false", value)
		}
	}
	for _, value := range invalid {
		if ValidVersion(value) {
			t.Errorf("ValidVersion(%q) = true", value)
		}
	}
}
