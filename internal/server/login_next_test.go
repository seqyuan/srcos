package server

import "testing"

func TestValidLoginNextKeepsDeepLinksAndRejectsRedirects(t *testing.T) {
	for _, ok := range []string{
		"/",
		"/tasks/alice-hello-abc",
		"/view?src=srcos%3A%2F%2Ffile%2Fhome%2Fnotes.md",
		"/admin",
		"/proxy/alice/jupyter/",
	} {
		if !validLoginNext(ok) {
			t.Errorf("validLoginNext(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",
		"//evil.example",
		"https://evil.example/",
		"/\\evil.example",
		"relative/path",
		"/path\r\nSet-Cookie: x=y",
	} {
		if validLoginNext(bad) {
			t.Errorf("validLoginNext(%q) = true, want false", bad)
		}
	}
}
