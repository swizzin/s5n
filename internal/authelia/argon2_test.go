package authelia

import (
	"strings"
	"testing"
)

// TestAutheliaTemplateVectorVerifies is the one thing box cannot learn from
// its own code: that its verifier reads the same digest Authelia writes.
//
// The digest is Authelia v4.39.28's own users_database template row
// (internal/authentication/users_database.template.yml) for the password
// "authelia". It uses template parameters (m=32768,t=1,p=8), not box's, which
// is the point: the parameters travel with the digest.
func TestAutheliaTemplateVectorVerifies(t *testing.T) {
	const digest = `$argon2id$v=19$m=32768,t=1,p=8$eUhVT1dQa082YVk2VUhDMQ$E8QI4jHbUBt3EdsU1NFDu4Bq5jObKNx7nBKSn1EYQxk`

	ok, err := VerifyPassword(digest, "authelia")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("Authelia's own template vector did not verify: box would write logins Authelia cannot check")
	}
	if ok, err := VerifyPassword(digest, "not-the-password"); err != nil || ok {
		t.Fatalf("a wrong password verified (err %v): box's verifier is not checking the key", err)
	}
}

// TestHashPasswordMatchesAutheliaSchema: Authelia validates the whole users
// file on load against a schema regex, so a digest box writes that does not
// match it fails every login in the file, not one.
func TestHashPasswordMatchesAutheliaSchema(t *testing.T) {
	digest, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !DigestPattern.MatchString(digest) {
		t.Fatalf("digest %q does not match Authelia's schema pattern", digest)
	}
	// The parameters are Authelia's defaults, which are also what
	// configuration.yml tells it to hash with: a row hashed with other
	// parameters still verifies, but a rehash on a future login would move
	// the row to another cost for no reason.
	if !strings.HasPrefix(digest, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("digest %q does not start with box's declared parameters", digest)
	}
	// Salt and key are base64 without padding: the schema's alphabet has no
	// '=', and a padded digest is refused by Authelia's regex.
	if strings.Contains(strings.Join(strings.Split(digest, "$")[4:], ""), "=") {
		t.Fatalf("digest %q is padded base64", digest)
	}
	ok, err := VerifyPassword(digest, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("a digest box wrote did not round-trip (ok %v, err %v)", ok, err)
	}
	if ok, _ := VerifyPassword(digest, "correct horse battery stapl"); ok {
		t.Fatal("a truncated password verified")
	}
	// Two hashes of one password differ: the salt is random, and a fixed one
	// would make every box's login hash identical.
	again, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if again == digest {
		t.Fatal("two hashes of one password are identical: the salt is not random")
	}
}

func TestVerifyPasswordRefusesWhatAutheliaWould(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"a bare password":   "hunter2",
		"no salt":           `$argon2id$v=19$m=65536,t=3,p=4$$key`,
		"padded base64":     `$argon2id$v=19$m=65536,t=3,p=4$c2FsdA==$a2V5`,
		"argon2 version 16": `$argon2id$v=16$m=65536,t=3,p=4$c2FsdA$a2V5`,
		"unknown field":     `$argon2id$v=19$m=65536,t=3,p=4,x=1$c2FsdA$a2V5`,
	}
	for name, digest := range cases {
		t.Run(name, func(t *testing.T) {
			if ok, err := VerifyPassword(digest, "hunter2"); err == nil {
				t.Fatalf("VerifyPassword(%q) = %v, nil; want an error", digest, ok)
			}
		})
	}
}
