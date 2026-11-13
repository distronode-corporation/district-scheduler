package branding

import (
	"testing"

	"github.com/calnode/calnode/internal/mailer"
)

func TestApplyTo_assetURLs(t *testing.T) {
	for _, tc := range []struct {
		name, stored, base, want string
	}{
		{"relative joins the base", "/branding/logo?v=1", "https://book.acme.example", "https://book.acme.example/branding/logo?v=1"},
		{"trailing slash on the base", "/branding/logo?v=1", "https://book.acme.example/", "https://book.acme.example/branding/logo?v=1"},
		{"absolute is left alone", "https://cdn.example/logo.png", "https://book.acme.example", "https://cdn.example/logo.png"},
		{"unset stays unset", "", "https://book.acme.example", ""},
		{"relative with no base is dropped", "/branding/logo?v=1", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d mailer.BookingData
			Settings{BusinessName: "Acme", LogoURL: tc.stored, BannerURL: tc.stored, LogoHeight: 40}.ApplyTo(&d, tc.base)
			if d.LogoURL != tc.want || d.BannerURL != tc.want {
				t.Errorf("logo %q, banner %q; want %q for both", d.LogoURL, d.BannerURL, tc.want)
			}
			if d.BrandName != "Acme" || d.LogoHeight != 40 {
				t.Errorf("brand %q height %d; want Acme and 40", d.BrandName, d.LogoHeight)
			}
		})
	}
}
