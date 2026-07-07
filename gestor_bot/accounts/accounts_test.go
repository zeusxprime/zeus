package accounts

import "testing"

func TestNormalizeAndValidateUsername(t *testing.T) {
	got := NormalizeUsername("cliente1")
	if got != "Cliente1" {
		t.Fatalf("NormalizeUsername = %q", got)
	}
	if err := ValidateUsername(got); err != nil {
		t.Fatalf("valid username rejected: %v", err)
	}
	bad := []string{"cli", "Cliente123456", "Cliente-1", "cliente 1", "1Cliente"}
	for _, u := range bad {
		if err := ValidateUsername(NormalizeUsername(u)); err == nil {
			t.Fatalf("bad username accepted: %q", u)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("12345"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "12 34", "12:34", "12\n34"} {
		if err := ValidatePassword(p); err == nil {
			t.Fatalf("bad password accepted: %q", p)
		}
	}
}
