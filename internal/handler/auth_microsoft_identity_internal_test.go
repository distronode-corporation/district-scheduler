package handler

import "testing"

func TestMicrosoftIdentityEmail(t *testing.T) {
	cases := []struct {
		name      string
		mail, upn string
		want      string
		wantErr   bool
	}{
		{"mail on the UPN's domain is used", "Jane.Doe@Contoso.com", "jdoe@contoso.com", "jane.doe@contoso.com", false},
		{"no mail falls back to the UPN", "", "jdoe@contoso.com", "jdoe@contoso.com", false},
		// mail is settable by a tenant admin on an account without a mailbox, so an
		// address on a domain the tenant has not verified is not an identity.
		{"mail on another domain is ignored", "ceo@victim.example", "attacker@attacker.onmicrosoft.com", "attacker@attacker.onmicrosoft.com", false},
		{"guest UPN is refused", "", "user_victim.example#EXT#@tenant.onmicrosoft.com", "", true},
		{"guest UPN is refused even with a mail", "user@victim.example", "user_victim.example#EXT#@tenant.onmicrosoft.com", "", true},
		{"mail without a UPN is not trusted", "ceo@victim.example", "", "", true},
		{"nothing at all", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := microsoftIdentityEmail(microsoftUserInfo{Mail: tc.mail, UserPrincipalName: tc.upn})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v; wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestEmailDomain(t *testing.T) {
	for in, want := range map[string]string{"a@b.c": "b.c", "a@": "", "nodomain": "", "a@b@c.d": "c.d"} {
		if got := emailDomain(in); got != want {
			t.Errorf("emailDomain(%q) = %q; want %q", in, got, want)
		}
	}
}
