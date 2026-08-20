package domainname

import "testing"

func TestNormalizeAndPatterns(t *testing.T) {
	domain, err := Normalize("WWW.Example.COM.")
	if err != nil || domain != "www.example.com" {
		t.Fatalf("Normalize() = (%q, %v)", domain, err)
	}
	pattern, err := ParsePattern("*.Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"example.com", "www.example.com"} {
		if !pattern.Matches(candidate) {
			t.Errorf("pattern does not match %q", candidate)
		}
	}
	if pattern.Matches("notexample.com") {
		t.Fatal("suffix pattern matched a label without a dot boundary")
	}
}

func TestNormalizeRejectsInvalidNames(t *testing.T) {
	for _, value := range []string{"", "192.0.2.1", "-bad.example", "bad..example", "bad space.example"} {
		if _, err := Normalize(value); err == nil {
			t.Errorf("Normalize(%q) succeeded", value)
		}
	}
}
