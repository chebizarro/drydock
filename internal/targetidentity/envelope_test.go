package targetidentity

import "testing"

func TestEnvelopeHashBindsEveryFieldAndMaterials(t *testing.T) {
	e := New("repo", "ROOT", "PATCH", "sha256:remote", "BASE", "TIP", "", "diff", "bundle")
	first, err := e.Hash()
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	changed := e
	changed.RootID = "other"
	second, err := changed.Hash()
	if err != nil {
		t.Fatalf("hash changed envelope: %v", err)
	}
	if first == second {
		t.Fatal("envelope hash did not bind root_id")
	}
	if err := e.VerifyMaterials("other diff", "bundle"); err == nil {
		t.Fatal("expected diff mismatch")
	}
	if err := e.VerifyMaterials("diff", "other bundle"); err == nil {
		t.Fatal("expected bundle mismatch")
	}
}

func TestRemoteIdentityIsOrderIndependentAndCredentialSafe(t *testing.T) {
	a := RemoteIdentity([]string{"https://user:secret@example.com/repo.git/", "ssh://git@example.com/repo"})
	b := RemoteIdentity([]string{"ssh://git@example.com/repo", "https://user:secret@example.com/repo.git"})
	if a != b {
		t.Fatalf("remote identity depends on URL order: %q != %q", a, b)
	}
	if a == "" || a == "https://user:secret@example.com/repo.git" {
		t.Fatalf("remote identity is absent or exposes URL: %q", a)
	}
}
