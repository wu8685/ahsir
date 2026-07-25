package obs

import "testing"

func TestResult_ValidAndTaxonomyComplete(t *testing.T) {
	// Exactly the §7 taxonomy, no more no less.
	want := map[Result]bool{
		ResultDone: true, ResultBusy: true, ResultCancel: true,
		ResultTimeout: true, ResultEvict: true, ResultProviderError: true,
		ResultUpstreamError: true, ResultInternalError: true,
	}
	if len(AllResults) != len(want) {
		t.Fatalf("AllResults has %d entries, want %d", len(AllResults), len(want))
	}
	for _, r := range AllResults {
		if !want[r] {
			t.Fatalf("unexpected result in taxonomy: %q", r)
		}
		if !r.Valid() {
			t.Fatalf("AllResults member %q reports !Valid()", r)
		}
	}
	if Result("bogus").Valid() {
		t.Fatal("bogus result must be invalid")
	}
}

func TestResult_String(t *testing.T) {
	if ResultProviderError.String() != "provider_error" {
		t.Fatalf("String() = %q", ResultProviderError.String())
	}
}

func TestRegistry_RegistererAccessor(t *testing.T) {
	reg := NewRegistry()
	if reg.Registerer() == nil {
		t.Fatal("Registerer() returned nil")
	}
}

func TestResult_BusyIsNotAnError(t *testing.T) {
	// The whole point of §7: backpressure must never be counted as failure.
	nonErrors := []Result{ResultDone, ResultBusy, ResultCancel, ResultTimeout, ResultEvict}
	for _, r := range nonErrors {
		if r.IsError() {
			t.Fatalf("%q must NOT be classified as an error", r)
		}
	}
	errs := []Result{ResultProviderError, ResultUpstreamError, ResultInternalError}
	for _, r := range errs {
		if !r.IsError() {
			t.Fatalf("%q must be classified as an error", r)
		}
	}
}
