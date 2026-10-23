package acceptance

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCaseFailureBlocksDependentsButNotIndependentCases(t *testing.T) {
	s := &suite{ctx: context.Background(), report: TargetReport{Name: "test"}}
	s.run("first", nil, func() error { return errors.New("broken") })
	called := false
	s.run("dependent", []string{"first"}, func() error { called = true; return nil })
	s.run("independent", nil, func() error { return nil })
	if called || s.report.Results[1].Status != Blocked || s.report.Results[2].Status != Pass {
		t.Fatalf("results: %+v", s.report.Results)
	}
}

func TestNegativeAssertionDoesNotAcceptWrongError(t *testing.T) {
	if expectError(errors.New("permission denied"), "missing-object") == nil {
		t.Fatal("permission failure counted as missing table")
	}
	if err := expectError(errors.New("ORA-00942: table or view does not exist"), "missing-object"); err != nil {
		t.Fatal(err)
	}
	if expectError(nil, "missing-object") == nil {
		t.Fatal("successful bad SQL accepted")
	}
}

func TestRedactionRemovesEncodedAndRawCredentials(t *testing.T) {
	dsn := "oracle://u:p%40ss%2Bword@host/service?password=second%21"
	clean := redactConnection("failed "+dsn+" p@ss+word second!", dsn)
	for _, secret := range []string{dsn, "p@ss+word", "second!"} {
		if strings.Contains(clean, secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
	raw := "dm://u:p@ss?word@host:5236?schema=MP_TEST"
	if clean := redactConnection("native error: p@ss?word", raw); strings.Contains(clean, "p@ss?word") {
		t.Fatal("raw DM password leaked")
	}
}
