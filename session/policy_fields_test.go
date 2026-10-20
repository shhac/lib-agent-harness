package session

import (
	"reflect"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Every Policy field has exactly one owning engine, so a new field cannot be
// added without saying which engine reads it, and the table's set test reads
// the field it names.
func TestEveryPolicyFieldHasOneOwner(t *testing.T) {
	owners := map[string]harness.Engine{}
	for _, field := range policyFields {
		if _, dup := owners[field.name]; dup {
			t.Errorf("Policy.%s is listed twice", field.name)
		}
		owners[field.name] = field.owner
		if _, ok := harness.Describe(field.owner); !ok {
			t.Errorf("Policy.%s is owned by an unknown engine %q", field.name, field.owner)
		}
	}
	typ := reflect.TypeOf(Policy{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if _, ok := owners[name]; !ok {
			t.Errorf("Policy.%s has no owning engine in policyFields", name)
			continue
		}
		var p Policy
		value := reflect.ValueOf(&p).Elem().Field(i)
		switch value.Kind() {
		case reflect.String:
			value.SetString("x")
		case reflect.Slice:
			value.Set(reflect.MakeSlice(value.Type(), 0, 0))
		}
		for _, field := range policyFields {
			if got := field.set(p); got != (field.name == name) {
				t.Errorf("setting only Policy.%s: policyFields reports %s set=%v", name, field.name, got)
			}
		}
	}
	if len(owners) != typ.NumField() {
		t.Errorf("policyFields lists %d fields, Policy has %d", len(owners), typ.NumField())
	}
}
