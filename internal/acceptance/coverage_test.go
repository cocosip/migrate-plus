package acceptance

import (
	"errors"
	"reflect"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

const testTableKind = "TABLE"

func TestPublicMethodsRemainMapped(t *testing.T) {
	for prefix, typ := range map[string]reflect.Type{"migrate.": reflect.TypeOf((*migrate.Migrate)(nil)), "driver.": reflect.TypeOf((*database.Driver)(nil)).Elem()} {
		for i := 0; i < typ.NumMethod(); i++ {
			operation := prefix + typ.Method(i).Name
			if len(apiCases[operation]) == 0 {
				t.Errorf("upstream operation %s has no regression mapping", operation)
			}
		}
	}
	if err := VerifyManifest(); err != nil {
		t.Fatal(err)
	}
}

func TestExpectedNativeDiagnosticRejectsUnrelatedFailures(t *testing.T) {
	for _, err := range []error{nil, errors.New("permission denied"), errors.New("connection reset")} {
		if expectDiagnostic(err, "corrupt migration history") == nil {
			t.Fatalf("wrong failure accepted: %v", err)
		}
	}
	if err := expectDiagnostic(errors.New("dm: corrupt migration history"), "corrupt migration history"); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedInventoryRefusesUnknownRoot(t *testing.T) {
	if err := checkOwned([]object{{Name: "mp_type", Kind: testTableKind}, {Name: "customer_data", Kind: testTableKind}}); err == nil {
		t.Fatal("foreign object allowed destructive cleanup")
	}
	if err := checkOwned([]object{{Name: "MP_TYPE", Kind: testTableKind}}); err != nil {
		t.Fatal(err)
	}
}

func TestTypeMetadataRejectsWrongTypeAndLostPrecision(t *testing.T) {
	tc := typeCase{Name: "decimal", DDL: "DECIMAL(20,4)"}
	for _, column := range []columnInfo{{Type: "VARCHAR", Nullable: true, Precision: 20, Scale: 4}, {Type: typeNumber, Nullable: true, Precision: 19, Scale: 4}, {Type: typeNumber, Nullable: false, Precision: 20, Scale: 4}} {
		if verifyTypeMetadata(tc, column) == nil {
			t.Fatalf("wrong metadata accepted: %+v", column)
		}
	}
	if err := verifyTypeMetadata(tc, columnInfo{Type: "numeric", Nullable: true, Precision: 20, Scale: 4}); err != nil {
		t.Fatal(err)
	}
	if !nativeTypeMatches("TIMESTAMP(6) WITH TIME ZONE", "timestamp with time zone") || nativeTypeMatches("TIMESTAMP WITH TIME ZONE", "timestamp without time zone") {
		t.Fatal("timezone metadata distinction lost")
	}
}
