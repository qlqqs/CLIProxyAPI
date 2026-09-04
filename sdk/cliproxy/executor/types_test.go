package executor

import (
	"reflect"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCredentialScopeNormalizesAndCopiesAuthIDs(t *testing.T) {
	authIDs := []string{" auth-b ", "auth-a", "auth-b", "", "  "}
	scope := NewCredentialScope(authIDs...)
	authIDs[0] = "mutated"

	if !scope.Enforced() {
		t.Fatal("CredentialScope.Enforced() = false, want true")
	}
	if got, want := scope.IDs(), []string{"auth-a", "auth-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CredentialScope.IDs() = %#v, want %#v", got, want)
	}
	if !scope.Allows(" auth-a ") || !scope.Allows("auth-b") {
		t.Fatal("CredentialScope.Allows() rejected a normalized member")
	}
	if scope.Allows("mutated") || scope.Allows("") {
		t.Fatal("CredentialScope.Allows() accepted an ID outside the scope")
	}

	ids := scope.IDs()
	ids[0] = "changed"
	if got, want := scope.IDs(), []string{"auth-a", "auth-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CredentialScope.IDs() after caller mutation = %#v, want %#v", got, want)
	}
}

func TestCredentialScopeNilAndEmptySemantics(t *testing.T) {
	var unrestricted *CredentialScope
	if unrestricted.Enforced() {
		t.Fatal("nil CredentialScope.Enforced() = true, want false")
	}
	if !unrestricted.Allows("any-auth") {
		t.Fatal("nil CredentialScope.Allows() = false, want unrestricted legacy behavior")
	}

	empty := NewCredentialScope()
	if !empty.Enforced() {
		t.Fatal("empty CredentialScope.Enforced() = false, want true")
	}
	if empty.Allows("any-auth") {
		t.Fatal("empty CredentialScope.Allows() = true, want deny all")
	}
	if got := empty.IDs(); got != nil {
		t.Fatalf("empty CredentialScope.IDs() = %#v, want nil", got)
	}
}

func TestResponseFormatOrSourceUsesExplicitResponseFormat(t *testing.T) {
	opts := Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatClaude,
	}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatClaude {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatClaude)
	}
}

func TestResponseFormatOrSourceFallsBackToSourceFormat(t *testing.T) {
	opts := Options{SourceFormat: sdktranslator.FormatGemini}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatGemini {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatGemini)
	}
}
