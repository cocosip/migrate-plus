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

func TestDMNativeErrorClassification(t *testing.T) {
	for _, tc := range []struct{ class, message string }{
		{errorCheck, "Error -6604: 违反CHECK约束[CK]"},
		{errorForeignKey, "Error -6607: 违反引用约束[FK]"},
		{"object-exists", "Error -2116: 对象已存在"},
		{"object-exists", "Error -2124: 对象[MP_HISTORY]已存在"},
		{"overflow", "Error -6149: 数据转换丢失警告"},
	} {
		t.Run(tc.class+tc.message, func(t *testing.T) {
			if err := expectError(errors.New(tc.message), tc.class); err != nil {
				t.Fatal(err)
			}
			if expectError(errors.New("Error -5515: 没有权限"), tc.class) == nil {
				t.Fatal("permission failure accepted as expected rejection")
			}
			if expectError(nil, tc.class) == nil {
				t.Fatal("successful operation accepted as expected rejection")
			}
		})
	}
}

func TestRecordRedactsResolvedTargetDSNsCapturedOnce(t *testing.T) {
	t.Setenv("MIGRATE_ACCEPTANCE_REDACT_PEER", "oracle://u:peer-pass@host/service")
	s := &suite{ctx: context.Background(), all: []Target{{DSNEnv: "MIGRATE_ACCEPTANCE_REDACT_PEER"}}, report: TargetReport{Name: "test"}}
	s.resolveRedactions()
	// Removing the variable afterwards must not weaken redaction.
	t.Setenv("MIGRATE_ACCEPTANCE_REDACT_PEER", "")
	s.record("case", Fail, errors.New("connect failed for peer-pass"), 0)
	if strings.Contains(s.report.Results[0].Detail, "peer-pass") {
		t.Fatalf("peer DSN secret leaked: %s", s.report.Results[0].Detail)
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
